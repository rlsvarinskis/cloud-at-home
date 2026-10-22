package vmcli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/cli"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/cli/vmclitest"
)

const testToken = "test-token-value"

// harness is a fake vmd, an Env pointing at it, and the group's commands.
type harness struct {
	t      *testing.T
	srv    *vmclitest.Server
	env    *cli.Env
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	stdin  *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	srv := vmclitest.New()
	t.Cleanup(srv.Close)
	h := &harness{t: t, srv: srv, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, stdin: &bytes.Buffer{}}
	h.env = &cli.Env{
		Stdin:   h.stdin,
		Stdout:  h.stdout,
		Stderr:  h.stderr,
		Config:  cli.Config{VMDURL: srv.URL(), Project: "demo", Token: testToken},
		HTTP:    srv.Client(),
		Command: "attic vm",
	}
	return h
}

// run calls a command of the group by name.
func (h *harness) run(name string, args ...string) error {
	h.t.Helper()
	for _, c := range Group().Commands {
		if c.Name == name {
			h.env.Command = "attic vm " + name
			return c.Run(context.Background(), h.env, args)
		}
	}
	h.t.Fatalf("no command %q", name)
	return nil
}

func (h *harness) addAlpha() string {
	return h.srv.AddVM("demo", "alpha", vmdv1.VMState_VM_STATE_RUNNING, 2, 1536)
}

func TestListOnePerVMWithPaging(t *testing.T) {
	h := newHarness(t)
	h.srv.AddVM("demo", "alpha", vmdv1.VMState_VM_STATE_RUNNING, 2, 1536)
	h.srv.AddVM("demo", "beta", vmdv1.VMState_VM_STATE_STOPPED, 1, 512)
	h.srv.AddVM("demo", "gamma", vmdv1.VMState_VM_STATE_PENDING, 4, 8192)
	h.srv.AddVM("other", "elsewhere", vmdv1.VMState_VM_STATE_RUNNING, 1, 1024)
	h.srv.SetPageSize(1) // forces three ListVMs pages

	if err := h.run("list"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"alpha running 2 1.5",
		"beta stopped 1 0.5",
		"gamma pending 4 8",
	}
	lines := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
	if len(lines) != len(want) {
		t.Fatalf("got %d lines %q, want %d", len(lines), lines, len(want))
	}
	for i := range want {
		if got := strings.Join(strings.Fields(lines[i]), " "); got != want[i] {
			t.Errorf("line %d = %q, want %q", i, got, want[i])
		}
	}
	for _, a := range h.srv.Authorizations() {
		if a != "Bearer "+testToken {
			t.Fatalf("Authorization = %q", a)
		}
	}
	if h.stderr.Len() != 0 {
		t.Fatalf("list wrote to stderr: %q", h.stderr.String())
	}
}

func TestListRefusesArguments(t *testing.T) {
	h := newHarness(t)
	err := h.run("list", "extra")
	if _, ok := err.(*cli.UsageError); !ok {
		t.Fatalf("err = %v, want a UsageError", err)
	}
}

