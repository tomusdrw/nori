package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"deploybot/internal/auth"
	"deploybot/internal/docker"
	"deploybot/internal/executor"
	"deploybot/internal/notify"
	"deploybot/internal/poller"
	"deploybot/internal/store"
)

func TestSettings_CustomBotNameInTitleAndBrand(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	ex := executor.New(st, &executor.OSRunner{}, func(context.Context, string) (string, error) { return "", nil }, 0)
	pl := poller.New(st, func(context.Context, string) (string, error) { return "", nil }, ex, 0)
	srv := NewServer(st, &docker.Fake{}, ex, pl, a)

	cookies := loginCookies(t, srv)

	// Default name on dashboard
	body := getAuthed(t, srv, cookies, "/")
	if !strings.Contains(body, "<title>Services · Nori</title>") {
		t.Fatalf("expected default title, got:\n%s", body)
	}
	if !strings.Contains(body, ">Nori</span>") {
		t.Fatalf("expected default brand, got:\n%s", body)
	}

	csrf := csrfFromBody(body)
	form := url.Values{"csrf_token": {csrf}, "bot_name": {"Staging Dubai"}}
	postAuthed(t, srv, cookies, "/settings", form.Encode())

	if got := st.BotName(context.Background()); got != "Staging Dubai" {
		t.Fatalf("BotName = %q, want Staging Dubai", got)
	}

	body = getAuthed(t, srv, cookies, "/")
	if !strings.Contains(body, "<title>Services · Staging Dubai</title>") {
		t.Fatalf("expected custom title, got:\n%s", body)
	}
	if !strings.Contains(body, ">Staging Dubai</span>") {
		t.Fatalf("expected custom brand, got:\n%s", body)
	}

	loginBody := getPublic(t, srv, "/login")
	if !strings.Contains(loginBody, "<title>Sign in · Staging Dubai</title>") {
		t.Fatalf("expected custom login title, got:\n%s", loginBody)
	}
}

func TestSettings_NotificationModeDefaultsToAlways(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	ex := executor.New(st, &executor.OSRunner{}, func(context.Context, string) (string, error) { return "", nil }, 0)
	pl := poller.New(st, func(context.Context, string) (string, error) { return "", nil }, ex, 0)
	srv := NewServer(st, &docker.Fake{}, ex, pl, a)
	cookies := loginCookies(t, srv)

	body := getAuthed(t, srv, cookies, "/settings")
	// "always" must be the pre-selected option when nothing is stored yet.
	if !strings.Contains(body, `<option value="always" selected`) {
		t.Fatalf("expected 'always' to be selected by default; body snippet:\n%s", substring(body, "notify-mode"))
	}
}

func TestSettings_PostPersistsNotificationMode(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	ex := executor.New(st, &executor.OSRunner{}, func(context.Context, string) (string, error) { return "", nil }, 0)
	pl := poller.New(st, func(context.Context, string) (string, error) { return "", nil }, ex, 0)
	srv := NewServer(st, &docker.Fake{}, ex, pl, a)
	cookies := loginCookies(t, srv)

	csrf := csrfFromBody(getAuthed(t, srv, cookies, "/settings"))
	form := url.Values{
		"csrf_token":  {csrf},
		"bot_name":    {"Nori"},
		"notify_mode": {"auto-only"},
	}
	postAuthed(t, srv, cookies, "/settings", form.Encode())

	if got := st.NotifyMode(context.Background()); got != "auto-only" {
		t.Fatalf("NotifyMode = %q, want auto-only", got)
	}

	body := getAuthed(t, srv, cookies, "/settings")
	if !strings.Contains(body, `<option value="auto-only" selected`) {
		t.Fatalf("expected auto-only to be selected after save; body snippet:\n%s", substring(body, "notify-mode"))
	}
}

func TestSettings_PostRejectsInvalidNotificationMode(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	ex := executor.New(st, &executor.OSRunner{}, func(context.Context, string) (string, error) { return "", nil }, 0)
	pl := poller.New(st, func(context.Context, string) (string, error) { return "", nil }, ex, 0)
	srv := NewServer(st, &docker.Fake{}, ex, pl, a)
	cookies := loginCookies(t, srv)

	csrf := csrfFromBody(getAuthed(t, srv, cookies, "/settings"))
	form := url.Values{
		"csrf_token":  {csrf},
		"bot_name":    {"Nori"},
		"notify_mode": {"txt-me-instead"},
	}
	// Invalid mode must NOT 3xx-redirect; it re-renders the form with an error.
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (re-rendered form)", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "Couldn’t save") {
		t.Fatalf("expected error banner in body; got:\n%s", rr.Body.String())
	}
	if got := st.NotifyMode(context.Background()); got != "" {
		t.Fatalf("invalid mode must not persist; got %q", got)
	}
	if _, err := notify.NormalizeMode("txt-me-instead"); err == nil {
		t.Fatal("NormalizeMode should reject unknown value")
	}
}

// substring returns a small window around the first occurrence of needle in s,
// to make test failure output readable when the page is large.
func substring(s, needle string) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return "(not found)"
	}
	start := i - 200
	if start < 0 {
		start = 0
	}
	end := i + 400
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}

func loginCookies(t *testing.T, srv *Server) []*http.Cookie {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("password=test"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d", rr.Code)
	}
	return rr.Result().Cookies()
}

func getAuthed(t *testing.T, srv *Server, cookies []*http.Cookie, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d body=%s", path, rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

func getPublic(t *testing.T, srv *Server, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d", path, rr.Code)
	}
	return rr.Body.String()
}

func postAuthed(t *testing.T, srv *Server, cookies []*http.Cookie, path, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d body=%s", path, rr.Code, rr.Body.String())
	}
}

func csrfFromBody(body string) string {
	const marker = `name="csrf_token" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		const meta = `name="csrf-token" content="`
		i = strings.Index(body, meta)
		if i < 0 {
			return ""
		}
		rest := body[i+len(meta):]
		end := strings.Index(rest, `"`)
		return rest[:end]
	}
	rest := body[i+len(marker):]
	end := strings.Index(rest, `"`)
	return rest[:end]
}
