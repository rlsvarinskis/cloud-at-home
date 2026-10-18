package service_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1/vmdv1connect"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
)

// exectHook is the signature of fake.Driver.Exec.
type exectHook = func(vmID string, spec *nodev1.JobSpec) (int, string, string, time.Duration)

type exectResult struct {
	exit           int
	stdout, stderr string
	runFor         time.Duration
}

// exectScript answers by exact command text. Unknown commands exit 127 with a message on stderr.
func exectScript(results map[string]exectResult) exectHook {
	return func(_ string, spec *nodev1.JobSpec) (int, string, string, time.Duration) {
		r, ok := results[spec.GetCommand()]
		if !ok {
			return 127, "", spec.GetCommand() + ": not found\n", 0
		}
		return r.exit, r.stdout, r.stderr, r.runFor
	}
}

// exectFixed makes every job exit with the same code and output after runFor.
func exectFixed(exit int, stdout string, runFor time.Duration) exectHook {
	return func(string, *nodev1.JobSpec) (int, string, string, time.Duration) {
		return exit, stdout, "", runFor
	}
}

func exectStart(c *clients, req *vmdv1.StartJobRequest) (*vmdv1.JobOutput, error) {
	res, err := c.Exec.StartJob(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func exectMustStart(t *testing.T, c *clients, req *vmdv1.StartJobRequest) *vmdv1.JobOutput {
	t.Helper()
	out, err := exectStart(c, req)
	if err != nil {
		t.Fatalf("StartJob(%q): %v", req.Command, err)
	}
	return out
}

func exectPoll(t *testing.T, c *clients, req *vmdv1.PollJobRequest) *vmdv1.JobOutput {
	t.Helper()
	res, err := c.Exec.PollJob(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("PollJob(%s): %v", req.JobId, err)
	}
	return res.Msg
}

// exectJob reads a job's current record without waiting.
func exectJob(t *testing.T, c *clients, jobID string) *vmdv1.Job {
	t.Helper()
	return exectPoll(t, c, &vmdv1.PollJobRequest{JobId: jobID}).Job
}

// exectList lists every job of a VM by following page tokens (page size 2).
func exectList(t *testing.T, c *clients, vmID string) []*vmdv1.Job {
	t.Helper()
	var all []*vmdv1.Job
	token := ""
	for range 100 {
		res, err := c.Exec.ListJobs(context.Background(), connect.NewRequest(&vmdv1.ListJobsRequest{VmId: vmID, PageSize: 2, PageToken: token}))
		if err != nil {
			t.Fatalf("ListJobs: %v", err)
		}
		all = append(all, res.Msg.Jobs...)
		if res.Msg.NextPageToken == "" {
			return all
		}
		token = res.Msg.NextPageToken
	}
	t.Fatal("ListJobs did not terminate")
	return nil
}

func exectWantCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %v error, got nil", want)
	}
	if got := code(err); got != want {
		t.Fatalf("want code %v, got %v: %v", want, got, err)
	}
}

func exectCount(calls []string, method string) int {
	n := 0
	for _, c := range calls {
		if c == method {
			n++
		}
	}
	return n
}

func exectFinal(t *testing.T, j *vmdv1.Job, state vmdv1.JobState) {
	t.Helper()
	if j.State != state {
		t.Fatalf("job %s: state %v, want %v (killed_reason %q)", j.Id, j.State, state, j.KilledReason)
	}
}

// --- happy path ---------------------------------------------------------------------------------

func TestExecStartJobSucceededAndFailed(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("happy")
	e.node.Exec = exectScript(map[string]exectResult{
		"echo hi":  {exit: 0, stdout: "hi\n"},
		"make all": {exit: 2, stdout: "compiling\n", stderr: "error: no rule\n"},
	})
	ed := e.editor()

	ok := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "echo hi", WaitSeconds: 5})
	exectFinal(t, ok.Job, vmdv1.JobState_JOB_STATE_SUCCEEDED)
	if ok.Job.ExitCode == nil || *ok.Job.ExitCode != 0 {
		t.Fatalf("exit code: %v, want 0", ok.Job.ExitCode)
	}
	if string(ok.Stdout) != "hi\n" || len(ok.Stderr) != 0 {
		t.Fatalf("output stdout=%q stderr=%q", ok.Stdout, ok.Stderr)
	}
	if ok.Job.VmId != vm.Id || ok.Job.ProjectId != "p1" || ok.Job.Command != "echo hi" {
		t.Fatalf("job metadata: %+v", ok.Job)
	}
	if ok.Job.StartedAt == nil || ok.Job.FinishedAt == nil {
		t.Fatalf("timestamps missing: started=%v finished=%v", ok.Job.StartedAt, ok.Job.FinishedAt)
	}
	if ok.Job.FinishedAt.AsTime().Before(ok.Job.StartedAt.AsTime()) {
		t.Fatal("finished before started")
	}
	if ok.Job.KilledReason != "" {
		t.Fatalf("killed_reason on success: %q", ok.Job.KilledReason)
	}

	bad := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "make all", WaitSeconds: 5})
	exectFinal(t, bad.Job, vmdv1.JobState_JOB_STATE_FAILED)
	if bad.Job.ExitCode == nil || *bad.Job.ExitCode != 2 {
		t.Fatalf("exit code: %v, want 2", bad.Job.ExitCode)
	}
	if string(bad.Stdout) != "compiling\n" || string(bad.Stderr) != "error: no rule\n" {
		t.Fatalf("output stdout=%q stderr=%q", bad.Stdout, bad.Stderr)
	}
}

