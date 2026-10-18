package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/types/known/timestamppb"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1/vmdv1connect"
)

// exeVMs answers the machine lookup: box is vm-1, other is vm-2, both in project p1.
type exeVMs struct {
	vmdv1connect.VMServiceClient
}

func (f *exeVMs) ListVMs(_ context.Context, _ *connect.Request[vmdv1.ListVMsRequest]) (*connect.Response[vmdv1.ListVMsResponse], error) {
	return connect.NewResponse(&vmdv1.ListVMsResponse{Vms: []*vmdv1.VM{
		{Id: "vm-1", Name: "box", ProjectId: "p1"},
		{Id: "vm-2", Name: "other", ProjectId: "p1"},
	}}), nil
}

// exeExec is a scripted ExecService. Poll n gets pollReplies[min(n, len-1)], so the last reply repeats.
type exeExec struct {
	vmdv1connect.ExecServiceClient

	startReply  *vmdv1.JobOutput
	startErr    error
	sleepStart  bool // block for wait_seconds in StartJob, as vmd does
	sleepPoll   bool // block for wait_seconds in PollJob, as vmd does
	pollReplies []*vmdv1.JobOutput
	pollErr     error
	cancelReply *vmdv1.JobOutput
	cancelErr   error
	listFn      func(*vmdv1.ListJobsRequest) (*vmdv1.ListJobsResponse, error)

	startReq *vmdv1.StartJobRequest
	polls    []*vmdv1.PollJobRequest
	cancels  []string
	lists    []*vmdv1.ListJobsRequest
}

func (f *exeExec) StartJob(_ context.Context, req *connect.Request[vmdv1.StartJobRequest]) (*connect.Response[vmdv1.JobOutput], error) {
	f.startReq = req.Msg
	if f.startErr != nil {
		return nil, f.startErr
	}
	if f.sleepStart {
		time.Sleep(time.Duration(req.Msg.GetWaitSeconds()) * time.Second)
	}
	return connect.NewResponse(f.startReply), nil
}

func (f *exeExec) PollJob(_ context.Context, req *connect.Request[vmdv1.PollJobRequest]) (*connect.Response[vmdv1.JobOutput], error) {
	f.polls = append(f.polls, req.Msg)
	if f.pollErr != nil {
		return nil, f.pollErr
	}
	if f.sleepPoll {
		time.Sleep(time.Duration(req.Msg.GetWaitSeconds()) * time.Second)
	}
	i := min(len(f.polls)-1, len(f.pollReplies)-1)
	return connect.NewResponse(f.pollReplies[i]), nil
}

func (f *exeExec) CancelJob(_ context.Context, req *connect.Request[vmdv1.CancelJobRequest]) (*connect.Response[vmdv1.JobOutput], error) {
	f.cancels = append(f.cancels, req.Msg.GetJobId())
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	return connect.NewResponse(f.cancelReply), nil
}

