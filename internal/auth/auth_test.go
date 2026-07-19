package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func newTestAuth(t *testing.T) *Auth {
	t.Helper()
	hash, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(hash, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func cookieByName(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestHashAndLogin(t *testing.T) {
	a := newTestAuth(t)
	if err := bcrypt.CompareHashAndPassword(a.passwordHash, []byte("secret")); err != nil {
		t.Fatal("password should match")
	}
	if err := bcrypt.CompareHashAndPassword(a.passwordHash, []byte("wrong")); err == nil {
		t.Fatal("wrong password should fail")
	}
}

func TestLoginCookieTTL(t *testing.T) {
	a := newTestAuth(t)

	tests := []struct {
		name     string
		remember bool
		wantTTL  time.Duration
	}{
		{"default session", false, sessionTTL},
		{"remember me", true, rememberTTL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/login", nil)
			start := time.Now()
			if err := a.Login(rec, req, "secret", tc.remember); err != nil {
				t.Fatalf("login: %v", err)
			}

			cookies := rec.Result().Cookies()
			session := cookieByName(cookies, sessionCookie)
			csrf := cookieByName(cookies, csrfCookie)
			if session == nil || csrf == nil {
				t.Fatalf("missing cookies: session=%v csrf=%v", session, csrf)
			}
			// The session and CSRF cookies must expire together, otherwise a
			// remembered session would keep reading but fail every POST.
			if !session.Expires.Equal(csrf.Expires) {
				t.Errorf("session and csrf expiry differ: %v vs %v", session.Expires, csrf.Expires)
			}
			got := session.Expires.Sub(start)
			if got < tc.wantTTL-time.Minute || got > tc.wantTTL+time.Minute {
				t.Errorf("cookie TTL = %v, want ~%v", got, tc.wantTTL)
			}
		})
	}
}

// TestSlidingRefresh checks that a session past the halfway point of its idle
// window is re-issued with a fresh full-length expiry, preserving its TTL.
func TestSlidingRefresh(t *testing.T) {
	a := newTestAuth(t)
	ttl := time.Hour
	// Only a quarter of the window remains, i.e. past the half-life threshold.
	token, err := a.signSession("admin", ttl, time.Now().Add(ttl/4))
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})

	served := false
	a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
	})).ServeHTTP(rec, req)

	if !served {
		t.Fatal("authenticated handler was not reached")
	}
	refreshed := cookieByName(rec.Result().Cookies(), sessionCookie)
	if refreshed == nil {
		t.Fatal("expected a refreshed session cookie")
	}
	sess, err := a.verifySession(refreshed.Value)
	if err != nil {
		t.Fatalf("refreshed token invalid: %v", err)
	}
	if sess.ttl != ttl {
		t.Errorf("refreshed ttl = %v, want %v", sess.ttl, ttl)
	}
	if remaining := time.Until(sess.exp); remaining < ttl-time.Minute {
		t.Errorf("refreshed session has %v remaining, want ~%v", remaining, ttl)
	}
}

// TestSlidingNoRefreshWhenFresh checks that a session still in the first half
// of its window is not re-issued, so most requests set no cookie.
func TestSlidingNoRefreshWhenFresh(t *testing.T) {
	a := newTestAuth(t)
	ttl := time.Hour
	token, err := a.signSession("admin", ttl, time.Now().Add(ttl))
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})

	a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(rec, req)

	if c := cookieByName(rec.Result().Cookies(), sessionCookie); c != nil {
		t.Errorf("fresh session should not be refreshed, got Set-Cookie %q", c.Value)
	}
}

// TestExpiredSessionRejected guards the core expiry check the sliding logic
// relies on.
func TestExpiredSessionRejected(t *testing.T) {
	a := newTestAuth(t)
	token, err := a.signSession("admin", time.Hour, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.verifySession(token); err == nil {
		t.Fatal("expired session should be rejected")
	}
}
