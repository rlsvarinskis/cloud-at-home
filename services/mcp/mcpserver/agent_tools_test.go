package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/agentd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/agentd/v1/agentdv1connect"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1/vmdv1connect"
)

var tskBase = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// tskVMs fakes vmd's VM listing.
type tskVMs struct {
	vmdv1connect.VMServiceClient
	vms []*vmdv1.VM
}

func (f *tskVMs) ListVMs(_ context.Context, _ *connect.Request[vmdv1.ListVMsRequest]) (*connect.Response[vmdv1.ListVMsResponse], error) {
	return connect.NewResponse(&vmdv1.ListVMsResponse{Vms: f.vms}), nil
}

// tskAgents fakes agentd's agent service. Agents are listed two per page.
type tskAgents struct {
	agentdv1connect.AgentServiceClient
	agents    []*agentdv1.Agent
	created   []*agentdv1.CreateAgentRequest
	updateErr error
	updated   []*agentdv1.UpdateAgentRequest
	listErr   error
	createErr error
}

func (f *tskAgents) ListAgents(_ context.Context, req *connect.Request[agentdv1.ListAgentsRequest]) (*connect.Response[agentdv1.ListAgentsResponse], error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var rows []*agentdv1.Agent
	for _, a := range f.agents {
		if a.GetVmId() == req.Msg.GetVmId() {
			rows = append(rows, a)
		}
	}
	start := 0
	if tok := req.Msg.GetPageToken(); tok != "" {
		fmt.Sscanf(tok, "%d", &start)
	}
	end := min(start+2, len(rows))
	res := &agentdv1.ListAgentsResponse{Agents: rows[start:end]}
	if end < len(rows) {
		res.NextPageToken = fmt.Sprint(end)
	}
	return connect.NewResponse(res), nil
}

func (f *tskAgents) CreateAgent(_ context.Context, req *connect.Request[agentdv1.CreateAgentRequest]) (*connect.Response[agentdv1.Agent], error) {
	f.created = append(f.created, req.Msg)
	if f.createErr != nil {
		return nil, f.createErr
	}
	a := &agentdv1.Agent{Id: "agent_new", ProjectId: req.Msg.GetProjectId(), Name: req.Msg.GetName(),
		VmId: req.Msg.GetVmId(), Model: req.Msg.GetModel()}
	f.agents = append(f.agents, a)
	return connect.NewResponse(a), nil
}