func TestNameResolution(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		h := newHarness(t)
		h.srv.AddVM("demo", "beta", vmdv1.VMState_VM_STATE_RUNNING, 1, 512)
		err := h.run("cp", "alpha:/root/x", "-")
		if err == nil || !strings.Contains(err.Error(), `no VM named "alpha" in project "demo"`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("one", func(t *testing.T) {
		h := newHarness(t)
		id := h.addAlpha()
		h.srv.SetFile(id, "/root/x", []byte("data"))
		if err := h.run("cp", "alpha:/root/x", "-"); err != nil {
			t.Fatal(err)
		}
		if h.stdout.String() != "data" {
			t.Fatalf("stdout = %q", h.stdout.String())
		}
	})
	t.Run("many, found on a later page", func(t *testing.T) {
		h := newHarness(t)
		h.srv.AddVM("demo", "alpha", vmdv1.VMState_VM_STATE_RUNNING, 1, 512)
		h.srv.AddVM("demo", "alpha", vmdv1.VMState_VM_STATE_RUNNING, 1, 512)
		h.srv.SetPageSize(1)
		err := h.run("cp", "alpha:/root/x", "-")
		if err == nil || !strings.Contains(err.Error(), `2 VMs are named "alpha"`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("other project does not match", func(t *testing.T) {
		h := newHarness(t)
		h.srv.AddVM("other", "alpha", vmdv1.VMState_VM_STATE_RUNNING, 1, 512)
		if err := h.run("cp", "alpha:/root/x", "-"); err == nil {
			t.Fatal("a VM of another project must not match")
		}
	})
}

func TestUploadDownloadRoundTripKeepsMode(t *testing.T) {
	h := newHarness(t)
	id := h.addAlpha()
	dir := t.TempDir()
	src := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(src, []byte("hello attic\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := h.run("cp", src, "alpha:/root/in.txt"); err != nil {
		t.Fatalf("upload: %v", err)
	}
	puts := h.srv.Puts()
	if len(puts) != 1 || puts[0].VMID != id || puts[0].Path != "/root/in.txt" || puts[0].Mode != 0o640 || puts[0].Size != 12 {
		t.Fatalf("PutFile calls = %+v", puts)
	}
	if got := h.stderr.String(); got != "wrote 12 bytes to alpha:/root/in.txt\n" {
		t.Fatalf("stderr = %q", got)
	}

	h.stderr.Reset()
	dst := filepath.Join(dir, "out.txt")
	if err := h.run("cp", "alpha:/root/in.txt", dst); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "hello attic\n" {
		t.Fatalf("downloaded %q, %v", got, err)
	}
	if h.stderr.String() != "read 12 bytes from alpha:/root/in.txt\n" {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
	if h.stdout.Len() != 0 {
		t.Fatalf("download to a file wrote to stdout: %q", h.stdout.String())
	}
	if files := dirEntries(t, dir); len(files) != 2 {
		t.Fatalf("directory has %v, want only the two files", files)
	}
}

func TestUploadModeFlagAndStdinDefault(t *testing.T) {
	h := newHarness(t)
	h.addAlpha()
	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.run("cp", "--mode", "0600", src, "alpha:/a"); err != nil {
		t.Fatal(err)
	}
	h.stdin.WriteString("piped")
	if err := h.run("cp", "-", "alpha:/b"); err != nil {
		t.Fatal(err)
	}
	puts := h.srv.Puts()
	if len(puts) != 2 || puts[0].Mode != 0o600 || puts[1].Mode != 0o644 || puts[1].Size != 5 {
		t.Fatalf("PutFile calls = %+v", puts)
	}
}

func TestStdinAndStdoutForms(t *testing.T) {
	h := newHarness(t)
	id := h.addAlpha()
	h.stdin.WriteString("from stdin")
	if err := h.run("cp", "-", "alpha:/root/s"); err != nil {
		t.Fatal(err)
	}
	if data, ok := h.srv.File(id, "/root/s"); !ok || string(data) != "from stdin" {
		t.Fatalf("stored %q, %v", data, ok)
	}
	h.stdout.Reset()
	h.stderr.Reset()
	if err := h.run("cp", "alpha:/root/s", "-"); err != nil {
		t.Fatal(err)
	}
	if h.stdout.String() != "from stdin" {
		t.Fatalf("stdout must carry only the data, got %q", h.stdout.String())
	}
	if h.stderr.String() != "read 10 bytes from alpha:/root/s\n" {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
}

func TestLocalPathWithColonNeedsDotSlash(t *testing.T) {
	h := newHarness(t)
	h.addAlpha()
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("notes:v1", []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.run("cp", "./notes:v1", "alpha:/root/n"); err != nil {
		t.Fatalf("./ must force a local path: %v", err)
	}
	if err := h.run("cp", "notes:v1", "alpha:/root/n"); err == nil ||
		!strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("a relative path inside a VM must be refused, got %v", err)
	}
}

func TestOperandValidationMakesNoCall(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"relative remote path", []string{"-", "alpha:relative/x"}},
		{"remote path empty", []string{"-", "alpha:"}},
		{"both remote", []string{"alpha:/a", "beta:/b"}},
		{"both local", []string{"/tmp/a", "/tmp/b"}},
		{"name missing", []string{"-", ":/x"}},
		{"mode on download", []string{"--mode", "0600", "alpha:/a", "-"}},
		{"bad mode", []string{"--mode", "0999", "-", "alpha:/a"}},
		{"one operand", []string{"alpha:/a"}},
		{"empty operand", []string{"", "alpha:/a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			err := h.run("cp", tc.args...)
			if _, ok := err.(*cli.UsageError); !ok {
				t.Fatalf("err = %v, want a UsageError", err)
			}
			if n := len(h.srv.Authorizations()); n != 0 {
				t.Fatalf("validation sent %d calls", n)
			}
		})
	}
}

func TestUploadLimit(t *testing.T) {
	const limit = vmclitest.MaxFileBytes
	t.Run("file over 8 MiB is refused before sending", func(t *testing.T) {
		h := newHarness(t)
		h.addAlpha()
		src := filepath.Join(t.TempDir(), "big")
		if err := os.WriteFile(src, make([]byte, limit+1), 0o644); err != nil {
			t.Fatal(err)
		}
		err := h.run("cp", src, "alpha:/root/big")
		if err == nil || !strings.Contains(err.Error(), "larger than 8 MiB") || strings.Contains(err.Error(), "invalid_argument") {
			t.Fatalf("err = %v", err)
		}
		if n := len(h.srv.Authorizations()); n != 0 {
			t.Fatalf("an oversize upload sent %d calls; it must be refused before any call", n)
		}
	})
	t.Run("exactly 8 MiB is allowed", func(t *testing.T) {
		h := newHarness(t)
		h.addAlpha()
		src := filepath.Join(t.TempDir(), "edge")
		if err := os.WriteFile(src, make([]byte, limit), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := h.run("cp", src, "alpha:/root/edge"); err != nil {
			t.Fatal(err)
		}
		if puts := h.srv.Puts(); len(puts) != 1 || puts[0].Size != limit {
			t.Fatalf("PutFile calls = %+v", puts)
		}
	})
	t.Run("stdin over 8 MiB is refused", func(t *testing.T) {
		h := newHarness(t)
		h.addAlpha()
		h.stdin.Write(make([]byte, limit+1))
		err := h.run("cp", "-", "alpha:/root/s")
		if err == nil || !strings.Contains(err.Error(), "larger than 8 MiB") {
			t.Fatalf("err = %v", err)
		}
		if n := len(h.srv.Authorizations()); n != 0 {
			t.Fatalf("oversize stdin sent %d calls; it must be refused before any call", n)
		}
	})
}

func TestDownloadLimitWritesNothing(t *testing.T) {
	const limit = vmclitest.MaxFileBytes
	h := newHarness(t)
	id := h.addAlpha()
	h.srv.SetFile(id, "/root/big", make([]byte, limit+1))
	dir := t.TempDir()
	dst := filepath.Join(dir, "out")
	err := h.run("cp", "alpha:/root/big", dst)
	if err == nil || !strings.Contains(err.Error(), "larger than 8 MiB") || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("err = %v", err)
	}
	if files := dirEntries(t, dir); len(files) != 0 {
		t.Fatalf("truncated download left %v", files)
	}
	if h.stdout.Len() != 0 {
		t.Fatal("truncated download wrote to stdout")
	}
}

func TestDownloadLimitKeepsExistingFile(t *testing.T) {
	const limit = vmclitest.MaxFileBytes
	h := newHarness(t)
	id := h.addAlpha()
	h.srv.SetFile(id, "/root/big", make([]byte, limit+1))
	dst := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(dst, []byte("previous"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.run("cp", "alpha:/root/big", dst); err == nil {
		t.Fatal("expected an error")
	}
	if got, _ := os.ReadFile(dst); string(got) != "previous" {
		t.Fatalf("existing file changed to %q", got)
	}
}

func TestDownloadExactlyLimitIsAllowed(t *testing.T) {
	const limit = vmclitest.MaxFileBytes
	h := newHarness(t)
	id := h.addAlpha()
	h.srv.SetFile(id, "/root/edge", make([]byte, limit))
	dst := filepath.Join(t.TempDir(), "out")
	if err := h.run("cp", "alpha:/root/edge", dst); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dst); err != nil || fi.Size() != limit {
		t.Fatalf("stat %v, %v", fi, err)
	}
}

func TestServerErrorIsCodeAndMessage(t *testing.T) {
	h := newHarness(t)
	h.srv.Fail("ListVMs", connect.CodePermissionDenied, "no role in project demo")
	err := h.run("list")
	if err == nil || err.Error() != "permission_denied: no role in project demo" {
		t.Fatalf("err = %v", err)
	}

	h = newHarness(t)
	h.addAlpha()
	h.srv.Fail("PutFile", connect.CodeResourceExhausted, "disk full")
	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.run("cp", src, "alpha:/f"); err == nil || err.Error() != "resource_exhausted: disk full" {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingTokenMakesNoCall(t *testing.T) {
	h := newHarness(t)
	h.env.Config.Token = ""
	err := h.run("list")
	if err == nil || !strings.Contains(err.Error(), "ATTIC_TOKEN") {
		t.Fatalf("err = %v", err)
	}
	if n := len(h.srv.Authorizations()); n != 0 {
		t.Fatalf("sent %d calls without a token", n)
	}
}

func TestGibString(t *testing.T) {
	cases := map[int64]string{0: "0", 512: "0.5", 1024: "1", 1536: "1.5", 8192: "8", 100: "0.098", 1000: "0.977"}
	for mb, want := range cases {
		if got := gibString(mb); got != want {
			t.Errorf("gibString(%d) = %q, want %q", mb, got, want)
		}
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]uint32{"0644": 0o644, "600": 0o600, "7777": 0o7777} {
		got, set, err := parseMode(in)
		if err != nil || !set || got != want {
			t.Errorf("parseMode(%q) = %o, %v, %v", in, got, set, err)
		}
	}
	if _, set, err := parseMode(""); err != nil || set {
		t.Errorf("empty mode: set %v, err %v", set, err)
	}
	for _, bad := range []string{"0o644", "9", "10000", "rw"} {
		if _, _, err := parseMode(bad); err == nil {
			t.Errorf("parseMode(%q) accepted", bad)
		}
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
