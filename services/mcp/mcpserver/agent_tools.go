package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/protobuf/proto"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1"
)

const (
	agentDefaultMaxSteps = 40
	agentDefaultWait     = 30 // seconds agent_status waits when wait_seconds is not given
	agentListPageSize    = 200
	agentStepPageSize    = 500 // agentd's maximum for ListSteps
	agentTailSteps       = 10  // commands shown by agent_status when max_commands is not given
	agentCommandMaxRunes = 200
	agentHeaderCommand   = 120 // characters of the last command in the status header
	agentDefaultMaxMin   = 0   // max_minutes: no time limit
	agentDefaultCallTok  = 32768
	agentCallsShown      = 5 // model calls listed by agent_status
	agentAnswerMaxRunes  = 600
	agentReasonMaxRunes  = 300
)

type agentModelsArgs struct{}

// agentModelInfo is one entry of agent_models: a model as the caller needs to choose it.
type agentModelInfo struct {
	Name     string  `json:"name"`
	SizeGiB  float64 `json:"size_gib"`
	Params   string  `json:"params"`
	Quant    string  `json:"quant"`
	FitsVRAM bool    `json:"fits_vram"`
	Context  *int64  `json:"context"` // null when the backend does not report it
}

// machineNamePattern is the rule for a machine name, which is also the name of its agent.
const machineNamePattern = `^[a-z]([a-z0-9-]{0,29}[a-z0-9])?$`

// int32Max is the largest value agentd's int32 fields can carry.
const int32Max = math.MaxInt32

type agentTaskArgs struct {
	Name          string `json:"name" jsonschema:"name of the machine (VM) the agent works on: 1 to 31 characters, lowercase letters, digits and hyphens, starting with a letter and not ending in a hyphen. The agent is created with this name"`
	Task          string `json:"task" jsonschema:"what the agent must do, in plain language; required and not blank"`
	Model         string `json:"model" jsonschema:"required: the model to run, one of the names from agent_models. There is no default. Pick one with fits_vram true, or it runs partly on the CPU"`
	CheckCommand  string `json:"check_command,omitempty" jsonschema:"shell command run in the machine, in the agent's working directory, after each batch of commands (unless check_each_step is false) and at the end. Exit status 0 means the goal was met and a pass ends the task at once. Killed after 5 minutes. Omit it for no check"`
	MaxSteps      int    `json:"max_steps,omitempty" jsonschema:"maximum number of commands the agent may run; each apply_patch counts as one. Default 40; 0 also means 40; values above 200 are cut to 200"`
	MaxMinutes    int    `json:"max_minutes,omitempty" jsonschema:"stop the task after this many minutes, counted from when it starts running. A running command is stopped and the task ends cancelled with reason timeout. Default 0: no time limit"`
	MaxCallTokens *int   `json:"max_call_tokens,omitempty" jsonschema:"token cap for one model reply. Default 32768; 0 = no cap. A reply that reaches the cap, or repeats itself, is discarded and the model is asked to wrap up"`
	CheckEachStep *bool  `json:"check_each_step,omitempty" jsonschema:"run check_command after every batch of commands and end the task as soon as it passes. Default true; false runs the check only at the end"`
	Project       string `json:"project,omitempty" jsonschema:"project of the machine. Default: the server's default project"`
}

type agentStatusArgs struct {
	Name        string `json:"name" jsonschema:"name of the machine whose agent to check: 1 to 31 characters, lowercase letters, digits and hyphens, starting with a letter and not ending in a hyphen"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"seconds to wait while the newest task is queued or running; the call returns early when its state changes. Default 30; 0 returns at once; capped at the server's max wait (40 s by default)"`
	SinceStep   *int   `json:"since_step,omitempty" jsonschema:"poll cursor: the header step value of the previous reply (0 for the first poll). Only steps after it are listed, as [N] $ command lines, with no timing line and no live text. Omit it for the latest commands"`
	MaxCommands *int   `json:"max_commands,omitempty" jsonschema:"how many commands to list: the newest ones. Default 10; 0 lists none. In since_step mode a line says how many older new steps were not shown"`
	Brief       bool   `json:"brief,omitempty" jsonschema:"true: only the progress header while the task runs; the header plus the RESULT section once it has ended"`
	Project     string `json:"project,omitempty" jsonschema:"project of the machine. Default: the server's default project"`
}

type agentCancelArgs struct {
	Name    string `json:"name" jsonschema:"name of the machine whose unfinished agent tasks to cancel: 1 to 31 characters, lowercase letters, digits and hyphens, starting with a letter and not ending in a hyphen"`
	Project string `json:"project,omitempty" jsonschema:"project of the machine. Default: the server's default project"`
}