func (f *exeExec) ListJobs(_ context.Context, req *connect.Request[vmdv1.ListJobsRequest]) (*connect.Response[vmdv1.ListJobsResponse], error) {
	f.lists = append(f.lists, req.Msg)
	res, err := f.listFn(req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func exeJob(state vmdv1.JobState) *vmdv1.Job {
	return &vmdv1.Job{Id: "job-1", VmId: "vm-1", Command: "make", State: state, StartedAt: timestamppb.New(time.Now())}
}

func exeReply(j *vmdv1.Job, stdout string) *vmdv1.JobOutput {
	return &vmdv1.JobOutput{Job: j, Stdout: []byte(stdout)}
}

func exeExit(j *vmdv1.Job, code int32) *vmdv1.Job {
	j.ExitCode = &code
	return j
}

func exeSession(t *testing.T, ex *exeExec, mutate ...func(*Config)) *mcp.ClientSession {
	t.Helper()
	return newTestSession(t, Clients{VMs: &exeVMs{}, Exec: ex}, mutate...)
}

func exeDecode(t *testing.T, text string) jobView {
	t.Helper()
	var v jobView
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("output is not a job view: %v\n%s", err, text)
	}
	return v
}

func exeMustOK(t *testing.T, text string, isErr bool) jobView {
	t.Helper()
	if isErr {
		t.Fatalf("tool error: %s", text)
	}
	return exeDecode(t, text)
}

func TestExeAgentExecFollowsUntilSucceeded(t *testing.T) {
	running := exeJob(vmdv1.JobState_JOB_STATE_RUNNING)
	done := exeExit(exeJob(vmdv1.JobState_JOB_STATE_SUCCEEDED), 0)
	ex := &exeExec{
		startReply:  exeReply(running, "start\n"),
		pollReplies: []*vmdv1.JobOutput{exeReply(running, ""), exeReply(running, ""), exeReply(running, ""), exeReply(done, "done\n")},
	}
	cs := exeSession(t, ex)
	text, isErr := callTool(t, cs, "agent_exec", map[string]any{"name": "box", "command": "make"})
	v := exeMustOK(t, text, isErr)

	if !v.Done || v.State != "succeeded" || v.ExitCode == nil || *v.ExitCode != 0 {
		t.Fatalf("want done succeeded exit 0, got %+v", v)
	}
	if v.Stdout != "done\n" || v.JobID != "job-1" {
		t.Errorf("stdout/job_id: %+v", v)
	}
	req := ex.startReq
	if req.GetVmId() != "vm-1" || req.GetCommand() != "make" || req.GetTimeoutSeconds() != 3600 ||
		req.GetMaxOutput() != 8192 || req.GetIdempotencyKey() != "" || req.GetWorkdir() != "" {
		t.Errorf("StartJob request: %+v", req)
	}
	if len(ex.polls) != 4 {
		t.Fatalf("want 4 polls (3 running, 1 finished), got %d", len(ex.polls))
	}
	for _, p := range ex.polls {
		if !p.GetTail() || p.GetMaxOutput() != 8192 {
			t.Errorf("agent_exec must poll in tail mode with max_output 8192: %+v", p)
		}
	}
}

func TestExeAgentExecPassesOptionsAndZeroWaitReturnsSnapshot(t *testing.T) {
	ex := &exeExec{startReply: exeReply(exeJob(vmdv1.JobState_JOB_STATE_RUNNING), "partial")}
	cs := exeSession(t, ex)
	text, isErr := callTool(t, cs, "agent_exec", map[string]any{
		"name": "box", "command": "sleep 5", "timeout": 120, "wait_seconds": 0, "max_output": 100, "workdir": "/srv"})
	v := exeMustOK(t, text, isErr)
	if v.Done || v.State != "running" || v.ExitCode != nil || v.Stdout != "partial" {
		t.Errorf("want running snapshot with stdout partial, got %+v", v)
	}
	if ex.startReq.GetTimeoutSeconds() != 120 || ex.startReq.GetMaxOutput() != 100 || ex.startReq.GetWorkdir() != "/srv" || ex.startReq.GetWaitSeconds() != 0 {
		t.Errorf("StartJob request: %+v", ex.startReq)
	}
	if len(ex.polls) != 0 {
		t.Errorf("wait_seconds 0 must not poll, polled %d times", len(ex.polls))
	}
}

func TestExeAgentExecWaitBudgetCappedByMaxWait(t *testing.T) {
	ex := &exeExec{startReply: exeReply(exeJob(vmdv1.JobState_JOB_STATE_RUNNING), ""), sleepStart: true}
	cs := exeSession(t, ex, func(c *Config) { c.MaxWait = time.Second })
	start := time.Now()
	text, isErr := callTool(t, cs, "agent_exec", map[string]any{"name": "box", "command": "make"})
	elapsed := time.Since(start)
	v := exeMustOK(t, text, isErr)
	if v.Done {
		t.Errorf("job still runs, want done=false: %+v", v)
	}
	if ex.startReq.GetWaitSeconds() != 1 {
		t.Errorf("StartJob wait must be capped to MaxWait (1s), got %d", ex.startReq.GetWaitSeconds())
	}
	if len(ex.polls) != 0 {
		t.Errorf("budget spent by StartJob, want no polls, got %d", len(ex.polls))
	}
	if elapsed < 900*time.Millisecond || elapsed > 4*time.Second {
		t.Errorf("call took %v, want about MaxWait (1s)", elapsed)
	}
}

func TestExeAgentExecBudgetRunsOutWhilePolling(t *testing.T) {
	running := exeJob(vmdv1.JobState_JOB_STATE_RUNNING)
	ex := &exeExec{startReply: exeReply(running, ""), pollReplies: []*vmdv1.JobOutput{exeReply(running, "")}, sleepPoll: true}
	cs := exeSession(t, ex) // MaxWait 2s
	start := time.Now()
	text, isErr := callTool(t, cs, "agent_exec", map[string]any{"name": "box", "command": "make", "wait_seconds": 2})
	elapsed := time.Since(start)
	v := exeMustOK(t, text, isErr)
	if v.Done {
		t.Errorf("want done=false, got %+v", v)
	}
	if len(ex.polls) != 1 || ex.polls[0].GetWaitSeconds() != 2 {
		t.Fatalf("want one poll waiting the remaining 2 s, got %d polls: %+v", len(ex.polls), ex.polls)
	}
	if elapsed < 1900*time.Millisecond || elapsed > 4*time.Second {
		t.Errorf("call took %v, want about the 2 s budget", elapsed)
	}
}

func TestExeAgentExecValidation(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no name", map[string]any{"command": "make"}, "missing properties"},
		{"empty name", map[string]any{"name": "", "command": "make"}, "name is required"},
		{"empty command", map[string]any{"name": "box", "command": "  "}, "command is required"},
		{"zero timeout", map[string]any{"name": "box", "command": "make", "timeout": 0}, "timeout must be between"},
		{"huge timeout", map[string]any{"name": "box", "command": "make", "timeout": 604801}, "timeout must be between"},
		{"negative wait", map[string]any{"name": "box", "command": "make", "wait_seconds": -1}, "wait_seconds must not be negative"},
		{"zero max_output", map[string]any{"name": "box", "command": "make", "max_output": 0}, "max_output must be between"},
		{"big max_output", map[string]any{"name": "box", "command": "make", "max_output": 70000}, "max_output must be between"},
		{"unknown machine", map[string]any{"name": "nope", "command": "make"}, "not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ex := &exeExec{startReply: exeReply(exeJob(vmdv1.JobState_JOB_STATE_RUNNING), "")}
			cs := exeSession(t, ex)
			text, isErr := callTool(t, cs, "agent_exec", c.args)
			if !isErr || !strings.Contains(text, c.want) {
				t.Fatalf("want error containing %q, got isErr=%v %q", c.want, isErr, text)
			}
			if ex.startReq != nil {
				t.Errorf("StartJob must not be called after a validation error")
			}
		})
	}
}

