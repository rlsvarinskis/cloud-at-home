package sshrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/guestexec"
)

// signalNames maps the signals the test server reports with exit-signal to their SSH names.
var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP:  "HUP",
	syscall.SIGINT:  "INT",
	syscall.SIGKILL: "KILL",
	syscall.SIGTERM: "TERM",
	syscall.SIGUSR1: "USR1",
}

// exitStatusMsg is the payload of an exit-status request.
type exitStatusMsg struct {
	Status uint32
}

// exitSignalMsg is the payload of an exit-signal request.
type exitSignalMsg struct {
	Signal     string
	CoreDumped bool
	Error      string
	Lang       string
}

// newTestServer starts an in-process SSH server on loopback that accepts public-key
// authentication only for the authorized key, and returns its address. Exec requests run
// with bash on this host; when the client connection closes, the command's process group is
// killed, which mirrors closing a session in a real guest.
func newTestServer(t *testing.T, authorized ssh.PublicKey) string {
	t.Helper()
	hostSigner := newSigner(t)
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if conn.User() == "agent" && bytes.Equal(key.Marshal(), authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("key rejected for user %q", conn.User())
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { ln.Close() })
	go acceptLoop(ln, cfg)
	return ln.Addr().String()
}

// acceptLoop serves connections until the listener is closed.
func acceptLoop(ln net.Listener, cfg *ssh.ServerConfig) {
	for {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		go serveConn(nc, cfg)
	}
}

// serveConn handles one SSH connection, accepting session channels only.
func serveConn(nc net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}
	defer sc.Close()
	connDone := make(chan struct{})
	go func() {
		sc.Wait()
		close(connDone)
	}()
	go ssh.DiscardRequests(reqs)

	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go serveSession(ch, chReqs, connDone)
	}
}

// serveSession waits for the exec request on a session channel and runs it.
func serveSession(ch ssh.Channel, reqs <-chan *ssh.Request, connDone <-chan struct{}) {
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var p struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &p); err != nil {
			req.Reply(false, nil)
			continue
		}
		req.Reply(true, nil)
		go ssh.DiscardRequests(reqs)
		runExec(ch, p.Command, connDone)
		return
	}
	ch.Close()
}

// runExec runs command with bash -c, wires it to the channel, reports its status and
// closes the channel.
func runExec(ch ssh.Channel, command string, connDone <-chan struct{}) {
	defer ch.Close()
	cmd := exec.Command("bash", "-c", command)
	cmd.Stdin = ch
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusMsg{Status: 127}))
		return
	}

	finished := make(chan struct{})
	go func() {
		select {
		case <-connDone:
			// The client went away (cancel or disconnect): kill the whole command.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-finished:
		}
	}()
	err := cmd.Wait()
	close(finished)

	if err == nil {
		ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusMsg{Status: 0}))
		return
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusMsg{Status: 255}))
		return
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if ok && ws.Signaled() {
		if name, known := signalNames[ws.Signal()]; known {
			ch.SendRequest("exit-signal", false, ssh.Marshal(exitSignalMsg{Signal: name}))
			return
		}
		ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusMsg{Status: 128 + uint32(ws.Signal())}))
		return
	}
	ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusMsg{Status: uint32(ee.ExitCode())}))
}

// newFixture returns a Runner whose test server authorizes its own fresh client key.
// tweak may adjust the Config before New is called.
func newFixture(t *testing.T, tweak func(*Config)) *Runner {
	t.Helper()
	signer := newSigner(t)
	c := Config{Addr: newTestServer(t, signer.PublicKey()), Signer: signer}
	if tweak != nil {
		tweak(&c)
	}
	return New(c)
}

// newSigner returns a fresh in-memory Ed25519 signer.
func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	must(t, err)
	s, err := ssh.NewSignerFromKey(priv)
	must(t, err)
	return s
}