func (s *Server) registerAgentTools() {
	addTool(s, "agent_models",
		"List the LLM models installed on the agent backend. Takes no arguments. The reply is a JSON array, [] when no model "+
			"is installed, of objects with: name (pass it as agent_task model); size_gib (weights size in GiB, rounded to one "+
			"decimal); params (parameter count as the backend reports it, may be empty); quant (quantisation, may be empty); "+
			"fits_vram (true when size_gib is at most the agent backend's GPU memory budget less 0.5 GiB; the budget is 16 GiB "+
			"by default; a rough check that ignores the KV cache and runtime memory); context (context length in tokens, or "+
			"null when the backend does not report it). There is no default model. A model with fits_vram false probably runs "+
			"partly on the CPU and is much slower. A backend that cannot be reached fails the call with a tool error such as "+
			"'unavailable: ...'.",
		s.agentModels)
	addAgentTool(s, "agent_task",
		"Give the LLM agent on a machine a task and queue it. The agent is created on first use, named after the machine. "+
			"If it exists with another model, the agent is switched to model at once; a task that is already running keeps "+
			"the model it started with. The call does not wait for the task: it returns at once, and a worker claims the task "+
			"when one is free. The reply is 'task <id> queued with model <model>', followed by '; N unfinished task(s) of this "+
			"agent existed at creation' when other tasks were queued or running, and then a 'warning: model ...' line when the "+
			"model does not fit in GPU memory. Follow the task with agent_status. Up to two tasks of one agent run at the same "+
			"time (agentd's default), so a task can run beside another one in the same machine and working directory. The "+
			"machine must be running; a task for a stopped machine ends in state error ('vm is stopped'). Calling again queues "+
			"another task; there is no retry protection. It fails with failed_precondition when the agent is not active (for "+
			"example orphaned) or the machine is being deleted. agent_cancel stops all unfinished tasks of the machine's agents. "+
			"With check_command the check decides the result: it runs after each batch of commands (unless check_each_step is "+
			"false) and at the end, and a pass ends the task at once as done. Without check_command a model SUCCESS is reported "+
			"as done_unverified. Limits: max_steps caps the commands (default 40); at the cap the task ends with reason "+
			"step_limit. max_minutes (default 0, no limit) ends the task cancelled with reason timeout. max_call_tokens (default "+
			"32768, 0 = no cap) caps one model reply; a reply that reaches the cap or repeats itself is discarded and the model "+
			"is asked to wrap up, and the third such reply ends the task with reason token_limit or repetition. Those reasons "+
			"end the task as gave_up unless the check passes. Refused with no change made: a missing or blank task, name or "+
			"model, a model that is not installed, a machine that does not exist, and negative max_steps, max_minutes or "+
			"max_call_tokens.",
		s.agentTask, constrainAgentTask)
	addAgentTool(s, "agent_status",
		"Show the newest task of the agent on a machine (highest task ID). An older task that still runs is not shown while "+
			"a newer one exists. While that task is queued or running, the call waits up to wait_seconds (default 30; capped by "+
			"the server's max wait, 40 s by default) and returns early when the state changes. Output, one item per line: "+
			"(1) 'status: <state>  (<N> commands run, model <model>[, verdict: <V>][, verdict_source: <check|model>][, reason: <r>])'. "+
			"model is the agent's current model, which can differ from the model the task started with. verdict is the model's "+
			"own VERDICT line (SUCCESS or FAILED). reason is one of repetition, token_limit, step_limit, timeout or 'check passed'. "+
			"(2) 'header: {json}' with the keys status, step (the since_step cursor), max_steps, elapsed_s, prompt_tokens, "+
			"gen_tokens, calls, first_command_s (null until the first command), verdict_source, reason, last_command (clipped to "+
			"120 characters, null when none). (3) 'timing: ...' with created, started and finished times in UTC and the elapsed "+
			"seconds; a stamp that is not set is left out. (4) 'tokens: prompt P, generated G over C model calls', with a note on "+
			"aborted calls, and 'last calls: #i Ns N tok [partial] [end]; ...' listing up to the latest 5 model calls, when the "+
			"task has made a model call. (5) The newest max_commands commands (default 10), oldest first, as '$ <command>' lines, "+
			"clipped to 200 characters with newlines shown as spaces; their output is not shown. (6) While a model call streams "+
			"and the task runs: '[model answer so far: ...]' (last 600 characters) and '[model reasoning (raw, may be noise): ...]' "+
			"(last 300 characters). Reasoning is not kept after the call. (7) A blank line, '=== RESULT ===' and the model's "+
			"final report, capped at 32 KiB (the head is kept, then '...[truncated]'). It ends with '[check_command exit N]' when "+
			"a check ran (the check's end state replaces 'exit N' when it did not exit). It reads '(no result yet)' while the task runs and '(none)' when there is none. "+
			"States: running (queued or running; a queued task shows step 0 and no started stamp); done (check_command passed); done_unverified (model SUCCESS, no check_command); "+
			"failed (check_command failed, or no check and model FAILED); gave_up (a limit ended the task and no check passed, or "+
			"no check and no VERDICT); cancelled (agent_cancel, or max_minutes ran out); error (infrastructure or protocol failure: "+
			"the VM is not running or was deleted, a VM command failed, the model backend failed, or more than 3 unusable tool calls in a row). "+
			"A done_unverified task has no reason. A cancel request that has not been acted on is not shown, and a running task "+
			"stays running if its worker died. "+
			"brief=true returns the header only while the task runs, and the header plus the RESULT section once it has ended. "+
			"since_step=N lists only the steps after N (N is the header step value of the previous reply; 0 for the first poll), as "+
			"'[N] $ <command>' lines, with no timing line and no live text, and the RESULT section only once the task has ended. "+
			"If more than max_commands steps are newer, only the newest are listed and the line '(N earlier steps after since_step "+
			"not shown; raise max_commands)' is added. For a poll that should return at once, pass wait_seconds 0. max_commands 0 "+
			"lists no commands. Errors: 'no agent/task yet on machine '<name>' (use agent_task)' when the machine has no task, and "+
			"'not_found: no machine named ...' for an unknown machine.",
		s.agentStatus, constrainAgentStatus)
	addAgentTool(s, "agent_cancel",
		"Cancel the unfinished tasks of the agents on a machine: every task that is queued or running, not only the newest one. "+
			"A queued task is cancelled at once. A running task is flagged and stops within about a second, and its running "+
			"command is stopped; it then shows state cancelled. Finished tasks are not changed. The reply is 'cancel requested "+
			"for N task(s): <task IDs, comma-separated>', where N is the number of tasks asked to stop, or 'no running task' when "+
			"there is none, including when the machine has no agent. A failed cancel is an error that also lists the tasks "+
			"cancelled before it. An unknown machine fails with 'not_found: no machine named ...'.",
		s.agentCancel, constrainAgentCancel)
}

