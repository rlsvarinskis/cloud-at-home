package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
)

type tokenKey struct{}

// withToken stores the caller's bearer token so it can be forwarded to vmd and agentd.
func withToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, tokenKey{}, token)
}

// tokenFrom is an authz.TokenSource: every outgoing vmd/agentd call carries the token the MCP caller presented,
// so the downstream services enforce their own roles and project scoping. The MCP server never holds more power
// than its caller.
func tokenFrom(ctx context.Context) (string, error) {
	t, _ := ctx.Value(tokenKey{}).(string)
	if t == "" {
		return "", errors.New("no caller token in context")
	}
	return t, nil
}

// requireBearer rejects requests without a valid homecloud token, and puts the token and principal in the context.
func requireBearer(v *authz.Verifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || tok == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="homecloud-mcp"`)
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		p, err := v.Verify(tok)
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		ctx := authz.WithPrincipal(withToken(r.Context(), tok), p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