func TestExeAgentExecDownstreamError(t *testing.T) {
	ex := &exeExec{startErr: connect.NewError(connect.CodeFailedPrecondition, errors.New("vm vm-1 is stopped"))}
	cs := exeSession(t, ex)
	text, isErr := callTool(t, cs, "agent_exec", map[string]any{"name": "box", "command": "make"})
	if !isErr || text != "failed_precondition: vm vm-1 is stopped" {
		t.Fatalf("want the downstream error as tool error, got isErr=%v %q", isErr, text)
	}
}

func TestExeAgentExecPollHappyPathTailDefault(t *testing.T) {
	running := exeJob(vmdv1.JobState_JOB_STATE_RUNNING)
	done := exeExit(exeJob(vmdv1.JobState_JOB_STATE_FAILED), 2)
	ex := &exeExec{pollReplies: []*vmdv1.JobOutput{exeReply(running, "a"), exeReply(done, "bye")}}
	cs := exeSession(t, ex)
	text, isErr := callTool(t, cs, "agent_exec_poll", map[string]any{"name": "box", "job_id": "job-1", "wait_seconds": 30})
	v := exeMustOK(t, text, isErr)
	if !v.Done || v.State != "failed" || v.ExitCode == nil || *v.ExitCode != 2 || v.Stdout != "bye" {
		t.Errorf("got %+v", v)
	}
	if len(ex.polls) != 2 {
		t.Fatalf("want snapshot poll + one waiting poll, got %d", len(ex.polls))
	}
	if ex.polls[0].GetWaitSeconds() != 0 || !ex.polls[0].GetTail() {
		t.Errorf("first poll must be a tail snapshot without waiting: %+v", ex.polls[0])
	}
	if ex.polls[1].GetWaitSeconds() != 2 {
		// MaxWait is 2 s and just under 2 s of budget is left after the snapshot, so the wait rounds up to 2 s.
		t.Errorf("waiting poll wait %d, want 2", ex.polls[1].GetWaitSeconds())
	}
}