func TestExecLongJobRunsThenSucceeds(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("long")
	e.node.Exec = exectFixed(0, "done\n", 30*time.Second)
	ed := e.editor()

	started := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "sleep 30"})
	if started.Job.State != vmdv1.JobState_JOB_STATE_RUNNING || started.Job.ExitCode != nil || started.Job.FinishedAt != nil {
		t.Fatalf("long job should be RUNNING without exit: %+v", started.Job)
	}
	id := started.Job.Id

	// A waiting poll on a running job returns RUNNING once its wait expires.
	p := exectPoll(t, ed, &vmdv1.PollJobRequest{JobId: id, WaitSeconds: 1})
	if p.Job.State != vmdv1.JobState_JOB_STATE_RUNNING {
		t.Fatalf("poll with wait: state %v, want RUNNING", p.Job.State)
	}
	e.clk.Advance(10 * time.Second)
	if p := exectPoll(t, ed, &vmdv1.PollJobRequest{JobId: id}); p.Job.State != vmdv1.JobState_JOB_STATE_RUNNING || len(p.Stdout) != 0 {
		t.Fatalf("still running at 10s: state %v stdout %q", p.Job.State, p.Stdout)
	}

	e.clk.Advance(21 * time.Second)
	done := exectPoll(t, ed, &vmdv1.PollJobRequest{JobId: id})
	exectFinal(t, done.Job, vmdv1.JobState_JOB_STATE_SUCCEEDED)
	if done.Job.ExitCode == nil || *done.Job.ExitCode != 0 || string(done.Stdout) != "done\n" {
		t.Fatalf("finished job: exit %v stdout %q", done.Job.ExitCode, done.Stdout)
	}
	if d := done.Job.FinishedAt.AsTime().Sub(done.Job.StartedAt.AsTime()); d != 31*time.Second {
		t.Fatalf("runtime %v, want 31s", d)
	}

	listed := exectList(t, ed, vm.Id)
	if len(listed) != 1 || listed[0].State != vmdv1.JobState_JOB_STATE_SUCCEEDED || listed[0].ExitCode == nil {
		t.Fatalf("ListJobs should show the updated record: %+v", listed)
	}
}

// --- timeouts -----------------------------------------------------------------------------------

func TestExecTimeoutKillsJob(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("timeout")
	e.node.Exec = exectFixed(0, "", 10*time.Minute)
	ed := e.editor()

	out := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "sleep 600", TimeoutSeconds: 60})
	id := out.Job.Id
	if out.Job.State != vmdv1.JobState_JOB_STATE_RUNNING || out.Job.TimeoutSeconds != 60 {
		t.Fatalf("StartJob: %+v", out.Job)
	}
	e.clk.Advance(59 * time.Second)
	if j := exectJob(t, ed, id); j.State != vmdv1.JobState_JOB_STATE_RUNNING {
		t.Fatalf("at 59s of 60s: state %v", j.State)
	}
	e.clk.Advance(2 * time.Second)
	j := exectJob(t, ed, id)
	exectFinal(t, j, vmdv1.JobState_JOB_STATE_TIMED_OUT)
	if j.ExitCode == nil || *j.ExitCode != 124 {
		t.Fatalf("timed out exit code %v, want 124", j.ExitCode)
	}
	if !strings.Contains(j.KilledReason, "timed out") {
		t.Fatalf("killed_reason %q should mention the timeout", j.KilledReason)
	}
}

func TestExecDefaultTimeoutIsOneHour(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("deftimeout")
	e.node.Exec = exectFixed(0, "", 2*time.Hour)
	ed := e.editor()

	out := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "long"})
	if out.Job.TimeoutSeconds != 3600 {
		t.Fatalf("default timeout %d, want 3600", out.Job.TimeoutSeconds)
	}
	e.clk.Advance(3599 * time.Second)
	if j := exectJob(t, ed, out.Job.Id); j.State != vmdv1.JobState_JOB_STATE_RUNNING {
		t.Fatalf("at 3599s: state %v", j.State)
	}
	e.clk.Advance(time.Second)
	exectFinal(t, exectJob(t, ed, out.Job.Id), vmdv1.JobState_JOB_STATE_TIMED_OUT)
}

func TestExecTimeoutLimits(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("limits")
	ed := e.editor()

	_, err := exectStart(ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "true", TimeoutSeconds: 7*24*3600 + 1})
	exectWantCode(t, err, connect.CodeInvalidArgument)

	// The maximum itself is accepted; the default exec finishes at once, so it succeeds.
	out := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "true", TimeoutSeconds: 7 * 24 * 3600})
	exectFinal(t, out.Job, vmdv1.JobState_JOB_STATE_SUCCEEDED)
	if out.Job.TimeoutSeconds != 7*24*3600 {
		t.Fatalf("timeout %d", out.Job.TimeoutSeconds)
	}
}

// --- cancel -------------------------------------------------------------------------------------

func TestExecCancelRunningJob(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("cancel")
	e.node.Exec = exectFixed(0, "never\n", time.Hour)
	ed := e.editor()

	run := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "sleep 3600"})
	res, err := ed.Exec.CancelJob(context.Background(), connect.NewRequest(&vmdv1.CancelJobRequest{JobId: run.Job.Id}))
	if err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	exectFinal(t, res.Msg.Job, vmdv1.JobState_JOB_STATE_CANCELLED)
	if res.Msg.Job.ExitCode == nil || *res.Msg.Job.ExitCode != 130 {
		t.Fatalf("cancelled exit code %v, want 130", res.Msg.Job.ExitCode)
	}
	if res.Msg.Job.KilledReason == "" {
		t.Fatal("cancelled job should carry a killed_reason")
	}

	// Time passing must not revive a cancelled job. The token is minted for an hour, so re-mint after the jump.
	e.clk.Advance(2 * time.Hour)
	exectFinal(t, exectJob(t, e.editor(), run.Job.Id), vmdv1.JobState_JOB_STATE_CANCELLED)
}

