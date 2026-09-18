package mcpauth

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

	"deploybot/internal/auth"
	"deploybot/internal/store"
)

func TestOAuthFlowAndAttacks(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Settings are intentionally initialized through the same persisted instance settings API.
	if err := st.SetMCPConfig(context.Background(), true, "https://nori.example", false); err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("password")
	a, _ := auth.New(hash, make([]byte, 32))
	s := New(st, a)
	call := func(method, path, body string, cookies []*http.Cookie, origins ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://nori.example"+path, strings.NewReader(body))
		if method == "POST" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "https://nori.example")
		}
		if len(origins) > 0 {
			r.Header.Set("Origin", origins[0])
		}
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	registration := call("POST", "/oauth/register", `{"client_name":"Test agent","redirect_uris":["http://localhost:8765/callback"],"token_endpoint_auth_method":"none"}`, nil)
	if registration.Code != 201 {
		t.Fatalf("registration: %d %s", registration.Code, registration.Body)
	}
	var client map[string]any
	_ = json.Unmarshal(registration.Body.Bytes(), &client)
	id := client["client_id"].(string)
	pending, err := st.GetOAuth(context.Background(), digest(id), "client")
	if err != nil || pending.Expires > time.Now().Add(11*time.Minute).Unix() {
		t.Fatalf("unapproved registration lifetime: %+v %v", pending, err)
	}
	verifier := strings.Repeat("a", 43)
	h := sha256.Sum256([]byte(verifier))
	params := url.Values{"client_id": {id}, "redirect_uri": {"http://localhost:8765/callback"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(h[:])}, "resource": {"https://nori.example/mcp"}, "scope": {"nori:read nori:write"}, "state": {"original-state"}}
	unauth := call("GET", "/oauth/authorize?"+params.Encode(), "", nil)
	if unauth.Code != 303 || !strings.HasPrefix(unauth.Header().Get("Location"), "/login?next=") {
		t.Fatalf("login redirect %d %s", unauth.Code, unauth.Header())
	}
	lw := httptest.NewRecorder()
	lr := httptest.NewRequest("POST", "https://nori.example/login", nil)
	if err := a.Login(lw, lr, "password", false); err != nil {
		t.Fatal(err)
	}
	cookies := lw.Result().Cookies()
	csrf := ""
	for _, c := range cookies {
		if c.Name == "deploybot_csrf" {
			csrf = c.Value
		}
	}
	consent := call("GET", "/oauth/authorize?"+params.Encode(), "", cookies)
	if consent.Code != 200 || !strings.Contains(consent.Body.String(), "Docker host") {
		t.Fatalf("consent %d %s", consent.Code, consent.Body)
	}
	// HTML form POSTs under no-referrer carry Origin: null and are rejected
	// by the same-origin consent guard. Preserve Origin without forwarding the
	// authorization query to the client's callback.
	if got := consent.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("consent form must preserve its POST Origin; Referrer-Policy=%q", got)
	}
	for _, origin := range []string{"", "null", "https://chatgpt.com", "https://other-client.example", "http://localhost", "http://localhost:8765", "http://127.0.0.1:8765", "http://[::1]:8765"} {
		unauth := call("GET", "/oauth/authorize?"+params.Encode(), "", nil, origin)
		if unauth.Code != 303 || !strings.HasPrefix(unauth.Header().Get("Location"), "/login?next=") {
			t.Errorf("login navigation from %q: %d %s", origin, unauth.Code, unauth.Body)
		}
		w := call("GET", "/oauth/authorize?"+params.Encode(), "", cookies, origin)
		if w.Code != 200 {
			t.Errorf("consent navigation from %q: %d %s", origin, w.Code, w.Body)
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Error("consent page must not enable cross-origin reads")
		}
	}
	originalRedirect := params.Get("redirect_uri")
	params.Set("redirect_uri", "https://attacker.example/callback")
	if w := call("GET", "/oauth/authorize?"+params.Encode(), "", cookies); w.Code != 400 || w.Header().Get("Location") != "" {
		t.Fatal("unregistered redirect accepted")
	}
	params.Set("redirect_uri", originalRedirect)
	params.Set("decision", "allow")
	denied := call("POST", "/oauth/authorize", params.Encode(), cookies)
	if denied.Code != 403 {
		t.Fatalf("CSRF accepted %d", denied.Code)
	}
	params.Set("csrf_token", csrf)
	for _, origin := range []string{"", "http://localhost:8765", "http://127.0.0.1:8765", "https://evil.example", "null"} {
		w := call("POST", "/oauth/authorize", params.Encode(), cookies, origin)
		if w.Code != 403 {
			t.Errorf("consent submission from %q accepted: %d", origin, w.Code)
		}
	}
	params.Set("decision", "deny")
	refused := call("POST", "/oauth/authorize", params.Encode(), cookies)
	refusedURL, err := url.Parse(refused.Header().Get("Location"))
	if err != nil || refused.Code != 303 || refusedURL.Query().Get("error") != "access_denied" || refusedURL.Query().Get("code") != "" || refusedURL.Query().Get("state") != "original-state" {
		t.Fatalf("denied consent: %d %s", refused.Code, refused.Header().Get("Location"))
	}
	params.Set("decision", "allow")
	issueCode := func() string {
		t.Helper()
		w := call("POST", "/oauth/authorize", params.Encode(), cookies)
		if w.Code != 303 {
			t.Fatalf("authorize %d %s", w.Code, w.Body)
		}
		u, _ := url.Parse(w.Header().Get("Location"))
		if u.Scheme+"://"+u.Host+u.Path != originalRedirect {
			t.Fatal("authorization did not return to the registered localhost callback")
		}
		if u.Query().Get("state") != "original-state" {
			t.Fatal("state lost")
		}
		return u.Query().Get("code")
	}
	code := issueCode()
	approvedClient, err := st.GetOAuth(context.Background(), digest(id), "client")
	if err != nil || approvedClient.Expires < time.Now().Add(300*24*time.Hour).Unix() {
		t.Fatalf("approved registration not retained: %+v %v", approvedClient, err)
	}
	// Anonymous registration abuse must not exhaust the token budget of an
	// already-approved client. Only a few short-lived registrations are retained.
	for i := 0; i < 130; i++ {
		w := call("POST", "/oauth/register", `{"redirect_uris":["http://localhost:8765/callback"],"token_endpoint_auth_method":"none"}`, nil)
		if i > 10 && w.Code != 429 {
			t.Fatalf("unbounded registration: %d", w.Code)
		}
	}
	tokenParams := url.Values{"client_id": {id}, "grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {params.Get("redirect_uri")}, "resource": {params.Get("resource")}, "code_verifier": {"wrong"}}
	bad := call("POST", "/oauth/token", tokenParams.Encode(), nil)
	if bad.Code != 400 {
		t.Fatalf("bad PKCE accepted %d", bad.Code)
	}
	tokenParams.Set("code_verifier", verifier)
	tokenParams.Set("resource", "https://other.example/mcp")
	if w := call("POST", "/oauth/token", tokenParams.Encode(), nil); w.Code != 400 {
		t.Fatal("wrong audience accepted")
	}
	tokenParams.Set("resource", params.Get("resource"))
	exchange := func(p url.Values) map[string]any {
		t.Helper()
		w := call("POST", "/oauth/token", p.Encode(), nil)
		if w.Code != 200 {
			t.Fatalf("token %d %s", w.Code, w.Body)
		}
		var v map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}
	tokens := exchange(tokenParams)
	protect := func(access, host, origin string) int {
		r := httptest.NewRequest("POST", "https://"+host+"/mcp", nil)
		r.Header.Set("Authorization", "Bearer "+access)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		s.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if RequireScope(r.Context(), "nori:write") != nil {
				t.Error("scope lost")
			}
			if RequireScope(r.Context(), "nori:secrets") == nil {
				t.Error("scope escalation")
			}
			w.WriteHeader(204)
		})).ServeHTTP(w, r)
		return w.Code
	}
	access := tokens["access_token"].(string)
	if protect(access, "nori.example", "") != 204 {
		t.Fatal("access rejected")
	}
	if protect(access, "evil.example", "") != 403 || protect(access, "nori.example", "https://evil.example") != 403 {
		t.Fatal("host/origin attack accepted")
	}
	refresh := url.Values{"client_id": {id}, "grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)}, "resource": {params.Get("resource")}}
	rotated := exchange(refresh)
	replay := call("POST", "/oauth/token", refresh.Encode(), nil)
	if replay.Code != 400 {
		t.Fatal("refresh replay accepted")
	}
	if protect(rotated["access_token"].(string), "nori.example", "") != 401 {
		t.Fatal("replayed family still valid")
	}
	tokenParams.Set("code", issueCode())
	tokens = exchange(tokenParams)
	if err := st.SetMCPConfig(context.Background(), false, "", false); err != nil {
		t.Fatal(err)
	}
	if protect(tokens["access_token"].(string), "nori.example", "") != 404 {
		t.Fatal("disabled server accessible")
	}
	if err := st.SetMCPConfig(context.Background(), true, "https://nori.example", false); err != nil {
		t.Fatal(err)
	}
	if protect(tokens["access_token"].(string), "nori.example", "") != 401 {
		t.Fatal("old epoch token accepted")
	}
}

