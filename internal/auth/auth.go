package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "deploybot_session"
	csrfCookie    = "deploybot_csrf"
	sessionTTL    = 24 * time.Hour
	rememberTTL   = 30 * 24 * time.Hour
)

type contextKey string

const userKey contextKey = "user"

type Auth struct {
	passwordHash []byte
	sessionKey   []byte
}

func New(passwordHashBcrypt string, sessionKey []byte) (*Auth, error) {
	if passwordHashBcrypt == "" {
		return nil, errors.New("admin password hash is required")
	}
	if len(sessionKey) < 32 {
		return nil, errors.New("session key must be at least 32 bytes")
	}
	return &Auth{passwordHash: []byte(passwordHashBcrypt), sessionKey: sessionKey}, nil
}

func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func (a *Auth) Login(w http.ResponseWriter, r *http.Request, password string, remember bool) error {
	if err := bcrypt.CompareHashAndPassword(a.passwordHash, []byte(password)); err != nil {
		return errors.New("invalid credentials")
	}
	ttl := sessionTTL
	if remember {
		ttl = rememberTTL
	}
	csrf, err := randomToken(32)
	if err != nil {
		return err
	}
	return a.writeSession(w, r.TLS != nil, "admin", ttl, csrf)
}

// writeSession (re)issues the session and CSRF cookies with a fresh idle window
// of ttl. Both cookies share the same expiry so a live session never starts
// failing CSRF checks. The ttl is baked into the signed token so the sliding
// refresh knows how far to extend. Callers supply the CSRF value: Login mints a
// new one; the sliding refresh reuses the existing one so in-flight forms stay
// valid.
func (a *Auth) writeSession(w http.ResponseWriter, secure bool, user string, ttl time.Duration, csrf string) error {
	exp := time.Now().Add(ttl)
	token, err := a.signSession(user, ttl, exp)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		Expires:  exp,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    csrf,
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		Expires:  exp,
	})
	return nil
}

func (a *Auth) Logout(w http.ResponseWriter) {
	clearCookie(w, sessionCookie)
	clearCookie(w, csrfCookie)
}

func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" || strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		sess, err := a.userFromRequest(r)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		// Sliding expiry: once a session is past the halfway point of its idle
		// window, re-issue it so continued activity keeps it alive. Refreshing
		// only in the second half avoids setting a cookie on every request
		// (the dashboard polls every few seconds).
		if time.Until(sess.exp) < sess.ttl/2 {
			csrf := a.CSRFToken(r)
			if csrf == "" {
				if csrf, err = randomToken(32); err != nil {
					http.Error(w, "session error", http.StatusInternalServerError)
					return
				}
			}
			if err := a.writeSession(w, r.TLS != nil, sess.user, sess.ttl, csrf); err != nil {
				http.Error(w, "session error", http.StatusInternalServerError)
				return
			}
		}
		ctx := context.WithValue(r.Context(), userKey, sess.user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *Auth) CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/login" {
			next.ServeHTTP(w, r)
			return
		}
		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
			token = r.FormValue("csrf_token")
		}
		cookie, err := r.Cookie(csrfCookie)
		if err != nil || cookie.Value == "" || token != cookie.Value {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Auth) CSRFToken(r *http.Request) string {
	cookie, err := r.Cookie(csrfCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// session is a verified, unexpired session decoded from the cookie token. ttl
// is the idle window the session was issued with; exp is when it currently
// lapses. The sliding refresh uses both to decide whether, and how far, to
// extend.
type session struct {
	user string
	ttl  time.Duration
	exp  time.Time
}

func (a *Auth) userFromRequest(r *http.Request) (session, error) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, err
	}
	return a.verifySession(cookie.Value)
}

func (a *Auth) signSession(user string, ttl time.Duration, exp time.Time) (string, error) {
	payload := fmt.Sprintf("%s|%d|%d", user, int64(ttl.Seconds()), exp.Unix())
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + sig)), nil
}

func (a *Auth) verifySession(token string) (session, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return session{}, err
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 4 {
		return session{}, errors.New("bad session")
	}
	user, ttlStr, expStr, sig := parts[0], parts[1], parts[2], parts[3]
	payload := user + "|" + ttlStr + "|" + expStr
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte(payload))
	if !hmac.Equal([]byte(sig), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		return session{}, errors.New("bad signature")
	}
	ttlSecs, err := strconv.ParseInt(ttlStr, 10, 64)
	if err != nil || ttlSecs <= 0 {
		return session{}, errors.New("bad session ttl")
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return session{}, errors.New("bad session exp")
	}
	if time.Now().Unix() > exp {
		return session{}, errors.New("session expired")
	}
	return session{user: user, ttl: time.Duration(ttlSecs) * time.Second, exp: time.Unix(exp, 0)}, nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1})
}
