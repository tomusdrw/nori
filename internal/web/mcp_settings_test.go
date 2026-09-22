package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
	"nori/internal/auth"
	"nori/internal/docker"
	"nori/internal/executor"
	"nori/internal/poller"
	"nori/internal/store"
)

func mcpSettingsServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	ex := executor.New(st, &executor.OSRunner{}, func(context.Context, string) (string, error) { return "", nil }, 0)
	pl := poller.New(st, func(context.Context, string) (string, error) { return "", nil }, ex, 0)
	return NewServer(st, &docker.Fake{}, ex, pl, a, Channels{}), st
}

func TestMCPOAuthEndToEnd(t *testing.T) {
	srv, st := mcpSettingsServer(t)
	httpServer := httptest.NewServer(srv)
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := st.SetMCPConfig(ctx, true, httpServer.URL, false); err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, srv)
	call := func(method, path, body, contentType string, authenticated bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, httpServer.URL+path, strings.NewReader(body))
		r.Header.Set("Content-Type", contentType)
		if authenticated {
			r.Header.Set("Origin", httpServer.URL)
			for _, c := range cookies {
				r.AddCookie(c)
			}
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}
	registration := call("POST", "/oauth/register", `{"client_name":"Integration client","redirect_uris":["http://127.0.0.1:8765/callback"],"token_endpoint_auth_method":"none"}`, "application/json", false)
	if registration.Code != 201 {
		t.Fatalf("register: %d %s", registration.Code, registration.Body)
	}
	var client struct {
		ID string `json:"client_id"`
	}
	if err := json.Unmarshal(registration.Body.Bytes(), &client); err != nil {
		t.Fatal(err)
	}
	verifier := strings.Repeat("x", 43)
	hash := sha256.Sum256([]byte(verifier))
	params := url.Values{"client_id": {client.ID}, "redirect_uri": {"http://127.0.0.1:8765/callback"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "resource": {httpServer.URL + "/mcp"}, "state": {"test-state"}}
	// Omitted scope uses the advertised management permissions, not secrets.
	page := call("GET", "/oauth/authorize?"+params.Encode(), "", "", true)
	if page.Code != 200 {
		t.Fatalf("consent: %d %s", page.Code, page.Body)
	}
	params.Set("csrf_token", csrfFromBody(page.Body.String()))
	params.Set("decision", "allow")
	approved := call("POST", "/oauth/authorize", params.Encode(), "application/x-www-form-urlencoded", true)
	if approved.Code != 303 {
		t.Fatalf("approve: %d %s", approved.Code, approved.Body)
	}
	u, err := url.Parse(approved.Header().Get("Location"))
	if err != nil || u.Query().Get("state") != "test-state" {
		t.Fatalf("callback: %v %v", u, err)
	}
	token := call("POST", "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {u.Query().Get("code")}, "code_verifier": {verifier}, "client_id": {client.ID}, "redirect_uri": {"http://127.0.0.1:8765/callback"}, "resource": {httpServer.URL + "/mcp"}}.Encode(), "application/x-www-form-urlencoded", false)
	if token.Code != 200 {
		t.Fatalf("token: %d %s", token.Code, token.Body)
	}
	var credentials struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(token.Body.Bytes(), &credentials); err != nil {
		t.Fatal(err)
	}
	if credentials.Scope != "nori:read nori:write" {
		t.Fatalf("default scope: %q", credentials.Scope)
	}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "integration", Version: "1"}, nil)
	session, err := mcpClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL + "/mcp", HTTPClient: &http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: credentials.AccessToken})}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "create_service", Arguments: map[string]any{"name": "oauth-created", "watched_image": "nginx:latest", "deploy_script": "true"}})
	if err != nil || result.IsError {
		t.Fatalf("authenticated create: %+v %v", result, err)
	}
	svc, err := st.GetServiceByName(ctx, "oauth-created")
	if err != nil {
		t.Fatal(err)
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "get_service_environment", Arguments: map[string]any{"service_id": svc.ID}})
	if err != nil || result.IsError {
		t.Fatalf("default grant cannot read environment structure: %+v %v", result, err)
	}
	postAuthed(t, srv, cookies, "/settings/mcp/revoke", url.Values{"csrf_token": {csrfFromBody(page.Body.String())}}.Encode())
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_services", Arguments: map[string]any{}}); err == nil {
		t.Fatal("existing MCP connection survived grant revocation")
	}
}