func (f *tskAgents) UpdateAgent(_ context.Context, req *connect.Request[agentdv1.UpdateAgentRequest]) (*connect.Response[agentdv1.Agent], error) {
	f.updated = append(f.updated, req.Msg)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	for _, a := range f.agents {
		if a.GetId() == req.Msg.GetId() && req.Msg.Model != nil {
			a.Model = req.Msg.GetModel()
			return connect.NewResponse(a), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no agent"))
}

// tskTasks fakes agentd's task service. Tasks must be stored in ID order. ListTasks pages two at a time.
// GetTask returns the scripted tasks first (when set), then the stored task after honouring wait_seconds.
type tskTasks struct {
	agentdv1connect.TaskServiceClient
	tasks     []*agentdv1.Task
	steps     map[string][]*agentdv1.Step
	script    []*agentdv1.Task
	gets      []*agentdv1.GetTaskRequest
	created   []*agentdv1.CreateTaskRequest
	cancelled []string
	cancelErr error
	listErr   error
	stepsErr  error
	getErr    error
	createErr error
}

func (f *tskTasks) find(id string) *agentdv1.Task {
	for _, t := range f.tasks {
		if t.GetId() == id {
			return t
		}
	}
	return nil
}

func (f *tskTasks) ListTasks(_ context.Context, req *connect.Request[agentdv1.ListTasksRequest]) (*connect.Response[agentdv1.ListTasksResponse], error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var rows []*agentdv1.Task
	for _, t := range f.tasks {
		wantState := req.Msg.GetState()
		if t.GetAgentId() == req.Msg.GetAgentId() && t.GetId() > req.Msg.GetPageToken() &&
			(wantState == agentdv1.TaskState_TASK_STATE_UNSPECIFIED || t.GetState() == wantState) {
			rows = append(rows, t)
		}
	}
	res := &agentdv1.ListTasksResponse{}
	if len(rows) > 2 {
		rows = rows[:2]
		res.NextPageToken = rows[1].GetId()
	}
	res.Tasks = rows
	return connect.NewResponse(res), nil
}

func (f *tskTasks) GetTask(ctx context.Context, req *connect.Request[agentdv1.GetTaskRequest]) (*connect.Response[agentdv1.Task], error) {
	f.gets = append(f.gets, req.Msg)
	if f.getErr != nil {
		return nil, f.getErr
	}
	if len(f.script) > 0 {
		next := f.script[0]
		f.script = f.script[1:]
		return connect.NewResponse(next), nil
	}
	t := f.find(req.Msg.GetId())
	if t == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no task"))
	}
	// Simulate the long poll: block while the state is unchanged, up to the requested seconds.
	if req.Msg.GetWaitSeconds() > 0 && t.GetState() == req.Msg.GetWaitForNot() {
		select {
		case <-time.After(time.Duration(req.Msg.GetWaitSeconds()) * time.Second):
		case <-ctx.Done():
		}
	}
	return connect.NewResponse(t), nil
}

func (f *tskTasks) CreateTask(_ context.Context, req *connect.Request[agentdv1.CreateTaskRequest]) (*connect.Response[agentdv1.Task], error) {
	f.created = append(f.created, req.Msg)
	if f.createErr != nil {
		return nil, f.createErr
	}
	t := &agentdv1.Task{Id: "task_new", AgentId: req.Msg.GetAgentId(), Prompt: req.Msg.GetPrompt(),
		CheckCommand: req.Msg.GetCheckCommand(), MaxSteps: req.Msg.GetMaxSteps(), State: agentdv1.TaskState_TASK_STATE_QUEUED}
	return connect.NewResponse(t), nil
}

func (f *tskTasks) CancelTask(_ context.Context, req *connect.Request[agentdv1.CancelTaskRequest]) (*connect.Response[agentdv1.Task], error) {
	f.cancelled = append(f.cancelled, req.Msg.GetId())
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	return connect.NewResponse(f.find(req.Msg.GetId())), nil
}

// ListSteps honours after_seq and caps limit at agentd's maximum of 500.
func (f *tskTasks) ListSteps(_ context.Context, req *connect.Request[agentdv1.ListStepsRequest]) (*connect.Response[agentdv1.ListStepsResponse], error) {
	if f.stepsErr != nil {
		return nil, f.stepsErr
	}
	limit := int(min(req.Msg.GetLimit(), 500))
	var out []*agentdv1.Step
	for _, st := range f.steps[req.Msg.GetTaskId()] {
		if st.GetSeq() > req.Msg.GetAfterSeq() && len(out) < limit {
			out = append(out, st)
		}
	}
	return connect.NewResponse(&agentdv1.ListStepsResponse{Steps: out}), nil
}

type tskModels struct {
	agentdv1connect.ModelServiceClient
	models []*agentdv1.Model
	err    error
}

func (f *tskModels) ListModels(_ context.Context, _ *connect.Request[agentdv1.ListModelsRequest]) (*connect.Response[agentdv1.ListModelsResponse], error) {
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(&agentdv1.ListModelsResponse{Models: f.models}), nil
}

// tskEnv bundles the fakes for one test.
type tskEnv struct {
	vms    *tskVMs
	agents *tskAgents
	tasks  *tskTasks
	models *tskModels
}

func tskNewEnv() *tskEnv {
	return &tskEnv{
		vms:    &tskVMs{vms: []*vmdv1.VM{{Id: "vm_1", Name: "box"}, {Id: "vm_2", Name: "other"}}},
		agents: &tskAgents{},
		tasks:  &tskTasks{steps: map[string][]*agentdv1.Step{}},
		models: &tskModels{models: []*agentdv1.Model{
			{Name: "llama", SizeBytes: 2 << 30}, {Name: "qwen", SizeBytes: 4 << 30},
		}},
	}
}

func (e *tskEnv) clients() Clients {
	return Clients{VMs: e.vms, Agents: e.agents, Tasks: e.tasks, Models: e.models}
}

// tskAgent returns an agent on vm_1 named "box" with the given model and ID.
func tskAgent(id, model string) *agentdv1.Agent {
	return &agentdv1.Agent{Id: id, ProjectId: "p1", Name: "box", VmId: "vm_1", Model: model}
}

func tskTask(id string, state agentdv1.TaskState) *agentdv1.Task {
	return &agentdv1.Task{Id: id, AgentId: "agent_1", ProjectId: "p1", State: state, Prompt: "p",
		CreatedAt: timestamppb.New(tskBase)}
}

// --- agent_models ---

func TestTskAgentModelsListsWithSizes(t *testing.T) {
	env := tskNewEnv()
	env.models.models = []*agentdv1.Model{
		{Name: "llama3.1:8b", SizeBytes: 4_939_212_390}, // about 4.6 GiB
		{Name: "tiny", SizeBytes: 1536 << 20},           // 1.5 GiB
	}
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_models", map[string]any{})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	want := "llama3.1:8b  4.6 GiB\ntiny  1.5 GiB"
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestTskAgentModelsNoneInstalled(t *testing.T) {
	env := tskNewEnv()
	env.models.models = nil
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_models", nil)
	if isErr || out != "(none installed)" {
		t.Fatalf("got %q (error %v)", out, isErr)
	}
}

func TestTskAgentModelsBackendError(t *testing.T) {
	env := tskNewEnv()
	env.models.err = connect.NewError(connect.CodeUnavailable, fmt.Errorf("backend down"))
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_models", nil)
	if !isErr || !strings.Contains(out, "backend down") {
		t.Fatalf("want tool error, got %q (error %v)", out, isErr)
	}
}

// --- agent_task ---

func TestTskAgentTaskCreatesAgentAndQueuesTask(t *testing.T) {
	env := tskNewEnv()
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_task", map[string]any{
		"name": "box", "task": "fix it", "model": "qwen", "check_command": "test -f x", "max_steps": 7, "project": "p9",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	if out != "task task_new started with model qwen" {
		t.Fatalf("got %q", out)
	}
	if len(env.agents.created) != 1 {
		t.Fatalf("created %d agents, want 1", len(env.agents.created))
	}
	ca := env.agents.created[0]
	if ca.GetProjectId() != "p9" || ca.GetName() != "box" || ca.GetVmId() != "vm_1" ||
		ca.GetModel() != "qwen" || ca.GetIdempotencyKey() != "mcp-agent-vm_1" {
		t.Fatalf("bad CreateAgent request: %+v", ca)
	}
	if len(env.agents.updated) != 0 {
		t.Fatal("a new agent must not be updated")
	}
	if len(env.tasks.created) != 1 {
		t.Fatalf("created %d tasks, want 1", len(env.tasks.created))
	}
	ct := env.tasks.created[0]
	if ct.GetAgentId() != "agent_new" || ct.GetPrompt() != "fix it" || ct.GetCheckCommand() != "test -f x" || ct.GetMaxSteps() != 7 {
		t.Fatalf("bad CreateTask request: %+v", ct)
	}
}

func TestTskAgentTaskDefaultMaxSteps(t *testing.T) {
	env := tskNewEnv()
	env.agents.agents = []*agentdv1.Agent{tskAgent("agent_1", "qwen")}
	cs := newTestSession(t, env.clients())
	if out, isErr := callTool(t, cs, "agent_task", map[string]any{"name": "box", "task": "go", "model": "qwen"}); isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	if got := env.tasks.created[0]; got.GetMaxSteps() != 40 || got.GetAgentId() != "agent_1" {
		t.Fatalf("got max_steps %d agent %s, want 40 agent_1", got.GetMaxSteps(), got.GetAgentId())
	}
	if len(env.agents.created) != 0 || len(env.agents.updated) != 0 {
		t.Fatal("an existing agent with the same model must be reused unchanged")
	}
}

func TestTskAgentTaskSwitchesModel(t *testing.T) {
	env := tskNewEnv()
	env.agents.agents = []*agentdv1.Agent{tskAgent("agent_1", "llama")}
	cs := newTestSession(t, env.clients())
	if out, isErr := callTool(t, cs, "agent_task", map[string]any{"name": "box", "task": "go", "model": "qwen"}); isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	if len(env.agents.updated) != 1 || env.agents.updated[0].GetId() != "agent_1" || env.agents.updated[0].GetModel() != "qwen" {
		t.Fatalf("bad UpdateAgent calls: %+v", env.agents.updated)
	}
	if len(env.agents.created) != 0 || len(env.tasks.created) != 1 {
		t.Fatal("expected no new agent and one task")
	}
}

func TestTskAgentTaskFindsAgentOnLaterPage(t *testing.T) {
	env := tskNewEnv()
	env.agents.agents = []*agentdv1.Agent{
		{Id: "a1", Name: "other", VmId: "vm_1", Model: "llama"},
		{Id: "a2", Name: "third", VmId: "vm_1", Model: "llama"},
		tskAgent("agent_1", "qwen"),
	}
	cs := newTestSession(t, env.clients())
	if out, isErr := callTool(t, cs, "agent_task", map[string]any{"name": "box", "task": "go", "model": "qwen"}); isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	if len(env.agents.created) != 0 || env.tasks.created[0].GetAgentId() != "agent_1" {
		t.Fatalf("agent on page 2 not found: created %d, task agent %s", len(env.agents.created), env.tasks.created[0].GetAgentId())
	}
}

func TestTskAgentTaskValidationCreatesNothing(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"model missing", map[string]any{"name": "box", "task": "go"},
			"model is required: there is no default, pick one of the installed models. Installed: llama, qwen"},
		{"model not installed", map[string]any{"name": "box", "task": "go", "model": "gpt"},
			"model 'gpt' is not installed. Installed: llama, qwen"},
		{"negative max_steps", map[string]any{"name": "box", "task": "go", "model": "qwen", "max_steps": -1},
			"max_steps must not be negative"},
		{"empty task", map[string]any{"name": "box", "task": "  ", "model": "qwen"}, "task is required"},
		{"empty name", map[string]any{"name": "", "task": "go", "model": "qwen"}, "name is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := tskNewEnv()
			cs := newTestSession(t, env.clients())
			out, isErr := callTool(t, cs, "agent_task", tc.args)
			if !isErr || !strings.Contains(out, tc.want) {
				t.Fatalf("got %q (error %v), want %q", out, isErr, tc.want)
			}
			if len(env.agents.created)+len(env.agents.updated)+len(env.tasks.created) != 0 {
				t.Fatal("validation failure must not create or change anything")
			}
		})
	}
}