func TestExeAgentExecPollOffsetModeAccumulatesWithinMaxOutput(t *testing.T) {
	running := exeJob(vmdv1.JobState_JOB_STATE_RUNNING)
	first := &vmdv1.JobOutput{Job: running, Stdout: []byte("abc"), StdoutOffset: 6}
	second := &vmdv1.JobOutput{Job: running, Stdout: []byte("de"), StdoutOffset: 8, More: true}
	ex := &exeExec{pollReplies: []*vmdv1.JobOutput{first, second}}
	cs := exeSession(t, ex)
	text, isErr := callTool(t, cs, "agent_exec_poll", map[string]any{
		"name": "box", "job_id": "job-1", "stdout_offset": 3, "max_output": 5})
	v := exeMustOK(t, text, isErr)
	if v.Stdout != "abcde" || v.StdoutOffset != 8 || !v.More || v.Done {
		t.Errorf("want accumulated abcde at offset 8 with more, got %+v", v)
	}
	if len(ex.polls) != 2 {
		t.Fatalf("max_output 5 allows exactly two polls here, got %d", len(ex.polls))
	}
	if ex.polls[0].GetTail() || ex.polls[0].GetStdoutOffset() != 3 {
		t.Errorf("offset mode from stdout_offset 3 expected, got %+v", ex.polls[0])
	}
	if ex.polls[1].GetStdoutOffset() != 6 || ex.polls[1].GetMaxOutput() != 2 {
		t.Errorf("second poll must continue at offset 6 with the 2 bytes left, got %+v", ex.polls[1])
	}
}

func TestExeAgentExecPollTailOptionResolution(t *testing.T) {
	tr, fa := true, false
	cases := []struct {
		name     string
		args     map[string]any
		wantTail bool
	}{
		{"offsets zero", map[string]any{"stdout_offset": 0}, true},
		{"offset set", map[string]any{"stdout_offset": 4}, false},
		{"stderr offset set", map[string]any{"stderr_offset": 4}, false},
		{"explicit tail over offset", map[string]any{"stdout_offset": 4, "tail": tr}, true},
		{"explicit no tail", map[string]any{"tail": fa}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ex := &exeExec{pollReplies: []*vmdv1.JobOutput{exeReply(exeExit(exeJob(vmdv1.JobState_JOB_STATE_SUCCEEDED), 0), "")}}
			cs := exeSession(t, ex)
			args := map[string]any{"name": "box", "job_id": "job-1", "wait_seconds": 0}
			for k, v := range c.args {
				args[k] = v
			}
			text, isErr := callTool(t, cs, "agent_exec_poll", args)
			exeMustOK(t, text, isErr)
			if ex.polls[0].GetTail() != c.wantTail {
				t.Errorf("tail = %v, want %v", ex.polls[0].GetTail(), c.wantTail)
			}
		})
	}
}