func TestExecCancelFinishedJobIsNoop(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("cancelnoop")
	e.node.Exec = exectScript(map[string]exectResult{"fail": {exit: 3, stdout: "x"}})
	ed := e.editor()

	done := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "fail", WaitSeconds: 5})
	res, err := ed.Exec.CancelJob(context.Background(), connect.NewRequest(&vmdv1.CancelJobRequest{JobId: done.Job.Id}))
	if err != nil {
		t.Fatalf("CancelJob on finished job: %v", err)
	}
	exectFinal(t, res.Msg.Job, vmdv1.JobState_JOB_STATE_FAILED)
	if res.Msg.Job.ExitCode == nil || *res.Msg.Job.ExitCode != 3 || res.Msg.Job.KilledReason != "" {
		t.Fatalf("finished job changed by cancel: %+v", res.Msg.Job)
	}
}

func TestExecCancelUnknownJobIsNotFound(t *testing.T) {
	e := newEnv(t)
	_, err := e.editor().Exec.CancelJob(context.Background(), connect.NewRequest(&vmdv1.CancelJobRequest{JobId: "job_missing"}))
	exectWantCode(t, err, connect.CodeNotFound)
}

// --- output paging ------------------------------------------------------------------------------

// exectBig returns n bytes that repeat the alphabet, so any misplaced slice shows up in comparisons.
func exectBig(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

func TestExecOutputTailMode(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("tail")
	big := exectBig(20000)
	e.node.Exec = exectFixed(0, string(big), 0)
	ed := e.editor()

	out := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "cat big", MaxOutput: 100, WaitSeconds: 5})
	if len(out.Stdout) != 100 || !bytes.Equal(out.Stdout, big[len(big)-100:]) {
		t.Fatalf("tail mode returned %d bytes, want the last 100", len(out.Stdout))
	}
	if !out.Truncated {
		t.Fatal("tail mode with omitted bytes should set truncated")
	}
	if out.StdoutOffset != int64(len(big)) {
		t.Fatalf("stdout offset %d, want %d", out.StdoutOffset, len(big))
	}

	// PollJob with tail=true does the same.
	p := exectPoll(t, ed, &vmdv1.PollJobRequest{JobId: out.Job.Id, MaxOutput: 50, Tail: true})
	if len(p.Stdout) != 50 || !bytes.Equal(p.Stdout, big[len(big)-50:]) || !p.Truncated {
		t.Fatalf("PollJob tail: %d bytes truncated=%v", len(p.Stdout), p.Truncated)
	}
}

func TestExecOutputDefaultAndCap(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("cap")
	big := exectBig(70000)
	e.node.Exec = exectFixed(0, string(big), 0)
	ed := e.editor()

	def := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "big", WaitSeconds: 5})
	if len(def.Stdout) != 8192 {
		t.Fatalf("default max_output returned %d bytes, want 8192", len(def.Stdout))
	}

	capped := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "big", MaxOutput: 1 << 20, WaitSeconds: 5})
	if len(capped.Stdout) != 65536 {
		t.Fatalf("max_output above the cap returned %d bytes, want 65536", len(capped.Stdout))
	}
}

func TestExecPollWalksWholeOutput(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("walk")
	big := exectBig(20000)
	errBig := exectBig(3000)
	e.node.Exec = func(string, *nodev1.JobSpec) (int, string, string, time.Duration) {
		return 0, string(big), string(errBig), 0
	}
	ed := e.editor()

	id := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "walk", WaitSeconds: 5}).Job.Id

	var gotOut, gotErr []byte
	var so, se int64
	pages := 0
	for {
		pages++
		if pages > 100 {
			t.Fatal("paging did not finish")
		}
		p := exectPoll(t, ed, &vmdv1.PollJobRequest{JobId: id, StdoutOffset: so, StderrOffset: se, MaxOutput: 999})
		if len(p.Stdout) > 999 || len(p.Stderr) > 999 {
			t.Fatalf("page exceeds max_output: %d/%d", len(p.Stdout), len(p.Stderr))
		}
		if p.Truncated {
			t.Fatal("offset mode must not report truncated")
		}
		gotOut = append(gotOut, p.Stdout...)
		gotErr = append(gotErr, p.Stderr...)
		so, se = p.StdoutOffset, p.StderrOffset
		if !p.More {
			break
		}
	}
	if !bytes.Equal(gotOut, big) {
		t.Fatalf("stdout reassembled to %d bytes, want %d", len(gotOut), len(big))
	}
	if !bytes.Equal(gotErr, errBig) {
		t.Fatalf("stderr reassembled to %d bytes, want %d", len(gotErr), len(errBig))
	}
	if so != int64(len(big)) || se != int64(len(errBig)) {
		t.Fatalf("final offsets %d/%d", so, se)
	}
	if pages != 21 {
		t.Logf("note: walk took %d pages (20000/999 needs 21)", pages)
	}

	// A negative offset is refused.
	_, err := ed.Exec.PollJob(context.Background(), connect.NewRequest(&vmdv1.PollJobRequest{JobId: id, StdoutOffset: -1}))
	exectWantCode(t, err, connect.CodeInvalidArgument)
}

// --- preconditions and validation ---------------------------------------------------------------

