package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	// agent_exec_list reads at most execListMaxPages pages of execListPageSize jobs. vmd pages oldest first and has
	// no reverse order, so the newest jobs are only reached by reading every page up to the bound.
	execListPageSize = 200 // vmd's largest page
	execListMaxPages = 10
	// execCancelWait is how long agent_exec_cancel waits for the job to end (vmd's waitOutput bound).
	execCancelWait = 3
	// execJobIDPattern is the shape of vmd job IDs: "job_" and a lower-case ULID, optionally with a "-s<shard>" suffix.
	execJobIDPattern = `^job_[0-9a-hjkmnp-tv-z]{26}(-s[0-9a-f]{2})?$`
)

type execArgs struct {
	Name        string `json:"name" jsonschema:"name of the machine (VM) to run the command on"`
	Command     string `json:"command" jsonschema:"shell command, run with bash -lc as the agent user inside the machine. stdin is /dev/null (no input, no TTY). At most 256 KiB"`
	Timeout     *int   `json:"timeout,omitempty" jsonschema:"seconds the command may run in total before it is killed (1 to 604800, default 3600). A kill ends the job as state timed_out with exit_code 124"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"seconds this call waits for the command to finish (0 returns at once with a snapshot; default 45). Values above the server maximum wait are cut to it. The command keeps running after the wait ends"`
	MaxOutput   *int   `json:"max_output,omitempty" jsonschema:"maximum bytes returned per stream (stdout and stderr), 1 to 65536, default 8192; the newest bytes are returned"`
	Workdir     string `json:"workdir,omitempty" jsonschema:"directory to run in, created with mkdir -p if missing. Use an absolute path: a relative path is taken from the agent user's home, and ~ is not expanded. Default: the agent user's home directory"`
	Project     string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
}

type execPollArgs struct {
	Name         string `json:"name" jsonschema:"name of the machine the job runs on"`
	JobID        string `json:"job_id" jsonschema:"job_id returned by agent_exec, agent_fetch, agent_ssh_pubkey or agent_exec_list (any job on the machine)"`
	WaitSeconds  *int   `json:"wait_seconds,omitempty" jsonschema:"seconds this call waits for the job to finish (0 returns a snapshot at once; default 45). Values above the server maximum wait are cut to it"`
	StdoutOffset int64  `json:"stdout_offset,omitempty" jsonschema:"stdout position to read from: the stdout_offset of the previous reply, 0 or more (default 0). Both offsets 0 selects tail mode"`
	StderrOffset int64  `json:"stderr_offset,omitempty" jsonschema:"stderr position to read from: the stderr_offset of the previous reply, 0 or more (default 0)"`
	MaxOutput    *int   `json:"max_output,omitempty" jsonschema:"maximum bytes returned per stream, 1 to 65536 (default 8192). In offset mode one call returns at most this many bytes of each stream in total across its internal polls, so a stream can get fewer bytes when the other is longer"`
	Tail         *bool  `json:"tail,omitempty" jsonschema:"true returns the last max_output bytes of each stream; false reads from the offsets. Default: true when both offsets are 0, else false"`
	Project      string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
}

type execCancelArgs struct {
	Name    string `json:"name" jsonschema:"name of the machine the job runs on"`
	JobID   string `json:"job_id" jsonschema:"job_id of the job to cancel. A job that already finished is accepted and its final state is returned"`
	Project string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
}

