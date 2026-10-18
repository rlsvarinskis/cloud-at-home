package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
)

const (
	execDefaultTimeout = 3600
	execMaxTimeout     = 7 * 24 * 3600 // vmd's limit
	execDefaultWait    = 45
	execDefaultOutput  = 8192
	execMaxOutput      = 65536
	execPageSize       = 50
	execCommandClip    = 80
)

type execArgs struct {
	Name        string `json:"name" jsonschema:"name of the machine (VM) to run the command on"`
	Command     string `json:"command" jsonschema:"shell command, run as bash -lc inside the machine; required"`
	Timeout     *int   `json:"timeout,omitempty" jsonschema:"seconds the command may run in total before it is killed (1 to 604800, default 3600)"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"seconds this call waits for the command to finish (0 to 40 effectively, default 45); the command keeps running after the wait ends"`
	MaxOutput   *int   `json:"max_output,omitempty" jsonschema:"maximum bytes returned per stream (stdout and stderr), 1 to 65536, default 8192; the newest bytes are returned"`
	Workdir     string `json:"workdir,omitempty" jsonschema:"directory to run in; default is the agent user's home directory"`
	Project     string `json:"project,omitempty" jsonschema:"project of the machine; default is the server's default project"`
}

type execPollArgs struct {
	Name         string `json:"name" jsonschema:"name of the machine the job runs on"`
	JobID        string `json:"job_id" jsonschema:"job_id returned by agent_exec, agent_exec_poll or agent_exec_list"`
	WaitSeconds  *int   `json:"wait_seconds,omitempty" jsonschema:"seconds this call waits for the job to finish (default 45)"`
	StdoutOffset int64  `json:"stdout_offset,omitempty" jsonschema:"stdout position to read from, the stdout_offset of the previous result (default 0)"`
	StderrOffset int64  `json:"stderr_offset,omitempty" jsonschema:"stderr position to read from, the stderr_offset of the previous result (default 0)"`
	MaxOutput    *int   `json:"max_output,omitempty" jsonschema:"maximum bytes returned per stream, 1 to 65536 (default 8192)"`
	Tail         *bool  `json:"tail,omitempty" jsonschema:"true returns the last max_output bytes of each stream; false reads from the offsets. Default: true when both offsets are 0, else false"`
	Project      string `json:"project,omitempty" jsonschema:"project of the machine; default is the server's default project"`
}

type execCancelArgs struct {
	Name    string `json:"name" jsonschema:"name of the machine the job runs on"`
	JobID   string `json:"job_id" jsonschema:"job_id of a running job"`
	Project string `json:"project,omitempty" jsonschema:"project of the machine; default is the server's default project"`
}

type execListArgs struct {
	Name    string `json:"name" jsonschema:"name of the machine whose jobs to list"`
	Project string `json:"project,omitempty" jsonschema:"project of the machine; default is the server's default project"`
}

// jobView is the JSON object every exec tool returns. Its keys are always the same, except killed_reason, which is
// omitted when empty.
type jobView struct {
	JobID        string  `json:"job_id"`
	Done         bool    `json:"done"`
	State        string  `json:"state"`
	ExitCode     *int    `json:"exit_code"`
	Seconds      float64 `json:"seconds"`
	Stdout       string  `json:"stdout"`
	Stderr       string  `json:"stderr"`
	Truncated    bool    `json:"truncated"`
	StdoutOffset int64   `json:"stdout_offset"`
	StderrOffset int64   `json:"stderr_offset"`
	More         bool    `json:"more"`
	KilledReason string  `json:"killed_reason,omitempty"`
}

