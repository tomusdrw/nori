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
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "deploybot_session"
	csrfCookie    = "deploybot_csrf"
	sessionTTL    = 24 * time.Hour
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

func (a *Auth) Login(w http.ResponseWriter, r *http.Request, password string) error {
	if err := bcrypt.CompareHashAndPassword(a.passwordHash, []byte(password)); err != nil {
		return errors.New("invalid credentials")
	}
	token, err := a.signSession("admin", time.Now().Add(sessionTTL))
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		Expires:  time.Now().Add(sessionTTL),
	})
	csrf, err := randomToken(32)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    csrf,
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		Expires:  time.Now().Add(sessionTTL),
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
		user, err := a.userFromRequest(r)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), userKey, user)
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

func (a *Auth) userFromRequest(r *http.Request) (string, error) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", err
	}
	return a.verifySession(cookie.Value)
}

func (a *Auth) signSession(user string, exp time.Time) (string, error) {
	payload := fmt.Sprintf("%s|%d", user, exp.Unix())
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + sig)), nil
}

func (a *Auth) verifySession(token string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", err
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return "", errors.New("bad session")
	}
	user, expStr, sig := parts[0], parts[1], parts[2]
	payload := user + "|" + expStr
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte(payload))
	if !hmac.Equal([]byte(sig), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		return "", errors.New("bad signature")
	}
	var exp int64
	fmt.Sscanf(expStr, "%d", &exp)
	if time.Now().Unix() > exp {
		return "", errors.New("session expired")
	}
	return user, nil
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