// addAgentTool registers a tool like addTool, with an input schema derived from In. constrain adds the bounds and
// patterns that the SDK checks before the handler runs; a call that breaks one fails with a validation error that
// names the property.
func addAgentTool[In any](s *Server, name, description string, h func(ctx context.Context, in In) (string, error),
	constrain func(props map[string]*jsonschema.Schema)) {
	addToolSchema(s, name, description, func(sch *jsonschema.Schema) { constrain(sch.Properties) }, h)
}

// setName applies the machine name rule to the name property.
func setName(props map[string]*jsonschema.Schema) {
	props["name"].Pattern = machineNamePattern
}

// atLeast sets the lower bound of an integer property.
func atLeast(p *jsonschema.Schema, lo float64) { p.Minimum = &lo }

// atMostInt32 sets the upper bound of an integer property to the int32 range agentd takes.
func atMostInt32(p *jsonschema.Schema) {
	hi := float64(int32Max)
	p.Maximum = &hi
}

func constrainAgentTask(props map[string]*jsonschema.Schema) {
	setName(props)
	minLen := 1
	props["model"].MinLength = &minLen
	atLeast(props["max_steps"], 0)
	atMostInt32(props["max_steps"])
	atLeast(props["max_minutes"], 0)
	atMostInt32(props["max_minutes"])
	atLeast(props["max_call_tokens"], 0)
	atMostInt32(props["max_call_tokens"])
}

func constrainAgentStatus(props map[string]*jsonschema.Schema) {
	setName(props)
	atLeast(props["wait_seconds"], 0)
	atLeast(props["since_step"], 0)
	atMostInt32(props["since_step"])
	atLeast(props["max_commands"], 0)
}

func constrainAgentCancel(props map[string]*jsonschema.Schema) {
	setName(props)
}

// agentModels lists the installed models as a JSON list.
func (s *Server) agentModels(ctx context.Context, _ agentModelsArgs) (string, error) {
	models, err := s.installedModels(ctx)
	if err != nil {
		return "", err
	}
	list := make([]agentModelInfo, 0, len(models))
	for _, m := range models {
		list = append(list, modelInfo(m))
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return "", fmt.Errorf("encode models: %w", err)
	}
	return string(raw), nil
}

// modelInfo converts a model of agentd's list to the agent_models form.
func modelInfo(m *agentdv1.Model) agentModelInfo {
	size := m.GetSizeGib()
	if size == 0 {
		size = float64(m.GetSizeBytes()) / (1 << 30) // an agentd that does not send size_gib
	}
	info := agentModelInfo{
		Name: m.GetName(), SizeGiB: round1(size), Params: m.GetParams(),
		Quant: m.GetQuant(), FitsVRAM: m.GetFitsVram(),
	}
	if c := m.GetContextLength(); c > 0 {
		info.Context = &c
	}
	return info
}