func TestExecStartPreconditions(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	img := e.sharedImage("base-pre")

	pending := e.createVM(ed, "p1", "pending", img.Id, true)
	if got := e.getVM(ed, pending.Id).State; got != vmdv1.VMState_VM_STATE_PENDING {
		t.Fatalf("precondition setup: state %v, want PENDING", got)
	}
	_, err := exectStart(ed, &vmdv1.StartJobRequest{VmId: pending.Id, Command: "true"})
	exectWantCode(t, err, connect.CodeFailedPrecondition)

	running := e.runningVM("pre-run")
	if _, err := ed.VM.StopVM(context.Background(), connect.NewRequest(&vmdv1.StopVMRequest{Id: running.Id})); err != nil {
		t.Fatal(err)
	}
	e.until(10, "stopped", func() bool { return e.getVM(ed, running.Id).State == vmdv1.VMState_VM_STATE_STOPPED })
	_, err = exectStart(ed, &vmdv1.StartJobRequest{VmId: running.Id, Command: "true"})
	exectWantCode(t, err, connect.CodeFailedPrecondition)

	_, err = exectStart(ed, &vmdv1.StartJobRequest{VmId: "vm_missing", Command: "true"})
	exectWantCode(t, err, connect.CodeNotFound)

	if n := len(exectList(t, ed, running.Id)); n != 0 {
		t.Fatalf("refused StartJob left %d job records", n)
	}
}

func TestExecStartValidation(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("valid")
	ed := e.editor()

	cases := []struct {
		name string
		req  *vmdv1.StartJobRequest
	}{
		{"empty command", &vmdv1.StartJobRequest{Command: ""}},
		{"whitespace command", &vmdv1.StartJobRequest{Command: " \t\n"}},
		{"NUL in command", &vmdv1.StartJobRequest{Command: "echo\x00hi"}},
		{"NUL in workdir", &vmdv1.StartJobRequest{Command: "true", Workdir: "/tmp\x00"}},
		{"command over 256 KiB", &vmdv1.StartJobRequest{Command: strings.Repeat("a", 256<<10+1)}},
		{"timeout over 7 days", &vmdv1.StartJobRequest{Command: "true", TimeoutSeconds: 7*24*3600 + 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.VmId = vm.Id
			_, err := exectStart(ed, tc.req)
			exectWantCode(t, err, connect.CodeInvalidArgument)
		})
	}

	// Exactly 256 KiB is accepted.
	ok := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "#" + strings.Repeat("a", 256<<10-1)})
	exectFinal(t, ok.Job, vmdv1.JobState_JOB_STATE_SUCCEEDED)
}

func TestExecFileValidation(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("filevalid")
	ed := e.editor()
	ctx := context.Background()

	tooBig := make([]byte, 8<<20+1)
	cases := []struct {
		name string
		req  *vmdv1.PutFileRequest
	}{
		{"empty path", &vmdv1.PutFileRequest{Path: "", Content: []byte("x")}},
		{"NUL in path", &vmdv1.PutFileRequest{Path: "a\x00b", Content: []byte("x")}},
		{"content over 8 MiB", &vmdv1.PutFileRequest{Path: "big", Content: tooBig}},
		{"mode above 07777", &vmdv1.PutFileRequest{Path: "m", Content: []byte("x"), Mode: 0o10000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.VmId = vm.Id
			_, err := ed.Exec.PutFile(ctx, connect.NewRequest(tc.req))
			exectWantCode(t, err, connect.CodeInvalidArgument)
		})
	}

	for _, p := range []string{"", "a\x00b"} {
		_, err := ed.Exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{VmId: vm.Id, Path: p}))
		exectWantCode(t, err, connect.CodeInvalidArgument)
	}
}

// --- authorization ------------------------------------------------------------------------------

func TestExecAuthorizationByRole(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("roles")
	e.node.Exec = exectFixed(0, "out\n", time.Hour)
	ed, vw := e.editor(), e.viewer()
	ctx := context.Background()

	run := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "sleep"})
	if _, err := ed.Exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{VmId: vm.Id, Path: "f", Content: []byte("x")})); err != nil {
		t.Fatal(err)
	}

	// Viewers may read jobs.
	if p := exectPoll(t, vw, &vmdv1.PollJobRequest{JobId: run.Job.Id}); p.Job.Id != run.Job.Id {
		t.Fatalf("viewer PollJob: %+v", p.Job)
	}
	if l, err := vw.Exec.ListJobs(ctx, connect.NewRequest(&vmdv1.ListJobsRequest{VmId: vm.Id})); err != nil || len(l.Msg.Jobs) != 1 {
		t.Fatalf("viewer ListJobs: %v", err)
	}

	// Viewers may not start, cancel or move files.
	_, err := exectStart(vw, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "true"})
	exectWantCode(t, err, connect.CodePermissionDenied)
	_, err = vw.Exec.CancelJob(ctx, connect.NewRequest(&vmdv1.CancelJobRequest{JobId: run.Job.Id}))
	exectWantCode(t, err, connect.CodePermissionDenied)
	_, err = vw.Exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{VmId: vm.Id, Path: "f", Content: []byte("y")}))
	exectWantCode(t, err, connect.CodePermissionDenied)
	_, err = vw.Exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{VmId: vm.Id, Path: "f"}))
	exectWantCode(t, err, connect.CodePermissionDenied)

	// A refused cancel leaves the job running.
	if j := exectJob(t, ed, run.Job.Id); j.State != vmdv1.JobState_JOB_STATE_RUNNING {
		t.Fatalf("job after refused cancel: %v", j.State)
	}
}