// testCtx returns a context that is cancelled when the test ends or after a minute.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// must fails the test on a non-nil error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunSeparatesStdoutAndStderr(t *testing.T) {
	r := newFixture(t, nil)
	out, errOut, code, err := r.Run(testCtx(t), `printf 'to-out'; printf 'to-err' >&2`, nil)
	must(t, err)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if string(out) != "to-out" {
		t.Errorf("stdout = %q, want %q", out, "to-out")
	}
	if string(errOut) != "to-err" {
		t.Errorf("stderr = %q, want %q", errOut, "to-err")
	}
}

func TestRunExitStatusIsNotAnError(t *testing.T) {
	r := newFixture(t, nil)
	for _, tc := range []struct {
		command string
		want    int
	}{
		{"exit 0", 0},
		{"exit 7", 7},
		{"false", 1},
		{"exit 200", 200},
	} {
		t.Run(tc.command, func(t *testing.T) {
			// A child inherits SIG_IGN from this process. Under nohup or a launcher that ignores SIGHUP,
			// "kill -HUP $$" is then a no-op, so the case cannot be checked here.
			if tc.command == "kill -HUP $$" && signal.Ignored(syscall.SIGHUP) {
				t.Skip("SIGHUP is ignored in this process, so the child inherits it")
			}
			_, _, code, err := r.Run(testCtx(t), tc.command, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if code != tc.want {
				t.Errorf("exit code = %d, want %d", code, tc.want)
			}
		})
	}
}

func TestRunSignalExitCodes(t *testing.T) {
	r := newFixture(t, nil)
	for _, tc := range []struct {
		command string
		want    int
	}{
		{"kill -TERM $$", 143},
		{"kill -KILL $$", 137},
		{"kill -INT $$", 130},
		{"kill -HUP $$", 129},
		{"kill -USR1 $$", 255},
	} {
		t.Run(tc.command, func(t *testing.T) {
			if tc.command == "kill -HUP $$" && signal.Ignored(syscall.SIGHUP) { // same guard as the test above
				t.Skip("SIGHUP is ignored in this process, so the child inherits it")
			}
			_, _, code, err := r.Run(testCtx(t), tc.command, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if code != tc.want {
				t.Errorf("exit code = %d, want %d", code, tc.want)
			}
		})
	}
}

func TestRunStdin(t *testing.T) {
	r := newFixture(t, nil)
	ctx := testCtx(t)

	out, _, code, err := r.Run(ctx, "cat", nil)
	must(t, err)
	if code != 0 || len(out) != 0 {
		t.Errorf("nil stdin: code %d, stdout %q; want 0 and empty", code, out)
	}

	out, _, _, err = r.Run(ctx, "cat", []byte("hello\n"))
	must(t, err)
	if string(out) != "hello\n" {
		t.Errorf("stdin passthrough = %q, want %q", out, "hello\n")
	}

	big := make([]byte, 1<<20)
	_, err = rand.Read(big)
	must(t, err)
	out, _, code, err = r.Run(ctx, "cat", big)
	must(t, err)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !bytes.Equal(out, big) {
		t.Errorf("1 MiB stdin round trip: got %d bytes, want %d identical bytes", len(out), len(big))
	}
}

func TestRunLargeOutput(t *testing.T) {
	r := newFixture(t, nil)
	out, _, code, err := r.Run(testCtx(t), "head -c 1048576 /dev/zero", nil)
	must(t, err)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !bytes.Equal(out, make([]byte, 1<<20)) {
		t.Errorf("stdout: got %d bytes, want 1 MiB of zeros", len(out))
	}
}

func TestRunQuotesAndNewlines(t *testing.T) {
	r := newFixture(t, nil)
	command := `name='it'\''s "quoted"'` + "\n" +
		`printf '%s\n' "$name"` + "\n" +
		"echo \"two\nlines\"\n"
	out, _, code, err := r.Run(testCtx(t), command, nil)
	must(t, err)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if want := "it's \"quoted\"\ntwo\nlines\n"; string(out) != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

func TestRunOutputCap(t *testing.T) {
	r := newFixture(t, func(c *Config) { c.MaxOutput = 1000 })
	out, errOut, code, err := r.Run(testCtx(t),
		"head -c 5000 /dev/zero | tr '\\0' a; head -c 3000 /dev/zero | tr '\\0' b >&2", nil)
	must(t, err)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if want := strings.Repeat("a", 1000); string(out) != want {
		t.Errorf("stdout: got %d bytes, want the first 1000 bytes", len(out))
	}
	if want := strings.Repeat("b", 1000); string(errOut) != want {
		t.Errorf("stderr: got %d bytes, want the first 1000 bytes", len(errOut))
	}
}

func TestRunWrongKeyIsUnavailable(t *testing.T) {
	authorized := newSigner(t)
	addr := newTestServer(t, authorized.PublicKey())
	r := New(Config{Addr: addr, Signer: newSigner(t)})

	out, errOut, code, err := r.Run(testCtx(t), "true", nil)
	if !errors.Is(err, errs.ErrUnavailable) {
		t.Fatalf("err = %v, want one wrapping errs.ErrUnavailable", err)
	}
	if out != nil || errOut != nil || code != 0 {
		t.Errorf("on error got stdout %q, stderr %q, code %d; want zero values", out, errOut, code)
	}
}

func TestRunNothingListeningIsUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	addr := ln.Addr().String()
	ln.Close()

	r := New(Config{Addr: addr, Signer: newSigner(t)})
	_, _, _, err = r.Run(testCtx(t), "true", nil)
	if !errors.Is(err, errs.ErrUnavailable) {
		t.Fatalf("err = %v, want one wrapping errs.ErrUnavailable", err)
	}
}

func TestRunDialTimeout(t *testing.T) {
	// The kernel completes TCP connects into this listener's backlog, but nothing ever
	// answers the SSH handshake, so only the dial timeout can end the call.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { ln.Close() })

	const timeout = 300 * time.Millisecond
	r := New(Config{Addr: ln.Addr().String(), Signer: newSigner(t), DialTimeout: timeout})
	start := time.Now()
	_, _, _, err = r.Run(testCtx(t), "true", nil)
	elapsed := time.Since(start)

	if !errors.Is(err, errs.ErrUnavailable) {
		t.Fatalf("err = %v, want one wrapping errs.ErrUnavailable", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("Run took %v with a %v dial timeout", elapsed, timeout)
	}
}

func TestRunContextCancelMidCommand(t *testing.T) {
	r := newFixture(t, nil)
	pidFile := filepath.Join(t.TempDir(), "pid")
	command := "echo $$ > '" + pidFile + "'; sleep 30"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		err  error
		code int
	}
	done := make(chan result, 1)
	go func() {
		_, _, code, err := r.Run(ctx, command, nil)
		done <- result{err: err, code: code}
	}()

	pid := waitForPID(t, pidFile)
	cancelAt := time.Now()
	cancel()

	select {
	case res := <-done:
		if elapsed := time.Since(cancelAt); elapsed > 2*time.Second {
			t.Errorf("Run returned %v after cancel, want < 2s", elapsed)
		}
		if !errors.Is(res.err, context.Canceled) {
			t.Errorf("err = %v, want one wrapping context.Canceled", res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}

	// The guest-side command must not survive the cancel.
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(-pid, syscall.SIGKILL)
			t.Fatalf("command process %d still alive 2s after cancel", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForPID polls until pidFile holds a process id and returns it.
func waitForPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("command never reported its pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPublicKeyLineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")

	made, err := GenerateKey(keyPath)
	must(t, err)

	line := PublicKeyLine(made)
	if !strings.HasPrefix(line, "ssh-ed25519 ") {
		t.Errorf("PublicKeyLine = %q, want an ssh-ed25519 line", line)
	}
	if strings.Contains(line, "\n") {
		t.Errorf("PublicKeyLine = %q, must not contain a newline", line)
	}

	pubFile, err := os.ReadFile(keyPath + ".pub")
	must(t, err)
	if got := strings.TrimSpace(string(pubFile)); got != line {
		t.Errorf(".pub file = %q, want %q", got, line)
	}

	loaded, err := LoadSigner(keyPath)
	must(t, err)
	if !bytes.Equal(loaded.PublicKey().Marshal(), made.PublicKey().Marshal()) {
		t.Error("loaded key differs from generated key")
	}

	info, err := os.Stat(keyPath)
	must(t, err)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("private key mode = %o, want 600", perm)
	}

	// The server authorizes exactly the key parsed back from the public line.
	authorized, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line + "\n"))
	must(t, err)
	r := New(Config{Addr: newTestServer(t, authorized), Signer: loaded})
	out, _, code, err := r.Run(testCtx(t), "echo ok", nil)
	must(t, err)
	if code != 0 || string(out) != "ok\n" {
		t.Errorf("Run with generated key: code %d, stdout %q", code, out)
	}
}

func TestGenerateKeyDoesNotOverwrite(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")

	first, err := GenerateKey(keyPath)
	must(t, err)
	before, err := os.ReadFile(keyPath)
	must(t, err)

	second, err := GenerateKey(keyPath)
	must(t, err)
	after, err := os.ReadFile(keyPath)
	must(t, err)

	if !bytes.Equal(before, after) {
		t.Error("GenerateKey overwrote an existing key file")
	}
	if !bytes.Equal(first.PublicKey().Marshal(), second.PublicKey().Marshal()) {
		t.Error("GenerateKey on an existing file returned a different key")
	}
}

func TestLoadSignerPKCS8(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	must(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	must(t, err)
	must(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))

	s, err := LoadSigner(keyPath)
	must(t, err)
	if !bytes.Equal(s.PublicKey().Marshal(), mustSSHKey(t, pub).Marshal()) {
		t.Error("PKCS#8 key loaded with the wrong public key")
	}
}

func TestLoadSignerErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadSigner(filepath.Join(dir, "missing")); err == nil {
		t.Error("LoadSigner on a missing file: want error")
	}
	garbage := filepath.Join(dir, "garbage")
	must(t, os.WriteFile(garbage, []byte("not a key"), 0o600))
	if _, err := LoadSigner(garbage); err == nil {
		t.Error("LoadSigner on garbage: want error")
	}
}

// mustSSHKey converts an Ed25519 public key to an ssh.PublicKey.
func mustSSHKey(t *testing.T, pub ed25519.PublicKey) ssh.PublicKey {
	t.Helper()
	k, err := ssh.NewPublicKey(pub)
	must(t, err)
	return k
}

func TestGuestexecIntegration(t *testing.T) {
	r := newFixture(t, nil)
	var _ guestexec.Runner = r
	ctx := testCtx(t)
	ex := &guestexec.Exec{R: r, JobsDir: filepath.Join(t.TempDir(), "jobs")}

	err := ex.Start(ctx, guestexec.Spec{
		JobID:          "job-1",
		Command:        "echo hello; echo oops >&2; exit 3",
		Workdir:        t.TempDir(),
		TimeoutSeconds: 30,
	})
	must(t, err)

	var resp *nodev1.PollJobResponse
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err = ex.Poll(ctx, "job-1", 0, 0, 0, false)
		must(t, err)
		if resp.State != nodev1.GuestJobState_GUEST_JOB_STATE_RUNNING {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job still running after 20s")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.State != nodev1.GuestJobState_GUEST_JOB_STATE_EXITED {
		t.Fatalf("state = %v, want EXITED", resp.State)
	}
	if resp.ExitCode == nil || *resp.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", resp.ExitCode)
	}
	if string(resp.Stdout) != "hello\n" {
		t.Errorf("stdout = %q, want %q", resp.Stdout, "hello\n")
	}
	if string(resp.Stderr) != "oops\n" {
		t.Errorf("stderr = %q, want %q", resp.Stderr, "oops\n")
	}
}
