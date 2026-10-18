package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/agentd/v1"
)

const (
	agentDefaultMaxSteps = 40
	agentDefaultWait     = 30 // seconds agent_status waits when wait_seconds is not given
	agentListPageSize    = 200
	agentStepPageSize    = 500 // agentd's maximum for ListSteps
	agentTailSteps       = 10  // commands shown by agent_status
	agentCommandMaxRunes = 200
)

type agentModelsArgs struct{}

type agentTaskArgs struct {
	Name         string `json:"name" jsonschema:"name of the machine (VM) the agent works on; the agent is created with this name"`
	Task         string `json:"task" jsonschema:"what the agent must do, in plain language; required"`
	Model        string `json:"model,omitempty" jsonschema:"REQUIRED: the model to run, one of the names from agent_models. There is no default; pick one that fits in GPU memory"`
	CheckCommand string `json:"check_command,omitempty" jsonschema:"shell command run in the machine after the agent finishes; exit status 0 means the goal was met"`
	MaxSteps     int    `json:"max_steps,omitempty" jsonschema:"maximum number of commands the agent may run (default 40; agentd caps it at 200)"`
	Project      string `json:"project,omitempty" jsonschema:"project of the machine (default: the server's default project)"`
}

type agentStatusArgs struct {
	Name        string `json:"name" jsonschema:"name of the machine whose agent to check"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"seconds to wait while the latest task is queued or running (default 30, capped by the server's limit of about 40 s); 0 returns at once"`
	Project     string `json:"project,omitempty" jsonschema:"project of the machine (default: the server's default project)"`
}

type agentCancelArgs struct {
	Name    string `json:"name" jsonschema:"name of the machine whose running task to cancel"`
	Project string `json:"project,omitempty" jsonschema:"project of the machine (default: the server's default project)"`
}

func (s *Server) registerAgentTools() {
	addTool(s, "agent_models",
		"List the LLM models installed on the agent backend, with their size (e.g. 'llama3.1:8b  4.9 GiB'). "+
			"There is no default model: the caller must pick one for agent_task. A model must fit in the GPU memory "+
			"to run fast; a model larger than the GPU memory spills to the CPU and runs much slower.",
		s.agentModels)
	addTool(s, "agent_task",
		"Give the LLM agent on a machine a task. The agent is created on first use, named after the machine, or its "+
			"model is switched to the one given. The task is queued (agentd does not refuse a task while another one "+
			"runs) and this call returns at once with the task id; follow it with agent_status. "+
			"model is REQUIRED: take one from agent_models. check_command, if set, runs in the machine afterwards; "+
			"exit status 0 means success. max_steps caps the commands the agent runs (default 40).",
		s.agentTask)
	addTool(s, "agent_status",
		"Show the latest task of the agent on a machine: its status, timing, the last 10 commands it ran and its "+
			"result. While the task is queued or running the call waits up to wait_seconds (default 30, capped at "+
			"about 40 s), so call it again to keep following a task. Fails with 'no agent/task yet' if agent_task "+
			"was never used on the machine.",
		s.agentStatus)
	addTool(s, "agent_cancel",
		"Cancel the latest task of the agent on a machine if it is queued or running. The cancel is asked for; a "+
			"running task stops within about a second. Says 'no running task' when there is nothing to cancel.",
		s.agentCancel)
}

// agentModels lists the installed models.
func (s *Server) agentModels(ctx context.Context, _ agentModelsArgs) (string, error) {
	models, err := s.installedModels(ctx)
	if err != nil {
		return "", err
	}
	if len(models) == 0 {
		return "(none installed)", nil
	}
	lines := make([]string, 0, len(models))
	for _, m := range models {
		lines = append(lines, fmt.Sprintf("%s  %.1f GiB", m.GetName(), float64(m.GetSizeBytes())/(1<<30)))
	}
	return strings.Join(lines, "\n"), nil
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
	if !hasModel(models, in.Model) {
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
	res, err := s.cfg.Clients.Tasks.CreateTask(ctx, connect.NewRequest(&agentdv1.CreateTaskRequest{
		AgentId:      agent.GetId(),
		Prompt:       in.Task,
		CheckCommand: in.CheckCommand,
		MaxSteps:     int32(in.MaxSteps),
	}))
	if err != nil {
		return "", err
	}
	if ahead > 0 {
		return fmt.Sprintf("task %s queued with model %s behind %d unfinished task(s) of this machine's agent "+
			"(agent_cancel stops the running one)", res.Msg.GetId(), in.Model, ahead), nil
	}
	return fmt.Sprintf("task %s started with model %s", res.Msg.GetId(), in.Model), nil
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

	steps, err := s.tailSteps(ctx, task.GetId(), agentTailSteps)
	if err != nil {
		return "", err
	}
	return renderAgentStatus(agent, task, steps, time.Now()), nil
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

// tailSteps returns the last n steps of a task. agentd pages forward only, so every page is read and only the
// last n are kept.
func (s *Server) tailSteps(ctx context.Context, taskID string, n int) ([]*agentdv1.Step, error) {
	var tail []*agentdv1.Step
	after := int32(0)
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

// renderAgentStatus builds the agent_status output. The model's reasoning and token counts are not available
// from agentd, so they are not shown.
func renderAgentStatus(agent *agentdv1.Agent, task *agentdv1.Task, steps []*agentdv1.Step, now time.Time) string {
	live := isLive(task.GetState())

	var b strings.Builder
	fmt.Fprintf(&b, "status: %s  (%d commands run, model %s", statusLabel(task.GetState()), task.GetSteps(), agent.GetModel())
	if v := task.GetVerdict(); v != "" {
		fmt.Fprintf(&b, ", verdict: %s", v)
	}
	b.WriteString(")\n")
	b.WriteString(timingLine(task, now))
	b.WriteString("\n")
	for _, st := range steps {
		fmt.Fprintf(&b, "$ %s\n", clipCommand(st.GetCommand()))
	}
	b.WriteString("\n=== RESULT ===\n")
	switch result := strings.TrimRight(task.GetResult(), "\n"); {
	case result != "":
		b.WriteString(result)
	case live:
		b.WriteString("(no result yet)")
	default:
		b.WriteString("(none)")
	}
	return b.String()
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

func hasModel(models []*agentdv1.Model, name string) bool {
	for _, m := range models {
		if m.GetName() == name {
			return true
		}
	}
	return false
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
