package guestexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

// localRunner runs plumbing scripts with bash on this machine instead of over ssh.
type localRunner struct{}

// Run implements Runner by running command with `bash -c` locally.
func (localRunner) Run(ctx context.Context, command string, stdin []byte) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), stderr.Bytes(), 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return stdout.Bytes(), stderr.Bytes(), ee.ExitCode(), nil
	}
	return nil, nil, 0, err
}

// newExec returns an Exec whose jobs live in a fresh temporary directory.
func newExec(t *testing.T) *Exec {
	t.Helper()
	return &Exec{R: localRunner{}, JobsDir: t.TempDir()}
}

// killJob registers a cleanup that SIGKILLs the job's whole session, so no process the
// job started outlives the test.
func killJob(t *testing.T, e *Exec, id string) {
	t.Helper()
	t.Cleanup(func() {
		b, err := os.ReadFile(filepath.Join(e.JobsDir, id, "pgid"))
		if err != nil {
			return
		}
		pgid := strings.TrimSpace(string(b))
		if pgid == "" {
			return
		}
		_ = exec.Command("pkill", "-KILL", "-s", pgid).Run()
	})
}

// mustStart starts a job and registers its cleanup.
func mustStart(t *testing.T, e *Exec, s Spec) {
	t.Helper()
	if err := e.Start(t.Context(), s); err != nil {
		t.Fatalf("Start(%s): %v", s.JobID, err)
	}
	killJob(t, e, s.JobID)
}

// pollUntil polls job id every 50ms until done reports true, failing the test after d.
func pollUntil(t *testing.T, e *Exec, id string, d time.Duration, done func(*nodev1.PollJobResponse) bool) *nodev1.PollJobResponse {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		r, err := e.Poll(t.Context(), id, 0, 0, 0, false)
		if err != nil {
			t.Fatalf("Poll(%s): %v", id, err)
		}
		if done(r) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s: still state %v after %v", id, r.GetState(), d)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitExited waits for the job to reach EXITED and returns its poll result.
func waitExited(t *testing.T, e *Exec, id string) *nodev1.PollJobResponse {
	t.Helper()
	return pollUntil(t, e, id, 30*time.Second, func(r *nodev1.PollJobResponse) bool {
		return r.GetState() == nodev1.GuestJobState_GUEST_JOB_STATE_EXITED
	})
}

// anyProcess reports whether pgrep with args finds a process.
func anyProcess(t *testing.T, args ...string) bool {
	t.Helper()
	out, err := exec.Command("pgrep", args...).Output()
	if err == nil {
		return len(bytes.TrimSpace(out)) > 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false
	}
	t.Fatalf("pgrep %v: %v", args, err)
	return false
}

// readPgid returns the recorded session leader of a job.
func readPgid(t *testing.T, e *Exec, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.JobsDir, id, "pgid"))
	if err != nil {
		t.Fatalf("read pgid: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func TestFastJobStdoutStderrExitCode(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		command  string
		wantExit int32
		wantOut  string
		wantErr  string
	}{
		{"zero", "fast-ok", "echo out; echo err >&2; exit 0", 0, "out\n", "err\n"},
		{"nonzero", "fast-fail", "echo oops >&2; exit 7", 7, "", "oops\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newExec(t)
			mustStart(t, e, Spec{JobID: tc.id, Command: tc.command, TimeoutSeconds: 30})
			r := waitExited(t, e, tc.id)
			if r.GetExitCode() != tc.wantExit {
				t.Errorf("exit code = %d, want %d", r.GetExitCode(), tc.wantExit)
			}
			if string(r.GetStdout()) != tc.wantOut || string(r.GetStderr()) != tc.wantErr {
				t.Errorf("stdout %q stderr %q, want %q %q", r.GetStdout(), r.GetStderr(), tc.wantOut, tc.wantErr)
			}
			if r.GetCancelled() || r.GetMore() || r.GetTruncated() {
				t.Errorf("unexpected flags: cancelled=%v more=%v truncated=%v", r.GetCancelled(), r.GetMore(), r.GetTruncated())
			}
		})
	}
}