func TestTskAgentTaskModelsNoneInstalled(t *testing.T) {
	env := tskNewEnv()
	env.models.models = nil
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_task", map[string]any{"name": "box", "task": "go", "model": "qwen"})
	if !isErr || !strings.Contains(out, "Installed: (none installed)") {
		t.Fatalf("got %q (error %v)", out, isErr)
	}
}

func TestTskAgentTaskUnknownMachine(t *testing.T) {
	env := tskNewEnv()
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_task", map[string]any{"name": "ghost", "task": "go", "model": "qwen"})
	if !isErr || !strings.Contains(out, `no machine named "ghost"`) {
		t.Fatalf("got %q (error %v)", out, isErr)
	}
	if len(env.agents.created) != 0 || len(env.tasks.created) != 0 {
		t.Fatal("nothing may be created for an unknown machine")
	}
}

func TestTskAgentTaskDownstreamErrors(t *testing.T) {
	t.Run("list agents", func(t *testing.T) {
		env := tskNewEnv()
		env.agents.listErr = connect.NewError(connect.CodePermissionDenied, fmt.Errorf("no access"))
		cs := newTestSession(t, env.clients())
		out, isErr := callTool(t, cs, "agent_task", map[string]any{"name": "box", "task": "go", "model": "qwen"})
		if !isErr || !strings.Contains(out, "permission_denied: no access") {
			t.Fatalf("got %q (error %v)", out, isErr)
		}
	})
	t.Run("create agent", func(t *testing.T) {
		env := tskNewEnv()
		env.agents.createErr = connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("bad model"))
		cs := newTestSession(t, env.clients())
		out, isErr := callTool(t, cs, "agent_task", map[string]any{"name": "box", "task": "go", "model": "qwen"})
		if !isErr || !strings.Contains(out, "bad model") || len(env.tasks.created) != 0 {
			t.Fatalf("got %q (error %v)", out, isErr)
		}
	})
	t.Run("create task", func(t *testing.T) {
		env := tskNewEnv()
		env.agents.agents = []*agentdv1.Agent{tskAgent("agent_1", "qwen")}
		env.tasks.createErr = connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("agent agent_1 is ORPHANED"))
		cs := newTestSession(t, env.clients())
		out, isErr := callTool(t, cs, "agent_task", map[string]any{"name": "box", "task": "go", "model": "qwen"})
		if !isErr || !strings.Contains(out, "is ORPHANED") {
			t.Fatalf("got %q (error %v)", out, isErr)
		}
	})
	t.Run("update agent", func(t *testing.T) {
		env := tskNewEnv()
		env.agents.agents = []*agentdv1.Agent{tskAgent("agent_1", "llama")}
		env.agents.updateErr = connect.NewError(connect.CodeNotFound, fmt.Errorf("no agent"))
		cs := newTestSession(t, env.clients())
		out, isErr := callTool(t, cs, "agent_task", map[string]any{"name": "box", "task": "go", "model": "qwen"})
		if !isErr || !strings.Contains(out, "no agent") || len(env.tasks.created) != 0 {
			t.Fatalf("got %q (error %v)", out, isErr)
		}
	})
	t.Run("models", func(t *testing.T) {
		env := tskNewEnv()
		env.models.err = connect.NewError(connect.CodeUnavailable, fmt.Errorf("down"))
		cs := newTestSession(t, env.clients())
		out, isErr := callTool(t, cs, "agent_task", map[string]any{"name": "box", "task": "go", "model": "qwen"})
		if !isErr || !strings.Contains(out, "down") {
			t.Fatalf("got %q (error %v)", out, isErr)
		}
	})
}

