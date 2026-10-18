package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
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

type agentTaskArgs struct {
	Name          string `json:"name" jsonschema:"name of the machine (VM) the agent works on; the agent is created with this name"`
	Task          string `json:"task" jsonschema:"what the agent must do, in plain language; required"`
	Model         string `json:"model,omitempty" jsonschema:"REQUIRED: the model to run, one of the names from agent_models. There is no default; pick one that fits in GPU memory"`
	CheckCommand  string `json:"check_command,omitempty" jsonschema:"shell command run in the machine after the agent finishes; exit status 0 means the goal was met"`
	MaxSteps      int    `json:"max_steps,omitempty" jsonschema:"maximum number of commands the agent may run (default 40; agentd caps it at 200)"`
	MaxMinutes    int    `json:"max_minutes,omitempty" jsonschema:"stop the task after this many minutes, cancelling a running command (default 0: no time limit)"`
	MaxCallTokens *int   `json:"max_call_tokens,omitempty" jsonschema:"token cap for one model reply (default 32768; 0 = no cap). A reply that reaches it is discarded and the model is asked to wrap up"`
	CheckEachStep *bool  `json:"check_each_step,omitempty" jsonschema:"run check_command after every batch of commands and end the task as soon as it passes (default true)"`
	Project       string `json:"project,omitempty" jsonschema:"project of the machine (default: the server's default project)"`
}

type agentStatusArgs struct {
	Name        string `json:"name" jsonschema:"name of the machine whose agent to check"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"seconds to wait while the latest task is queued or running (default 30, capped by the server's limit of about 40 s); 0 returns at once"`
	SinceStep   *int   `json:"since_step,omitempty" jsonschema:"to poll cheaply: the last step number you have seen. Only newer commands are listed and the live model text is left out"`
	MaxCommands *int   `json:"max_commands,omitempty" jsonschema:"how many of the latest commands to list (default 10; 0 lists none)"`
	Brief       bool   `json:"brief,omitempty" jsonschema:"true returns only the progress header, plus the final report once the task has ended"`
	Project     string `json:"project,omitempty" jsonschema:"project of the machine (default: the server's default project)"`
}

type agentCancelArgs struct {
	Name    string `json:"name" jsonschema:"name of the machine whose running task to cancel"`
	Project string `json:"project,omitempty" jsonschema:"project of the machine (default: the server's default project)"`
}

func (s *Server) registerAgentTools() {
	addTool(s, "agent_models",
		"List the LLM models installed on the agent backend. The reply is a JSON list of objects with name, size_gib, "+
			"params (parameter count), quant (quantisation), fits_vram (whether the weights fit the GPU memory budget of "+
			"16 GiB less 0.5 GiB headroom; a rough check that ignores the KV cache) and context (context length, or null). "+
			"There is no default model: the caller must pick one for agent_task. A model with fits_vram=false probably "+
			"runs partly on the CPU and is much slower.",
		s.agentModels)
	addTool(s, "agent_task",
		"Give the LLM agent on a machine a task. The agent is created on first use, named after the machine, or its "+
			"model is switched to the one given. The task is queued (agentd does not refuse a task while another one "+
			"runs) and this call returns at once with the task id; follow it with agent_status. "+
			"model is REQUIRED: take one from agent_models. check_command, if set, runs in the machine; exit status 0 "+
			"means success. With check_command the check decides the result (after each batch of commands unless "+
			"check_each_step is false, and at the end). Without it a claimed success is reported as done_unverified. "+
			"max_steps caps the commands the agent runs (default 40). max_minutes (default none) stops the task, and "+
			"max_call_tokens (default 32768, 0 = no cap) caps one model reply; replies that repeat themselves or hit the "+
			"cap are discarded and the model is asked to wrap up.",
		s.agentTask)
	addTool(s, "agent_status",
		"Show the latest task of the agent on a machine: a progress header (status, step, max_steps, elapsed_s, token "+
			"counts, calls, first_command_s, verdict_source, reason, last_command), the latest commands it ran and its "+
			"result. While the task is queued or running the call waits up to wait_seconds (default 30, capped at about "+
			"40 s). Poll cheaply with since_step (the last step number you have seen) and brief=true for the header "+
			"alone. max_commands (default 10) limits the commands listed. Statuses: running, done, done_unverified, "+
			"failed, gave_up, cancelled, error. Fails with 'no agent/task yet' if agent_task was never used on the machine.",
		s.agentStatus)
	addTool(s, "agent_cancel",
		"Cancel the latest task of the agent on a machine if it is queued or running. The cancel is asked for; a "+
			"running task stops within about a second. Says 'no running task' when there is nothing to cancel.",
		s.agentCancel)
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
		lines = append(lines, fmt.Sprintf("task %s queued with model %s behind %d unfinished task(s) of this machine's agent "+
			"(agent_cancel stops the running one)", res.Msg.GetId(), in.Model, ahead))
	} else {
		lines = append(lines, fmt.Sprintf("task %s started with model %s", res.Msg.GetId(), in.Model))
	}
	if !chosen.GetFitsVram() {
		lines = append(lines, fmt.Sprintf("warning: model %s (%.1f GiB) exceeds the VRAM budget, so it will probably "+
			"run partly on the CPU and be slow", in.Model, chosen.GetSizeGib()))
	}
	return strings.Join(lines, "\n"), nil
}