// agentTask validates the request, makes sure the machine's agent exists with the requested model, and queues
// the task. Nothing is created or changed when validation fails.
func (s *Server) agentTask(ctx context.Context, in agentTaskArgs) (string, error) {
	if strings.TrimSpace(in.Name) == "" {
		return "", fmt.Errorf("name is required: the machine the agent works on")
	}
	if strings.TrimSpace(in.Task) == "" {
		return "", fmt.Errorf("task is required: say what the agent must do")
	}
	if in.MaxSteps < 0 {
		return "", fmt.Errorf("max_steps must not be negative (default %d)", agentDefaultMaxSteps)
	}
	if in.MaxSteps == 0 {
		in.MaxSteps = agentDefaultMaxSteps
	}
	if in.MaxMinutes < 0 {
		return "", fmt.Errorf("max_minutes must not be negative (0 means no time limit)")
	}
	if in.MaxCallTokens != nil && *in.MaxCallTokens < 0 {
		return "", fmt.Errorf("max_call_tokens must not be negative (0 means no cap)")
	}
	vm, err := s.vmByName(ctx, in.Project, in.Name)
	if err != nil {
		return "", err
	}
	models, err := s.installedModels(ctx)
	if err != nil {
		return "", err
	}
	if in.Model == "" {
		return "", fmt.Errorf("model is required: there is no default, pick one of the installed models. Installed: %s",
			modelNames(models))
	}
	chosen, ok := findModel(models, in.Model)
	if !ok {
		return "", fmt.Errorf("model '%s' is not installed. Installed: %s", in.Model, modelNames(models))
	}

	project := s.project(in.Project)
	agent, err := s.findAgent(ctx, project, vm.GetId(), in.Name)
	if err != nil {
		return "", err
	}
	switch {
	case agent == nil:
		res, err := s.cfg.Clients.Agents.CreateAgent(ctx, connect.NewRequest(&agentdv1.CreateAgentRequest{
			ProjectId:      project,
			Name:           in.Name,
			VmId:           vm.GetId(),
			Model:          in.Model,
			IdempotencyKey: "mcp-agent-" + vm.GetId(),
		}))
		if err != nil {
			return "", err
		}
		agent = res.Msg
	case agent.GetModel() != in.Model:
		model := in.Model
		if _, err := s.cfg.Clients.Agents.UpdateAgent(ctx, connect.NewRequest(&agentdv1.UpdateAgentRequest{
			Id: agent.GetId(), Model: &model,
		})); err != nil {
			return "", err
		}
	}

	ahead := s.tasksAhead(ctx, agent.GetId())
	req := &agentdv1.CreateTaskRequest{
		AgentId:       agent.GetId(),
		Prompt:        in.Task,
		CheckCommand:  in.CheckCommand,
		MaxSteps:      int32(in.MaxSteps),
		MaxMinutes:    int32(in.MaxMinutes),
		MaxCallTokens: proto.Int32(agentDefaultCallTok),
	}
	if in.MaxCallTokens != nil {
		req.MaxCallTokens = proto.Int32(int32(*in.MaxCallTokens))
	}
	if in.CheckEachStep != nil {
		req.CheckEachStep = proto.Bool(*in.CheckEachStep)
	}
	res, err := s.cfg.Clients.Tasks.CreateTask(ctx, connect.NewRequest(req))
	if err != nil {
		return "", err
	}
	var lines []string
	if ahead > 0 {
		lines = append(lines, fmt.Sprintf("task %s queued with model %s; %d unfinished task(s) of this agent existed at creation",
			res.Msg.GetId(), in.Model, ahead))
	} else {
		lines = append(lines, fmt.Sprintf("task %s queued with model %s", res.Msg.GetId(), in.Model))
	}
	if !chosen.GetFitsVram() {
		lines = append(lines, fmt.Sprintf("warning: model %s (%.1f GiB) exceeds the VRAM budget, so it will probably "+
			"run partly on the CPU and be slow", in.Model, chosen.GetSizeGib()))
	}
	return strings.Join(lines, "\n"), nil
}

// tasksAhead counts the agent's tasks that are queued or running when a new task is created, for the reply.
// It is best effort: on a listing error it reports 0.
func (s *Server) tasksAhead(ctx context.Context, agentID string) int {
	live, err := s.liveTasks(ctx, agentID)
	if err != nil {
		return 0
	}
	return len(live)
}