// --- agent_status ---

// tskStatusEnv returns an env where the machine box has agent_1 and the given tasks (in ID order).
func tskStatusEnv(tasks ...*agentdv1.Task) *tskEnv {
	env := tskNewEnv()
	env.agents.agents = []*agentdv1.Agent{tskAgent("agent_1", "qwen")}
	env.tasks.tasks = tasks
	return env
}

func TestTskAgentStatusFinishedTask(t *testing.T) {
	task := tskTask("task_1", agentdv1.TaskState_TASK_STATE_SUCCEEDED)
	task.Verdict = "SUCCESS"
	task.Result = "all good"
	task.Steps = 2
	task.StartedAt = timestamppb.New(tskBase.Add(2 * time.Second))
	task.FinishedAt = timestamppb.New(tskBase.Add(10 * time.Second))
	env := tskStatusEnv(task)
	env.tasks.steps["task_1"] = []*agentdv1.Step{
		{Seq: 1, Command: "ls -la"}, {Seq: 2, Command: "cat README"},
	}
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box", "wait_seconds": 0})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	want := "status: done  (2 commands run, model qwen, verdict: SUCCESS)\n" +
		"timing: created 12:00:00, started 12:00:02, finished 12:00:10, elapsed 10.0s\n" +
		"$ ls -la\n$ cat README\n\n=== RESULT ===\nall good"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	if len(env.tasks.gets) != 0 {
		t.Fatal("a finished task must not be polled")
	}
}