type execListArgs struct {
	Name    string `json:"name" jsonschema:"name of the machine whose jobs to list"`
	Project string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
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

// registerExecTools registers the job tools. Waits and caps in the text come from the server's MaxWait, and the
// schemas state the bounds the handlers enforce, so the SDK refuses out-of-range arguments before a handler runs.
func (s *Server) registerExecTools() {
	maxWait := s.maxWaitText()
	jobID := func(p map[string]*jsonschema.Schema) { schPattern(schProp(p, "job_id"), execJobIDPattern) }
	machine := func(p map[string]*jsonschema.Schema) { schMinLen(schProp(p, "name"), 1) }

	addToolWithSchema(s, "agent_exec",
		"Run a shell command (bash -lc) as the agent user inside machine `name`. The command keeps running after the call "+
			"returns, also across MCP restarts. Waits up to wait_seconds (default 45 s, cut to the server maximum wait of "+maxWait+") "+
			"for the command to finish. Reply: one JSON object with job_id; done (false while the command runs); state (running, succeeded, "+
			"failed (non-zero exit), cancelled, timed_out or lost (the machine stopped or the process vanished)); exit_code (null if none; "+
			"130 after cancel, 124 after timeout); seconds (run time); "+
			"stdout and stderr (UTF-8, the newest max_output bytes of each); truncated (true when older output was dropped); "+
			"stdout_offset and stderr_offset (where the next read starts); more (output remains unread); "+
			"killed_reason (only when the system ended the job). If done is false, call agent_exec_poll with the job_id and the offsets. "+
			"timeout limits the total run time (default 3600 s). Not safe to retry: after a failed call, check agent_exec_list first. "+
			"Errors: failed_precondition (machine stopped); not_found (no machine). A failure inside the machine (for example a "+
			"workdir that cannot be created) reads `unavailable: node <id> is unavailable`, not an outage.",
		toolInputSchema[execArgs](func(p map[string]*jsonschema.Schema) {
			machine(p)
			schMinLen(schProp(p, "command"), 1)
			schRange(schProp(p, "timeout"), 1, execMaxTimeout)
			schMin(schProp(p, "wait_seconds"), 0)
			schDescribe(schProp(p, "wait_seconds"), fmt.Sprintf("seconds this call waits for the command to finish (0 returns at once with a snapshot; default %d; values above the server maximum wait of %s are cut to it). The command keeps running after the wait ends", execDefaultWait, maxWait))
			schRange(schProp(p, "max_output"), 1, execMaxOutput)
		}),
		func(ctx context.Context, in execArgs) (string, error) { return s.execStart(ctx, in) })

	addToolWithSchema(s, "agent_exec_poll",
		"Read a job on machine `name` and wait up to wait_seconds (default 45 s, cut to the server maximum wait of "+maxWait+") for it to "+
			"finish. The job can be one started by agent_exec, agent_fetch or agent_ssh_pubkey, or any other job on the machine. Reply: the same "+
			"JSON object as agent_exec. To continue a job that did not finish, call again with the job_id and the stdout_offset and "+
			"stderr_offset of the last reply. Keep calling while more is true, even after done is true: in offset mode one call returns at most "+
			"max_output bytes per stream, so output can remain after the job ends. Tail mode (the default when both offsets are 0) returns the "+
			"last max_output bytes of each stream; offset mode returns the output after the given offsets. The job keeps running whether or not "+
			"you poll. The machine keeps the output of only the 50 most recently active jobs: for an older job the reply can have empty "+
			"stdout and stderr (its state is final) or state lost with killed_reason 'the job's files are gone from the VM'. "+
			"Errors: a job of another machine is refused with 'does not belong to machine'.",
		toolInputSchema[execPollArgs](func(p map[string]*jsonschema.Schema) {
			machine(p)
			jobID(p)
			schMin(schProp(p, "wait_seconds"), 0)
			schDescribe(schProp(p, "wait_seconds"), fmt.Sprintf("seconds this call waits for the job to finish (0 returns a snapshot at once; default %d; values above the server maximum wait of %s are cut to it)", execDefaultWait, maxWait))
			schMin(schProp(p, "stdout_offset"), 0)
			schMin(schProp(p, "stderr_offset"), 0)
			schRange(schProp(p, "max_output"), 1, execMaxOutput)
		}),
		func(ctx context.Context, in execPollArgs) (string, error) { return s.execPollTool(ctx, in) })

	addToolWithSchema(s, "agent_exec_cancel",
		"Cancel a job on machine `name`: the job's process group gets SIGTERM and then SIGKILL after about 2 s, and the job ends with state "+
			"cancelled and exit_code 130. Reply: the agent_exec JSON object, after waiting up to about "+fmt.Sprint(execCancelWait)+" s for the job "+
			"to end. If done is still false, call agent_exec_poll with the job_id. Output in the reply: the last 8 KiB of each stream when the job "+
			"was running, nothing when it had already finished. A job that already finished is not changed. If the machine is no longer running, "+
			"an unfinished job is reported as lost instead of cancelled. Errors: a job that is not a job of machine `name` (or does not exist) "+
			"is refused with 'does not belong to machine', and nothing is cancelled.",
		toolInputSchema[execCancelArgs](func(p map[string]*jsonschema.Schema) {
			machine(p)
			jobID(p)
		}),
		func(ctx context.Context, in execCancelArgs) (string, error) { return s.execCancelTool(ctx, in) })

	addToolWithSchema(s, "agent_exec_list",
		fmt.Sprintf("List the jobs of machine `name`, newest first, one line each: `job_id state exit=<code or -> <seconds>s <command>`. "+
			"The command is clipped to 80 characters and line breaks are shown as spaces. vmd pages jobs oldest first, so this tool reads up to "+
			"%d pages of %d jobs and lists the newest %d of them, newest first. If the machine has more jobs than that, the last line says so and "+
			"the newest jobs are not listed. state is the one last recorded: a job that finished but was never polled still shows running, so poll "+
			"its job_id to refresh it. Prints (no jobs) when there are none.", execListMaxPages, execListPageSize, execPageSize),
		toolInputSchema[execListArgs](func(p map[string]*jsonschema.Schema) { machine(p) }),
		func(ctx context.Context, in execListArgs) (string, error) { return s.execListTool(ctx, in) })
}

// maxWaitText is the server's maximum wait for one tool call, as text for descriptions, e.g. "40 s".
func (s *Server) maxWaitText() string {
	return strconv.FormatFloat(s.cfg.MaxWait.Seconds(), 'f', -1, 64) + " s"
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
	// vmd pages the jobs oldest first (ascending ID, which is creation order), so the newest are read last. Read up
	// to execListMaxPages pages, then show the last execPageSize jobs read, newest first.
	var jobs []*vmdv1.Job
	token := ""
	more := false
	for page := 1; ; page++ {
		res, err := s.cfg.Clients.Exec.ListJobs(ctx, connect.NewRequest(&vmdv1.ListJobsRequest{
			VmId: vm.GetId(), PageSize: execListPageSize, PageToken: token}))
		if err != nil {
			return "", err
		}
		jobs = append(jobs, res.Msg.GetJobs()...)
		token = res.Msg.GetNextPageToken()
		if token == "" {
			break
		}
		if page == execListMaxPages {
			more = true
			break
		}
	}
	if len(jobs) == 0 && !more {
		return "(no jobs)", nil
	}
	newest := jobs
	if len(newest) > execPageSize {
		newest = newest[len(newest)-execPageSize:]
	}
	now := time.Now()
	lines := make([]string, 0, len(newest)+1)
	for i := len(newest) - 1; i >= 0; i-- {
		j := newest[i]
		exit := "-"
		if j.ExitCode != nil {
			exit = strconv.Itoa(int(j.GetExitCode()))
		}
		lines = append(lines, fmt.Sprintf("%s %s exit=%s %.1fs %s", j.GetId(), stateName(j.GetState()), exit,
			jobSeconds(j, now), execClip(j.GetCommand(), execCommandClip)))
	}
	if more {
		lines = append(lines, fmt.Sprintf("(more than %d jobs on this machine: only the oldest %d were read, so the newest jobs may be missing)",
			execListMaxPages*execListPageSize, execListMaxPages*execListPageSize))
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

// toolInputSchema is the input schema the SDK infers for In, with edit applied to its properties. Constraints and
// descriptions that depend on the server's configuration are set in edit, at registration time.
func toolInputSchema[In any](edit func(props map[string]*jsonschema.Schema)) *jsonschema.Schema {
	sch, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("tool input schema: %v", err))
	}
	if edit != nil {
		edit(sch.Properties)
	}
	return sch
}

// addToolWithSchema is addTool with an explicit input schema. The SDK checks every call against the schema before
// the handler runs, so a violated bound is reported by the SDK and names the property.
func addToolWithSchema[In any](s *Server, name, description string, sch *jsonschema.Schema, h func(ctx context.Context, in In) (string, error)) {
	mcp.AddTool(s.mcp, &mcp.Tool{Name: name, Description: description, InputSchema: sch},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			out, err := h(ctx, in)
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: errText(err)}}}, nil, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil, nil
		})
}

// schProp returns the schema of one property. An unknown name is a registration bug, so it panics (the tests
// register every tool).
func schProp(props map[string]*jsonschema.Schema, name string) *jsonschema.Schema {
	p := props[name]
	if p == nil {
		panic("tool schema has no property " + name)
	}
	return p
}

// schRange bounds a numeric property to lo..hi, inclusive.
func schRange(p *jsonschema.Schema, lo, hi float64) {
	schMin(p, lo)
	p.Maximum = &hi
}

// schMin sets the smallest allowed value of a numeric property.
func schMin(p *jsonschema.Schema, lo float64) { p.Minimum = &lo }

// schMinLen sets the smallest allowed length of a string property.
func schMinLen(p *jsonschema.Schema, n int) { p.MinLength = &n }

// schPattern sets the regular expression a string property must match.
func schPattern(p *jsonschema.Schema, re string) { p.Pattern = re }

// schEnum restricts a string property to the given values.
func schEnum(p *jsonschema.Schema, vals ...string) {
	p.Enum = nil
	for _, v := range vals {
		p.Enum = append(p.Enum, v)
	}
}

// schDescribe replaces the description of a property.
func schDescribe(p *jsonschema.Schema, text string) { p.Description = text }