// liveTasks returns the agent's tasks that are queued or running, oldest first. It reads every page.
func (s *Server) liveTasks(ctx context.Context, agentID string) ([]*agentdv1.Task, error) {
	var live []*agentdv1.Task
	for _, st := range []agentdv1.TaskState{agentdv1.TaskState_TASK_STATE_QUEUED, agentdv1.TaskState_TASK_STATE_RUNNING} {
		token := ""
		for {
			res, err := s.cfg.Clients.Tasks.ListTasks(ctx, connect.NewRequest(&agentdv1.ListTasksRequest{
				AgentId: agentID, State: st, PageSize: agentListPageSize, PageToken: token}))
			if err != nil {
				return nil, err
			}
			live = append(live, res.Msg.GetTasks()...)
			token = res.Msg.GetNextPageToken()
			if token == "" {
				break
			}
		}
	}
	slices.SortFunc(live, func(a, b *agentdv1.Task) int { return strings.Compare(a.GetId(), b.GetId()) })
	return live, nil
}

// agentStatus reports the latest task of the machine's agent, waiting while it is queued or running.
func (s *Server) agentStatus(ctx context.Context, in agentStatusArgs) (string, error) {
	// The whole call, including the lookups, is bounded by the wait budget.
	wait := agentDefaultWait
	if in.WaitSeconds != nil {
		wait = *in.WaitSeconds
	}
	maxCommands := agentTailSteps
	if in.MaxCommands != nil {
		if *in.MaxCommands < 0 {
			return "", fmt.Errorf("max_commands must not be negative (0 lists none)")
		}
		maxCommands = *in.MaxCommands
	}
	budget := min(max(wait, 0), int(s.cfg.MaxWait/time.Second))
	deadline := time.Now().Add(time.Duration(budget) * time.Second)
	ctx, cancel := context.WithTimeout(ctx, s.cfg.MaxWait)
	defer cancel()

	vm, err := s.vmByName(ctx, in.Project, in.Name)
	if err != nil {
		return "", err
	}
	agent, task, err := s.agentAndLatestTask(ctx, s.project(in.Project), vm.GetId(), in.Name)
	if err != nil {
		return "", err
	}
	if agent == nil || task == nil {
		return "", noAgentTaskError(in.Name)
	}

	waitCtx, cancelWait := context.WithDeadline(ctx, deadline)
	defer cancelWait()
	for isLive(task.GetState()) {
		secs := s.waitSeconds(int(time.Until(deadline) / time.Second))
		if secs <= 0 {
			break
		}
		res, err := s.cfg.Clients.Tasks.GetTask(waitCtx, connect.NewRequest(&agentdv1.GetTaskRequest{
			Id:          task.GetId(),
			WaitForNot:  task.GetState(),
			WaitSeconds: int32(secs),
		}))
		if err != nil {
			if waitCtx.Err() != nil {
				break // the wait budget is spent; report what is known
			}
			return "", err
		}
		task = res.Msg
	}

	view := statusView{brief: in.Brief}
	if in.SinceStep != nil {
		view.since = int32(max(*in.SinceStep, 0))
		view.hasSince = true
	}
	var steps []*agentdv1.Step
	switch {
	case in.Brief || maxCommands == 0:
	case view.hasSince:
		steps, view.dropped, err = s.tailSteps(ctx, task.GetId(), view.since, maxCommands)
	default:
		steps, _, err = s.tailSteps(ctx, task.GetId(), 0, maxCommands)
	}
	if err != nil {
		return "", err
	}
	last, err := s.lastCommand(ctx, task)
	if err != nil {
		return "", err
	}
	return renderAgentStatus(agent, task, steps, last, time.Now(), view), nil
}

// statusView says which parts of the status the caller asked for.
type statusView struct {
	brief    bool  // header only, plus the final report once the task ended
	hasSince bool  // poll mode: only commands after since
	since    int32 // the last step the caller has seen
	dropped  int   // poll mode: newer steps left out because only the newest max_commands are listed
}

// agentCancel cancels every unfinished task of the machine's agents: queued tasks at once, running tasks by a
// stop request. Agents of the machine are the ones agentd lists for its VM.
func (s *Server) agentCancel(ctx context.Context, in agentCancelArgs) (string, error) {
	vm, err := s.vmByName(ctx, in.Project, in.Name)
	if err != nil {
		return "", err
	}
	agents, err := s.vmAgents(ctx, s.project(in.Project), vm.GetId())
	if err != nil {
		return "", err
	}
	var live []*agentdv1.Task
	for _, a := range agents {
		tasks, err := s.liveTasks(ctx, a.GetId())
		if err != nil {
			return "", err
		}
		live = append(live, tasks...)
	}
	if len(live) == 0 {
		return "no running task", nil
	}
	slices.SortFunc(live, func(a, b *agentdv1.Task) int { return strings.Compare(a.GetId(), b.GetId()) })
	ids := make([]string, 0, len(live))
	for _, t := range live {
		if _, err := s.cfg.Clients.Tasks.CancelTask(ctx, connect.NewRequest(&agentdv1.CancelTaskRequest{Id: t.GetId()})); err != nil {
			return "", fmt.Errorf("cancelled %d of %d task(s) (%s) before task %s failed: %s",
				len(ids), len(live), strings.Join(ids, ", "), t.GetId(), errText(err))
		}
		ids = append(ids, t.GetId())
	}
	return fmt.Sprintf("cancel requested for %d task(s): %s", len(ids), strings.Join(ids, ", ")), nil
}