func TestExeAgentExecPollRefusesJobOfOtherMachine(t *testing.T) {
	foreign := exeJob(vmdv1.JobState_JOB_STATE_RUNNING)
	foreign.VmId = "vm-2"
	ex := &exeExec{pollReplies: []*vmdv1.JobOutput{exeReply(foreign, "secret")}}
	cs := exeSession(t, ex)
	text, isErr := callTool(t, cs, "agent_exec_poll", map[string]any{"name": "box", "job_id": "job-1"})
	if !isErr || text != "job job-1 does not belong to machine 'box'" {
		t.Fatalf("want ownership error, got isErr=%v %q", isErr, text)
	}
	if strings.Contains(text, "secret") {
		t.Errorf("output of a foreign job leaked: %q", text)
	}
}

func TestExeAgentExecPollValidationAndErrors(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no name", map[string]any{"job_id": "job-1"}, "missing properties"},
		{"empty name", map[string]any{"name": "", "job_id": "job-1"}, "name is required"},
		{"no job id", map[string]any{"name": "box"}, "missing properties"},
		{"empty job id", map[string]any{"name": "box", "job_id": ""}, "job_id is required"},
		{"negative offset", map[string]any{"name": "box", "job_id": "job-1", "stderr_offset": -1}, "must not be negative"},
		{"negative wait", map[string]any{"name": "box", "job_id": "job-1", "wait_seconds": -3}, "wait_seconds must not be negative"},
		{"bad max_output", map[string]any{"name": "box", "job_id": "job-1", "max_output": 65537}, "max_output must be between"},
		{"unknown machine", map[string]any{"name": "nope", "job_id": "job-1"}, "not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cs := exeSession(t, &exeExec{})
			text, isErr := callTool(t, cs, "agent_exec_poll", c.args)
			if !isErr || !strings.Contains(text, c.want) {
				t.Fatalf("want error containing %q, got isErr=%v %q", c.want, isErr, text)
			}
		})
	}

	ex := &exeExec{pollErr: connect.NewError(connect.CodeNotFound, errors.New("no job job-1"))}
	cs := exeSession(t, ex)
	text, isErr := callTool(t, cs, "agent_exec_poll", map[string]any{"name": "box", "job_id": "job-1"})
	if !isErr || text != "not_found: no job job-1" {
		t.Fatalf("want downstream error, got isErr=%v %q", isErr, text)
	}
}

func TestExeOutputIsValidUTF8AndKeysAreFixed(t *testing.T) {
	bad := exeExit(exeJob(vmdv1.JobState_JOB_STATE_SUCCEEDED), 0)
	ex := &exeExec{pollReplies: []*vmdv1.JobOutput{{Job: bad, Stdout: []byte("ok\xff\xfe"), Stderr: []byte("\xc3")}}}
	cs := exeSession(t, ex)
	text, isErr := callTool(t, cs, "agent_exec_poll", map[string]any{"name": "box", "job_id": "job-1", "wait_seconds": 0})
	exeMustOK(t, text, isErr)
	if !utf8.ValidString(text) {
		t.Fatalf("output is not valid UTF-8: %q", text)
	}
	v := exeDecode(t, text)
	if v.Stdout != "ok�" || v.Stderr != "�" {
		t.Errorf("invalid bytes not replaced: stdout %q stderr %q", v.Stdout, v.Stderr)
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"done", "exit_code", "job_id", "more", "seconds", "state", "stderr", "stderr_offset", "stdout", "stdout_offset", "truncated"}
	if got := sortedKeys(raw); !reflect.DeepEqual(got, wantKeys) {
		t.Errorf("keys without killed_reason = %v, want %v", got, wantKeys)
	}
	if raw["exit_code"] != float64(0) {
		t.Errorf("exit_code = %v, want 0", raw["exit_code"])
	}

	killed := exeJob(vmdv1.JobState_JOB_STATE_TIMED_OUT)
	killed.KilledReason = "timed out and was killed"
	ex2 := &exeExec{pollReplies: []*vmdv1.JobOutput{exeReply(killed, "")}}
	cs2 := exeSession(t, ex2)
	text2, isErr2 := callTool(t, cs2, "agent_exec_poll", map[string]any{"name": "box", "job_id": "job-1", "wait_seconds": 0})
	v2 := exeMustOK(t, text2, isErr2)
	if v2.State != "timed_out" || v2.KilledReason != "timed out and was killed" || v2.ExitCode != nil {
		t.Errorf("got %+v", v2)
	}
	var raw2 map[string]any
	if err := json.Unmarshal([]byte(text2), &raw2); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw2["killed_reason"]; !ok || raw2["exit_code"] != nil {
		t.Errorf("killed_reason must be present and exit_code null: %v", raw2)
	}
}