func TestRunningThenExited(t *testing.T) {
	e := newExec(t)
	mustStart(t, e, Spec{JobID: "slow", Command: "sleep 2; echo done", TimeoutSeconds: 30})

	first, err := e.Poll(t.Context(), "slow", 0, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_RUNNING {
		t.Fatalf("first poll state = %v, want RUNNING", first.GetState())
	}
	if first.ExitCode != nil {
		t.Errorf("running job has exit code %d", first.GetExitCode())
	}

	r := waitExited(t, e, "slow")
	if r.GetExitCode() != 0 || string(r.GetStdout()) != "done\n" {
		t.Errorf("exit %d stdout %q, want 0 and \"done\\n\"", r.GetExitCode(), r.GetStdout())
	}
	if r.GetElapsedSeconds() < 1 {
		t.Errorf("elapsed = %d, want at least 1", r.GetElapsedSeconds())
	}
}

func TestBackgroundChildDoesNotBlockOrKeepRunning(t *testing.T) {
	e := newExec(t)
	start := time.Now()
	mustStart(t, e, Spec{JobID: "bg-child", Command: "sleep 30 & echo started", TimeoutSeconds: 60})
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Start took %v; it must not wait for background children", d)
	}
	r := waitExited(t, e, "bg-child")
	if r.GetExitCode() != 0 || string(r.GetStdout()) != "started\n" {
		t.Errorf("exit %d stdout %q, want 0 and \"started\\n\"", r.GetExitCode(), r.GetStdout())
	}
}

func TestLifetimeTimeoutKills(t *testing.T) {
	e := newExec(t)
	mustStart(t, e, Spec{JobID: "timeout", Command: "sleep 30", TimeoutSeconds: 1})
	r := waitExited(t, e, "timeout")
	if r.GetExitCode() != 124 {
		t.Errorf("exit code = %d, want 124", r.GetExitCode())
	}
}

