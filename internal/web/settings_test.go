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