func TestExeStateNames(t *testing.T) {
	want := map[vmdv1.JobState]string{
		vmdv1.JobState_JOB_STATE_UNSPECIFIED: "unspecified",
		vmdv1.JobState_JOB_STATE_RUNNING:     "running",
		vmdv1.JobState_JOB_STATE_SUCCEEDED:   "succeeded",
		vmdv1.JobState_JOB_STATE_FAILED:      "failed",
		vmdv1.JobState_JOB_STATE_CANCELLED:   "cancelled",
		vmdv1.JobState_JOB_STATE_TIMED_OUT:   "timed_out",
		vmdv1.JobState_JOB_STATE_LOST:        "lost",
	}
	for st, w := range want {
		if got := stateName(st); got != w {
			t.Errorf("stateName(%v) = %q, want %q", st, got, w)
		}
	}
}

func TestExeAgentExecCancelChecksOwnershipThenCancels(t *testing.T) {
	cancelled := exeJob(vmdv1.JobState_JOB_STATE_CANCELLED)
	cancelled.KilledReason = "cancelled"
	ex := &exeExec{
		cancelReply: exeReply(cancelled, ""),
		listFn: func(r *vmdv1.ListJobsRequest) (*vmdv1.ListJobsResponse, error) {
			if r.GetPageToken() == "" {
				return &vmdv1.ListJobsResponse{Jobs: []*vmdv1.Job{{Id: "job-0", VmId: "vm-1"}}, NextPageToken: "p2"}, nil
			}
			return &vmdv1.ListJobsResponse{Jobs: []*vmdv1.Job{{Id: "job-1", VmId: "vm-1"}}}, nil
		},
	}
	cs := exeSession(t, ex)
	text, isErr := callTool(t, cs, "agent_exec_cancel", map[string]any{"name": "box", "job_id": "job-1"})
	v := exeMustOK(t, text, isErr)
	if v.State != "cancelled" || !v.Done || v.KilledReason != "cancelled" {
		t.Errorf("got %+v", v)
	}
	if len(ex.lists) != 2 || ex.lists[1].GetPageToken() != "p2" {
		t.Errorf("ownership must search every page of the machine's jobs, got %d list calls", len(ex.lists))
	}
	if !reflect.DeepEqual(ex.cancels, []string{"job-1"}) {
		t.Errorf("cancels = %v", ex.cancels)
	}
}

func TestExeAgentExecCancelRefusesJobOfOtherMachine(t *testing.T) {
	ex := &exeExec{
		listFn: func(*vmdv1.ListJobsRequest) (*vmdv1.ListJobsResponse, error) {
			return &vmdv1.ListJobsResponse{Jobs: []*vmdv1.Job{{Id: "job-9", VmId: "vm-2"}}}, nil
		},
	}
	cs := exeSession(t, ex)
	text, isErr := callTool(t, cs, "agent_exec_cancel", map[string]any{"name": "box", "job_id": "job-1"})
	if !isErr || text != "job job-1 does not belong to machine 'box'" {
		t.Fatalf("want ownership error, got isErr=%v %q", isErr, text)
	}
	if len(ex.cancels) != 0 {
		t.Errorf("a job of another machine must not be cancelled")
	}
}

