package authz

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

// ServerInterceptor authenticates every call (Authorization: Bearer <jwt>) and stores the
// Principal in the context. Authorization happens in the handlers, per resource.
type ServerInterceptor struct {
	Verifier *Verifier
	Log      *slog.Logger
}

func (s *ServerInterceptor) authenticate(ctx context.Context, h interface{ Get(string) string }, proc string) (context.Context, error) {
	raw := h.Get("Authorization")
	tok, ok := strings.CutPrefix(raw, "Bearer ")
	if !ok || tok == "" {
		return ctx, errs.ToConnect(errs.ErrUnauthenticated)
	}
	p, err := s.Verifier.Verify(tok)
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("authentication failed", "procedure", proc, "err", err)
		}
		return ctx, errs.ToConnect(errs.ErrUnauthenticated) // never tell the client why
	}
	return WithPrincipal(ctx, p), nil
}

func (s *ServerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := s.authenticate(ctx, req.Header(), req.Spec().Procedure)
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (s *ServerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (s *ServerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := s.authenticate(ctx, conn.RequestHeader(), conn.Spec().Procedure)
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// TokenSource supplies a bearer token for outgoing calls.
type TokenSource func(ctx context.Context) (string, error)

// StaticToken is a TokenSource for a fixed token (e.g. from an environment variable).
func StaticToken(t string) TokenSource {
	return func(context.Context) (string, error) { return t, nil }
}

// ServiceTokens returns a TokenSource that mints short-lived tokens for this service's identity
// and caches them until shortly before they expire.
func ServiceTokens(iss *Issuer, p Principal, ttl time.Duration) TokenSource {
	var mu sync.Mutex
	var cached string
	var expires time.Time
	return func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if cached != "" && iss.now().Before(expires.Add(-ttl/5)) {
			return cached, nil
		}
		t, err := iss.Mint(p, ttl)
		if err != nil {
			return "", err
		}
		cached, expires = t, iss.now().Add(ttl)
		return t, nil
	}
}

// ClientInterceptor attaches a bearer token to every outgoing call.
func ClientInterceptor(src TokenSource) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if req.Spec().IsClient {
				tok, err := src(ctx)
				if err != nil {
					return nil, connect.NewError(connect.CodeUnauthenticated, err)
				}
				req.Header().Set("Authorization", "Bearer "+tok)
			}
			return next(ctx, req)
		}
	})
}
