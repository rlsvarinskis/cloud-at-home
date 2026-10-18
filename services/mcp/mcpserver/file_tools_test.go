package mcpserver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1/vmdv1connect"
)

// filVMs fakes the VM service: only ListVMs is used by the file tools.
type filVMs struct {
	vmdv1connect.VMServiceClient
	vms   []*vmdv1.VM
	calls int
}

func (f *filVMs) ListVMs(_ context.Context, _ *connect.Request[vmdv1.ListVMsRequest]) (*connect.Response[vmdv1.ListVMsResponse], error) {
	f.calls++
	return connect.NewResponse(&vmdv1.ListVMsResponse{Vms: f.vms}), nil
}

// filExec fakes the exec service. Every call is recorded; the behaviour of each method is set by the test.
type filExec struct {
	vmdv1connect.ExecServiceClient
	puts     []*vmdv1.PutFileRequest
	putErr   error
	gets     []*vmdv1.GetFileRequest
	getRes   *vmdv1.GetFileResponse
	getErr   error
	starts   []*vmdv1.StartJobRequest
	startOut *vmdv1.JobOutput
	startErr error
	polls    []*vmdv1.PollJobRequest
	pollErr  error
	// pollFn returns the job state for the n-th poll (1-based). Nil means "still running".
	pollFn func(n int) *vmdv1.JobOutput
}

func (f *filExec) PutFile(_ context.Context, req *connect.Request[vmdv1.PutFileRequest]) (*connect.Response[vmdv1.PutFileResponse], error) {
	f.puts = append(f.puts, req.Msg)
	if f.putErr != nil {
		return nil, f.putErr
	}
	return connect.NewResponse(&vmdv1.PutFileResponse{}), nil
}

func (f *filExec) GetFile(_ context.Context, req *connect.Request[vmdv1.GetFileRequest]) (*connect.Response[vmdv1.GetFileResponse], error) {
	f.gets = append(f.gets, req.Msg)
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getRes == nil {
		return connect.NewResponse(&vmdv1.GetFileResponse{}), nil
	}
	return connect.NewResponse(f.getRes), nil
}

func (f *filExec) StartJob(_ context.Context, req *connect.Request[vmdv1.StartJobRequest]) (*connect.Response[vmdv1.JobOutput], error) {
	f.starts = append(f.starts, req.Msg)
	if f.startErr != nil {
		return nil, f.startErr
	}
	out := f.startOut
	if out == nil {
		out = filJobOut(vmdv1.JobState_JOB_STATE_RUNNING, nil, "", "")
	}
	return connect.NewResponse(out), nil
}

func (f *filExec) PollJob(_ context.Context, req *connect.Request[vmdv1.PollJobRequest]) (*connect.Response[vmdv1.JobOutput], error) {
	f.polls = append(f.polls, req.Msg)
	if f.pollErr != nil {
		return nil, f.pollErr
	}
	out := filJobOut(vmdv1.JobState_JOB_STATE_RUNNING, nil, "", "")
	if f.pollFn != nil {
		out = f.pollFn(len(f.polls))
	}
	if out.GetJob().GetState() == vmdv1.JobState_JOB_STATE_RUNNING {
		time.Sleep(10 * time.Millisecond) // like a real poll, do not spin
	}
	return connect.NewResponse(out), nil
}

func filJobOut(st vmdv1.JobState, exit *int32, stdout, stderr string) *vmdv1.JobOutput {
	return &vmdv1.JobOutput{
		Job:    &vmdv1.Job{Id: "job-1", VmId: "vm-1", State: st, ExitCode: exit},
		Stdout: []byte(stdout),
		Stderr: []byte(stderr),
	}
}

func filInt32(v int32) *int32 { return &v }

func filIntPtr(v int) *int { return &v }

// filDefaultVMs has one machine, "box", with ID vm-1.
func filDefaultVMs() *filVMs {
	return &filVMs{vms: []*vmdv1.VM{{Id: "vm-1", Name: "box", ProjectId: "p1"}}}
}

// filFast caps waits at 300 ms so polling tests finish quickly.
func filFast(c *Config) { c.MaxWait = 300 * time.Millisecond }

// filCalls counts every downstream call made by the fakes.
func filCalls(v *filVMs, e *filExec) int {
	return v.calls + len(e.puts) + len(e.gets) + len(e.starts) + len(e.polls)
}

// filCall runs a tool through the MCP layer and returns text and isError, with the fakes already wired in.
func filCall(t *testing.T, v *filVMs, e *filExec, tool string, args map[string]any, mutate ...func(*Config)) (string, bool) {
	t.Helper()
	cs := newTestSession(t, Clients{VMs: v, Exec: e}, mutate...)
	return callTool(t, cs, tool, args)
}