func TestTskAgentStatusRunningTaskNoResult(t *testing.T) {
	task := tskTask("task_1", agentdv1.TaskState_TASK_STATE_RUNNING)
	task.StartedAt = timestamppb.New(tskBase)
	env := tskStatusEnv(task)
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box", "wait_seconds": 0})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	lines := strings.Split(out, "\n")
	if lines[0] != "status: running  (0 commands run, model qwen)" {
		t.Fatalf("first line %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "timing: created 12:00:00, started 12:00:00, elapsed ") || strings.Contains(lines[1], "finished") {
		t.Fatalf("timing line %q", lines[1])
	}
	if !strings.HasSuffix(out, "\n\n=== RESULT ===\n(no result yet)") {
		t.Fatalf("bad tail: %q", out)
	}
}

func TestTskAgentStatusWaitsUntilDone(t *testing.T) {
	env := tskStatusEnv(tskTask("task_1", agentdv1.TaskState_TASK_STATE_QUEUED))
	env.tasks.script = []*agentdv1.Task{
		tskTask("task_1", agentdv1.TaskState_TASK_STATE_RUNNING),
		func() *agentdv1.Task {
			t := tskTask("task_1", agentdv1.TaskState_TASK_STATE_SUCCEEDED)
			t.Result = "ok"
			return t
		}(),
	}
	cs := newTestSession(t, env.clients(), func(c *Config) { c.MaxWait = 30 * time.Second })
	out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box"})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	if !strings.HasPrefix(out, "status: done  (0 commands run, model qwen)\n") || !strings.HasSuffix(out, "=== RESULT ===\nok") {
		t.Fatalf("got %q", out)
	}
	if len(env.tasks.gets) != 2 {
		t.Fatalf("got %d GetTask calls, want 2", len(env.tasks.gets))
	}
	if env.tasks.gets[0].GetWaitForNot() != agentdv1.TaskState_TASK_STATE_QUEUED ||
		env.tasks.gets[1].GetWaitForNot() != agentdv1.TaskState_TASK_STATE_RUNNING {
		t.Fatalf("wait_for_not sequence wrong: %v, %v", env.tasks.gets[0].GetWaitForNot(), env.tasks.gets[1].GetWaitForNot())
	}
	for _, g := range env.tasks.gets {
		if g.GetWaitSeconds() < 29 || g.GetWaitSeconds() > 30 {
			t.Fatalf("wait_seconds %d, want about 30 (default)", g.GetWaitSeconds())
		}
	}
}

