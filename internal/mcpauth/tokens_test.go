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

	"deploybot/internal/store"
)

type tokenResponse struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	ExpiresIn int64  `json:"expires_in"`
}

func seedTokenGrant(t *testing.T, st *store.Store, kind string, remaining time.Duration) (*Server, grant, url.Values) {
	t.Helper()
	ctx := context.Background()
	cfg, err := st.GetMCPConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled {
		if err := st.SetMCPConfig(ctx, true, "https://nori.example", false); err != nil {
			t.Fatal(err)
		}
		cfg, err = st.GetMCPConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	s := New(st, nil)
	clientID := secret()
	cl := storedClient{Client: client{ID: clientID, Method: "none"}, Epoch: cfg.Epoch}
	if err := s.put(ctx, clientID, "client", "", time.Now().Add(time.Hour), cl); err != nil {
		t.Fatal(err)
	}
	verifier := strings.Repeat("v", 43)
	hash := sha256.Sum256([]byte(verifier))
	g := grant{ClientID: clientID, Resource: cfg.PublicURL + "/mcp", Scope: ScopeRead, Epoch: cfg.Epoch, Family: secret(), FamilyExpires: time.Now().Add(remaining).Unix(), Redirect: "http://127.0.0.1:1234/callback", Challenge: base64.RawURLEncoding.EncodeToString(hash[:])}
	raw := secret()
	if err := s.put(ctx, raw, kind, g.Family, time.Now().Add(time.Hour), g); err != nil {
		t.Fatal(err)
	}
	p := url.Values{"client_id": {clientID}, "resource": {g.Resource}}
	if kind == "refresh" {
		p.Set("grant_type", "refresh_token")
		p.Set("refresh_token", raw)
	} else {
		p.Set("grant_type", "authorization_code")
		p.Set("code", raw)
		p.Set("code_verifier", verifier)
		p.Set("redirect_uri", g.Redirect)
	}
	return s, g, p
}

func tokenRequest(s *Server, params url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "https://nori.example/oauth/token", strings.NewReader(params.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func decodeTokens(t *testing.T, w *httptest.ResponseRecorder) tokenResponse {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("token endpoint: %d %s", w.Code, w.Body)
	}
	var result tokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Access == "" || result.Refresh == "" {
		t.Fatal("missing credentials")
	}
	return result
}

func accessStatus(s *Server, token string) int {
	r := httptest.NewRequest("POST", "https://nori.example/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
	return w.Code
}

func TestAccessLifetimeDoesNotExceedGrant(t *testing.T) {
	for _, remaining := range []time.Duration{time.Hour, 45 * time.Second} {
		t.Run(remaining.String(), func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "oauth.db"), make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			s, g, p := seedTokenGrant(t, st, "refresh", remaining)
			before := time.Now().Unix()
			result := decodeTokens(t, tokenRequest(s, p))
			record, err := st.GetOAuth(context.Background(), digest(result.Access), "access")
			if err != nil {
				t.Fatal(err)
			}
			if record.Expires > g.FamilyExpires {
				t.Fatal("access record outlives its grant")
			}
			if result.ExpiresIn <= 0 || result.ExpiresIn > 600 || before+result.ExpiresIn > g.FamilyExpires {
				t.Fatalf("misleading expires_in=%d", result.ExpiresIn)
			}
			if issuedAt := record.Expires - result.ExpiresIn; issuedAt < before || issuedAt > time.Now().Unix() {
				t.Fatal("advertised expiry differs from stored expiry")
			}
		})
	}
}

func TestConcurrentExchangeRevokesReplayedFamily(t *testing.T) {
	for _, kind := range []string{"code", "refresh"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "oauth.db")
			first, err := store.Open(path, make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			second, err := store.Open(path, make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			s, _, p := seedTokenGrant(t, first, kind, time.Hour)
			peer := New(second, nil)
			start := make(chan struct{})
			results := make(chan *httptest.ResponseRecorder, 2)
			for _, server := range []*Server{s, peer} {
				go func() { <-start; results <- tokenRequest(server, p) }()
			}
			close(start)
			success, replay := 0, 0
			var issued []tokenResponse
			responses := []*httptest.ResponseRecorder{<-results, <-results}
			for _, w := range responses {
				switch w.Code {
				case 200:
					success++
					issued = append(issued, decodeTokens(t, w))
				case 400:
					replay++
				case 503: // The replay may revoke the family before the winner can issue tokens.
				default:
					t.Fatalf("unexpected exchange response: %d %s", w.Code, w.Body)
				}
			}
			if success > 1 || replay != 1 {
				t.Fatalf("successes=%d replay rejections=%d", success, replay)
			}
			for _, tokens := range issued {
				if accessStatus(s, tokens.Access) != 401 || accessStatus(peer, tokens.Access) != 401 {
					t.Fatal("concurrent replay left a usable access token")
				}
			}
			if w := tokenRequest(peer, p); w.Code != 400 {
				t.Fatalf("replayed credential survived: %d", w.Code)
			}
		})
	}
}

func TestTokensAndRevocationSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.db")
	open := func() *store.Store {
		t.Helper()
		st, err := store.Open(path, make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return st
	}
	st := open()
	s, _, p := seedTokenGrant(t, st, "refresh", time.Hour)
	tokens := decodeTokens(t, tokenRequest(s, p))
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = open()
	s = New(st, nil)
	if accessStatus(s, tokens.Access) != 204 {
		t.Fatal("restart lost live grant")
	}
	p.Set("refresh_token", tokens.Refresh)
	rotated := decodeTokens(t, tokenRequest(s, p))
	if w := tokenRequest(s, p); w.Code != 400 {
		t.Fatal("refresh replay not rejected after restart")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	s = New(open(), nil)
	if accessStatus(s, tokens.Access) != 401 || accessStatus(s, rotated.Access) != 401 {
		t.Fatal("restart restored revoked access")
	}
	p.Set("refresh_token", rotated.Refresh)
	if w := tokenRequest(s, p); w.Code != 400 {
		t.Fatal("restart restored revoked refresh token")
	}
}

func TestRefreshCannotEscalateScope(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "oauth.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, _, p := seedTokenGrant(t, st, "refresh", time.Hour)
	p.Set("scope", ScopeWrite)
	if w := tokenRequest(s, p); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_scope") {
		t.Fatalf("scope escalation: %d %s", w.Code, w.Body)
	}
	p.Set("scope", ScopeRead)
	tokens := decodeTokens(t, tokenRequest(s, p))
	if accessStatus(s, tokens.Access) != 204 {
		t.Fatal("invalid scope request consumed the valid credential")
	}
}