func TestMCPSettingsRoutesAndRevocation(t *testing.T) {
	srv, st := mcpSettingsServer(t)
	cookies := loginCookies(t, srv)
	body := getAuthed(t, srv, cookies, "/settings")
	if !strings.Contains(body, `name="mcp_enabled"><option value="0" selected`) {
		t.Fatal("MCP must be disabled by default")
	}
	for _, path := range []string{"/mcp", "/oauth/authorize", "/oauth/token", "/oauth/register", "/oauth/revoke", "/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/mcp"} {
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 404 {
			t.Errorf("disabled %s = %d", path, rr.Code)
		}
	}
	form := url.Values{"bot_name": {"Nori"}, "notify_mode": {"always"}, "mcp_enabled": {"1"}, "mcp_public_url": {"https://nori.example"}, "csrf_token": {csrfFromBody(body)}}
	// Session cookies alone must never enable the server without CSRF proof.
	bad := url.Values{}
	for k, v := range form {
		bad[k] = v
	}
	bad.Del("csrf_token")
	r := httptest.NewRequest("POST", "/settings", strings.NewReader(bad.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, r)
	if rr.Code != 403 {
		t.Fatalf("settings without CSRF = %d", rr.Code)
	}
	postAuthed(t, srv, cookies, "/settings", form.Encode())
	cfg, _ := st.GetMCPConfig(context.Background())
	if !cfg.Enabled {
		t.Fatal("MCP not enabled")
	}
	rr = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "https://nori.example/mcp", strings.NewReader(`{}`))
	for _, c := range cookies {
		r.AddCookie(c)
	}
	srv.ServeHTTP(rr, r)
	if rr.Code != 401 || !strings.Contains(rr.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatalf("MCP cookie-only request: %d %s", rr.Code, rr.Body)
	}
	body = getAuthed(t, srv, cookies, "/settings")
	if !strings.Contains(body, "https://nori.example/mcp") {
		t.Fatal("settings omit MCP URL")
	}
	postAuthed(t, srv, cookies, "/settings/mcp/revoke", url.Values{"csrf_token": {csrfFromBody(body)}}.Encode())
	revoked, _ := st.GetMCPConfig(context.Background())
	if revoked.Epoch == cfg.Epoch || !revoked.Enabled {
		t.Fatal("revocation must invalidate grants without disabling MCP")
	}
	form.Set("mcp_public_url", "http://external.example")
	r = httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, r)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "MCP requires HTTPS") {
		t.Fatalf("insecure URL = %d %s", rr.Code, rr.Body)
	}
	after, _ := st.GetMCPConfig(context.Background())
	if after != revoked {
		t.Fatal("invalid settings changed MCP configuration")
	}
}

func TestMCPOAuthLoginReturnAndSecureCookies(t *testing.T) {
	srv, st := mcpSettingsServer(t)
	oldCookies := loginCookies(t, srv)
	if err := st.SetMCPConfig(context.Background(), true, "https://nori.example", false); err != nil {
		t.Fatal(err)
	}
	upgradeRequest := httptest.NewRequest("GET", "http://nori.example/oauth/authorize", nil)
	for _, c := range oldCookies {
		upgradeRequest.AddCookie(c)
	}
	upgrade := httptest.NewRecorder()
	srv.ServeHTTP(upgrade, upgradeRequest)
	if len(upgrade.Result().Cookies()) != 2 {
		t.Fatal("pre-existing session not upgraded for OAuth")
	}
	for _, c := range upgrade.Result().Cookies() {
		if !c.Secure {
			t.Fatal("pre-existing OAuth session cookie not Secure")
		}
	}
	next := "/oauth/authorize?client_id=test&state=a%26b"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest("GET", "https://nori.example"+next, nil))
	location, err := url.Parse(rr.Header().Get("Location"))
	if err != nil || location.Path != "/login" || location.Query().Get("next") != next {
		t.Fatalf("login redirect: %v %v", location, err)
	}
	// Simulate HTTP behind a TLS-terminating proxy with the configured Host.
	r := httptest.NewRequest("POST", "http://nori.example/login", strings.NewReader(url.Values{"password": {"test"}, "next": {next}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != next {
		t.Fatalf("login return: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	for _, cookie := range rr.Result().Cookies() {
		if !cookie.Secure {
			t.Errorf("cookie %s not Secure", cookie.Name)
		}
	}
	for _, bad := range []string{"https://evil.example", "//evil.example", "/\\evil.example", "/oauth/authorize#bad", "/%6fauth/authorize"} {
		if safeLoginNext(bad) != "/" {
			t.Errorf("unsafe return %q", bad)
		}
	}
}