func TestRedirectValidation(t *testing.T) {
	for _, v := range []string{"https://example.com/callback", "http://localhost", "http://localhost:8765/callback", "http://127.0.0.1:123/cb", "http://[::1]:123/cb"} {
		if !validRedirect(v) {
			t.Errorf("rejected %s", v)
		}
	}
	for _, v := range []string{"http://example.com/cb", "javascript:alert(1)", "https://a.example/cb#token", "https://user:pass@example.com/cb", "//example.com"} {
		if validRedirect(v) {
			t.Errorf("accepted %s", v)
		}
	}
}

func TestConfidentialClientRevocationAndExpiry(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.SetMCPConfig(ctx, true, "https://nori.example", false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := st.GetMCPConfig(ctx)
	s := New(st, nil)
	cl := storedClient{Client: client{ID: "client", Method: "client_secret_basic"}, SecretHash: digest("secret"), Epoch: cfg.Epoch}
	if err := s.put(ctx, "client", "client", "", time.Now().Add(time.Hour), cl); err != nil {
		t.Fatal(err)
	}
	g := grant{ClientID: "client", Scope: "nori:read", Epoch: cfg.Epoch, Resource: cfg.PublicURL + "/mcp", Family: "family", FamilyExpires: time.Now().Add(time.Hour).Unix()}
	if err := s.put(ctx, "access", "access", g.Family, time.Now().Add(time.Minute), g); err != nil {
		t.Fatal(err)
	}
	request := func(secret string) int {
		r := httptest.NewRequest("POST", cfg.PublicURL+"/oauth/revoke", strings.NewReader("token=access"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetBasicAuth("client", secret)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	if request("wrong") != 401 {
		t.Fatal("bad client secret accepted")
	}
	rec, _ := st.GetOAuth(ctx, digest("access"), "access")
	if rec.Used {
		t.Fatal("unauthorized revocation")
	}
	if request("secret") != 200 {
		t.Fatal("revocation failed")
	}
	rec, _ = st.GetOAuth(ctx, digest("access"), "access")
	if !rec.Used {
		t.Fatal("token not revoked")
	}
	if err := s.put(ctx, "new-access", "access", g.Family, time.Now().Add(time.Minute), g); err == nil {
		t.Fatal("revoked family resurrected by concurrent issuance")
	}
	for _, kind := range []string{"code", "access", "refresh"} {
		if err := s.put(ctx, "expired-"+kind, kind, "other", time.Now().Add(-time.Minute), g); err != nil {
			t.Fatal(err)
		}
		if _, err := st.GetOAuth(ctx, digest("expired-"+kind), kind); err == nil {
			t.Fatalf("expired %s record returned", kind)
		}
	}
}