func (s *Server) registerExecTools() {
	addTool(s, "agent_exec",
		"Run a shell command (bash -lc) inside the machine `name` and wait up to wait_seconds (default 45) for it to finish. "+
			"The command keeps running in the VM even after this call returns, also across MCP server restarts. "+
			"Returns one JSON object: job_id, done (false while the command runs), state (running, succeeded, failed, cancelled, timed_out or lost), "+
			"exit_code (null until it exits), seconds, stdout and stderr (the newest max_output bytes of each), truncated, stdout_offset, stderr_offset, more, "+
			"and killed_reason when the job was stopped by the system. "+
			"If done is false, the command is still running: call agent_exec_poll with the job_id to wait for the rest. "+
			"Timeout is the total run time of the command (default 3600 s). Waiting is capped by the server (about 40 s per call).",
		func(ctx context.Context, in execArgs) (string, error) { return s.execStart(ctx, in) })

	addTool(s, "agent_exec_poll",
		"Wait for a job started by agent_exec (or any job on the machine) and return its output, as one JSON object with the keys described for agent_exec. "+
			"Use it when agent_exec returned done=false: call it again with the job_id until done is true. "+
			"Output: with tail (the default when both offsets are 0) the last max_output bytes of each stream are returned; "+
			"otherwise the output after stdout_offset and stderr_offset is returned, and the returned offsets are the ones to pass on the next call. "+
			"more=true means more output is waiting. The job keeps running in the VM whether or not you poll.",
		func(ctx context.Context, in execPollArgs) (string, error) { return s.execPollTool(ctx, in) })

	addTool(s, "agent_exec_cancel",
		"Cancel a running job on the machine `name` (it stops with state cancelled) and return its state as one JSON object (see agent_exec). "+
			"Cancelling a job that already finished changes nothing and just returns its final state.",
		func(ctx context.Context, in execCancelArgs) (string, error) { return s.execCancelTool(ctx, in) })

	addTool(s, "agent_exec_list",
		"List the most recent jobs (up to 50) run on the machine `name`, newest first, one line each: "+
			"`job_id state exit=<code or -> <seconds>s <command>` (the command is clipped to 80 characters). "+
			"Use it to find a job_id for agent_exec_poll or agent_exec_cancel. Prints (no jobs) when there are none.",
		func(ctx context.Context, in execListArgs) (string, error) { return s.execListTool(ctx, in) })
}

// execStart implements agent_exec: start the job, then wait within the wait budget.
func (s *Server) execStart(ctx context.Context, in execArgs) (string, error) {
	if in.Name == "" {
		return "", errors.New("name is required: the machine to run the command on")
	}
	if strings.TrimSpace(in.Command) == "" {
		return "", errors.New("command is required")
	}
	timeout, wait, maxOut, err := execOptions(in.Timeout, in.WaitSeconds, in.MaxOutput)
	if err != nil {
		return "", err
	}
	vm, err := s.vmByName(ctx, in.Project, in.Name)
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(s.execBudget(wait))
	res, err := s.cfg.Clients.Exec.StartJob(ctx, connect.NewRequest(&vmdv1.StartJobRequest{
		VmId: vm.GetId(), Command: in.Command, Workdir: in.Workdir, TimeoutSeconds: int32(timeout),
		WaitSeconds: int32(s.waitSeconds(wait)), MaxOutput: int32(maxOut)}))
	if err != nil {
		return "", err
	}
	p := &execPoll{machine: in.Name, vmID: vm.GetId(), jobID: res.Msg.GetJob().GetId(), tail: true, maxOut: int32(maxOut)}
	p.absorb(res.Msg)
	if err := s.execAwait(ctx, p, deadline); err != nil {
		return "", err
	}
	return execRender(p, time.Now())
}

// execPollTool implements agent_exec_poll.
func (s *Server) execPollTool(ctx context.Context, in execPollArgs) (string, error) {
	if in.Name == "" {
		return "", errors.New("name is required: the machine the job runs on")
	}
	if in.JobID == "" {
		return "", errors.New("job_id is required")
	}
	if in.StdoutOffset < 0 || in.StderrOffset < 0 {
		return "", errors.New("stdout_offset and stderr_offset must not be negative")
	}
	wait := execDefaultWait
	if in.WaitSeconds != nil {
		wait = *in.WaitSeconds
	}
	if wait < 0 {
		return "", errors.New("wait_seconds must not be negative")
	}
	maxOut, err := execMaxOutputArg(in.MaxOutput)
	if err != nil {
		return "", err
	}
	tail := in.StdoutOffset == 0 && in.StderrOffset == 0
	if in.Tail != nil {
		tail = *in.Tail
	}
	vm, err := s.vmByName(ctx, in.Project, in.Name)
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(s.execBudget(wait))
	p := &execPoll{machine: in.Name, vmID: vm.GetId(), jobID: in.JobID, tail: tail, maxOut: int32(maxOut),
		outOff: in.StdoutOffset, errOff: in.StderrOffset}
	// A first poll that does not wait checks that the job belongs to the machine before the wait starts.
	if err := s.execPollOnce(ctx, p, 0); err != nil {
		return "", err
	}
	if err := s.execAwait(ctx, p, deadline); err != nil {
		return "", err
	}
	return execRender(p, time.Now())
}

// execCancelTool implements agent_exec_cancel.
func (s *Server) execCancelTool(ctx context.Context, in execCancelArgs) (string, error) {
	if in.Name == "" {
		return "", errors.New("name is required: the machine the job runs on")
	}
	if in.JobID == "" {
		return "", errors.New("job_id is required")
	}
	vm, err := s.vmByName(ctx, in.Project, in.Name)
	if err != nil {
		return "", err
	}
	if err := s.execRequireJob(ctx, vm, in.Name, in.JobID); err != nil {
		return "", err
	}
	res, err := s.cfg.Clients.Exec.CancelJob(ctx, connect.NewRequest(&vmdv1.CancelJobRequest{JobId: in.JobID}))
	if err != nil {
		return "", err
	}
	p := &execPoll{machine: in.Name, vmID: vm.GetId(), jobID: in.JobID, tail: true, maxOut: execDefaultOutput}
	p.absorb(res.Msg)
	return execRender(p, time.Now())
}

