// Package daemon holds what both servers (vmd, agentd) share: command-line parsing of trusted keys,
// logging, and an HTTP server that speaks HTTP/1.1, HTTP/2 over TLS and cleartext HTTP/2 (so gRPC
// clients work without TLS behind a proxy) and shuts down gracefully.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"example.com/homecloud/internal/auth"
)

// TrustedKeys is a repeatable flag: `-trusted-key kid=path[:role,role]` trusts the public key in `path`
// for tokens whose key id is `kid`, optionally limiting which roles that key may grant.
type TrustedKeys []string

func (k *TrustedKeys) String() string     { return strings.Join(*k, " ") }
func (k *TrustedKeys) Set(v string) error { *k = append(*k, v); return nil }

// Verifier loads the keys and builds a token verifier.
func (k TrustedKeys) Verifier(maxTTL time.Duration) (*auth.Verifier, error) {
	if len(k) == 0 {
		return nil, errors.New("no trusted keys: pass at least one -trusted-key kid=path")
	}
	v := &auth.Verifier{Keys: map[string]auth.TrustedKey{}, MaxTTL: maxTTL}
	for _, spec := range k {
		kid, rest, ok := strings.Cut(spec, "=")
		if !ok || kid == "" || rest == "" {
			return nil, fmt.Errorf("bad -trusted-key %q: want kid=path[:role,role]", spec)
		}
		path, roles, _ := strings.Cut(rest, ":")
		pub, err := auth.LoadPublicKey(path)
		if err != nil {
			return nil, fmt.Errorf("trusted key %q: %w", kid, err)
		}
		tk := auth.TrustedKey{Public: pub}
		if roles != "" {
			for _, r := range strings.Split(roles, ",") {
				role := auth.Role(strings.TrimSpace(r))
				if !auth.KnownRole(role) {
					return nil, fmt.Errorf("trusted key %q: unknown role %q", kid, r)
				}
				tk.AllowedRoles = append(tk.AllowedRoles, role)
			}
		}
		if _, dup := v.Keys[kid]; dup {
			return nil, fmt.Errorf("trusted key id %q given twice", kid)
		}
		v.Keys[kid] = tk
	}
	return v, nil
}

// LoadIssuer loads a private key for minting this service's own tokens.
func LoadIssuer(path, kid string) (*auth.Issuer, error) {
	if path == "" || kid == "" {
		return nil, errors.New("both a key file and a key id are required")
	}
	k, err := auth.LoadPrivateKey(path)
	if err != nil {
		return nil, err
	}
	return &auth.Issuer{KeyID: kid, Key: k}, nil
}

// NewLogger builds a text logger at the given level (debug, info, warn, error).
func NewLogger(level string, w io.Writer) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("bad log level %q", level)
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: l})), nil
}

// Serve serves h on ln until ctx is cancelled, then drains in-flight requests for up to grace.
// With certFile/keyFile it serves TLS (HTTP/1.1 + HTTP/2); without, HTTP/1.1 and cleartext HTTP/2.
func Serve(ctx context.Context, log *slog.Logger, ln net.Listener, h http.Handler, certFile, keyFile string, grace time.Duration) error {
	protos := new(http.Protocols)
	protos.SetHTTP1(true)
	protos.SetHTTP2(true)
	protos.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Handler:           h,
		Protocols:         protos,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: long-poll and streaming calls legitimately hold a response open.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() {
		if certFile != "" {
			errc <- srv.ServeTLS(ln, certFile, keyFile)
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	log.Info("listening", "addr", ln.Addr().String(), "tls", certFile != "")
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	log.Info("shutting down", "grace", grace)
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