// vmAgents returns every agentd agent on the VM vmID.
func (s *Server) vmAgents(ctx context.Context, project, vmID string) ([]*agentdv1.Agent, error) {
	var all []*agentdv1.Agent
	token := ""
	for {
		res, err := s.cfg.Clients.Agents.ListAgents(ctx, connect.NewRequest(&agentdv1.ListAgentsRequest{
			ProjectId: project, VmId: vmID, PageSize: agentListPageSize, PageToken: token}))
		if err != nil {
			return nil, err
		}
		all = append(all, res.Msg.GetAgents()...)
		token = res.Msg.GetNextPageToken()
		if token == "" {
			return all, nil
		}
	}
}

// installedModels returns the models installed on the LLM backend.
func (s *Server) installedModels(ctx context.Context) ([]*agentdv1.Model, error) {
	res, err := s.cfg.Clients.Models.ListModels(ctx, connect.NewRequest(&agentdv1.ListModelsRequest{}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetModels(), nil
}

// findAgent returns the agent named name on the VM vmID, or nil when there is none.
func (s *Server) findAgent(ctx context.Context, project, vmID, name string) (*agentdv1.Agent, error) {
	token := ""
	for {
		res, err := s.cfg.Clients.Agents.ListAgents(ctx, connect.NewRequest(&agentdv1.ListAgentsRequest{
			ProjectId: project, VmId: vmID, PageSize: agentListPageSize, PageToken: token}))
		if err != nil {
			return nil, err
		}
		for _, a := range res.Msg.GetAgents() {
			if a.GetName() == name {
				return a, nil
			}
		}
		token = res.Msg.GetNextPageToken()
		if token == "" {
			return nil, nil
		}
	}
}

// agentAndLatestTask finds the machine's agent and the agent's latest task. Either may be nil.
func (s *Server) agentAndLatestTask(ctx context.Context, project, vmID, name string) (*agentdv1.Agent, *agentdv1.Task, error) {
	agent, err := s.findAgent(ctx, project, vmID, name)
	if err != nil || agent == nil {
		return agent, nil, err
	}
	task, err := s.latestTask(ctx, agent.GetId())
	return agent, task, err
}

// latestTask returns the newest task of an agent, or nil. agentd lists tasks oldest first (by ID, which sorts
// by creation time), so the newest one is the last row; the list is paged through to reach it.
func (s *Server) latestTask(ctx context.Context, agentID string) (*agentdv1.Task, error) {
	var last *agentdv1.Task
	token := ""
	for {
		res, err := s.cfg.Clients.Tasks.ListTasks(ctx, connect.NewRequest(&agentdv1.ListTasksRequest{
			AgentId: agentID, PageSize: agentListPageSize, PageToken: token}))
		if err != nil {
			return nil, err
		}
		if tasks := res.Msg.GetTasks(); len(tasks) > 0 {
			last = tasks[len(tasks)-1]
		}
		token = res.Msg.GetNextPageToken()
		if token == "" {
			return last, nil
		}
	}
}

// tailSteps returns the last n steps of a task that come after step number after (0 for all), and how many
// steps after it were left out. agentd pages forward only, so every page is read and only the last n are kept.
func (s *Server) tailSteps(ctx context.Context, taskID string, after int32, n int) ([]*agentdv1.Step, int, error) {
	var tail []*agentdv1.Step
	total := 0
	for {
		res, err := s.cfg.Clients.Tasks.ListSteps(ctx, connect.NewRequest(&agentdv1.ListStepsRequest{
			TaskId: taskID, AfterSeq: after, Limit: agentStepPageSize}))
		if err != nil {
			return nil, 0, err
		}
		page := res.Msg.GetSteps()
		total += len(page)
		tail = append(tail, page...)
		if len(tail) > n {
			tail = tail[len(tail)-n:]
		}
		if len(page) < agentStepPageSize {
			return tail, total - len(tail), nil
		}
		after = page[len(page)-1].GetSeq()
	}
}

// lastCommand returns the command of the task's newest step, or "" when it ran none.
func (s *Server) lastCommand(ctx context.Context, task *agentdv1.Task) (string, error) {
	if task.GetSteps() == 0 {
		return "", nil
	}
	res, err := s.cfg.Clients.Tasks.ListSteps(ctx, connect.NewRequest(&agentdv1.ListStepsRequest{
		TaskId: task.GetId(), AfterSeq: task.GetSteps() - 1, Limit: 1}))
	if err != nil {
		return "", err
	}
	if steps := res.Msg.GetSteps(); len(steps) > 0 {
		return steps[0].GetCommand(), nil
	}
	return "", nil
}

// renderAgentStatus builds the agent_status output. The live model text is shown only for a task that is still
// running, and never inside the result. In poll mode the output is kept short.
func renderAgentStatus(agent *agentdv1.Agent, task *agentdv1.Task, steps []*agentdv1.Step, last string, now time.Time, view statusView) string {
	live := isLive(task.GetState())
	var b strings.Builder
	if view.brief {
		b.WriteString(statusHeader(task, last, now))
		if !live {
			b.WriteString("\n\n=== RESULT ===\n" + resultText(task))
		}
		return b.String()
	}

	fmt.Fprintf(&b, "status: %s  (%d commands run, model %s", statusLabel(task.GetState()), task.GetSteps(), agent.GetModel())
	if v := task.GetVerdict(); v != "" {
		fmt.Fprintf(&b, ", verdict: %s", v)
	}
	if v := task.GetVerdictSource(); v != "" {
		fmt.Fprintf(&b, ", verdict_source: %s", v)
	}
	if r := task.GetReason(); r != "" {
		fmt.Fprintf(&b, ", reason: %s", r)
	}
	b.WriteString(")\n")
	b.WriteString(statusHeader(task, last, now) + "\n")
	if !view.hasSince {
		b.WriteString(timingLine(task, now) + "\n")
	}
	if task.GetModelCalls() > 0 {
		fmt.Fprintf(&b, "tokens: prompt %d, generated %d over %d model calls", task.GetPromptTokens(),
			task.GetCompletionTokens(), task.GetModelCalls())
		if task.GetPartialCalls() > 0 {
			fmt.Fprintf(&b, " (+%d aborted: their streamed chunks are counted in generated)", task.GetPartialCalls())
		}
		b.WriteString("\n")
		if calls := callsLine(task.GetCalls()); calls != "" {
			b.WriteString(calls + "\n")
		}
	}
	if view.hasSince && view.dropped > 0 {
		fmt.Fprintf(&b, "(%d earlier steps after since_step not shown; raise max_commands)\n", view.dropped)
	}
	for _, st := range steps {
		if view.hasSince {
			fmt.Fprintf(&b, "[%d] $ %s\n", st.GetSeq(), clipCommand(st.GetCommand()))
		} else {
			fmt.Fprintf(&b, "$ %s\n", clipCommand(st.GetCommand()))
		}
	}
	if live && !view.hasSince {
		if a := task.GetLiveAnswer(); a != "" {
			fmt.Fprintf(&b, "[model answer so far: ...%s]\n", lastChars(a, agentAnswerMaxRunes))
		}
		if r := task.GetLiveReasoning(); r != "" {
			fmt.Fprintf(&b, "[model reasoning (raw, may be noise): ...%s]\n", lastChars(r, agentReasonMaxRunes))
		}
	}
	if view.hasSince && !isLive(task.GetState()) {
		b.WriteString("\n=== RESULT ===\n" + resultText(task))
		return b.String()
	}
	if !view.hasSince {
		b.WriteString("\n=== RESULT ===\n" + resultText(task))
	}
	return strings.TrimRight(b.String(), "\n")
}

// statusHeader is the compact progress line of agent_status: a JSON object, always the same keys.
func statusHeader(task *agentdv1.Task, last string, now time.Time) string {
	h := struct {
		Status        string   `json:"status"`
		Step          int32    `json:"step"`
		MaxSteps      int32    `json:"max_steps"`
		ElapsedS      float64  `json:"elapsed_s"`
		PromptTokens  int64    `json:"prompt_tokens"`
		GenTokens     int64    `json:"gen_tokens"`
		Calls         int32    `json:"calls"`
		FirstCommandS *float64 `json:"first_command_s"`
		VerdictSource string   `json:"verdict_source"`
		Reason        string   `json:"reason"`
		LastCommand   *string  `json:"last_command"`
	}{
		Status: statusLabel(task.GetState()), Step: task.GetSteps(), MaxSteps: task.GetMaxSteps(),
		ElapsedS: round1(elapsed(task, now).Seconds()), PromptTokens: task.GetPromptTokens(),
		GenTokens: task.GetCompletionTokens(), Calls: task.GetModelCalls(),
		VerdictSource: task.GetVerdictSource(), Reason: task.GetReason(),
	}
	if f := task.GetFirstCommandSeconds(); f > 0 {
		h.FirstCommandS = &f
	}
	if last != "" {
		c := clipTo(strings.ReplaceAll(last, "\n", " "), agentHeaderCommand)
		h.LastCommand = &c
	}
	raw, _ := json.Marshal(h)
	return "header: " + string(raw)
}

// elapsed is the time from the task's creation to its end, or to now while it is live.
func elapsed(task *agentdv1.Task, now time.Time) time.Duration {
	created := now
	if c := task.GetCreatedAt(); c != nil {
		created = c.AsTime()
	}
	end := now
	if fin := task.GetFinishedAt(); fin != nil {
		end = fin.AsTime()
	}
	return end.Sub(created)
}

// callsLine lists the latest model calls: index, seconds, generated tokens, and how the call ended.
func callsLine(calls []*agentdv1.CallStat) string {
	if len(calls) == 0 {
		return ""
	}
	if len(calls) > agentCallsShown {
		calls = calls[len(calls)-agentCallsShown:]
	}
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		p := fmt.Sprintf("#%d %.1fs %d tok", c.GetIndex(), c.GetSeconds(), c.GetGenTokens())
		if c.GetPartial() {
			p += " partial"
		}
		if c.GetEnd() != "" && c.GetEnd() != "ok" {
			p += " " + c.GetEnd()
		}
		parts = append(parts, p)
	}
	return "last calls: " + strings.Join(parts, "; ")
}

// resultText is the final report of a task, or what stands in for it.
func resultText(task *agentdv1.Task) string {
	switch result := strings.TrimRight(task.GetResult(), "\n"); {
	case result != "":
		return result
	case isLive(task.GetState()):
		return "(no result yet)"
	default:
		return "(none)"
	}
}

// round1 rounds to one decimal place.
func round1(f float64) float64 { return math.Round(f*10) / 10 }

// clipTo returns s shortened to at most n characters, ending in "...".
func clipTo(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-3]) + "..."
}