// execListTool implements agent_exec_list.
func (s *Server) execListTool(ctx context.Context, in execListArgs) (string, error) {
	if in.Name == "" {
		return "", errors.New("name is required: the machine whose jobs to list")
	}
	vm, err := s.vmByName(ctx, in.Project, in.Name)
	if err != nil {
		return "", err
	}
	res, err := s.cfg.Clients.Exec.ListJobs(ctx, connect.NewRequest(&vmdv1.ListJobsRequest{VmId: vm.GetId(), PageSize: execPageSize}))
	if err != nil {
		return "", err
	}
	jobs := append([]*vmdv1.Job(nil), res.Msg.GetJobs()...)
	// vmd returns jobs in ascending ID order, so the newest first is established here by start time.
	sort.SliceStable(jobs, func(i, j int) bool {
		return jobs[i].GetStartedAt().AsTime().After(jobs[j].GetStartedAt().AsTime())
	})
	if len(jobs) == 0 && res.Msg.GetNextPageToken() == "" {
		return "(no jobs)", nil
	}
	now := time.Now()
	lines := make([]string, 0, len(jobs)+1)
	for _, j := range jobs {
		exit := "-"
		if j.ExitCode != nil {
			exit = strconv.Itoa(int(j.GetExitCode()))
		}
		lines = append(lines, fmt.Sprintf("%s %s exit=%s %.1fs %s", j.GetId(), stateName(j.GetState()), exit,
			jobSeconds(j, now), execClip(j.GetCommand(), execCommandClip)))
	}
	if res.Msg.GetNextPageToken() != "" {
		lines = append(lines, fmt.Sprintf("(more jobs exist; only the first %d are listed)", execPageSize))
	}
	return strings.Join(lines, "\n"), nil
}

// execOptions validates and defaults the arguments shared by agent_exec and the poll tool (timeout is only used by
// agent_exec, which passes it on as the total lifetime of the command).
func execOptions(timeout, wait, maxOut *int) (int, int, int, error) {
	t := execDefaultTimeout
	if timeout != nil {
		t = *timeout
	}
	if t < 1 || t > execMaxTimeout {
		return 0, 0, 0, fmt.Errorf("timeout must be between 1 and %d seconds", execMaxTimeout)
	}
	w := execDefaultWait
	if wait != nil {
		w = *wait
	}
	if w < 0 {
		return 0, 0, 0, errors.New("wait_seconds must not be negative")
	}
	m, err := execMaxOutputArg(maxOut)
	if err != nil {
		return 0, 0, 0, err
	}
	return t, w, m, nil
}

func execMaxOutputArg(maxOut *int) (int, error) {
	m := execDefaultOutput
	if maxOut != nil {
		m = *maxOut
	}
	if m < 1 || m > execMaxOutput {
		return 0, fmt.Errorf("max_output must be between 1 and %d bytes", execMaxOutput)
	}
	return m, nil
}

// execBudget is the total time one tool call may wait: wait_seconds, but never more than the server's MaxWait.
func (s *Server) execBudget(wait int) time.Duration {
	return min(time.Duration(wait)*time.Second, s.cfg.MaxWait)
}

// execRequireJob fails unless jobID is a job of vm. vmd has no lookup for one job, so the machine's job list is read
// (every page) and searched. It runs before anything is cancelled.
func (s *Server) execRequireJob(ctx context.Context, vm *vmdv1.VM, name, jobID string) error {
	token := ""
	for {
		res, err := s.cfg.Clients.Exec.ListJobs(ctx, connect.NewRequest(&vmdv1.ListJobsRequest{
			VmId: vm.GetId(), PageSize: execPageSize, PageToken: token}))
		if err != nil {
			return err
		}
		for _, j := range res.Msg.GetJobs() {
			if j.GetId() == jobID {
				return nil
			}
		}
		token = res.Msg.GetNextPageToken()
		if token == "" {
			return fmt.Errorf("job %s does not belong to machine '%s'", jobID, name)
		}
	}
}

// execPoll tracks the output of one job across the polls of one tool call.
type execPoll struct {
	machine string // the machine name the caller gave
	vmID    string
	jobID   string
	tail    bool
	maxOut  int32
	outOff  int64
	errOff  int64
	stdout  []byte
	stderr  []byte
	last    *vmdv1.JobOutput
}

