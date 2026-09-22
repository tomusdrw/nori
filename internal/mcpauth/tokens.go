package mcpauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"nori/internal/store"
)

var verifierPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

type grant struct {
	ClientID, Redirect, Resource, Scope, Challenge, Epoch, Family string
	FamilyExpires                                                 int64
}

func secret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func digest(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func (s *Server) put(ctx context.Context, key, kind, family string, expires time.Time, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return s.st.PutOAuth(ctx, store.OAuthRecord{Key: digest(key), Kind: kind, Family: family, Expires: expires.Unix(), Data: b})
}

func (s *Server) token(w http.ResponseWriter, r *http.Request, c store.MCPConfig) {
	cl, err := s.authenticateClient(r, c)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="nori OAuth"`)
		failure(w, 401, "invalid_client")
		return
	}
	p := r.PostForm
	kind, raw := "", ""
	switch p.Get("grant_type") {
	case "authorization_code":
		kind, raw = "code", p.Get("code")
	case "refresh_token":
		kind, raw = "refresh", p.Get("refresh_token")
	default:
		failure(w, 400, "unsupported_grant_type")
		return
	}
	rec, err := s.st.GetOAuth(r.Context(), digest(raw), kind)
	var g grant
	if err != nil || json.Unmarshal(rec.Data, &g) != nil || g.ClientID != cl.Client.ID || g.Epoch != c.Epoch || g.Resource != c.PublicURL+"/mcp" || p.Get("resource") != g.Resource {
		failure(w, 400, "invalid_grant")
		return
	}
	if kind == "code" {
		v := p.Get("code_verifier")
		b := sha256.Sum256([]byte(v))
		if p.Get("redirect_uri") != g.Redirect || !verifierPattern.MatchString(v) || !equal(base64.RawURLEncoding.EncodeToString(b[:]), g.Challenge) {
			failure(w, 400, "invalid_grant")
			return
		}
	}
	if kind == "refresh" && p.Has("scope") {
		wanted, ok := normalizeScope(p.Get("scope"))
		if !ok {
			failure(w, 400, "invalid_scope")
			return
		}
		for _, v := range strings.Fields(wanted) {
			if !slices.Contains(strings.Fields(g.Scope), v) {
				failure(w, 400, "invalid_scope")
				return
			}
		}
		g.Scope = wanted
	}
	if !s.allow(w, "grant:"+cl.Client.ID, 120) {
		return
	}
	consumed, err := s.st.ConsumeOAuth(r.Context(), rec.Key)
	if err != nil {
		failure(w, 503, "temporarily_unavailable")
		return
	}
	if !consumed {
		if err := s.st.RevokeOAuthFamily(r.Context(), g.Family); err != nil {
			failure(w, 503, "temporarily_unavailable")
			return
		}
		failure(w, 400, "invalid_grant")
		return
	}
	access, refresh := secret(), secret()
	expires := time.Unix(g.FamilyExpires, 0)
	now := time.Now().Unix()
	if g.FamilyExpires <= now {
		failure(w, 400, "invalid_grant")
		return
	}
	// Persist and advertise the same deadline, including the final minutes of
	// a grant. Refreshing never extends the grant's original lifetime.
	accessExpires := min(now+int64(accessTokenLifetime/time.Second), g.FamilyExpires)
	if err = s.put(r.Context(), access, "access", g.Family, time.Unix(accessExpires, 0), g); err == nil {
		err = s.put(r.Context(), refresh, "refresh", g.Family, expires, g)
	}
	if err != nil {
		_ = s.st.RevokeOAuthFamily(r.Context(), g.Family)
		failure(w, 503, "temporarily_unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": accessExpires - now, "refresh_token": refresh, "scope": g.Scope})
}
func (s *Server) revoke(w http.ResponseWriter, r *http.Request, c store.MCPConfig) {
	cl, err := s.authenticateClient(r, c)
	if err != nil {
		failure(w, 401, "invalid_client")
		return
	}
	for _, kind := range []string{"access", "refresh"} {
		rec, err := s.st.GetOAuth(r.Context(), digest(r.PostForm.Get("token")), kind)
		if err != nil {
			continue
		}
		var g grant
		if json.Unmarshal(rec.Data, &g) == nil && g.ClientID == cl.Client.ID {
			if err := s.st.RevokeOAuthFamily(r.Context(), g.Family); err != nil {
				failure(w, 503, "temporarily_unavailable")
				return
			}
		}
	}
	w.WriteHeader(200)
}