func filWantErr(t *testing.T, text string, isErr bool, want string) {
	t.Helper()
	if !isErr {
		t.Fatalf("expected a tool error containing %q, got success: %q", want, text)
	}
	if !strings.Contains(text, want) {
		t.Fatalf("error %q does not contain %q", text, want)
	}
}

func TestFilShellQuote(t *testing.T) {
	cases := map[string]string{
		"":                    `''`,
		"plain":               `'plain'`,
		"it's":                `'it'\''s'`,
		"a b":                 `'a b'`,
		"$(id) `whoami`":      "'$(id) `whoami`'",
		"line1\nline2":        "'line1\nline2'",
		"'":                   `''\'''`,
		`back\slash`:          `'back\slash'`,
		`$HOME "q"; rm -rf /`: `'$HOME "q"; rm -rf /'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFilShellQuoteRoundTrip runs the quoted word through a real POSIX shell and checks it comes back unchanged.
func TestFilShellQuoteRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this machine")
	}
	inputs := []string{
		"", "it's", `a'b"c$(echo x)` + "`echo y`" + "\n\\ end", "''''", "$HOME ${HOME} *", "tab\tand space ",
	}
	for _, in := range inputs {
		out, err := exec.Command("sh", "-c", "printf '%s' "+shellQuote(in)).Output()
		if err != nil {
			t.Fatalf("sh failed for %q: %v", in, err)
		}
		if !bytes.Equal(out, []byte(in)) {
			t.Errorf("round trip of %q gave %q", in, out)
		}
	}
}

func TestFilFetchCommandExact(t *testing.T) {
	got := filFetchCommand("/home/agent/it's here/f.tgz", "https://example.com/f?x=$(id)&y=`whoami`'q")
	want := "curl -fsSL --create-dirs -o '/home/agent/it'\\''s here/f.tgz' " +
		"'https://example.com/f?x=$(id)&y=`whoami`'\\''q' && ls -l '/home/agent/it'\\''s here/f.tgz'"
	if got != want {
		t.Fatalf("command\n got: %s\nwant: %s", got, want)
	}
}

func TestFilFetchSendsExactCommand(t *testing.T) {
	v, e := filDefaultVMs(), &filExec{startOut: filJobOut(vmdv1.JobState_JOB_STATE_SUCCEEDED, filInt32(0),
		"-rw-r--r-- 1 agent agent 12 Oct 18 14:00 /home/agent/a.tgz\n", "")}
	text, isErr := filCall(t, v, e, "agent_fetch", map[string]any{
		"name": "box", "url": "https://example.com/a.tgz", "path": "/home/agent/a.tgz"}, filFast)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if want := "ok: -rw-r--r-- 1 agent agent 12 Oct 18 14:00 /home/agent/a.tgz"; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	if len(e.starts) != 1 {
		t.Fatalf("StartJob calls = %d, want 1", len(e.starts))
	}
	st := e.starts[0]
	wantCmd := "curl -fsSL --create-dirs -o '/home/agent/a.tgz' 'https://example.com/a.tgz' && ls -l '/home/agent/a.tgz'"
	if st.Command != wantCmd {
		t.Errorf("sent command\n got: %s\nwant: %s", st.Command, wantCmd)
	}
	if st.VmId != "vm-1" || st.TimeoutSeconds != 3600 {
		t.Errorf("VmId=%q TimeoutSeconds=%d, want vm-1 and 3600", st.VmId, st.TimeoutSeconds)
	}
	if len(e.polls) != 0 {
		t.Errorf("finished job should not be polled, got %d polls", len(e.polls))
	}
}

func TestFilFetchTrickyCommandReachesVM(t *testing.T) {
	v, e := filDefaultVMs(), &filExec{startOut: filJobOut(vmdv1.JobState_JOB_STATE_SUCCEEDED, filInt32(0), "ls\n", "")}
	url := "https://example.com/f?x=$(id)&y=`whoami`'q"
	path := "/home/agent/it's here/$(rm -rf x)/f.tgz"
	if _, isErr := filCall(t, v, e, "agent_fetch", map[string]any{"name": "box", "url": url, "path": path}, filFast); isErr {
		t.Fatal("unexpected tool error")
	}
	want := "curl -fsSL --create-dirs -o " + shellQuote(path) + " " + shellQuote(url) + " && ls -l " + shellQuote(path)
	if e.starts[0].Command != want {
		t.Fatalf("command\n got: %s\nwant: %s", e.starts[0].Command, want)
	}
}

func TestFilFetchPollsUntilDone(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{
		startOut: filJobOut(vmdv1.JobState_JOB_STATE_RUNNING, nil, "", ""),
		pollFn: func(n int) *vmdv1.JobOutput {
			if n < 2 {
				return filJobOut(vmdv1.JobState_JOB_STATE_RUNNING, nil, "", "")
			}
			return filJobOut(vmdv1.JobState_JOB_STATE_SUCCEEDED, filInt32(0), "-rw 1 a a 9 x /p\n", "")
		},
	}
	text, isErr := filCall(t, v, e, "agent_fetch", map[string]any{"name": "box", "url": "https://e.test/x", "path": "/p", "wait_seconds": 2}, filFast)
	if isErr || text != "ok: -rw 1 a a 9 x /p" {
		t.Fatalf("text=%q isErr=%v", text, isErr)
	}
	if len(e.polls) != 2 {
		t.Fatalf("polls = %d, want 2", len(e.polls))
	}
	if e.polls[0].JobId != "job-1" || !e.polls[0].Tail {
		t.Errorf("poll request %+v: want job-1 in tail mode", e.polls[0])
	}
}

func TestFilFetchStillRunning(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{}
	start := time.Now()
	text, isErr := filCall(t, v, e, "agent_fetch", map[string]any{"name": "box", "url": "https://e.test/x", "path": "/p"}, filFast)
	elapsed := time.Since(start)
	if isErr {
		t.Fatalf("still running is not an error: %s", text)
	}
	if want := "download still running as job job-1: poll it with agent_exec_poll"; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	if elapsed > 2*time.Second {
		t.Errorf("call took %v; budget was 300 ms", elapsed)
	}
	if e.starts[0].WaitSeconds != 0 {
		t.Errorf("StartJob WaitSeconds = %d, want 0 under a 300 ms MaxWait", e.starts[0].WaitSeconds)
	}
}

// TestFilFetchWaitBudget checks the total budget is min(wait_seconds, MaxWait) and that every downstream wait
// is capped by the server's maximum (here 1 s because MaxWait is 1.1 s).
func TestFilFetchWaitBudget(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{}
	start := time.Now()
	text, isErr := filCall(t, v, e, "agent_fetch", map[string]any{"name": "box", "url": "https://e.test/x", "path": "/p"},
		func(c *Config) { c.MaxWait = 1100 * time.Millisecond })
	elapsed := time.Since(start)
	if isErr || !strings.Contains(text, "still running") {
		t.Fatalf("text=%q isErr=%v", text, isErr)
	}
	if elapsed < 1000*time.Millisecond || elapsed > 2500*time.Millisecond {
		t.Errorf("elapsed %v, want about the 1.1 s budget (default wait 45 s capped by MaxWait)", elapsed)
	}
	if e.starts[0].WaitSeconds != 1 {
		t.Errorf("StartJob WaitSeconds = %d, want 1", e.starts[0].WaitSeconds)
	}
	if len(e.polls) < 2 {
		t.Fatalf("polls = %d, want several while the budget lasts", len(e.polls))
	}
	for i, p := range e.polls {
		if p.WaitSeconds < 0 || p.WaitSeconds > 1 {
			t.Errorf("poll %d WaitSeconds = %d, want 0..1", i, p.WaitSeconds)
		}
	}
}

func TestFilFetchWaitZeroReturnsAtOnce(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{}
	text, isErr := filCall(t, v, e, "agent_fetch", map[string]any{"name": "box", "url": "https://e.test/x", "path": "/p", "wait_seconds": 0}, filFast)
	if isErr || !strings.HasPrefix(text, "download still running as job job-1") {
		t.Fatalf("text=%q isErr=%v", text, isErr)
	}
	if len(e.polls) != 0 {
		t.Errorf("wait_seconds=0 should not poll, got %d polls", len(e.polls))
	}
}

func TestFilFetchFailedExitReportsStderrTail(t *testing.T) {
	v := filDefaultVMs()
	stderr := strings.Repeat("x", 2000) + "END"
	e := &filExec{startOut: filJobOut(vmdv1.JobState_JOB_STATE_FAILED, filInt32(2), "", stderr)}
	text, isErr := filCall(t, v, e, "agent_fetch", map[string]any{"name": "box", "url": "https://e.test/x", "path": "/p"}, filFast)
	filWantErr(t, text, isErr, "download failed with exit code 2: ")
	tail := text[strings.Index(text, ": ")+2:]
	if len(tail) != filStderrTail || !strings.HasSuffix(tail, "END") {
		t.Errorf("stderr tail has %d bytes, want the last %d", len(tail), filStderrTail)
	}
}

func TestFilFetchEndedWithoutExitCode(t *testing.T) {
	v := filDefaultVMs()
	out := filJobOut(vmdv1.JobState_JOB_STATE_LOST, nil, "", "")
	out.Job.KilledReason = "vm stopped"
	e := &filExec{startOut: out}
	text, isErr := filCall(t, v, e, "agent_fetch", map[string]any{"name": "box", "url": "https://e.test/x", "path": "/p"}, filFast)
	filWantErr(t, text, isErr, "download failed: job job-1 ended as lost (vm stopped)")
}

func TestFilFetchPollError(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{pollErr: connect.NewError(connect.CodeNotFound, errors.New("job gone"))}
	text, isErr := filCall(t, v, e, "agent_fetch", map[string]any{"name": "box", "url": "https://e.test/x", "path": "/p"}, filFast)
	filWantErr(t, text, isErr, "not_found: job gone")
}

func TestFilFetchStartError(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{startErr: connect.NewError(connect.CodeFailedPrecondition, errors.New("vm is not running"))}
	text, isErr := filCall(t, v, e, "agent_fetch", map[string]any{"name": "box", "url": "https://e.test/x", "path": "/p"}, filFast)
	filWantErr(t, text, isErr, "failed_precondition: vm is not running")
}

func TestFilFetchNoJobReturned(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{startOut: &vmdv1.JobOutput{}}
	text, isErr := filCall(t, v, e, "agent_fetch", map[string]any{"name": "box", "url": "https://e.test/x", "path": "/p"}, filFast)
	filWantErr(t, text, isErr, "did not return a job")
}

func TestFilFetchValidation(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no name", map[string]any{"name": "", "url": "https://e.test/x", "path": "/p"}, "name is required"},
		{"ftp scheme", map[string]any{"name": "box", "url": "ftp://e.test/x", "path": "/p"}, "http:// or https://"},
		{"no scheme", map[string]any{"name": "box", "url": "e.test/x", "path": "/p"}, "http:// or https://"},
		{"no host", map[string]any{"name": "box", "url": "https:///x", "path": "/p"}, "http:// or https://"},
		{"empty path", map[string]any{"name": "box", "url": "https://e.test/x", "path": ""}, "path is required"},
		{"NUL in path", map[string]any{"name": "box", "url": "https://e.test/x", "path": "/a\x00b"}, "NUL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, e := filDefaultVMs(), &filExec{}
			text, isErr := filCall(t, v, e, "agent_fetch", tc.args, filFast)
			filWantErr(t, text, isErr, tc.want)
			if n := filCalls(v, e); n != 0 {
				t.Errorf("validation failure made %d downstream calls", n)
			}
		})
	}
}

func TestFilSSHPubkeyExisting(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{startOut: filJobOut(vmdv1.JobState_JOB_STATE_SUCCEEDED, filInt32(0), "ssh-ed25519 AAAAC3 box@home\n", "")}
	text, isErr := filCall(t, v, e, "agent_ssh_pubkey", map[string]any{"name": "box"}, filFast)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	want := "ssh-ed25519 AAAAC3 box@home\n(add this to GitHub or authorized_keys; the private key never leaves the machine)"
	if text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	wantCmd := "cat ~/.ssh/id_ed25519.pub 2>/dev/null || (mkdir -p ~/.ssh && ssh-keygen -q -t ed25519 -N '' -f ~/.ssh/id_ed25519 && cat ~/.ssh/id_ed25519.pub)"
	if e.starts[0].Command != wantCmd {
		t.Fatalf("sent command\n got: %s\nwant: %s", e.starts[0].Command, wantCmd)
	}
	if e.starts[0].VmId != "vm-1" {
		t.Errorf("VmId = %q", e.starts[0].VmId)
	}
}

func TestFilSSHPubkeyPollsUntilDone(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{pollFn: func(int) *vmdv1.JobOutput {
		return filJobOut(vmdv1.JobState_JOB_STATE_SUCCEEDED, filInt32(0), "ssh-ed25519 K\n", "")
	}}
	text, isErr := filCall(t, v, e, "agent_ssh_pubkey", map[string]any{"name": "box"}, filFast)
	if isErr || !strings.HasPrefix(text, "ssh-ed25519 K\n") {
		t.Fatalf("text=%q isErr=%v", text, isErr)
	}
	if len(e.polls) != 1 {
		t.Errorf("polls = %d, want 1", len(e.polls))
	}
}

func TestFilSSHPubkeyFailure(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{startOut: filJobOut(vmdv1.JobState_JOB_STATE_FAILED, filInt32(1), "", "ssh-keygen: boom\n")}
	text, isErr := filCall(t, v, e, "agent_ssh_pubkey", map[string]any{"name": "box"}, filFast)
	filWantErr(t, text, isErr, "ssh key setup failed with exit code 1: ssh-keygen: boom")
}

func TestFilSSHPubkeyEmptyOutput(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{startOut: filJobOut(vmdv1.JobState_JOB_STATE_SUCCEEDED, filInt32(0), "  \n", "")}
	text, isErr := filCall(t, v, e, "agent_ssh_pubkey", map[string]any{"name": "box"}, filFast)
	filWantErr(t, text, isErr, "printed no public key")
}

func TestFilSSHPubkeyStillRunning(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{}
	text, isErr := filCall(t, v, e, "agent_ssh_pubkey", map[string]any{"name": "box"}, filFast)
	if isErr || !strings.HasPrefix(text, "ssh key setup still running as job job-1: poll it with agent_exec_poll") {
		t.Fatalf("text=%q isErr=%v", text, isErr)
	}
}

func TestFilSSHPubkeyNoName(t *testing.T) {
	v, e := filDefaultVMs(), &filExec{}
	text, isErr := filCall(t, v, e, "agent_ssh_pubkey", map[string]any{"name": ""}, filFast)
	filWantErr(t, text, isErr, "name is required")
	if n := filCalls(v, e); n != 0 {
		t.Errorf("made %d downstream calls", n)
	}
}

func TestFilUploadText(t *testing.T) {
	v, e := filDefaultVMs(), &filExec{}
	text, isErr := filCall(t, v, e, "agent_upload", map[string]any{"name": "box", "path": "/home/agent/a.txt", "content": "hello"})
	if isErr || text != "wrote 5 bytes to /home/agent/a.txt" {
		t.Fatalf("text=%q isErr=%v", text, isErr)
	}
	p := e.puts[0]
	if p.VmId != "vm-1" || p.Path != "/home/agent/a.txt" || string(p.Content) != "hello" || p.Mode != 0o644 {
		t.Errorf("PutFile request %+v, want vm-1, path, hello, mode 0644", p)
	}
}

func TestFilUploadBase64WithMode(t *testing.T) {
	raw := []byte{0, 1, 2, 255, '\n'}
	enc := base64.StdEncoding.EncodeToString(raw)
	// Line-wrapped base64, as base64(1) prints it, must also be accepted.
	wrapped := enc[:4] + "\n" + enc[4:]
	v, e := filDefaultVMs(), &filExec{}
	text, isErr := filCall(t, v, e, "agent_upload", map[string]any{
		"name": "box", "path": "/opt/bin.dat", "content": wrapped, "base64": true, "mode": "0755"})
	if isErr || text != "wrote 5 bytes to /opt/bin.dat" {
		t.Fatalf("text=%q isErr=%v", text, isErr)
	}
	if !bytes.Equal(e.puts[0].Content, raw) || e.puts[0].Mode != 0o755 {
		t.Errorf("content %v mode %o, want %v 0755", e.puts[0].Content, e.puts[0].Mode, raw)
	}
}

func TestFilUploadValidation(t *testing.T) {
	big := strings.Repeat("a", filMaxFileBytes+1)
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"relative path", map[string]any{"name": "box", "path": "a.txt", "content": "x"}, "absolute path"},
		{"no name", map[string]any{"name": "", "path": "/a", "content": "x"}, "name is required"},
		{"NUL in path", map[string]any{"name": "box", "path": "/a\x00b", "content": "x"}, "NUL"},
		{"bad base64", map[string]any{"name": "box", "path": "/a", "content": "@@@", "base64": true}, "not valid base64"},
		{"mode not octal", map[string]any{"name": "box", "path": "/a", "content": "x", "mode": "rwx"}, "not an octal permission"},
		{"mode above 7777", map[string]any{"name": "box", "path": "/a", "content": "x", "mode": "10000"}, "not an octal permission"},
		{"mode 0000", map[string]any{"name": "box", "path": "/a", "content": "x", "mode": "0000"}, "mode 0000 is not allowed"},
		{"text over 8 MiB", map[string]any{"name": "box", "path": "/a", "content": big}, "limited to"},
		{"base64 over 8 MiB", map[string]any{"name": "box", "path": "/a", "base64": true,
			"content": base64.StdEncoding.EncodeToString(make([]byte, filMaxFileBytes+1))}, "limited to"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, e := filDefaultVMs(), &filExec{}
			text, isErr := filCall(t, v, e, "agent_upload", tc.args)
			filWantErr(t, text, isErr, tc.want)
			if n := filCalls(v, e); n != 0 {
				t.Errorf("validation failure made %d downstream calls", n)
			}
		})
	}
}

func TestFilUploadDownstreamErrors(t *testing.T) {
	t.Run("machine missing", func(t *testing.T) {
		v, e := &filVMs{}, &filExec{}
		text, isErr := filCall(t, v, e, "agent_upload", map[string]any{"name": "box", "path": "/a", "content": "x"})
		filWantErr(t, text, isErr, `not_found: no machine named "box"`)
	})
	t.Run("put refused", func(t *testing.T) {
		v := filDefaultVMs()
		e := &filExec{putErr: connect.NewError(connect.CodePermissionDenied, errors.New("nope"))}
		text, isErr := filCall(t, v, e, "agent_upload", map[string]any{"name": "box", "path": "/a", "content": "x"})
		filWantErr(t, text, isErr, "permission_denied: nope")
	})
}

// filDownloaded decodes the JSON reply of agent_download.
func filDownloaded(t *testing.T, text string) filDownloadResult {
	t.Helper()
	var r filDownloadResult
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatalf("reply is not JSON: %v\n%s", err, text)
	}
	return r
}

func TestFilDownloadText(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{getRes: &vmdv1.GetFileResponse{Content: []byte("héllo\n")}}
	text, isErr := filCall(t, v, e, "agent_download", map[string]any{"name": "box", "path": "/home/agent/a.txt"})
	if isErr {
		t.Fatalf("text=%q", text)
	}
	r := filDownloaded(t, text)
	sum := sha256.Sum256([]byte("héllo\n"))
	if r.Path != "/home/agent/a.txt" || r.Encoding != "utf-8" || r.Content != "héllo\n" || r.Size != 7 ||
		r.SHA256 != hex.EncodeToString(sum[:]) || r.Truncated {
		t.Fatalf("result = %+v", r)
	}
	g := e.gets[0]
	if g.VmId != "vm-1" || g.Path != "/home/agent/a.txt" || g.MaxBytes != 1<<20 {
		t.Errorf("GetFile request %+v, want default max 1 MiB", g)
	}
}

func TestFilDownloadMaxBytes(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{getRes: &vmdv1.GetFileResponse{Content: []byte("abc")}}
	if _, isErr := filCall(t, v, e, "agent_download", map[string]any{"name": "box", "path": "/a", "max_bytes": 4096}); isErr {
		t.Fatal("unexpected error")
	}
	if e.gets[0].MaxBytes != 4096 {
		t.Errorf("MaxBytes sent = %d, want 4096", e.gets[0].MaxBytes)
	}
}

func TestFilDownloadBinaryAutoIsBase64(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{getRes: &vmdv1.GetFileResponse{Content: []byte{0, 255}}}
	text, isErr := filCall(t, v, e, "agent_download", map[string]any{"name": "box", "path": "/a", "max_bytes": 2 << 20})
	if isErr {
		t.Fatalf("text=%q", text)
	}
	r := filDownloaded(t, text)
	if r.Encoding != "base64" || r.Content != "AP8=" || r.Size != 2 {
		t.Fatalf("result = %+v", r)
	}
	if e.gets[0].MaxBytes != 2<<20 {
		t.Errorf("MaxBytes sent = %d, want 2 MiB (auto may read up to 8 MiB)", e.gets[0].MaxBytes)
	}
}

func TestFilDownloadExplicitEncodings(t *testing.T) {
	v := filDefaultVMs()
	// Valid UTF-8, asked for as base64: the bytes come back base64 encoded.
	e := &filExec{getRes: &vmdv1.GetFileResponse{Content: []byte("abc")}}
	text, _ := filCall(t, v, e, "agent_download", map[string]any{"name": "box", "path": "/a", "encoding": "base64"})
	if r := filDownloaded(t, text); r.Encoding != "base64" || r.Content != "YWJj" {
		t.Fatalf("base64: %+v", r)
	}
	// The deprecated flag means the same as encoding=base64.
	text, _ = filCall(t, v, e, "agent_download", map[string]any{"name": "box", "path": "/a", "base64": true})
	if r := filDownloaded(t, text); r.Encoding != "base64" {
		t.Fatalf("base64 flag: %+v", r)
	}
	// An unknown encoding is refused before any downstream call.
	n := filCalls(v, e)
	text, isErr := filCall(t, v, e, "agent_download", map[string]any{"name": "box", "path": "/a", "encoding": "latin1"})
	filWantErr(t, text, isErr, "encoding must be auto, utf-8 or base64")
	if filCalls(v, e) != n {
		t.Error("an invalid encoding made a downstream call")
	}
}

func TestFilDownloadNotUTF8(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{getRes: &vmdv1.GetFileResponse{Content: []byte{0xff, 'a'}}}
	text, isErr := filCall(t, v, e, "agent_download", map[string]any{"name": "box", "path": "/a", "encoding": "utf-8"})
	filWantErr(t, text, isErr, "encoding=base64")
}

func TestFilDownloadTruncatedText(t *testing.T) {
	v := filDefaultVMs()
	// The cut fell inside a two-byte character: the incomplete first byte must not make the file look binary.
	e := &filExec{getRes: &vmdv1.GetFileResponse{Content: []byte("abc\xc3"), Truncated: true}}
	text, isErr := filCall(t, v, e, "agent_download", map[string]any{"name": "box", "path": "/a", "max_bytes": 4})
	if isErr {
		t.Fatalf("text=%q", text)
	}
	if r := filDownloaded(t, text); r.Content != "abc" || !r.Truncated || r.Encoding != "utf-8" || r.Size != 3 {
		t.Fatalf("result = %+v", r)
	}
}

func TestFilDownloadTruncatedBinary(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{getRes: &vmdv1.GetFileResponse{Content: []byte{0xff, 1}, Truncated: true}}
	text, isErr := filCall(t, v, e, "agent_download", map[string]any{"name": "box", "path": "/a"})
	if isErr {
		t.Fatalf("text=%q", text)
	}
	if r := filDownloaded(t, text); r.Content != "/wE=" || !r.Truncated || r.Encoding != "base64" {
		t.Fatalf("result = %+v", r)
	}
}

// TestFilDownloadTarGzRoundTrip reads a real 200 KB tar.gz, as a binary file, and checks that the bytes, the size
// and the sha256 all survive the trip through the reply.
func TestFilDownloadTarGzRoundTrip(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	payload := make([]byte, 200<<10)
	rand.New(rand.NewSource(1)).Read(payload)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tw.WriteHeader(&tar.Header{Name: "data.bin", Mode: 0o644, Size: int64(len(payload))}))
	_, err := tw.Write(payload)
	must(err)
	must(tw.Close())
	must(gz.Close())
	raw := archive.Bytes()

	v := filDefaultVMs()
	e := &filExec{getRes: &vmdv1.GetFileResponse{Content: raw}}
	text, isErr := filCall(t, v, e, "agent_download", map[string]any{"name": "box", "path": "/home/agent/x.tar.gz"})
	if isErr {
		t.Fatalf("text=%q", text)
	}
	r := filDownloaded(t, text)
	got, err := base64.StdEncoding.DecodeString(r.Content)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if r.Encoding != "base64" || !bytes.Equal(got, raw) || r.Size != len(raw) || r.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("round trip: encoding=%s size=%d sha=%s, bytes equal=%v", r.Encoding, r.Size, r.SHA256, bytes.Equal(got, raw))
	}
}

func TestFilDownloadValidation(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no name", map[string]any{"name": "", "path": "/a"}, "name is required"},
		{"no path", map[string]any{"name": "box", "path": ""}, "path is required"},
		{"NUL in path", map[string]any{"name": "box", "path": "/a\x00"}, "NUL"},
		{"zero max", map[string]any{"name": "box", "path": "/a", "max_bytes": 0}, "max_bytes must be between 1"},
		{"base64 above 8 MiB", map[string]any{"name": "box", "path": "/a", "encoding": "base64", "max_bytes": 9 << 20}, "max_bytes must be between 1 and 8388608"},
		{"utf-8 above 1 MiB", map[string]any{"name": "box", "path": "/a", "encoding": "utf-8", "max_bytes": 2 << 20}, "max_bytes must be between 1 and 1048576"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, e := filDefaultVMs(), &filExec{}
			text, isErr := filCall(t, v, e, "agent_download", tc.args)
			filWantErr(t, text, isErr, tc.want)
			if n := filCalls(v, e); n != 0 {
				t.Errorf("validation failure made %d downstream calls", n)
			}
		})
	}
}

func TestFilDownloadDownstreamError(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{getErr: connect.NewError(connect.CodeNotFound, errors.New("no such file"))}
	text, isErr := filCall(t, v, e, "agent_download", map[string]any{"name": "box", "path": "/a"})
	filWantErr(t, text, isErr, "not_found: no such file")
}

func TestFilTrimPartialUTF8(t *testing.T) {
	cases := map[string]string{
		"abc":            "abc",
		"abc\xe2\x82":    "abc",            // incomplete three-byte sequence
		"ab\xe2\x82\xac": "ab\xe2\x82\xac", // complete euro sign stays
		"":               "",
		"x\xf0\x9f\x98":  "x", // incomplete four-byte sequence
	}
	for in, want := range cases {
		if got := string(filTrimPartialUTF8([]byte(in))); got != want {
			t.Errorf("filTrimPartialUTF8(%q) = %q, want %q", in, got, want)
		}
	}
	if !utf8.Valid(filTrimPartialUTF8([]byte("abc\xc3"))) {
		t.Error("trimmed output should be valid UTF-8")
	}
}

func TestFilParseMode(t *testing.T) {
	if m, err := filParseMode(""); err != nil || m != 0o644 {
		t.Errorf("default mode = %o, %v", m, err)
	}
	if m, err := filParseMode("0755"); err != nil || m != 0o755 {
		t.Errorf("0755 = %o, %v", m, err)
	}
	if m, err := filParseMode("4755"); err != nil || m != 0o4755 {
		t.Errorf("4755 = %o, %v", m, err)
	}
	for _, bad := range []string{"0o755", "8", "10000", "0000", "-1"} {
		if _, err := filParseMode(bad); err == nil {
			t.Errorf("filParseMode(%q) accepted", bad)
		}
	}
}

func TestFilDecodeContent(t *testing.T) {
	got, err := filDecodeContent("aGk=\r\n", true)
	if err != nil || string(got) != "hi" {
		t.Errorf("wrapped base64: %q, %v", got, err)
	}
	got, err = filDecodeContent("not base64!", false)
	if err != nil || string(got) != "not base64!" {
		t.Errorf("plain text should be kept as is: %q, %v", got, err)
	}
	if _, err := filDecodeContent("%%%", true); err == nil {
		t.Error("invalid base64 accepted")
	}
}

func TestFilMissingMachine(t *testing.T) {
	tools := map[string]map[string]any{
		"agent_upload":     {"name": "box", "path": "/a", "content": "x"},
		"agent_download":   {"name": "box", "path": "/a"},
		"agent_fetch":      {"name": "box", "url": "https://e.test/x", "path": "/p"},
		"agent_ssh_pubkey": {"name": "box"},
	}
	for tool, args := range tools {
		t.Run(tool, func(t *testing.T) {
			v, e := &filVMs{}, &filExec{}
			text, isErr := filCall(t, v, e, tool, args, filFast)
			filWantErr(t, text, isErr, `not_found: no machine named "box" in project "p1"`)
			if n := filCalls(v, e); n != 1 { // only the ListVMs lookup
				t.Errorf("made %d downstream calls, want only the lookup", n)
			}
		})
	}
}

func TestFilSSHPubkeyStartError(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{startErr: connect.NewError(connect.CodeUnavailable, errors.New("node down"))}
	text, isErr := filCall(t, v, e, "agent_ssh_pubkey", map[string]any{"name": "box"}, filFast)
	filWantErr(t, text, isErr, "unavailable: node down")
}

func TestFilSSHPubkeyEndedWithoutExitCode(t *testing.T) {
	v := filDefaultVMs()
	e := &filExec{startOut: filJobOut(vmdv1.JobState_JOB_STATE_TIMED_OUT, nil, "", "")}
	text, isErr := filCall(t, v, e, "agent_ssh_pubkey", map[string]any{"name": "box"}, filFast)
	filWantErr(t, text, isErr, "ssh key setup failed: job job-1 ended as timed_out (no exit code)")
}

func TestFilFetchNegativeWaitIsZero(t *testing.T) {
	v, e := filDefaultVMs(), &filExec{}
	text, isErr := filCall(t, v, e, "agent_fetch", map[string]any{"name": "box", "url": "https://e.test/x", "path": "/p", "wait_seconds": -5}, filFast)
	if isErr || !strings.HasPrefix(text, "download still running") {
		t.Fatalf("text=%q isErr=%v", text, isErr)
	}
	if e.starts[0].WaitSeconds != 0 || len(e.polls) != 0 {
		t.Errorf("negative wait: StartJob WaitSeconds=%d polls=%d, want 0 and 0", e.starts[0].WaitSeconds, len(e.polls))
	}
}