func TestExecTenantIsolation(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("tenant")
	e.node.Exec = exectFixed(0, "", time.Hour)
	ed, other := e.editor(), e.otherTenant()
	ctx := context.Background()

	run := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "sleep"})
	if _, err := ed.Exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{VmId: vm.Id, Path: "f", Content: []byte("x")})); err != nil {
		t.Fatal(err)
	}

	_, err := exectStart(other, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "true"})
	exectWantCode(t, err, connect.CodeNotFound)
	_, err = other.Exec.PollJob(ctx, connect.NewRequest(&vmdv1.PollJobRequest{JobId: run.Job.Id}))
	exectWantCode(t, err, connect.CodeNotFound)
	_, err = other.Exec.CancelJob(ctx, connect.NewRequest(&vmdv1.CancelJobRequest{JobId: run.Job.Id}))
	exectWantCode(t, err, connect.CodeNotFound)
	_, err = other.Exec.ListJobs(ctx, connect.NewRequest(&vmdv1.ListJobsRequest{VmId: vm.Id}))
	exectWantCode(t, err, connect.CodeNotFound)
	_, err = other.Exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{VmId: vm.Id, Path: "f", Content: []byte("z")}))
	exectWantCode(t, err, connect.CodeNotFound)
	_, err = other.Exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{VmId: vm.Id, Path: "f"}))
	exectWantCode(t, err, connect.CodeNotFound)

	// The job was not touched by the other tenant's attempts.
	if j := exectJob(t, ed, run.Job.Id); j.State != vmdv1.JobState_JOB_STATE_RUNNING {
		t.Fatalf("job state after foreign calls: %v", j.State)
	}
}

func TestExecServicePrincipal(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("agentd")
	e.node.Exec = exectFixed(0, "from agentd\n", 0)
	svc := e.service()
	ctx := context.Background()

	out := exectMustStart(t, svc, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "hello", WaitSeconds: 5})
	exectFinal(t, out.Job, vmdv1.JobState_JOB_STATE_SUCCEEDED)
	if p := exectPoll(t, svc, &vmdv1.PollJobRequest{JobId: out.Job.Id}); p.Job.State != vmdv1.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("service PollJob: %v", p.Job.State)
	}
	if l := exectList(t, svc, vm.Id); len(l) != 1 {
		t.Fatalf("service ListJobs: %d jobs", len(l))
	}

	e.node.Exec = exectFixed(0, "", time.Hour)
	run := exectMustStart(t, svc, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "sleep2"})
	if _, err := svc.Exec.CancelJob(ctx, connect.NewRequest(&vmdv1.CancelJobRequest{JobId: run.Job.Id})); err != nil {
		t.Fatalf("service CancelJob: %v", err)
	}

	if _, err := svc.Exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{VmId: vm.Id, Path: "/a", Content: []byte("svc")})); err != nil {
		t.Fatalf("service PutFile: %v", err)
	}
	got, err := svc.Exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{VmId: vm.Id, Path: "/a"}))
	if err != nil || string(got.Msg.Content) != "svc" {
		t.Fatalf("service GetFile: %v %q", err, got)
	}
}

func TestExecUnauthenticated(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("anon")
	ctx := context.Background()

	anon := vmdv1connect.NewExecServiceClient(e.srv.Client(), e.srv.URL)
	_, err := anon.StartJob(ctx, connect.NewRequest(&vmdv1.StartJobRequest{VmId: vm.Id, Command: "true"}))
	exectWantCode(t, err, connect.CodeUnauthenticated)
	_, err = anon.ListJobs(ctx, connect.NewRequest(&vmdv1.ListJobsRequest{VmId: vm.Id}))
	exectWantCode(t, err, connect.CodeUnauthenticated)
	_, err = anon.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{VmId: vm.Id, Path: "x"}))
	exectWantCode(t, err, connect.CodeUnauthenticated)

	bad := e.withToken("not-a-token")
	_, err = bad.Exec.PollJob(ctx, connect.NewRequest(&vmdv1.PollJobRequest{JobId: "job_x"}))
	exectWantCode(t, err, connect.CodeUnauthenticated)
}

// --- idempotency --------------------------------------------------------------------------------

func TestExecIdempotencyKeyRunsOnce(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("idem")
	e.node.Exec = exectFixed(0, "once\n", time.Hour)
	ed := e.editor()

	first := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "deploy", IdempotencyKey: "k-1"})
	second := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "deploy", IdempotencyKey: "k-1"})
	if first.Job.Id != second.Job.Id {
		t.Fatalf("same key returned jobs %s and %s", first.Job.Id, second.Job.Id)
	}
	if n := exectCount(e.node.Calls(), "StartJob"); n != 1 {
		t.Fatalf("command started %d times on the node, want 1", n)
	}
	if l := exectList(t, ed, vm.Id); len(l) != 1 {
		t.Fatalf("ListJobs: %d records, want 1", len(l))
	}

	// After the job finishes, a replay returns the final record without running again.
	e.clk.Advance(2 * time.Hour)
	ed = e.editor() // the token is minted for an hour, so re-mint after the jump
	third := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "deploy", IdempotencyKey: "k-1"})
	// The job ran for exactly its 1h timeout, which is not over the limit, so it succeeded.
	if third.Job.Id != first.Job.Id || third.Job.State != vmdv1.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("replay after finish: %+v", third.Job)
	}
	if n := exectCount(e.node.Calls(), "StartJob"); n != 1 {
		t.Fatalf("replay started the command again (%d StartJob calls)", n)
	}

	// A different key is a different job.
	other := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "deploy", IdempotencyKey: "k-2"})
	if other.Job.Id == first.Job.Id || exectCount(e.node.Calls(), "StartJob") != 2 {
		t.Fatalf("new key should start a new job: %s", other.Job.Id)
	}
}