func TestTskAgentStatusWaitBudgetIsCapped(t *testing.T) {
	env := tskStatusEnv(tskTask("task_1", agentdv1.TaskState_TASK_STATE_RUNNING))
	cs := newTestSession(t, env.clients()) // MaxWait is 2 s in the test session
	start := time.Now()
	out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box", "wait_seconds": 30})
	elapsed := time.Since(start)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("call took %v; the wait must be capped at MaxWait (2 s)", elapsed)
	}
	if !strings.HasPrefix(out, "status: running") || !strings.HasSuffix(out, "(no result yet)") {
		t.Fatalf("got %q", out)
	}
	for _, g := range env.tasks.gets {
		if g.GetWaitSeconds() > 2 {
			t.Fatalf("wait_seconds %d exceeds MaxWait", g.GetWaitSeconds())
		}
	}
}

func TestTskAgentStatusGetTaskErrorIsReported(t *testing.T) {
	env := tskStatusEnv(tskTask("task_1", agentdv1.TaskState_TASK_STATE_QUEUED))
	env.tasks.getErr = connect.NewError(connect.CodeUnavailable, fmt.Errorf("agentd gone"))
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box"})
	if !isErr || !strings.Contains(out, "agentd gone") {
		t.Fatalf("got %q (error %v)", out, isErr)
	}
}

func TestTskAgentStatusNoAgentOrTasks(t *testing.T) {
	want := "no agent/task yet on machine 'box' (use agent_task)"
	t.Run("no agent", func(t *testing.T) {
		env := tskNewEnv()
		cs := newTestSession(t, env.clients())
		out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box"})
		if !isErr || out != want {
			t.Fatalf("got %q (error %v)", out, isErr)
		}
	})
	t.Run("agent without tasks", func(t *testing.T) {
		env := tskStatusEnv()
		cs := newTestSession(t, env.clients())
		out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box"})
		if !isErr || out != want {
			t.Fatalf("got %q (error %v)", out, isErr)
		}
	})
	t.Run("tasks of another agent only", func(t *testing.T) {
		env := tskStatusEnv()
		env.tasks.tasks = []*agentdv1.Task{{Id: "task_x", AgentId: "agent_other"}}
		cs := newTestSession(t, env.clients())
		out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box"})
		if !isErr || out != want {
			t.Fatalf("got %q (error %v)", out, isErr)
		}
	})
}

func TestTskAgentStatusUnknownMachine(t *testing.T) {
	env := tskNewEnv()
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "ghost"})
	if !isErr || !strings.Contains(out, "no machine named") {
		t.Fatalf("got %q (error %v)", out, isErr)
	}
}

func TestTskAgentStatusUsesNewestTaskAcrossPages(t *testing.T) {
	first := tskTask("task_01", agentdv1.TaskState_TASK_STATE_SUCCEEDED)
	first.Result = "old"
	second := tskTask("task_02", agentdv1.TaskState_TASK_STATE_FAILED)
	second.Result = "middle"
	third := tskTask("task_03", agentdv1.TaskState_TASK_STATE_GAVE_UP)
	third.Result = "newest"
	env := tskStatusEnv(first, second, third)
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box", "wait_seconds": 0})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	if !strings.HasPrefix(out, "status: gave_up  ") || !strings.HasSuffix(out, "=== RESULT ===\nnewest") {
		t.Fatalf("did not use the newest task: %q", out)
	}
}

func TestTskAgentStatusStateLabels(t *testing.T) {
	cases := map[agentdv1.TaskState]string{
		agentdv1.TaskState_TASK_STATE_QUEUED:      "running",
		agentdv1.TaskState_TASK_STATE_RUNNING:     "running",
		agentdv1.TaskState_TASK_STATE_SUCCEEDED:   "done",
		agentdv1.TaskState_TASK_STATE_FAILED:      "failed",
		agentdv1.TaskState_TASK_STATE_GAVE_UP:     "gave_up",
		agentdv1.TaskState_TASK_STATE_CANCELLED:   "cancelled",
		agentdv1.TaskState_TASK_STATE_ERROR:       "error",
		agentdv1.TaskState_TASK_STATE_UNSPECIFIED: "unknown",
	}
	for state, label := range cases {
		task := tskTask("task_1", state)
		task.Result = "r"
		env := tskStatusEnv(task)
		cs := newTestSession(t, env.clients())
		out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box", "wait_seconds": 0})
		if isErr || !strings.HasPrefix(out, "status: "+label+"  ") {
			t.Fatalf("state %v: got %q (error %v), want label %s", state, out, isErr, label)
		}
	}
}

