package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
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

func capacityURL(base string) string { return base + "/homecloud.vmd.v1.MetricsService/GetCapacity" }

// Several -postgres-dsn flags shard projects across the databases: every database gets its schema AND receives
// rows, the rows of a project stay together, and the API behaves as with one database.
func TestVMDShardsAcrossSeveralDatabases(t *testing.T) {
	k := newKeys(t)
	dsns := []string{pgtest.DSN(t), pgtest.DSN(t), pgtest.DSN(t)}
	args := []string{"-role", "all", "-listen", "127.0.0.1:0", "-node-id", "n1"}
	for _, d := range dsns {
		args = append(args, "-postgres-dsn", d)
	}
	base := start(t, append(append(args, k.args...), nodeArgs(t)...)...)

	if res, err := http.Get(base + "/readyz"); err != nil || res.StatusCode != 200 { // pings every database
		t.Fatalf("readyz: %v %v", res, err)
	}
	create := base + "/homecloud.vmd.v1.DiskService/CreateDisk"
	const projects = 30 // enough that an empty shard is practically impossible by chance
	for i := range projects {
		p := fmt.Sprintf("proj-%02d", i)
		var code int
		var body string
		for range 50 { // wait for the node to register
			code, body = call(t, create, k.token(t, auth.RoleEditor, p), `{"projectId":"`+p+`","name":"data","sizeGb":1}`)
			if code != 412 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if code != 200 {
			t.Fatalf("create disk in %s: %d %s", p, code, body)
		}
		// Reading it back through the API finds it on whichever shard holds the project.
		code, body = call(t, base+"/homecloud.vmd.v1.DiskService/ListDisks", k.token(t, auth.RoleViewer, p), `{"projectId":"`+p+`"}`)
		if code != 200 || strings.Count(body, `"name":"data"`) != 1 {
			t.Fatalf("list disks in %s: %d %s", p, code, body)
		}
	}

	total := 0
	for i, d := range dsns {
		conn, err := pgx.Connect(context.Background(), d)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM disks").Scan(&n); err != nil {
			t.Fatalf("database %d has no schema: %v", i, err)
		}
		conn.Close(context.Background())
		if n == 0 {
			t.Errorf("database %d received no rows: the projects are not being spread across the databases", i)
		}
		total += n
	}
	if total != projects {
		t.Errorf("rows across databases = %d, want %d (one disk per project, none duplicated)", total, projects)
	}
}

// -quota-* flags reach the services: a project cannot create more disk than its quota.
func TestVMDQuotaFlags(t *testing.T) {
	k := newKeys(t)
	args := append([]string{"-role", "all", "-listen", "127.0.0.1:0", "-node-id", "n1", "-quota-disk-gb", "10"}, k.args...)
	base := start(t, append(args, nodeArgs(t)...)...)
	ed := k.token(t, auth.RoleEditor, "p1")
	create := base + "/homecloud.vmd.v1.DiskService/CreateDisk"

	var code int
	var body string
	for range 50 { // wait for the node to register
		code, body = call(t, create, ed, `{"projectId":"p1","name":"d1","sizeGb":6}`)
		if code != 412 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code != 200 {
		t.Fatalf("first disk within quota: %d %s", code, body)
	}
	code, body = call(t, create, ed, `{"projectId":"p1","name":"d2","sizeGb":6}`)
	if code != 429 || !strings.Contains(body, "quota") {
		t.Fatalf("second disk exceeds the 10 GB quota: %d %s", code, body)
	}
}

// -revoked-tokens: a token that works is refused once its id is added to the file, without restarting the daemon.
func TestVMDRevokedTokens(t *testing.T) {
	k := newKeys(t)
	revoked := filepath.Join(t.TempDir(), "revoked.txt")
	if err := os.WriteFile(revoked, []byte("# nothing revoked yet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-role", "all", "-listen", "127.0.0.1:0", "-node-id", "n1", "-revoked-tokens", revoked}, k.args...)
	base := start(t, append(args, nodeArgs(t)...)...)

	tok := k.token(t, auth.RoleOperator, auth.AnyProject)
	if code, _ := call(t, capacityURL(base), tok, "{}"); code != 200 {
		t.Fatalf("token before revocation: %d", code)
	}
	v := &auth.Verifier{Keys: map[string]auth.TrustedKey{"admin": mustPub(t, k)}}
	p, err := v.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(revoked, []byte(p.TokenID+" # compromised\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, _ := call(t, capacityURL(base), tok, "{}")
		if code == 401 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the revoked token still works (%d)", code)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if code, _ := call(t, capacityURL(base), k.token(t, auth.RoleOperator, auth.AnyProject), "{}"); code != 200 {
		t.Fatalf("other tokens must keep working: %d", code)
	}
}

func mustPub(t *testing.T, k keys) auth.TrustedKey {
	t.Helper()
	pub, err := auth.LoadPublicKey(filepath.Join(k.dir, "admin.pub"))
	if err != nil {
		t.Fatal(err)
	}
	return auth.TrustedKey{Public: pub}
}