// --- VM lifecycle effects on jobs ---------------------------------------------------------------

func TestExecVMStoppedMarksJobLost(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("stopme")
	e.node.Exec = exectFixed(0, "", time.Hour)
	ed := e.editor()

	run := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "sleep"})
	if _, err := ed.VM.StopVM(context.Background(), connect.NewRequest(&vmdv1.StopVMRequest{Id: vm.Id})); err != nil {
		t.Fatal(err)
	}
	e.until(10, "stopped", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_STOPPED })

	j := exectJob(t, ed, run.Job.Id)
	exectFinal(t, j, vmdv1.JobState_JOB_STATE_LOST)
	if !strings.Contains(strings.ToLower(j.KilledReason), "vm") {
		t.Fatalf("killed_reason %q should mention the VM", j.KilledReason)
	}
	if j.FinishedAt == nil {
		t.Fatal("lost job should have a finished time")
	}
	if again := exectJob(t, ed, run.Job.Id); again.State != vmdv1.JobState_JOB_STATE_LOST || again.KilledReason != j.KilledReason {
		t.Fatalf("LOST should be stable: %+v", again)
	}
	if l := exectList(t, ed, vm.Id); len(l) != 1 || l[0].State != vmdv1.JobState_JOB_STATE_LOST {
		t.Fatalf("ListJobs after stop: %+v", l)
	}
}

func TestExecVMCrashMarksJobLost(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("crashme")
	e.node.Exec = exectFixed(0, "", time.Hour)
	ed := e.editor()

	run := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "sleep"})
	e.node.Crash(vm.Id, "kernel panic")
	e.until(20, "running again after crash", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_RUNNING })

	j := exectJob(t, ed, run.Job.Id)
	exectFinal(t, j, vmdv1.JobState_JOB_STATE_LOST)
	if j.KilledReason == "" {
		t.Fatal("lost job should carry a killed_reason")
	}
}

func TestExecVMDeletedJobOutlivesVM(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("gone")
	e.node.Exec = exectFixed(0, "", time.Hour)
	ed := e.editor()
	ctx := context.Background()

	run := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "sleep"})
	if _, err := ed.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: vm.Id, DeleteDisks: true})); err != nil {
		t.Fatal(err)
	}
	e.until(10, "gone", func() bool {
		_, err := ed.VM.GetVM(ctx, connect.NewRequest(&vmdv1.GetVMRequest{Id: vm.Id}))
		return code(err) == connect.CodeNotFound
	})

	p, err := ed.Exec.PollJob(ctx, connect.NewRequest(&vmdv1.PollJobRequest{JobId: run.Job.Id}))
	if err != nil {
		t.Fatalf("PollJob on a deleted VM's job should not error: %v", err)
	}
	exectFinal(t, p.Msg.Job, vmdv1.JobState_JOB_STATE_LOST)
	if p.Msg.Job.KilledReason != "the VM was deleted" {
		t.Fatalf("killed_reason %q, want \"the VM was deleted\"", p.Msg.Job.KilledReason)
	}

	// The lost state is stored: a second poll agrees.
	again := exectJob(t, ed, run.Job.Id)
	if again.State != vmdv1.JobState_JOB_STATE_LOST || again.KilledReason != "the VM was deleted" {
		t.Fatalf("second poll: %+v", again)
	}
}

// --- node failures ------------------------------------------------------------------------------

func TestExecNodeDownFailures(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("down")
	e.node.Exec = exectFixed(0, "", time.Hour)
	ed := e.editor()
	ctx := context.Background()

	run := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "sleep"})

	e.node.SetDown(true)
	_, err := ed.Exec.PollJob(ctx, connect.NewRequest(&vmdv1.PollJobRequest{JobId: run.Job.Id}))
	exectWantCode(t, err, connect.CodeUnavailable)
	_, err = ed.Exec.CancelJob(ctx, connect.NewRequest(&vmdv1.CancelJobRequest{JobId: run.Job.Id}))
	exectWantCode(t, err, connect.CodeUnavailable)
	_, err = exectStart(ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "other"})
	exectWantCode(t, err, connect.CodeUnavailable)
	e.node.SetDown(false)

	// The failed StartJob left a FAILED record; the running job is untouched.
	var failed *vmdv1.Job
	for _, j := range exectList(t, ed, vm.Id) {
		if j.Command == "other" {
			failed = j
		}
	}
	if failed == nil {
		t.Fatal("StartJob that hit a down node left no record")
	}
	exectFinal(t, failed, vmdv1.JobState_JOB_STATE_FAILED)
	if !strings.Contains(failed.KilledReason, "could not start") {
		t.Fatalf("killed_reason %q should say the job could not start", failed.KilledReason)
	}
	exectFinal(t, exectJob(t, ed, run.Job.Id), vmdv1.JobState_JOB_STATE_RUNNING)

	// After recovery new jobs work.
	e.node.Exec = exectFixed(0, "back\n", 0)
	ok := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "after", WaitSeconds: 5})
	exectFinal(t, ok.Job, vmdv1.JobState_JOB_STATE_SUCCEEDED)
	if string(ok.Stdout) != "back\n" {
		t.Fatalf("stdout after recovery %q", ok.Stdout)
	}
}

