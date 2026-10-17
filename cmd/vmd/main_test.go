package main

import (
	"bytes"
	"context"
	"encoding/json"
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

type keys struct {
	dir  string
	iss  *auth.Issuer
	args []string // -trusted-key flag
}

func newKeys(t *testing.T) keys {
	t.Helper()
	dir := t.TempDir()
	pub, priv, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.SavePublicKey(filepath.Join(dir, "admin.pub"), pub); err != nil {
		t.Fatal(err)
	}
	return keys{dir: dir, iss: &auth.Issuer{KeyID: "admin", Key: priv},
		args: []string{"-trusted-key", "admin=" + filepath.Join(dir, "admin.pub")}}
}

func (k keys) token(t *testing.T, role auth.Role, project string) string {
	t.Helper()
	tok, err := k.iss.Mint(auth.Principal{Subject: "user:test", Type: auth.User, Bindings: []auth.Binding{{Project: project, Role: role}}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// start runs the real daemon entry point and returns its base URL; the daemon is stopped (gracefully) at test end.
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
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// nodeArgs are the flags a development all-in-one needs when it cannot boot guests (no KVM, no iptables rights).
func nodeArgs(t *testing.T) []string {
	d := t.TempDir()
	return []string{"-data-dir", filepath.Join(d, "data"), "-ssh-key", filepath.Join(d, "node_ssh"),
		"-no-firewall", "-kvm=false", "-run-as="}
}

// The deploy/docker-compose.yml flags, against real Postgres: migrations run, the API answers, auth is enforced
// and the process stops gracefully when asked.
func TestVMDAllInOneWithPostgres(t *testing.T) {
	k := newKeys(t)
	args := append([]string{"-role", "all", "-listen", "127.0.0.1:0", "-postgres-dsn", pgtest.DSN(t), "-node-id", "n1"}, k.args...)
	base := start(t, append(args, nodeArgs(t)...)...)

	if res, err := http.Get(base + "/healthz"); err != nil || res.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", res, err)
	}
	if res, err := http.Get(base + "/readyz"); err != nil || res.StatusCode != 200 {
		t.Fatalf("readyz (store reachable): %v %v", res, err)
	}

	cap := base + "/homecloud.vmd.v1.MetricsService/GetCapacity"
	if code, _ := call(t, cap, "", "{}"); code != 401 {
		t.Errorf("no token: %d, want 401", code)
	}
	if code, _ := call(t, cap, k.token(t, auth.RoleEditor, "p1"), "{}"); code != 403 {
		t.Errorf("editor: %d, want 403", code)
	}
	var out struct {
		Nodes []struct{ NodeID, State string }
		Total struct{ CpusTotal int32 }
	}
	var code int
	var body string
	for range 50 { // the node registers itself on the first poll
		code, body = call(t, cap, k.token(t, auth.RoleOperator, auth.AnyProject), "{}")
		if code == 200 && strings.Contains(body, `"nodeId":"n1"`) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code != 200 {
		t.Fatalf("operator: %d %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Nodes) != 1 || out.Nodes[0].State != "ready" || out.Total.CpusTotal < 1 {
		t.Fatalf("capacity: %s (%v)", body, err)
	}

	req, _ := http.NewRequest("GET", base+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+k.token(t, auth.RoleOperator, auth.AnyProject))
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("metrics: %v %v", res, err)
	}
	text, _ := io.ReadAll(res.Body)
	for _, want := range []string{"homecloud_cluster_cpu_threads", "homecloud_node_up", "homecloud_rpc_requests_total"} {
		if !strings.Contains(string(text), want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}

func TestVMDRefusesBadConfiguration(t *testing.T) {
	k := newKeys(t)
	cases := map[string][]string{
		"unknown role":             {"-role", "banana"},
		"no trusted keys":          {"-role", "all"},
		"control without nodes":    append([]string{"-role", "control"}, k.args...),
		"node without ssh key":     append([]string{"-role", "node", "-listen", "127.0.0.1:0"}, k.args...),
		"bad node spec":            append([]string{"-role", "control", "-node", "oops"}, k.args...),
		"node needs service key":   append([]string{"-role", "control", "-node", "n=http://127.0.0.1:1"}, k.args...),
		"bad log level":            {"-log-level", "loud"},
		"trusted key file missing": {"-role", "all", "-trusted-key", "admin=/nonexistent.pub"},
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
