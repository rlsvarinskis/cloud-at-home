package daemon

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/homecloud/internal/auth"
)

func writeKey(t *testing.T, dir, name string) {
	t.Helper()
	pub, _, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.SavePublicKey(filepath.Join(dir, name+".pub"), pub); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedKeysParsing(t *testing.T) {
	dir := t.TempDir()
	writeKey(t, dir, "admin")
	writeKey(t, dir, "agentd")
	var k TrustedKeys
	for _, s := range []string{"admin=" + filepath.Join(dir, "admin.pub"), "agentd=" + filepath.Join(dir, "agentd.pub") + ":service, node"} {
		if err := k.Set(s); err != nil {
			t.Fatal(err)
		}
	}
	v, err := k.Verifier(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Keys) != 2 || v.MaxTTL != time.Hour {
		t.Fatalf("verifier: %+v", v)
	}
	if got := v.Keys["agentd"].AllowedRoles; len(got) != 2 || got[0] != auth.RoleService || got[1] != auth.RoleNode {
		t.Fatalf("roles: %v", got)
	}
	if v.Keys["admin"].AllowedRoles != nil {
		t.Fatal("unrestricted key must allow any role")
	}
}

func TestTrustedKeysErrors(t *testing.T) {
	dir := t.TempDir()
	writeKey(t, dir, "a")
	p := filepath.Join(dir, "a.pub")
	cases := map[string]TrustedKeys{
		"none":         nil,
		"no equals":    {"abc"},
		"empty kid":    {"=" + p},
		"empty path":   {"a="},
		"missing file": {"a=" + filepath.Join(dir, "nope.pub")},
		"unknown role": {"a=" + p + ":wizard"},
		"duplicate":    {"a=" + p, "a=" + p},
	}
	for name, k := range cases {
		if _, err := k.Verifier(0); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoadIssuer(t *testing.T) {
	dir := t.TempDir()
	_, priv, _ := auth.GenerateKey()
	path := filepath.Join(dir, "k")
	if err := auth.SavePrivateKey(path, priv); err != nil {
		t.Fatal(err)
	}
	iss, err := LoadIssuer(path, "svc")
	if err != nil || iss.KeyID != "svc" {
		t.Fatalf("%v %v", iss, err)
	}
	for _, c := range [][2]string{{"", "x"}, {path, ""}, {filepath.Join(dir, "none"), "x"}} {
		if _, err := LoadIssuer(c[0], c[1]); err == nil {
			t.Errorf("LoadIssuer(%q,%q) should fail", c[0], c[1])
		}
	}
}

func TestNewLogger(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewLogger("warn", &buf)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hidden")
	l.Warn("shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") {
		t.Fatalf("level filtering: %q", buf.String())
	}
	if _, err := NewLogger("loud", &buf); err == nil {
		t.Fatal("bad level accepted")
	}
}

func TestServeSpeaksHTTP1AndCleartextHTTP2AndShutsDownGracefully(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-release
		}
		_, _ = w.Write([]byte(r.Proto))
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	log, _ := NewLogger("error", &bytes.Buffer{})
	go func() { done <- Serve(ctx, log, ln, h, "", "", 5*time.Second) }()

	get := func(c *http.Client, path string) string {
		res, err := c.Get("http://" + ln.Addr().String() + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var b bytes.Buffer
		_, _ = b.ReadFrom(res.Body)
		return b.String()
	}
	if got := get(http.DefaultClient, "/"); got != "HTTP/1.1" {
		t.Fatalf("http/1.1 request served as %q", got)
	}
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	h2 := &http.Client{Transport: &http.Transport{Protocols: p}}
	if got := get(h2, "/"); got != "HTTP/2.0" {
		t.Fatalf("cleartext h2 request served as %q", got)
	}

	// A request in flight when shutdown starts must complete.
	result := make(chan string, 1)
	go func() { result <- get(http.DefaultClient, "/slow") }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)
	close(release)
	if got := <-result; got != "HTTP/1.1" {
		t.Fatalf("in-flight request lost: %q", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
	_ = httptest.NewRecorder
}