// tasksAhead counts the agent's tasks that are still queued or running, so a new task that has to wait is
// reported as queued. It is best effort: on a listing error it reports 0.
func (s *Server) tasksAhead(ctx context.Context, agentID string) int {
	n := 0
	for _, st := range []agentdv1.TaskState{agentdv1.TaskState_TASK_STATE_RUNNING, agentdv1.TaskState_TASK_STATE_QUEUED} {
		res, err := s.cfg.Clients.Tasks.ListTasks(ctx, connect.NewRequest(&agentdv1.ListTasksRequest{
			AgentId: agentID, State: st, PageSize: 50}))
		if err != nil {
			return 0
		}
		n += len(res.Msg.GetTasks())
	}
	return n
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
		steps, err = s.tailSteps(ctx, task.GetId(), view.since, maxCommands)
	default:
		steps, err = s.tailSteps(ctx, task.GetId(), 0, maxCommands)
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
}

// agentCancel cancels the latest task when it is queued or running.
func (s *Server) agentCancel(ctx context.Context, in agentCancelArgs) (string, error) {
	vm, err := s.vmByName(ctx, in.Project, in.Name)
	if err != nil {
		return "", err
	}
	_, task, err := s.agentAndLatestTask(ctx, s.project(in.Project), vm.GetId(), in.Name)
	if err != nil {
		return "", err
	}
	if task == nil || !isLive(task.GetState()) {
		return "no running task", nil
	}
	if _, err := s.cfg.Clients.Tasks.CancelTask(ctx, connect.NewRequest(&agentdv1.CancelTaskRequest{Id: task.GetId()})); err != nil {
		return "", err
	}
	return fmt.Sprintf("cancel requested for task %s", task.GetId()), nil
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

// tailSteps returns the last n steps of a task that come after step number after (0 for all). agentd pages
// forward only, so every page is read and only the last n are kept.
func (s *Server) tailSteps(ctx context.Context, taskID string, after int32, n int) ([]*agentdv1.Step, error) {
	var tail []*agentdv1.Step
	for {
		res, err := s.cfg.Clients.Tasks.ListSteps(ctx, connect.NewRequest(&agentdv1.ListStepsRequest{
			TaskId: taskID, AfterSeq: after, Limit: agentStepPageSize}))
		if err != nil {
			return nil, err
		}
		page := res.Msg.GetSteps()
		tail = append(tail, page...)
		if len(tail) > n {
			tail = tail[len(tail)-n:]
		}
		if len(page) < agentStepPageSize {
			return tail, nil
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