// absorb folds a reply into the poll. A tail reply replaces the output so far; an offset reply is appended, so the
// caller gets everything from its starting offsets up to what this call read.
func (p *execPoll) absorb(out *vmdv1.JobOutput) {
	if p.tail {
		p.stdout, p.stderr = out.GetStdout(), out.GetStderr()
	} else {
		p.stdout = append(p.stdout, out.GetStdout()...)
		p.stderr = append(p.stderr, out.GetStderr()...)
	}
	p.outOff, p.errOff = out.GetStdoutOffset(), out.GetStderrOffset()
	p.last = out
}

// room reports whether another offset poll may read more output without a stream exceeding max_output in total.
func (p *execPoll) room() bool {
	return p.tail || (len(p.stdout) < int(p.maxOut) && len(p.stderr) < int(p.maxOut))
}

// execPollOnce makes one PollJob call that waits up to wait seconds and refuses a job of another machine.
func (s *Server) execPollOnce(ctx context.Context, p *execPoll, wait int) error {
	maxOut := p.maxOut
	if !p.tail {
		// Keep each stream within max_output across the polls of this call. room() guarantees this stays positive.
		maxOut -= int32(max(len(p.stdout), len(p.stderr)))
	}
	res, err := s.cfg.Clients.Exec.PollJob(ctx, connect.NewRequest(&vmdv1.PollJobRequest{
		JobId: p.jobID, WaitSeconds: int32(wait), StdoutOffset: p.outOff, StderrOffset: p.errOff,
		MaxOutput: maxOut, Tail: p.tail}))
	if err != nil {
		return err
	}
	if res.Msg.GetJob().GetVmId() != p.vmID {
		return fmt.Errorf("job %s does not belong to machine '%s'", p.jobID, p.machine)
	}
	p.absorb(res.Msg)
	return nil
}

// execAwait keeps polling while the job runs, the deadline has not passed and offset output has room left. Each
// poll waits for what is left of the budget, rounded up to whole seconds (waitSeconds caps it at MaxWait), so the
// call returns at most a second after the deadline. A remainder too small to wait for gets one snapshot poll.
func (s *Server) execAwait(ctx context.Context, p *execPoll, deadline time.Time) error {
	for p.last.GetJob().GetState() == vmdv1.JobState_JOB_STATE_RUNNING && p.room() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		wait := s.waitSeconds(int((remaining + time.Second - 1) / time.Second))
		if err := s.execPollOnce(ctx, p, wait); err != nil {
			return err
		}
		if wait == 0 {
			return nil
		}
	}
	return nil
}

// view builds the jobView of the poll's state.
func (p *execPoll) view(now time.Time) jobView {
	j := p.last.GetJob()
	state := j.GetState()
	v := jobView{
		JobID:        p.jobID,
		Done:         state != vmdv1.JobState_JOB_STATE_RUNNING,
		State:        stateName(state),
		Seconds:      jobSeconds(j, now),
		Stdout:       strings.ToValidUTF8(string(p.stdout), "�"),
		Stderr:       strings.ToValidUTF8(string(p.stderr), "�"),
		Truncated:    p.last.GetTruncated(),
		StdoutOffset: p.outOff,
		StderrOffset: p.errOff,
		More:         p.last.GetMore(),
		KilledReason: j.GetKilledReason(),
	}
	if j.ExitCode != nil {
		code := int(j.GetExitCode())
		v.ExitCode = &code
	}
	return v
}

func execRender(p *execPoll, now time.Time) (string, error) {
	b, err := json.Marshal(p.view(now))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// stateName is the lower-case JobState without its JOB_STATE_ prefix, e.g. "timed_out".
func stateName(s vmdv1.JobState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "JOB_STATE_"))
}

// jobSeconds is finished-started for a finished job and now-started for a running one.
func jobSeconds(j *vmdv1.Job, now time.Time) float64 {
	if j.GetStartedAt() == nil {
		return 0
	}
	end := now
	if j.GetState() != vmdv1.JobState_JOB_STATE_RUNNING {
		if j.GetFinishedAt() == nil {
			return 0
		}
		end = j.GetFinishedAt().AsTime()
	}
	d := end.Sub(j.GetStartedAt().AsTime()).Seconds()
	return math.Max(0, math.Round(d*1000)/1000)
}

// execClip shortens a command to at most n characters on one line, for the list output.
func execClip(cmd string, n int) string {
	cmd = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(cmd)
	if utf8.RuneCountInString(cmd) <= n {
		return cmd
	}
	r := []rune(cmd)
	return string(r[:n-3]) + "..."
}
