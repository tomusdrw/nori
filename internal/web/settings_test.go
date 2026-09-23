package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"nori/internal/auth"
	"nori/internal/docker"
	"nori/internal/executor"
	"nori/internal/notify"
	"nori/internal/poller"
	"nori/internal/store"
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
	srv := NewServer(st, &docker.Fake{}, ex, pl, a, Channels{})

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

func TestSettings_NotificationMatrixDefaultsToAllEvents(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	ex := executor.New(st, &executor.OSRunner{}, func(context.Context, string) (string, error) { return "", nil }, 0)
	pl := poller.New(st, func(context.Context, string) (string, error) { return "", nil }, ex, 0)
	srv := NewServer(st, &docker.Fake{}, ex, pl, a, Channels{Twilio: true, Telegram: true})
	cookies := loginCookies(t, srv)

	body := getAuthed(t, srv, cookies, "/settings")
	// A fresh install routes every event to every configured channel,
	// matching the historical default behavior.
	for _, ch := range []string{"twilio", "telegram"} {
		for _, kind := range []string{"deploy_failed", "down", "recovered", "success"} {
			want := fmt.Sprintf(`name="notify_%s_%s" value="1" checked`, ch, kind)
			if !strings.Contains(body, want) {
				t.Fatalf("expected %s/%s checked by default; snippet:\n%s", ch, kind, substring(body, "notify_"+ch+"_"+kind))
			}
		}
	}
	if !strings.Contains(body, "Service down or unhealthy") {
		t.Fatal("notification matrix must expose monitored outages")
	}
}

func TestSettings_ShowsChannelConfigurationStatus(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	ex := executor.New(st, &executor.OSRunner{}, func(context.Context, string) (string, error) { return "", nil }, 0)
	pl := poller.New(st, func(context.Context, string) (string, error) { return "", nil }, ex, 0)
	srv := NewServer(st, &docker.Fake{}, ex, pl, a, Channels{Twilio: true, Telegram: false})
	cookies := loginCookies(t, srv)

	body := getAuthed(t, srv, cookies, "/settings")
	if !strings.Contains(body, "Configured") {
		t.Fatal("expected a Configured badge for the Twilio channel")
	}
	if !strings.Contains(body, "Not configured") {
		t.Fatal("expected a Not configured badge for the Telegram channel")
	}
	// Checkboxes of unconfigured channels render disabled so a configured
	// look cannot deceive, and disabled inputs cannot submit values.
	if !strings.Contains(body, `name="notify_telegram_down" value="1" disabled`) {
		t.Fatalf("expected telegram checkboxes disabled; snippet:\n%s", substring(body, "notify_telegram_down"))
	}
}

func TestSettings_MatrixReflectsStoredRouting(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	ex := executor.New(st, &executor.OSRunner{}, func(context.Context, string) (string, error) { return "", nil }, 0)
	pl := poller.New(st, func(context.Context, string) (string, error) { return "", nil }, ex, 0)
	srv := NewServer(st, &docker.Fake{}, ex, pl, a, Channels{Twilio: true, Telegram: true})
	cookies := loginCookies(t, srv)

	if err := st.SetSetting(context.Background(), store.SettingNotifyRouting,
		`{"twilio":{"deploy_failed":false,"down":true,"recovered":false,"success":false},"telegram":{"deploy_failed":true,"down":false,"recovered":false,"success":true}}`); err != nil {
		t.Fatal(err)
	}
	body := getAuthed(t, srv, cookies, "/settings")
	if !strings.Contains(body, `name="notify_twilio_down" value="1" checked`) {
		t.Fatal("expected twilio/down checked")
	}
	if strings.Contains(body, `name="notify_twilio_deploy_failed" value="1" checked`) {
		t.Fatal("expected twilio/deploy_failed unchecked independently of down")
	}
	if strings.Contains(body, `name="notify_twilio_recovered" value="1" checked`) {
		t.Fatal("expected twilio/recovered unchecked")
	}
	if strings.Contains(body, `name="notify_telegram_down" value="1" checked`) {
		t.Fatal("expected telegram/down unchecked")
	}
	if !strings.Contains(body, `name="notify_telegram_deploy_failed" value="1" checked`) {
		t.Fatal("expected telegram/deploy_failed checked independently of down")
	}
	if !strings.Contains(body, `name="notify_telegram_success" value="1" checked`) {
		t.Fatal("expected telegram/success checked")
	}
}