func TestExecFailNextStartJob(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("failnext")
	ed := e.editor()

	e.node.FailNext("StartJob", errs.Unavailable("boom"))
	_, err := exectStart(ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "doomed"})
	exectWantCode(t, err, connect.CodeUnavailable)

	l := exectList(t, ed, vm.Id)
	if len(l) != 1 {
		t.Fatalf("want 1 record, got %d", len(l))
	}
	exectFinal(t, l[0], vmdv1.JobState_JOB_STATE_FAILED)
	if !strings.Contains(l[0].KilledReason, "could not start") || l[0].ExitCode != nil {
		t.Fatalf("failed record: reason %q exit %v", l[0].KilledReason, l[0].ExitCode)
	}

	ok := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "fine", WaitSeconds: 5})
	exectFinal(t, ok.Job, vmdv1.JobState_JOB_STATE_SUCCEEDED)
	if n := exectCount(e.node.Calls(), "StartJob"); n != 2 {
		t.Fatalf("node StartJob calls %d, want 2", n)
	}
}

// --- files --------------------------------------------------------------------------------------

func TestExecFileRoundTrip(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("files")
	ed := e.editor()
	ctx := context.Background()

	binary := make([]byte, 0, 512)
	for i := range 256 {
		binary = append(binary, byte(i))
	}
	binary = append(binary, 0, 0xff, '\n', 0, 'x')

	if _, err := ed.Exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{VmId: vm.Id, Path: "/opt/bin.dat", Content: binary, Mode: 0o755})); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	got, err := ed.Exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{VmId: vm.Id, Path: "/opt/bin.dat"}))
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !bytes.Equal(got.Msg.Content, binary) || got.Msg.Truncated {
		t.Fatalf("round trip: %d bytes back, want %d (truncated=%v)", len(got.Msg.Content), len(binary), got.Msg.Truncated)
	}

	// Overwrite replaces the content.
	if _, err := ed.Exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{VmId: vm.Id, Path: "/opt/bin.dat", Content: []byte("v2")})); err != nil {
		t.Fatal(err)
	}
	got, _ = ed.Exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{VmId: vm.Id, Path: "/opt/bin.dat"}))
	if string(got.Msg.Content) != "v2" {
		t.Fatalf("overwrite: %q", got.Msg.Content)
	}

	// Exactly 8 MiB is accepted.
	full := exectBig(8 << 20)
	if _, err := ed.Exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{VmId: vm.Id, Path: "/big", Content: full})); err != nil {
		t.Fatalf("PutFile 8 MiB: %v", err)
	}
	got, err = ed.Exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{VmId: vm.Id, Path: "/big"}))
	if err != nil || !bytes.Equal(got.Msg.Content, full) {
		t.Fatalf("8 MiB round trip: err=%v len=%d", err, len(got.Msg.Content))
	}
}

func TestExecGetFileTruncation(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("trunc")
	ed := e.editor()
	ctx := context.Background()

	content := exectBig(100)
	if _, err := ed.Exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{VmId: vm.Id, Path: "f", Content: content})); err != nil {
		t.Fatal(err)
	}
	got, err := ed.Exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{VmId: vm.Id, Path: "f", MaxBytes: 10}))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Msg.Truncated || !bytes.Equal(got.Msg.Content, content[:10]) {
		t.Fatalf("max_bytes=10: %d bytes truncated=%v", len(got.Msg.Content), got.Msg.Truncated)
	}

	got, err = ed.Exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{VmId: vm.Id, Path: "f", MaxBytes: 1000}))
	if err != nil || got.Msg.Truncated || !bytes.Equal(got.Msg.Content, content) {
		t.Fatalf("max_bytes above size: err=%v truncated=%v len=%d", err, got.Msg.Truncated, len(got.Msg.Content))
	}
}

func TestExecFileMissingAndStoppedVM(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("nofile")
	ed := e.editor()
	ctx := context.Background()

	_, err := ed.Exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{VmId: vm.Id, Path: "/nope"}))
	exectWantCode(t, err, connect.CodeNotFound)

	if _, err := ed.VM.StopVM(ctx, connect.NewRequest(&vmdv1.StopVMRequest{Id: vm.Id})); err != nil {
		t.Fatal(err)
	}
	e.until(10, "stopped", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_STOPPED })

	_, err = ed.Exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{VmId: vm.Id, Path: "f", Content: []byte("x")}))
	exectWantCode(t, err, connect.CodeFailedPrecondition)
	_, err = ed.Exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{VmId: vm.Id, Path: "f"}))
	exectWantCode(t, err, connect.CodeFailedPrecondition)
}

// --- listing ------------------------------------------------------------------------------------

