package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rlsvarinskis/cloud-at-home/libs/cli"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/cli/vmclitest"
)

const testToken = "attic-test-token"

// runAttic runs the hub against a fake vmd. The environment holds the vmd URL, the token and the project, so the
// test never reads the real home directory.
func runAttic(t *testing.T, srv *vmclitest.Server, stdin string, args ...string) (int, string, string) {
	t.Helper()
	env := map[string]string{
		cli.EnvVMDURL:  srv.URL(),
		cli.EnvToken:   testToken,
		cli.EnvProject: "demo",
	}
	app := newApp()
	app.Sources = cli.Sources{
		LookupEnv: func(name string) (string, bool) { v, ok := env[name]; return v, ok },
		HomeDir:   func() (string, error) { return t.TempDir(), nil },
	}
	app.HTTP = srv.Client()
	var out, errOut bytes.Buffer
	code := app.Run(context.Background(), args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestHelpAndVersion(t *testing.T) {
	srv := vmclitest.New()
	defer srv.Close()

	code, out, _ := runAttic(t, srv, "", "help")
	if code != 0 || !strings.Contains(out, "vm") || !strings.Contains(out, "Global flags") {
		t.Fatalf("help: exit %d, out %q", code, out)
	}
	code, out, _ = runAttic(t, srv, "", "vm", "help")
	if code != 0 || !strings.Contains(out, "list") || !strings.Contains(out, "cp") {
		t.Fatalf("vm help: exit %d, out %q", code, out)
	}
	code, out, _ = runAttic(t, srv, "", "vm", "cp", "-h")
	if code != 0 || !strings.Contains(out, "usage: attic vm cp [--mode OCTAL] SRC DST") {
		t.Fatalf("vm cp -h: exit %d, out %q", code, out)
	}
	old := version
	version = "1.2.3-test"
	defer func() { version = old }()
	if code, out, _ = runAttic(t, srv, "", "version"); code != 0 || out != "1.2.3-test\n" {
		t.Fatalf("version: exit %d, out %q", code, out)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	srv := vmclitest.New()
	defer srv.Close()
	for _, args := range [][]string{
		{},
		{"nope"},
		{"vm"},
		{"vm", "nope"},
		{"vm", "cp", "only-one"},
		{"vm", "cp", "alpha:/a", "beta:/b"},
		{"--bogus", "vm", "list"},
	} {
		if code, _, errOut := runAttic(t, srv, "", args...); code != 2 {
			t.Errorf("args %q: exit %d, want 2 (stderr %q)", args, code, errOut)
		}
	}
}

func TestVMListEndToEnd(t *testing.T) {
	srv := vmclitest.New()
	defer srv.Close()
	srv.AddVM("demo", "alpha", vmclitest.Running, 2, 2048)
	srv.AddVM("demo", "beta", vmclitest.Stopped, 1, 512)
	srv.AddVM("elsewhere", "gamma", vmclitest.Running, 1, 512)

	code, out, errOut := runAttic(t, srv, "", "vm", "list")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "alpha running 2 2" ||
		strings.Join(strings.Fields(lines[1]), " ") != "beta stopped 1 0.5" {
		t.Fatalf("list output %q", out)
	}

	// Global flags before the group name override the environment.
	srv2 := vmclitest.New()
	defer srv2.Close()
	srv2.AddVM("other", "delta", vmclitest.Running, 1, 1024)
	code, out, _ = runAttic(t, srv, "", "--vmd-url", srv2.URL(), "-p", "other", "vm", "list")
	if code != 0 || strings.Join(strings.Fields(out), " ") != "delta running 1 1" {
		t.Fatalf("with --vmd-url and -p: exit %d, out %q", code, out)
	}
	for _, a := range srv2.Authorizations() {
		if a != "Bearer "+testToken {
			t.Fatalf("Authorization = %q", a)
		}
	}
}

func TestVMCpRoundTripEndToEnd(t *testing.T) {
	srv := vmclitest.New()
	defer srv.Close()
	id := srv.AddVM("demo", "alpha", vmclitest.Running, 1, 1024)
	dir := t.TempDir()
	src := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := runAttic(t, srv, "", "vm", "cp", src, "alpha:/root/script.sh")
	if code != 0 {
		t.Fatalf("upload: exit %d, stderr %q", code, errOut)
	}
	if puts := srv.Puts(); len(puts) != 1 || puts[0].VMID != id || puts[0].Mode != 0o755 {
		t.Fatalf("PutFile calls = %+v", puts)
	}

	dst := filepath.Join(dir, "back.sh")
	code, out, errOut := runAttic(t, srv, "", "vm", "cp", "alpha:/root/script.sh", dst)
	if code != 0 || out != "" {
		t.Fatalf("download: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "#!/bin/sh\necho hi\n" {
		t.Fatalf("downloaded %q, %v", got, err)
	}
}

func TestVMCpStdinAndStdoutEndToEnd(t *testing.T) {
	srv := vmclitest.New()
	defer srv.Close()
	srv.AddVM("demo", "alpha", vmclitest.Running, 1, 1024)

	code, out, errOut := runAttic(t, srv, "piped data", "vm", "cp", "-", "alpha:/root/p")
	if code != 0 || out != "" || errOut != "wrote 10 bytes to alpha:/root/p\n" {
		t.Fatalf("upload from stdin: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	code, out, errOut = runAttic(t, srv, "", "vm", "cp", "alpha:/root/p", "-")
	if code != 0 || out != "piped data" || errOut != "read 10 bytes from alpha:/root/p\n" {
		t.Fatalf("download to stdout: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestVMCpRuntimeErrorShowsCode(t *testing.T) {
	srv := vmclitest.New()
	defer srv.Close()
	srv.AddVM("demo", "alpha", vmclitest.Running, 1, 1024)

	code, _, errOut := runAttic(t, srv, "", "vm", "cp", "alpha:/root/missing", "-")
	if code != 1 || errOut != "attic vm cp: not_found: no such file\n" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	code, _, errOut = runAttic(t, srv, "", "vm", "cp", "ghost:/root/x", "-")
	if code != 1 || !strings.Contains(errOut, `no VM named "ghost"`) {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestTokenNeverInOutput(t *testing.T) {
	srv := vmclitest.New()
	defer srv.Close()
	srv.AddVM("demo", "alpha", vmclitest.Running, 1, 1024)
	for _, args := range [][]string{
		{"vm", "list"},
		{"vm", "cp", "alpha:/missing", "-"},
		{"--token-file", "/no/such/token", "vm", "list"},
		{"--vmd-url", "ftp://192.0.2.10", "vm", "list"},
	} {
		_, out, errOut := runAttic(t, srv, "", args...)
		if strings.Contains(out, testToken) || strings.Contains(errOut, testToken) {
			t.Fatalf("args %q printed the token: stdout %q stderr %q", args, out, errOut)
		}
	}
}