func TestTskAgentStatusTailOfSteps(t *testing.T) {
	task := tskTask("task_1", agentdv1.TaskState_TASK_STATE_RUNNING)
	task.Steps = 600
	env := tskStatusEnv(task)
	for i := 1; i <= 600; i++ {
		env.tasks.steps["task_1"] = append(env.tasks.steps["task_1"], &agentdv1.Step{Seq: int32(i), Command: fmt.Sprintf("cmd %d", i)})
	}
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box", "wait_seconds": 0})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	var cmds []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "$ ") {
			cmds = append(cmds, l)
		}
	}
	if len(cmds) != 10 || cmds[0] != "$ cmd 591" || cmds[9] != "$ cmd 600" {
		t.Fatalf("want the last 10 commands, got %d: %v", len(cmds), cmds)
	}
}

func TestTskAgentStatusClipsLongCommands(t *testing.T) {
	task := tskTask("task_1", agentdv1.TaskState_TASK_STATE_RUNNING)
	env := tskStatusEnv(task)
	long := strings.Repeat("é", 300)
	env.tasks.steps["task_1"] = []*agentdv1.Step{{Seq: 1, Command: long}, {Seq: 2, Command: "echo a\necho b"}}
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box", "wait_seconds": 0})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	lines := strings.Split(out, "\n")
	if got := []rune(lines[2]); len(got) != 2+200 || !strings.HasSuffix(lines[2], "...") {
		t.Fatalf("long command not clipped to 200 runes: %d runes", len(got))
	}
	if lines[3] != "$ echo a echo b" {
		t.Fatalf("multi-line command %q", lines[3])
	}
}

func TestTskAgentStatusStepErrorIsReported(t *testing.T) {
	env := tskStatusEnv(tskTask("task_1", agentdv1.TaskState_TASK_STATE_RUNNING))
	env.tasks.stepsErr = connect.NewError(connect.CodeInternal, fmt.Errorf("db down"))
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box", "wait_seconds": 0})
	if !isErr || !strings.Contains(out, "db down") {
		t.Fatalf("got %q (error %v)", out, isErr)
	}
}

func TestTskAgentStatusListTasksErrorIsReported(t *testing.T) {
	env := tskStatusEnv(tskTask("task_1", agentdv1.TaskState_TASK_STATE_RUNNING))
	env.tasks.listErr = connect.NewError(connect.CodeInternal, fmt.Errorf("list broke"))
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box"})
	if !isErr || !strings.Contains(out, "list broke") {
		t.Fatalf("got %q (error %v)", out, isErr)
	}
}

func TestTskAgentStatusListAgentsErrorIsReported(t *testing.T) {
	env := tskStatusEnv()
	env.agents.listErr = connect.NewError(connect.CodeInternal, fmt.Errorf("agents broke"))
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box"})
	if !isErr || !strings.Contains(out, "agents broke") {
		t.Fatalf("got %q (error %v)", out, isErr)
	}
}

func TestTskAgentStatusMissingCreatedTimestamp(t *testing.T) {
	task := tskTask("task_1", agentdv1.TaskState_TASK_STATE_RUNNING)
	task.CreatedAt = nil
	env := tskStatusEnv(task)
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box", "wait_seconds": 0})
	if isErr || !strings.Contains(out, "timing: created --:--:--, elapsed 0.0s") {
		t.Fatalf("got %q (error %v)", out, isErr)
	}
}

func TestTskAgentStatusSubSecondBudgetDoesNotPoll(t *testing.T) {
	env := tskStatusEnv(tskTask("task_1", agentdv1.TaskState_TASK_STATE_QUEUED))
	cs := newTestSession(t, env.clients(), func(c *Config) { c.MaxWait = 300 * time.Millisecond })
	// Less than one second of budget means no whole-second poll: the status is reported at once.
	out, isErr := callTool(t, cs, "agent_status", map[string]any{"name": "box"})
	if isErr || !strings.HasPrefix(out, "status: running") {
		t.Fatalf("got %q (error %v)", out, isErr)
	}
	if len(env.tasks.gets) != 0 {
		t.Fatalf("polled %d times with under a second of budget", len(env.tasks.gets))
	}
}

// --- agent_cancel ---

func TestTskAgentCancelRunningTask(t *testing.T) {
	env := tskStatusEnv(tskTask("task_1", agentdv1.TaskState_TASK_STATE_RUNNING))
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_cancel", map[string]any{"name": "box"})
	if isErr || out != "cancel requested for task task_1" {
		t.Fatalf("got %q (error %v)", out, isErr)
	}
	if len(env.tasks.cancelled) != 1 || env.tasks.cancelled[0] != "task_1" {
		t.Fatalf("cancelled %v", env.tasks.cancelled)
	}
}