func TestSettings_MatrixFallsBackToLegacyMode(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	ex := executor.New(st, &executor.OSRunner{}, func(context.Context, string) (string, error) { return "", nil }, 0)
	pl := poller.New(st, func(context.Context, string) (string, error) { return "", nil }, ex, 0)
	srv := NewServer(st, &docker.Fake{}, ex, pl, a, Channels{Twilio: true, Telegram: true})
	cookies := loginCookies(t, srv)

	// An installation that had opted out via the legacy mode stays opted out
	// until the routing table is saved for the first time.
	if err := st.SetSetting(context.Background(), store.SettingNotifyMode, "never"); err != nil {
		t.Fatal(err)
	}
	body := getAuthed(t, srv, cookies, "/settings")
	if strings.Contains(body, `name="notify_twilio_down" value="1" checked`) {
		t.Fatal("legacy never must render all checkboxes unchecked")
	}
	if strings.Contains(body, `name="notify_telegram_success" value="1" checked`) {
		t.Fatal("legacy never must render all checkboxes unchecked")
	}
}

func TestSettings_PostPersistsRoutingAndClearsLegacyMode(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	ex := executor.New(st, &executor.OSRunner{}, func(context.Context, string) (string, error) { return "", nil }, 0)
	pl := poller.New(st, func(context.Context, string) (string, error) { return "", nil }, ex, 0)
	srv := NewServer(st, &docker.Fake{}, ex, pl, a, Channels{Twilio: true, Telegram: true})
	cookies := loginCookies(t, srv)

	// A legacy choice exists from before the upgrade.
	if err := st.SetSetting(context.Background(), store.SettingNotifyMode, "never"); err != nil {
		t.Fatal(err)
	}

	csrf := csrfFromBody(getAuthed(t, srv, cookies, "/settings"))
	form := url.Values{
		"csrf_token":                    {csrf},
		"bot_name":                      {"Nori"},
		"notify_twilio_down":            {"1"},
		"notify_twilio_recovered":       {"1"},
		"notify_telegram_deploy_failed": {"1"},
		"notify_telegram_recovered":     {"1"},
		"notify_telegram_success":       {"1"},
	}
	postAuthed(t, srv, cookies, "/settings", form.Encode())

	raw := st.NotifyRoutingRaw(context.Background())
	if raw == "" {
		t.Fatal("POST must persist the routing table")
	}
	routing, err := notify.ParseRouting(raw)
	if err != nil {
		t.Fatalf("stored routing must parse: %v", err)
	}
	if !routing.Allowed("twilio", "down") || !routing.Allowed("twilio", "recovered") {
		t.Errorf("twilio down/recovered must be on: %v", raw)
	}
	if routing.Allowed("twilio", "success") {
		t.Errorf("twilio success was unchecked: %v", raw)
	}
	if routing.Allowed("twilio", "deploy_failed") {
		t.Errorf("twilio deploy_failed was unchecked: %v", raw)
	}
	if routing.Allowed("telegram", "down") {
		t.Errorf("telegram down was unchecked: %v", raw)
	}
	if !routing.Allowed("telegram", "deploy_failed") {
		t.Errorf("telegram deploy_failed must be on: %v", raw)
	}
	if !routing.Allowed("telegram", "recovered") || !routing.Allowed("telegram", "success") {
		t.Errorf("telegram recovered/success must be on: %v", raw)
	}
	if got := st.NotifyMode(context.Background()); got != "" {
		t.Fatalf("saving routing must clear the legacy notify_mode; got %q", got)
	}

	// The saved matrix renders back with exactly the saved state.
	body := getAuthed(t, srv, cookies, "/settings")
	if !strings.Contains(body, `name="notify_twilio_down" value="1" checked`) {
		t.Fatal("expected twilio/down checked after save")
	}
	if strings.Contains(body, `name="notify_twilio_deploy_failed" value="1" checked`) {
		t.Fatal("expected twilio/deploy_failed unchecked after save")
	}
	if strings.Contains(body, `name="notify_twilio_success" value="1" checked`) {
		t.Fatal("expected twilio/success unchecked after save")
	}
}

func TestSettings_ZeroChannelsSaveKeepsLegacyOptOut(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	ex := executor.New(st, &executor.OSRunner{}, func(context.Context, string) (string, error) { return "", nil }, 0)
	pl := poller.New(st, func(context.Context, string) (string, error) { return "", nil }, ex, 0)
	// No channels are configured: the matrix offers no editable checkboxes,
	// so a save must not silently discard a legacy opt-out the operator
	// cannot currently re-express.
	srv := NewServer(st, &docker.Fake{}, ex, pl, a, Channels{})
	cookies := loginCookies(t, srv)
	if err := st.SetSetting(context.Background(), store.SettingNotifyMode, "never"); err != nil {
		t.Fatal(err)
	}

	csrf := csrfFromBody(getAuthed(t, srv, cookies, "/settings"))
	form := url.Values{"csrf_token": {csrf}, "bot_name": {"Nori"}}
	postAuthed(t, srv, cookies, "/settings", form.Encode())

	if got := st.NotifyMode(context.Background()); got != "never" {
		t.Fatalf("zero-channel save must keep the legacy opt-out; notify_mode = %q", got)
	}
	if raw := st.NotifyRoutingRaw(context.Background()); raw != "" {
		t.Fatalf("zero-channel save must not write a routing table; got %q", raw)
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
