package mcpauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

type identityKey struct{}
type Identity struct{ ClientID, Scope string }

func FromContext(ctx context.Context) (Identity, bool) {
	i, ok := ctx.Value(identityKey{}).(Identity)
	return i, ok
}

// WithIdentity attaches a verified principal. Production callers should use Protect.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, identity)
}

func RequireScope(ctx context.Context, scope string) error {
	i, ok := FromContext(ctx)
	if !ok || !slices.Contains(strings.Fields(i.Scope), scope) {
		return errors.New("insufficient_scope: requires " + scope)
	}
	return nil
}

func (s *Server) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		c, ok := s.config(w, r)
		if !ok {
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != c.PublicURL {
			failure(w, 403, "invalid_origin")
			return
		}
		parts := strings.Fields(r.Header.Get("Authorization"))
		var g grant
		valid := false
		if len(r.Header.Values("Authorization")) == 1 && len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && !r.URL.Query().Has("access_token") {
			rec, err := s.st.GetOAuth(r.Context(), digest(parts[1]), "access")
			valid = err == nil && !rec.Used && json.Unmarshal(rec.Data, &g) == nil && g.Epoch == c.Epoch && g.Resource == c.PublicURL+"/mcp" && g.FamilyExpires > time.Now().Unix()
		}
		if !valid {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+c.PublicURL+`/.well-known/oauth-protected-resource/mcp", scope="`+defaultScopes+`"`)
			failure(w, 401, "invalid_token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, Identity{ClientID: g.ClientID, Scope: g.Scope})))
	})
}