func TestTskAgentCancelQueuedTask(t *testing.T) {
	env := tskStatusEnv(tskTask("task_1", agentdv1.TaskState_TASK_STATE_QUEUED))
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_cancel", map[string]any{"name": "box"})
	if isErr || out != "cancel requested for task task_1" || len(env.tasks.cancelled) != 1 {
		t.Fatalf("got %q (error %v), cancelled %v", out, isErr, env.tasks.cancelled)
	}
}

func TestTskAgentCancelNoRunningTask(t *testing.T) {
	cases := map[string]*tskEnv{
		"finished task": tskStatusEnv(tskTask("task_1", agentdv1.TaskState_TASK_STATE_SUCCEEDED)),
		"no agent":      tskNewEnv(),
		"no tasks":      tskStatusEnv(),
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			cs := newTestSession(t, env.clients())
			out, isErr := callTool(t, cs, "agent_cancel", map[string]any{"name": "box"})
			if isErr || out != "no running task" || len(env.tasks.cancelled) != 0 {
				t.Fatalf("got %q (error %v), cancelled %v", out, isErr, env.tasks.cancelled)
			}
		})
	}
}

func TestTskAgentCancelErrors(t *testing.T) {
	t.Run("unknown machine", func(t *testing.T) {
		cs := newTestSession(t, tskNewEnv().clients())
		out, isErr := callTool(t, cs, "agent_cancel", map[string]any{"name": "ghost"})
		if !isErr || !strings.Contains(out, "no machine named") {
			t.Fatalf("got %q (error %v)", out, isErr)
		}
	})
	t.Run("cancel fails", func(t *testing.T) {
		env := tskStatusEnv(tskTask("task_1", agentdv1.TaskState_TASK_STATE_RUNNING))
		env.tasks.cancelErr = connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not yours"))
		cs := newTestSession(t, env.clients())
		out, isErr := callTool(t, cs, "agent_cancel", map[string]any{"name": "box"})
		if !isErr || !strings.Contains(out, "not yours") {
			t.Fatalf("got %q (error %v)", out, isErr)
		}
	})
	t.Run("list tasks fails", func(t *testing.T) {
		env := tskStatusEnv(tskTask("task_1", agentdv1.TaskState_TASK_STATE_RUNNING))
		env.tasks.listErr = connect.NewError(connect.CodeInternal, fmt.Errorf("list broke"))
		cs := newTestSession(t, env.clients())
		out, isErr := callTool(t, cs, "agent_cancel", map[string]any{"name": "box"})
		if !isErr || !strings.Contains(out, "list broke") {
			t.Fatalf("got %q (error %v)", out, isErr)
		}
	})
}

func TestTskAgentToolsRegistered(t *testing.T) {
	cs := newTestSession(t, tskNewEnv().clients())
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	for _, n := range []string{"agent_models", "agent_task", "agent_status", "agent_cancel"} {
		if !got[n] {
			t.Errorf("tool %q not registered", n)
		}
	}
}

func TestTskAgentTaskReportsQueuedBehindRunning(t *testing.T) {
	env := tskNewEnv()
	env.agents.agents = []*agentdv1.Agent{tskAgent("agent_1", "qwen")}
	env.tasks.tasks = []*agentdv1.Task{
		tskTask("t1", agentdv1.TaskState_TASK_STATE_RUNNING),
		tskTask("t2", agentdv1.TaskState_TASK_STATE_QUEUED),
		tskTask("t0", agentdv1.TaskState_TASK_STATE_SUCCEEDED),
	}
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_task", map[string]any{"name": "box", "task": "go", "model": "qwen"})
	if isErr || !strings.Contains(out, "queued") || !strings.Contains(out, "behind 2 unfinished") || strings.Contains(out, "started") {
		t.Fatalf("a task created while others are unfinished must be reported as queued behind 2; got err=%v %q", isErr, out)
	}
}

func TestTskAgentTaskIdleAgentStartsImmediately(t *testing.T) {
	env := tskNewEnv()
	env.agents.agents = []*agentdv1.Agent{tskAgent("agent_1", "qwen")}
	env.tasks.tasks = []*agentdv1.Task{tskTask("t0", agentdv1.TaskState_TASK_STATE_SUCCEEDED)}
	cs := newTestSession(t, env.clients())
	out, isErr := callTool(t, cs, "agent_task", map[string]any{"name": "box", "task": "go", "model": "qwen"})
	if isErr || !strings.Contains(out, "started") || strings.Contains(out, "queued") {
		t.Fatalf("an idle agent must report started; got err=%v %q", isErr, out)
	}
}
