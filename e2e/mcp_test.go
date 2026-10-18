package e2e_test

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
	"github.com/rlsvarinskis/cloud-at-home/services/mcp/mcpserver"
)

// bearerTransport adds a fixed bearer token to every request.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.next.RoundTrip(r)
}

// mcpWorld serves the MCP server in front of the world's real vmd and agentd handlers over HTTP and connects an
// MCP client with the given token. A background goroutine plays the vmd reconciler so VMs actually boot (the
// clock is manual elsewhere, so it is advanced here).
func (w *world) mcpClient(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	pub := w.admin.Key.Public().(ed25519.PublicKey)
	ver := &authz.Verifier{Now: w.clk.Now, Keys: map[string]authz.TrustedKey{"admin": {Public: pub}}}
	srv := mcpserver.New(mcpserver.Config{
		Clients:        mcpserver.NewClients(http.DefaultClient, w.vmdSrv.URL, w.agentSrv.URL),
		Verifier:       ver,
		DefaultProject: "p1",
		DefaultImage:   "debian",
		MaxWait:        6 * time.Second,
	})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	ctx, cancel := context.WithCancel(context.Background())
	var stopped atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stopped.Load() && ctx.Err() == nil {
			w.rec.Tick(ctx)
			w.clk.Advance(time.Second)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	t.Cleanup(func() { stopped.Store(true); cancel(); <-done })

	hc := &http.Client{Transport: bearerTransport{token: token, next: http.DefaultTransport}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: hs.URL, HTTPClient: hc}, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func mcpCall(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func mustOK(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	out, isErr := mcpCall(t, cs, tool, args)
	if isErr {
		t.Fatalf("%s failed: %s", tool, out)
	}
	return out
}

// The whole MCP path against real vmd and agentd: create a machine, run a command, upload and read a file, run an
// agent task to completion, read its status and metrics, and stop and destroy the machine.
func TestMCPFullPath(t *testing.T) {
	eachBackend(t, true, func(t *testing.T, backend string) {
		w := newWorld(t, backend)
		w.sharedImage("debian")
		w.setExec(func(_ string, spec *nodev1.JobSpec) (int, string, string, time.Duration) {
			switch {
			case strings.Contains(spec.GetCommand(), "echo hi"):
				return 0, "hi\n", "", 0
			case spec.GetCommand() == "test -s out.txt":
				return 0, "", "", 0
			}
			return 0, "", "", 0
		})
		w.hub.script("scripted", llm.CallTool("bash", map[string]any{"command": "echo hello > out.txt"}),
			llm.Say("Done.\nVERDICT: SUCCESS"))
		tok, err := w.admin.Mint(authz.Principal{Subject: "user:claude", Type: authz.User,
			Bindings: []authz.Binding{{Project: "p1", Role: authz.RoleEditor}}}, time.Hour)
		must(t, err)
		cs := w.mcpClient(t, tok)

		if out := mustOK(t, cs, "agent_create", map[string]any{"name": "box", "cpus": 2, "memory_gb": 4}); !strings.Contains(out, "box") {
			t.Fatalf("agent_create: %q", out)
		}
		if out := mustOK(t, cs, "agent_list", nil); !strings.Contains(out, "box: running") {
			t.Fatalf("agent_list: %q", out)
		}
		if out := mustOK(t, cs, "agent_exec", map[string]any{"name": "box", "command": "echo hi"}); !strings.Contains(out, `"stdout":"hi\n"`) ||
			!strings.Contains(out, `"done":true`) {
			t.Fatalf("agent_exec: %q", out)
		}
		mustOK(t, cs, "agent_upload", map[string]any{"name": "box", "path": "/home/agent/a.txt", "content": "payload"})
		if out := mustOK(t, cs, "agent_download", map[string]any{"name": "box", "path": "/home/agent/a.txt"}); !strings.Contains(out, `"content":"payload"`) ||
			!strings.Contains(out, `"encoding":"utf-8"`) {
			t.Fatalf("download = %q, want the uploaded content in JSON", out)
		}

		if out := mustOK(t, cs, "agent_models", nil); !strings.Contains(out, "scripted") {
			t.Fatalf("agent_models: %q", out)
		}
		if out, isErr := mcpCall(t, cs, "agent_task", map[string]any{"name": "box", "task": "x", "model": "nope"}); !isErr ||
			!strings.Contains(out, "scripted") {
			t.Fatalf("unknown model must be refused with the installed list; got err=%v %q", isErr, out)
		}
		mustOK(t, cs, "agent_task", map[string]any{"name": "box", "task": "write hello to out.txt",
			"model": "scripted", "check_command": "test -s out.txt"})
		w.runTasks()
		out := mustOK(t, cs, "agent_status", map[string]any{"name": "box", "wait_seconds": 1})
		if !strings.Contains(out, "status: done") || !strings.Contains(out, "$ echo hello > out.txt") ||
			!strings.Contains(out, "=== RESULT ===") {
			t.Fatalf("agent_status: %q", out)
		}

		mustOK(t, cs, "agent_stop", map[string]any{"name": "box"})
		w.until(10, "stopped", func() bool {
			o, _ := mcpCall(t, cs, "agent_list", nil)
			return strings.Contains(o, "box: stopped")
		})
		mustOK(t, cs, "agent_destroy", map[string]any{"name": "box"})
	})
}

// MCP callers get exactly their own token's powers: no token or a bad token is refused, a viewer cannot create
// machines, and an editor of another project cannot see the machine.
func TestMCPAuth(t *testing.T) {
	w := newWorld(t, backendMemory)
	img := w.sharedImage("debian")

	hs := mcpserver.New(mcpserver.Config{Clients: mcpserver.NewClients(http.DefaultClient, w.vmdSrv.URL, w.agentSrv.URL),
		Verifier: &authz.Verifier{Now: w.clk.Now, Keys: map[string]authz.TrustedKey{
			"admin": {Public: w.admin.Key.Public().(ed25519.PublicKey)}}}, DefaultProject: "p1", DefaultImage: "debian"})
	srv := httptest.NewServer(hs.Handler())
	defer srv.Close()
	for _, h := range []string{"", "Bearer ", "Bearer garbage"} {
		req, _ := http.NewRequest("POST", srv.URL, strings.NewReader("{}"))
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status %d, want 401", h, resp.StatusCode)
		}
	}

	view, _ := w.admin.Mint(authz.Principal{Subject: "user:v", Type: authz.User,
		Bindings: []authz.Binding{{Project: "p1", Role: authz.RoleViewer}}}, time.Hour)
	cs := w.mcpClient(t, view)
	if out, isErr := mcpCall(t, cs, "agent_create", map[string]any{"name": "x"}); !isErr || !strings.Contains(out, "permission") {
		t.Errorf("a viewer creating a machine: err=%v %q, want permission denied", isErr, out)
	}

	w.createVM(w.root, "p1", "secret", img.Id, false)
	other, _ := w.admin.Mint(authz.Principal{Subject: "user:o", Type: authz.User,
		Bindings: []authz.Binding{{Project: "p2", Role: authz.RoleEditor}}}, time.Hour)
	cs2 := w.mcpClient(t, other)
	if out, isErr := mcpCall(t, cs2, "agent_stop", map[string]any{"name": "secret", "project": "p1"}); !isErr {
		t.Errorf("another project's editor stopped a machine: %q", out)
	}
}
