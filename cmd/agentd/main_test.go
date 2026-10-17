package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/homecloud/internal/auth"
	"example.com/homecloud/internal/pg/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

type fixture struct {
	args  []string
	admin *auth.Issuer
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	apub, apriv, _ := auth.GenerateKey()
	_, spriv, _ := auth.GenerateKey()
	must(t, auth.SavePublicKey(filepath.Join(dir, "admin.pub"), apub))
	must(t, auth.SavePrivateKey(filepath.Join(dir, "agentd.key"), spriv))
	return fixture{
		admin: &auth.Issuer{KeyID: "admin", Key: apriv},
		args: []string{"-trusted-key", "admin=" + filepath.Join(dir, "admin.pub"),
			"-service-key", filepath.Join(dir, "agentd.key"), "-service-key-id", "agentd",
			"-vmd-url", "http://vmd.internal.invalid:8080", "-ollama-url", "http://198.51.100.7:11434", "-listen", "127.0.0.1:0"},
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f fixture) token(t *testing.T, role auth.Role, project string) string {
	t.Helper()
	tok, err := f.admin.Mint(auth.Principal{Subject: "user:test", Type: auth.User, Bindings: []auth.Binding{{Project: project, Role: role}}}, time.Hour)
	must(t, err)
	return tok
}

func start(t *testing.T, args ...string) string {
	t.Helper()
	addrc := make(chan net.Addr, 1)
	onListen = func(a net.Addr) { addrc <- a }
	t.Cleanup(func() { onListen = nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var logs bytes.Buffer
	go func() { done <- run(ctx, args, &logs) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon did not shut down cleanly: %v\n%s", err, logs.String())
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon did not stop within 15 s of cancellation")
		}
	})
	select {
	case a := <-addrc:
		return "http://" + a.String()
	case err := <-done:
		t.Fatalf("daemon exited during start-up: %v\n%s", err, logs.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("daemon never started listening\n%s", logs.String())
	}
	return ""
}

func call(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestAgentdWithPostgres(t *testing.T) {
	f := newFixture(t)
	base := start(t, append(f.args, "-postgres-dsn", pgtest.DSN(t))...)

	for _, p := range []string{"/healthz", "/readyz"} {
		if res, err := http.Get(base + p); err != nil || res.StatusCode != 200 {
			t.Fatalf("%s: %v %v", p, res, err)
		}
	}
	list := base + "/homecloud.agentd.v1.AgentService/ListAgents"
	if code, _ := call(t, list, "", `{"projectId":"p1"}`); code != 401 {
		t.Errorf("no token: %d, want 401", code)
	}
	if code, _ := call(t, list, f.token(t, auth.RoleEditor, "other"), `{"projectId":"p1"}`); code != 403 {
		t.Errorf("wrong project: %d, want 403", code)
	}
	// Migrations ran: the store answers (an empty list), proving the schema exists in Postgres.
	code, body := call(t, list, f.token(t, auth.RoleViewer, "p1"), `{"projectId":"p1"}`)
	if code != 200 || strings.Contains(body, "error") {
		t.Fatalf("list agents: %d %s", code, body)
	}

	// Unreachable backends are reported, but their addresses are not.
	code, body = call(t, base+"/homecloud.agentd.v1.ModelService/ListModels", f.token(t, auth.RoleViewer, "p1"), "{}")
	if code != 503 {
		t.Fatalf("models with the LLM down: %d %s", code, body)
	}
	code2, body2 := call(t, base+"/homecloud.agentd.v1.AgentService/CreateAgent", f.token(t, auth.RoleEditor, "p1"),
		`{"projectId":"p1","name":"a1","vmId":"vm_x","model":"m"}`)
	if code2 != 503 {
		t.Fatalf("create agent with vmd down: %d %s", code2, body2)
	}
	for _, leak := range []string{"198.51.100.7", "11434", "vmd.internal", "8080", "dial tcp", "no such host"} {
		if strings.Contains(body, leak) || strings.Contains(body2, leak) {
			t.Errorf("API error leaks %q:\n%s\n%s", leak, body, body2)
		}
	}

	req, _ := http.NewRequest("GET", base+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+f.token(t, auth.RoleOperator, auth.AnyProject))
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	text, _ := io.ReadAll(res.Body)
	for _, want := range []string{"homecloud_agent_tasks_finished_total", `server="agentd"`} {
		if !strings.Contains(string(text), want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}

func TestAgentdRefusesBadConfiguration(t *testing.T) {
	f := newFixture(t)
	without := func(flag string) []string { // the fixture's args minus one flag and its value
		var out []string
		for i := 0; i < len(f.args); i++ {
			if f.args[i] == flag {
				i++
				continue
			}
			out = append(out, f.args[i])
		}
		return out
	}
	cases := map[string][]string{
		"no vmd url":           without("-vmd-url"),
		"no trusted key":       without("-trusted-key"),
		"no service key":       without("-service-key"),
		"missing service key":  append(without("-service-key"), "-service-key", "/nonexistent.key"),
		"bad log level":        append(append([]string{}, f.args...), "-log-level", "loud"),
		"unreachable database": append(append([]string{}, f.args...), "-postgres-dsn", "postgres://x:y@127.0.0.1:1/z?connect_timeout=1"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := run(ctx, args, &logs); err == nil {
				t.Fatalf("expected an error\n%s", logs.String())
			}
		})
	}
}