// lastChars returns the last n characters of s.
func lastChars(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// timingLine renders created/started/finished times (UTC) and the elapsed time, to finish or to now.
func timingLine(task *agentdv1.Task, now time.Time) string {
	created := now
	parts := []string{"created " + hms(time.Time{})}
	if c := task.GetCreatedAt(); c != nil {
		created = c.AsTime()
		parts[0] = "created " + hms(created)
	}
	if st := task.GetStartedAt(); st != nil {
		parts = append(parts, "started "+hms(st.AsTime()))
	}
	end := now
	if fin := task.GetFinishedAt(); fin != nil {
		end = fin.AsTime()
		parts = append(parts, "finished "+hms(end))
	}
	return fmt.Sprintf("timing: %s, elapsed %.1fs", strings.Join(parts, ", "), end.Sub(created).Seconds())
}

// hms formats a time as HH:MM:SS in UTC; a zero time (no timestamp) is shown as dashes.
func hms(t time.Time) string {
	if t.IsZero() {
		return "--:--:--"
	}
	return t.UTC().Format("15:04:05")
}

// clipCommand returns the command on one line, at most agentCommandMaxRunes runes long.
func clipCommand(cmd string) string {
	cmd = strings.ReplaceAll(cmd, "\n", " ")
	r := []rune(cmd)
	if len(r) <= agentCommandMaxRunes {
		return cmd
	}
	return string(r[:agentCommandMaxRunes-3]) + "..."
}

// statusLabel maps a task state to the word agent_status shows.
func statusLabel(s agentdv1.TaskState) string {
	switch s {
	case agentdv1.TaskState_TASK_STATE_QUEUED, agentdv1.TaskState_TASK_STATE_RUNNING:
		return "running"
	case agentdv1.TaskState_TASK_STATE_SUCCEEDED:
		return "done"
	case agentdv1.TaskState_TASK_STATE_SUCCEEDED_UNVERIFIED:
		return "done_unverified"
	case agentdv1.TaskState_TASK_STATE_FAILED:
		return "failed"
	case agentdv1.TaskState_TASK_STATE_GAVE_UP:
		return "gave_up"
	case agentdv1.TaskState_TASK_STATE_CANCELLED:
		return "cancelled"
	case agentdv1.TaskState_TASK_STATE_ERROR:
		return "error"
	}
	return "unknown"
}

// isLive reports whether a task may still change state.
func isLive(s agentdv1.TaskState) bool {
	return s == agentdv1.TaskState_TASK_STATE_QUEUED || s == agentdv1.TaskState_TASK_STATE_RUNNING
}

// findModel returns the installed model with the given name.
func findModel(models []*agentdv1.Model, name string) (*agentdv1.Model, bool) {
	for _, m := range models {
		if m.GetName() == name {
			return m, true
		}
	}
	return nil, false
}

// modelNames lists the installed model names for an error message.
func modelNames(models []*agentdv1.Model) string {
	if len(models) == 0 {
		return "(none installed)"
	}
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.GetName())
	}
	return strings.Join(names, ", ")
}

func noAgentTaskError(name string) error {
	return fmt.Errorf("no agent/task yet on machine '%s' (use agent_task)", name)
}
