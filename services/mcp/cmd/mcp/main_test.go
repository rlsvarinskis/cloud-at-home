package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
)

// syncBuf is a log sink safe to read while the daemon writes to it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// startDaemon runs the real daemon on a random port and returns its base URL and a token it trusts.
func startDaemon(t *testing.T, extra ...string) (base string, token string, stderr *syncBuf) {
	t.Helper()
	dir := t.TempDir()
	pub, priv, err := authz.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pubPath := filepath.Join(dir, "admin.pub")
	if err := authz.SavePublicKey(pubPath, pub); err != nil {
		t.Fatal(err)
	}
	iss := &authz.Issuer{KeyID: "admin", Key: priv}
	token, err = iss.Mint(authz.Principal{Subject: "user:t", Type: authz.User,
		Bindings: []authz.Binding{{Project: "p1", Role: authz.RoleEditor}}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	addr := make(chan net.Addr, 1)
	onListen = func(a net.Addr) { addr <- a }
	t.Cleanup(func() { onListen = nil })
	ctx, cancel := context.WithCancel(context.Background())
	stderr = &syncBuf{}
	done := make(chan error, 1)
	args := append([]string{"-listen", "127.0.0.1:0", "-vmd-url", "http://127.0.0.1:1", "-agentd-url", "http://127.0.0.1:1",
		"-default-project", "p1", "-trusted-key", "admin=" + pubPath}, extra...)
	go func() { done <- run(ctx, args, stderr) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("daemon did not shut down")
		}
	})
	select {
	case a := <-addr:
		return "http://" + a.String(), token, stderr
	case err := <-done:
		t.Fatalf("daemon exited early: %v\n%s", err, stderr)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not start")
	}
	return
}

func TestDaemonHealthAndAuth(t *testing.T) {
	base, token, _ := startDaemon(t)
	resp, err := http.Get(base + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", resp, err)
	}
	resp.Body.Close()

	for _, h := range []string{"", "Bearer nonsense"} {
		req, _ := http.NewRequest("POST", base+"/mcp", strings.NewReader("{}"))
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status %d, want 401", h, resp.StatusCode)
		}
	}

	// A valid token completes the MCP handshake and lists the tools.
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: &http.Client{Transport: bearer{token}}}, nil)
	if err != nil {
		t.Fatalf("MCP connect with a valid token: %v", err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) < 20 {
		t.Fatalf("tools: %v, %v", len(tools.Tools), err)
	}
}

func TestRequiredFlags(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-vmd-url", "http://x"}, &out); err == nil ||
		!strings.Contains(err.Error(), "required") {
		t.Fatalf("missing flags must be refused, got %v", err)
	}
	if err := run(context.Background(), []string{"-version"}, &out); err != nil {
		t.Fatal(err)
	}
}

func TestWarnsWithoutTLS(t *testing.T) {
	_, _, stderr := startDaemon(t)
	if !strings.Contains(stderr.String(), "no -tls-cert") {
		t.Errorf("expected a warning about clear-text tokens, log:\n%s", stderr)
	}
}