func TestCancelKillsProcessGroup(t *testing.T) {
	const marker = "guestexec-cancel-marker-7f3a"
	e := newExec(t)
	mustStart(t, e, Spec{
		JobID:          "cancel",
		Command:        fmt.Sprintf("sleep 1000 & sleep 1001 & echo %s; wait", marker),
		TimeoutSeconds: 120,
	})
	pollUntil(t, e, "cancel", 10*time.Second, func(r *nodev1.PollJobResponse) bool {
		return bytes.Contains(r.GetStdout(), []byte(marker))
	})
	pgid := readPgid(t, e, "cancel")
	if !anyProcess(t, "-s", pgid) {
		t.Fatal("expected processes in the job session before cancel")
	}

	if err := e.Cancel(t.Context(), "cancel"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	r := waitExited(t, e, "cancel")
	if r.GetExitCode() != 130 || !r.GetCancelled() {
		t.Errorf("exit %d cancelled %v, want 130 and true", r.GetExitCode(), r.GetCancelled())
	}

	deadline := time.Now().Add(10 * time.Second)
	for anyProcess(t, "-s", pgid) || anyProcess(t, "-f", marker) || anyProcess(t, "-f", "sleep 100[01]") {
		if time.Now().After(deadline) {
			t.Fatal("processes of the cancelled job are still running")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestCancelFinishedJobIsNoop(t *testing.T) {
	e := newExec(t)
	mustStart(t, e, Spec{JobID: "done-first", Command: "echo ok", TimeoutSeconds: 30})
	waitExited(t, e, "done-first")
	if err := e.Cancel(t.Context(), "done-first"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	r, err := e.Poll(t.Context(), "done-first", 0, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.GetExitCode() != 0 || r.GetCancelled() {
		t.Errorf("exit %d cancelled %v, want the original 0 and false", r.GetExitCode(), r.GetCancelled())
	}
}

func TestPagingOffsetAndTail(t *testing.T) {
	const total = 70000
	pattern := []byte("abcdefghij\n")
	want := make([]byte, total)
	for i := range want {
		want[i] = pattern[i%len(pattern)]
	}

	e := newExec(t)
	mustStart(t, e, Spec{JobID: "pages", Command: "yes abcdefghij | head -c 70000; printf err-tail >&2", TimeoutSeconds: 60})
	waitExited(t, e, "pages")

	t.Run("offset pages reassemble full output", func(t *testing.T) {
		var got []byte
		var off int64
		for pages := 0; ; pages++ {
			if pages > 100 {
				t.Fatal("too many pages")
			}
			r, err := e.Poll(t.Context(), "pages", off, 0, 10000, false)
			if err != nil {
				t.Fatal(err)
			}
			if r.GetStdoutFrom() != off {
				t.Fatalf("StdoutFrom = %d, want %d", r.GetStdoutFrom(), off)
			}
			if r.GetTruncated() {
				t.Fatal("offset mode must not set Truncated")
			}
			got = append(got, r.GetStdout()...)
			off = r.GetStdoutOffset()
			if !r.GetMore() {
				break
			}
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("reassembled %d bytes, want %d matching bytes", len(got), len(want))
		}
	})

	t.Run("offset stderr with max 3", func(t *testing.T) {
		var got []byte
		var off int64
		for pages := 0; pages < 100; pages++ {
			r, err := e.Poll(t.Context(), "pages", 0, off, 3, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.GetStderr()) > 3 {
				t.Fatalf("stderr chunk %d bytes, want at most 3", len(r.GetStderr()))
			}
			got = append(got, r.GetStderr()...)
			off = r.GetStderrOffset()
			if !r.GetMore() {
				break
			}
		}
		if string(got) != "err-tail" {
			t.Errorf("stderr = %q, want \"err-tail\"", got)
		}
	})

	t.Run("offset past end is empty", func(t *testing.T) {
		r, err := e.Poll(t.Context(), "pages", 80000, 0, 100, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.GetStdout()) != 0 || r.GetMore() || r.GetStdoutFrom() != 80000 {
			t.Errorf("got %d bytes more=%v from=%d", len(r.GetStdout()), r.GetMore(), r.GetStdoutFrom())
		}
	})

	t.Run("tail returns last bytes", func(t *testing.T) {
		r, err := e.Poll(t.Context(), "pages", 0, 0, 1000, true)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(r.GetStdout(), want[total-1000:]) {
			t.Error("tail bytes do not match the last 1000 bytes of output")
		}
		if r.GetStdoutFrom() != total-1000 || r.GetStdoutOffset() != total || !r.GetTruncated() || r.GetMore() {
			t.Errorf("from=%d offset=%d truncated=%v more=%v", r.GetStdoutFrom(), r.GetStdoutOffset(), r.GetTruncated(), r.GetMore())
		}
	})

	t.Run("tail default is 8192", func(t *testing.T) {
		r, err := e.Poll(t.Context(), "pages", 0, 0, 0, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.GetStdout()) != 8192 || r.GetStdoutFrom() != total-8192 {
			t.Errorf("got %d bytes from %d, want 8192 from %d", len(r.GetStdout()), r.GetStdoutFrom(), total-8192)
		}
	})

	t.Run("tail max is capped at 65536", func(t *testing.T) {
		r, err := e.Poll(t.Context(), "pages", 0, 0, 1<<20, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.GetStdout()) != 65536 || r.GetStdoutFrom() != total-65536 {
			t.Errorf("got %d bytes from %d, want 65536 from %d", len(r.GetStdout()), r.GetStdoutFrom(), total-65536)
		}
	})

	t.Run("negative max uses default", func(t *testing.T) {
		r, err := e.Poll(t.Context(), "pages", 0, 0, -5, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.GetStdout()) != 8192 || !r.GetMore() {
			t.Errorf("got %d bytes more=%v, want 8192 and more", len(r.GetStdout()), r.GetMore())
		}
	})

	t.Run("max of one byte", func(t *testing.T) {
		r, err := e.Poll(t.Context(), "pages", 5, 0, 1, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.GetStdout()) != 1 || r.GetStdout()[0] != want[5] || r.GetStdoutOffset() != 6 {
			t.Errorf("got %q offset %d", r.GetStdout(), r.GetStdoutOffset())
		}
	})

	t.Run("small output in tail mode is not truncated", func(t *testing.T) {
		mustStart(t, e, Spec{JobID: "small", Command: "echo hi", TimeoutSeconds: 30})
		waitExited(t, e, "small")
		r, err := e.Poll(t.Context(), "small", 0, 0, 100, true)
		if err != nil {
			t.Fatal(err)
		}
		if string(r.GetStdout()) != "hi\n" || r.GetTruncated() || r.GetStdoutFrom() != 0 {
			t.Errorf("stdout %q truncated=%v from=%d", r.GetStdout(), r.GetTruncated(), r.GetStdoutFrom())
		}
	})
}

func TestUnknownJobIsMissing(t *testing.T) {
	e := newExec(t)
	r, err := e.Poll(t.Context(), "no-such-job", 0, 0, 0, false)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if r.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_MISSING {
		t.Errorf("state = %v, want MISSING", r.GetState())
	}
}

func TestLostWhenSessionKilledWithoutExitCode(t *testing.T) {
	e := newExec(t)
	mustStart(t, e, Spec{JobID: "lost", Command: "sleep 60", TimeoutSeconds: 120})
	pollUntil(t, e, "lost", 10*time.Second, func(r *nodev1.PollJobResponse) bool {
		return r.GetState() == nodev1.GuestJobState_GUEST_JOB_STATE_RUNNING
	})
	pgid := readPgid(t, e, "lost")
	if out, err := exec.Command("pkill", "-KILL", "-s", pgid).CombinedOutput(); err != nil {
		t.Fatalf("pkill: %v: %s", err, out)
	}

	r := pollUntil(t, e, "lost", 10*time.Second, func(r *nodev1.PollJobResponse) bool {
		return r.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_RUNNING
	})
	if r.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_LOST {
		t.Fatalf("state = %v, want LOST", r.GetState())
	}
	if r.ExitCode != nil || r.GetCancelled() {
		t.Errorf("lost job: exit code %v cancelled %v, want none and false", r.ExitCode, r.GetCancelled())
	}
	if _, err := os.Stat(filepath.Join(e.JobsDir, "lost", "exit_code")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("exit_code must not exist after a plain kill, stat err = %v", err)
	}
}

func TestWorkdirHonoured(t *testing.T) {
	e := newExec(t)
	dir := filepath.Join(t.TempDir(), "new", "dir with 'quote'")
	mustStart(t, e, Spec{JobID: "workdir", Command: "pwd", Workdir: dir, TimeoutSeconds: 30})
	r := waitExited(t, e, "workdir")
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("workdir was not created: %v", err)
	}
	if got := strings.TrimSpace(string(r.GetStdout())); got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}
}

func TestSpecialCharactersInCommand(t *testing.T) {
	command := `echo "it's \$((1+2)) \"q\""
printf '%s\n' 'é ✓ "x" $HOME'
cat <<'EOF'
line a
'b'
EOF
`
	want := "it's $((1+2)) \"q\"\né ✓ \"x\" $HOME\nline a\n'b'\n"
	e := newExec(t)
	mustStart(t, e, Spec{JobID: "special-chars_1", Command: command, TimeoutSeconds: 30})
	r := waitExited(t, e, "special-chars_1")
	if r.GetExitCode() != 0 || string(r.GetStdout()) != want {
		t.Errorf("exit %d stdout %q, want %q", r.GetExitCode(), r.GetStdout(), want)
	}
}

func TestInvalidJobIDRejected(t *testing.T) {
	e := newExec(t)
	bad := []string{"", "../x", "a/b", "a b", "semi;colon", strings.Repeat("a", 65)}
	for _, id := range bad {
		if err := e.Start(t.Context(), Spec{JobID: id, Command: "true"}); !errors.Is(err, errs.ErrInvalidArgument) {
			t.Errorf("Start(%q) err = %v, want InvalidArgument", id, err)
		}
		if _, err := e.Poll(t.Context(), id, 0, 0, 0, false); !errors.Is(err, errs.ErrInvalidArgument) {
			t.Errorf("Poll(%q) err = %v, want InvalidArgument", id, err)
		}
		if err := e.Cancel(t.Context(), id); !errors.Is(err, errs.ErrInvalidArgument) {
			t.Errorf("Cancel(%q) err = %v, want InvalidArgument", id, err)
		}
	}
	r, err := e.Poll(t.Context(), strings.Repeat("a", 64), 0, 0, 0, false)
	if err != nil || r.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_MISSING {
		t.Errorf("64-char id: state %v err %v, want MISSING and nil", r, err)
	}
}

func TestRelativeJobsDirRejected(t *testing.T) {
	e := &Exec{R: localRunner{}, JobsDir: "relative/jobs"}
	if _, err := e.Poll(t.Context(), "abc", 0, 0, 0, false); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("err = %v, want InvalidArgument", err)
	}
}

func TestDuplicateJobIDRejected(t *testing.T) {
	e := newExec(t)
	mustStart(t, e, Spec{JobID: "dup", Command: "true", TimeoutSeconds: 30})
	waitExited(t, e, "dup")
	if err := e.Start(t.Context(), Spec{JobID: "dup", Command: "echo again", TimeoutSeconds: 30}); !errors.Is(err, errs.ErrAlreadyExists) {
		t.Errorf("second Start err = %v, want AlreadyExists", err)
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	e := newExec(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sub dir", `it's "quoted" $x.bin`)
	content := []byte("héllo \"world\" 'x'\n")
	for i := 0; i < 256; i++ {
		content = append(content, byte(i))
	}

	if err := e.PutFile(t.Context(), path, content, 0); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("default mode = %v, want 0644", info.Mode().Perm())
	}

	got, truncated, err := e.GetFile(t.Context(), path, 0)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if truncated || !bytes.Equal(got, content) {
		t.Errorf("round trip mismatch: truncated=%v len=%d want %d", truncated, len(got), len(content))
	}

	if err := e.PutFile(t.Context(), path, []byte("secret"), 0o600); err != nil {
		t.Fatalf("PutFile mode 0600: %v", err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestGetFileTruncation(t *testing.T) {
	e := newExec(t)
	path := filepath.Join(t.TempDir(), "hundred")
	data := bytes.Repeat([]byte("0123456789"), 10)
	if err := e.PutFile(t.Context(), path, data, 0); err != nil {
		t.Fatal(err)
	}

	got, truncated, err := e.GetFile(t.Context(), path, 10)
	if err != nil || !truncated || !bytes.Equal(got, data[:10]) {
		t.Errorf("max 10: len=%d truncated=%v err=%v", len(got), truncated, err)
	}
	got, truncated, err = e.GetFile(t.Context(), path, 99)
	if err != nil || !truncated || len(got) != 99 {
		t.Errorf("max 99: len=%d truncated=%v err=%v", len(got), truncated, err)
	}
	got, truncated, err = e.GetFile(t.Context(), path, 100)
	if err != nil || truncated || !bytes.Equal(got, data) {
		t.Errorf("max 100: len=%d truncated=%v err=%v", len(got), truncated, err)
	}
}

func TestGetFileMissingIsNotFound(t *testing.T) {
	e := newExec(t)
	_, _, err := e.GetFile(t.Context(), filepath.Join(t.TempDir(), "nope"), 0)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("err = %v, want NotFound", err)
	}
}