func TestExecListJobsPagination(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("list")
	other := e.runningVM("list-other")
	e.node.Exec = exectFixed(0, "", 0)
	ed := e.editor()
	ctx := context.Background()

	var created []string
	for range 5 {
		out := exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "job", WaitSeconds: 5})
		created = append(created, out.Job.Id)
	}
	exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: other.Id, Command: "elsewhere", WaitSeconds: 5})

	var pages [][]*vmdv1.Job
	token := ""
	for range 10 {
		res, err := ed.Exec.ListJobs(ctx, connect.NewRequest(&vmdv1.ListJobsRequest{VmId: vm.Id, PageSize: 2, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, res.Msg.Jobs)
		if res.Msg.NextPageToken == "" {
			break
		}
		token = res.Msg.NextPageToken
	}
	if len(pages) != 3 || len(pages[0]) != 2 || len(pages[1]) != 2 || len(pages[2]) != 1 {
		t.Fatalf("page sizes: %d pages", len(pages))
	}

	var got []string
	for _, p := range pages {
		for _, j := range p {
			if j.VmId != vm.Id {
				t.Fatalf("job %s belongs to %s, not %s", j.Id, j.VmId, vm.Id)
			}
			got = append(got, j.Id)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("ids not ascending: %v", got)
		}
	}
	if strings.Join(got, ",") != strings.Join(created, ",") {
		t.Fatalf("listed %v, created %v", got, created)
	}

	// Without paging parameters everything fits on one page with no token.
	one, err := ed.Exec.ListJobs(ctx, connect.NewRequest(&vmdv1.ListJobsRequest{VmId: vm.Id}))
	if err != nil || len(one.Msg.Jobs) != 5 || one.Msg.NextPageToken != "" {
		t.Fatalf("default page: err=%v jobs=%d token=%q", err, len(one.Msg.Jobs), one.Msg.NextPageToken)
	}

	// The other VM's job never shows up here.
	if n := len(exectList(t, ed, other.Id)); n != 1 {
		t.Fatalf("other VM has %d jobs, want 1", n)
	}
}

// An idempotency key names ONE request. Reusing it for a different command or a different VM is a client
// bug that must be reported, never answered with the first request's job.
func TestExecIdempotencyKeyReuseForDifferentRequestIsRejected(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("keyreuse")
	other := e.runningVM("keyreuse2")
	ed := e.editor()
	ctx := context.Background()

	exectMustStart(t, ed, &vmdv1.StartJobRequest{VmId: vm.Id, Command: "make build", IdempotencyKey: "same-key"})

	for name, req := range map[string]*vmdv1.StartJobRequest{
		"different command": {VmId: vm.Id, Command: "make deploy", IdempotencyKey: "same-key"},
		"different workdir": {VmId: vm.Id, Command: "make build", Workdir: "/tmp", IdempotencyKey: "same-key"},
		"different vm":      {VmId: other.Id, Command: "make build", IdempotencyKey: "same-key"},
	} {
		_, err := ed.Exec.StartJob(ctx, connect.NewRequest(req))
		if code(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: want InvalidArgument, got %v", name, err)
		}
	}
	if n := exectCount(e.node.Calls(), "StartJob"); n != 1 {
		t.Fatalf("rejected replays must not start anything; node saw %d StartJob calls", n)
	}
}

// A StartJob whose node call failed never reached the guest. The client's retry (same idempotency key) must
// start that very job, not replay the failure forever.
func TestExecRetryAfterFailedStartRunsTheSameJob(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("retry")
	e.node.Exec = exectFixed(0, "done\n", 0)
	ed := e.editor()
	ctx := context.Background()
	req := func() *connect.Request[vmdv1.StartJobRequest] {
		return connect.NewRequest(&vmdv1.StartJobRequest{VmId: vm.Id, Command: "make", IdempotencyKey: "retry-1"})
	}

	e.node.FailNext("StartJob", errs.Unavailable("node blip"))
	_, err := ed.Exec.StartJob(ctx, req())
	if code(err) != connect.CodeUnavailable {
		t.Fatalf("first attempt: want Unavailable, got %v", err)
	}
	jobs := exectList(t, ed, vm.Id)
	if len(jobs) != 1 || jobs[0].State != vmdv1.JobState_JOB_STATE_FAILED {
		t.Fatalf("after the failed start there should be one FAILED record: %+v", jobs)
	}
	failedID := jobs[0].Id

	res, err := ed.Exec.StartJob(ctx, req()) // the retry, node healthy again
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.Msg.Job.Id != failedID || res.Msg.Job.State != vmdv1.JobState_JOB_STATE_SUCCEEDED || string(res.Msg.Stdout) != "done\n" {
		t.Fatalf("retry must run the SAME job to completion: %+v stdout=%q", res.Msg.Job, res.Msg.Stdout)
	}
	if n := exectCount(e.node.Calls(), "StartJob"); n != 2 { // one that failed, one that worked
		t.Fatalf("node StartJob calls = %d, want 2", n)
	}
	if l := exectList(t, ed, vm.Id); len(l) != 1 {
		t.Fatalf("a retry must not create a second job record: %d", len(l))
	}

	again, err := ed.Exec.StartJob(ctx, req()) // a further replay is just a read
	if err != nil || again.Msg.Job.Id != failedID || exectCount(e.node.Calls(), "StartJob") != 2 {
		t.Fatalf("replay after success must not start anything: %v %v", again, err)
	}
}

// If the node actually had the job (the start succeeded but its reply was lost), the retry adopts it.
func TestExecRetryAdoptsAJobTheNodeAlreadyHas(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("adopt")
	e.node.Exec = exectFixed(0, "ok\n", 0)
	ed := e.editor()
	ctx := context.Background()
	req := func() *connect.Request[vmdv1.StartJobRequest] {
		return connect.NewRequest(&vmdv1.StartJobRequest{VmId: vm.Id, Command: "true", IdempotencyKey: "adopt-1"})
	}
	first, err := ed.Exec.StartJob(ctx, req())
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the record as the failed-start state while the node still has the job.
	j, err := e.st.GetJob(ctx, first.Msg.Job.Id)
	if err != nil {
		t.Fatal(err)
	}
	j.State, j.ExitCode, j.KilledReason = domain.JobFailed, nil, "could not start: node n1 is unavailable"
	if err := e.st.UpdateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	res, err := ed.Exec.StartJob(ctx, req())
	if err != nil || res.Msg.Job.Id != first.Msg.Job.Id || res.Msg.Job.State != vmdv1.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("retry should adopt the existing job: %+v %v", res, err)
	}
}