func TestExeAgentExecCancelDownstreamError(t *testing.T) {
	ex := &exeExec{
		cancelErr: connect.NewError(connect.CodeFailedPrecondition, errors.New("vm vm-1 is stopped")),
		listFn: func(*vmdv1.ListJobsRequest) (*vmdv1.ListJobsResponse, error) {
			return &vmdv1.ListJobsResponse{Jobs: []*vmdv1.Job{{Id: "job-1", VmId: "vm-1"}}}, nil
		},
	}
	cs := exeSession(t, ex)
	text, isErr := callTool(t, cs, "agent_exec_cancel", map[string]any{"name": "box", "job_id": "job-1"})
	if !isErr || text != "failed_precondition: vm vm-1 is stopped" {
		t.Fatalf("want downstream error, got isErr=%v %q", isErr, text)
	}
}

func TestExeAgentExecCancelValidationAndErrors(t *testing.T) {
	cs := exeSession(t, &exeExec{})
	if text, isErr := callTool(t, cs, "agent_exec_cancel", map[string]any{"name": "box", "job_id": ""}); !isErr || !strings.Contains(text, "job_id is required") {
		t.Errorf("empty job_id: isErr=%v %q", isErr, text)
	}
	if text, isErr := callTool(t, cs, "agent_exec_cancel", map[string]any{"job_id": "job-1"}); !isErr || !strings.Contains(text, "missing properties") {
		t.Errorf("missing name: isErr=%v %q", isErr, text)
	}
	ex := &exeExec{listFn: func(*vmdv1.ListJobsRequest) (*vmdv1.ListJobsResponse, error) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("no access"))
	}}
	cs2 := exeSession(t, ex)
	if text, isErr := callTool(t, cs2, "agent_exec_cancel", map[string]any{"name": "box", "job_id": "job-1"}); !isErr || text != "permission_denied: no access" {
		t.Errorf("list error: isErr=%v %q", isErr, text)
	}
}

func TestExeAgentExecListFormatsNewestFirst(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	long := strings.Repeat("x", 100) + "\nnext"
	a := &vmdv1.Job{Id: "job-a", VmId: "vm-1", Command: "first", State: vmdv1.JobState_JOB_STATE_FAILED,
		StartedAt: timestamppb.New(base), FinishedAt: timestamppb.New(base.Add(3 * time.Second))}
	a = exeExit(a, 2)
	b := &vmdv1.Job{Id: "job-b", VmId: "vm-1", Command: long, State: vmdv1.JobState_JOB_STATE_SUCCEEDED,
		StartedAt: timestamppb.New(base.Add(10 * time.Second)), FinishedAt: timestamppb.New(base.Add(12 * time.Second))}
	b = exeExit(b, 0)
	c := &vmdv1.Job{Id: "job-c", VmId: "vm-1", Command: "running now", State: vmdv1.JobState_JOB_STATE_RUNNING,
		StartedAt: timestamppb.New(base.Add(20 * time.Second))}
	ex := &exeExec{listFn: func(*vmdv1.ListJobsRequest) (*vmdv1.ListJobsResponse, error) {
		// vmd returns ascending ID order: a, b, c.
		return &vmdv1.ListJobsResponse{Jobs: []*vmdv1.Job{a, b, c}}, nil
	}}
	cs := exeSession(t, ex)
	text, isErr := callTool(t, cs, "agent_exec_list", map[string]any{"name": "box"})
	if isErr {
		t.Fatalf("tool error: %s", text)
	}
	lines := strings.Split(text, "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d: %q", len(lines), text)
	}
	if !strings.HasPrefix(lines[0], "job-c running exit=- ") || !strings.HasSuffix(lines[0], "s running now") {
		t.Errorf("line 0 = %q", lines[0])
	}
	if lines[1] != "job-b succeeded exit=0 2.0s "+strings.Repeat("x", 77)+"..." {
		t.Errorf("line 1 = %q", lines[1])
	}
	if lines[2] != "job-a failed exit=2 3.0s first" {
		t.Errorf("line 2 = %q", lines[2])
	}
	if ex.lists[0].GetPageSize() != 50 || ex.lists[0].GetVmId() != "vm-1" {
		t.Errorf("ListJobs request: %+v", ex.lists[0])
	}
}

func TestExeAgentExecListEmptyAndMorePages(t *testing.T) {
	ex := &exeExec{listFn: func(*vmdv1.ListJobsRequest) (*vmdv1.ListJobsResponse, error) {
		return &vmdv1.ListJobsResponse{}, nil
	}}
	cs := exeSession(t, ex)
	if text, isErr := callTool(t, cs, "agent_exec_list", map[string]any{"name": "box"}); isErr || text != "(no jobs)" {
		t.Errorf("empty list: isErr=%v %q", isErr, text)
	}

	ex2 := &exeExec{listFn: func(*vmdv1.ListJobsRequest) (*vmdv1.ListJobsResponse, error) {
		return &vmdv1.ListJobsResponse{Jobs: []*vmdv1.Job{{Id: "job-1", VmId: "vm-1", State: vmdv1.JobState_JOB_STATE_LOST}}, NextPageToken: "more"}, nil
	}}
	cs2 := exeSession(t, ex2)
	text, isErr := callTool(t, cs2, "agent_exec_list", map[string]any{"name": "box"})
	if isErr {
		t.Fatalf("tool error: %s", text)
	}
	lines := strings.Split(text, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "job-1 lost exit=- ") || !strings.Contains(lines[1], "more jobs exist") {
		t.Errorf("got %q", text)
	}
}

func TestExeAgentExecListValidationAndErrors(t *testing.T) {
	cs := exeSession(t, &exeExec{})
	if text, isErr := callTool(t, cs, "agent_exec_list", map[string]any{"name": ""}); !isErr || !strings.Contains(text, "name is required") {
		t.Errorf("missing name: isErr=%v %q", isErr, text)
	}
	if text, isErr := callTool(t, cs, "agent_exec_list", map[string]any{"name": "nope"}); !isErr || !strings.Contains(text, "not_found") {
		t.Errorf("unknown machine: isErr=%v %q", isErr, text)
	}
}

func TestExeClipCommand(t *testing.T) {
	if got := execClip("short", 80); got != "short" {
		t.Errorf("short command changed: %q", got)
	}
	got := execClip(strings.Repeat("é", 100), 80)
	if utf8.RuneCountInString(got) != 80 || !strings.HasSuffix(got, "...") {
		t.Errorf("clip to 80 runes with ellipsis: %q (%d runes)", got, utf8.RuneCountInString(got))
	}
	if got := execClip("a\r\nb\tc", 80); got != "a  b c" {
		t.Errorf("line breaks not flattened: %q", got)
	}
}

func TestExeJobSeconds(t *testing.T) {
	now := time.Unix(1000, 0)
	start := timestamppb.New(time.Unix(990, 0))
	finished := timestamppb.New(time.Unix(994, 500_000_000))
	cases := []struct {
		name string
		job  *vmdv1.Job
		want float64
	}{
		{"no start", &vmdv1.Job{State: vmdv1.JobState_JOB_STATE_RUNNING}, 0},
		{"running", &vmdv1.Job{State: vmdv1.JobState_JOB_STATE_RUNNING, StartedAt: start}, 10},
		{"finished", &vmdv1.Job{State: vmdv1.JobState_JOB_STATE_SUCCEEDED, StartedAt: start, FinishedAt: finished}, 4.5},
		{"finished without end", &vmdv1.Job{State: vmdv1.JobState_JOB_STATE_LOST, StartedAt: start}, 0},
	}
	for _, c := range cases {
		if got := jobSeconds(c.job, now); got != c.want {
			t.Errorf("%s: jobSeconds = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestExeExecOptionsDefaults(t *testing.T) {
	tm, w, m, err := execOptions(nil, nil, nil)
	if err != nil || tm != 3600 || w != 45 || m != 8192 {
		t.Errorf("defaults = %d %d %d %v", tm, w, m, err)
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
