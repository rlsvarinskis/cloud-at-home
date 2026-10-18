package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/agentd/v1"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/service"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/store"
)

// setTaskState moves a task to state through the store, the way the worker does. Tests use it to set up
// running or finished tasks.
func (e *env) setTaskState(id string, state domain.TaskState) *domain.Task {
	e.t.Helper()
	ctx := context.Background()
	t, err := e.st.GetTask(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	t.State = state
	if err := e.st.UpdateTask(ctx, t); err != nil {
		e.t.Fatal(err)
	}
	return t
}

func (e *env) getTaskProto(c *clients, id string) *agentdv1.Task {
	e.t.Helper()
	res, err := c.Task.GetTask(context.Background(), connect.NewRequest(&agentdv1.GetTaskRequest{Id: id}))
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Msg
}

func TestCreateTaskRules(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()
	a := e.createAgent(ed, "p1", "worker", "vm_p1")

	create := func(mutate func(*agentdv1.CreateTaskRequest)) (*agentdv1.Task, error) {
		req := &agentdv1.CreateTaskRequest{AgentId: a.Id, Prompt: "do the thing"}
		mutate(req)
		res, err := ed.Task.CreateTask(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}

	cases := []struct {
		name   string
		mutate func(*agentdv1.CreateTaskRequest)
		want   connect.Code
	}{
		{"agent id missing", func(r *agentdv1.CreateTaskRequest) { r.AgentId = "" }, connect.CodeInvalidArgument},
		{"agent unknown", func(r *agentdv1.CreateTaskRequest) { r.AgentId = "agent_nope" }, connect.CodeNotFound},
		{"prompt empty", func(r *agentdv1.CreateTaskRequest) { r.Prompt = "" }, connect.CodeInvalidArgument},
		{"prompt blank", func(r *agentdv1.CreateTaskRequest) { r.Prompt = " \n\t " }, connect.CodeInvalidArgument},
		{"prompt too big", func(r *agentdv1.CreateTaskRequest) { r.Prompt = strings.Repeat("p", 64<<10+1) }, connect.CodeInvalidArgument},
		{"prompt with NUL", func(r *agentdv1.CreateTaskRequest) { r.Prompt = "a\x00b" }, connect.CodeInvalidArgument},
		{"check command too big", func(r *agentdv1.CreateTaskRequest) {
			r.CheckCommand = strings.Repeat("c", 16<<10+1)
		}, connect.CodeInvalidArgument},
		{"check command with NUL", func(r *agentdv1.CreateTaskRequest) { r.CheckCommand = "test -f\x00x" }, connect.CodeInvalidArgument},
		{"negative max steps", func(r *agentdv1.CreateTaskRequest) { r.MaxSteps = -1 }, connect.CodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := create(tc.mutate); code(err) != tc.want {
				t.Fatalf("got %v, want %v", code(err), tc.want)
			}
		})
	}

	// Boundaries and defaults.
	exact, err := create(func(r *agentdv1.CreateTaskRequest) {
		r.Prompt = strings.Repeat("p", 64<<10)
		r.CheckCommand = strings.Repeat("c", 16<<10)
	})
	if err != nil {
		t.Fatalf("64 KiB prompt and 16 KiB check rejected: %v", err)
	}
	if exact.State != agentdv1.TaskState_TASK_STATE_QUEUED || exact.MaxSteps != 40 || exact.AgentId != a.Id ||
		exact.ProjectId != "p1" || exact.Steps != 0 || exact.Attempts != 0 || exact.CreatedAt == nil {
		t.Fatalf("new task: %+v", exact)
	}
	if tk, err := create(func(r *agentdv1.CreateTaskRequest) { r.MaxSteps = 500 }); err != nil || tk.MaxSteps != 200 {
		t.Fatalf("max_steps 500: %v %+v (want capped at 200)", err, tk)
	}
	if tk, err := create(func(r *agentdv1.CreateTaskRequest) { r.MaxSteps = 7 }); err != nil || tk.MaxSteps != 7 {
		t.Fatalf("max_steps 7: %v %+v", err, tk)
	}
}

func TestCreateTaskRequiresActiveAgent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.createAgent(e.editor(), "p1", "parked", "vm_p1")

	for _, state := range []domain.AgentState{domain.AgentOrphaned, domain.AgentDeleting} {
		cur, err := e.st.GetAgent(ctx, a.Id)
		if err != nil {
			t.Fatal(err)
		}
		cur.State = state
		if err := e.st.UpdateAgent(ctx, cur); err != nil {
			t.Fatal(err)
		}
		_, err = e.editor().Task.CreateTask(ctx, connect.NewRequest(&agentdv1.CreateTaskRequest{AgentId: a.Id, Prompt: "x"}))
		if code(err) != connect.CodeFailedPrecondition {
			t.Fatalf("agent %s: code = %v", state, code(err))
		}
	}
}

func TestCreateTaskIdempotency(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()
	a := e.createAgent(ed, "p1", "idem", "vm_p1")
	b := e.createAgent(ed, "p1", "idem-two", "vm_p1")
	req := func(agent, key string) *agentdv1.CreateTaskRequest {
		return &agentdv1.CreateTaskRequest{AgentId: agent, Prompt: "once", IdempotencyKey: key}
	}

	first, err := ed.Task.CreateTask(ctx, connect.NewRequest(req(a.Id, "k1")))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := ed.Task.CreateTask(ctx, connect.NewRequest(req(a.Id, "k1")))
	if err != nil {
		t.Fatal(err)
	}
	if replay.Msg.Id != first.Msg.Id {
		t.Fatalf("replay created %s, want %s", replay.Msg.Id, first.Msg.Id)
	}
	// The same key for a different agent of the project is a conflicting reuse, not a replay.
	if _, err := ed.Task.CreateTask(ctx, connect.NewRequest(req(b.Id, "k1"))); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("key reused for another agent: %v", code(err))
	}
	// Tasks without a key are never deduplicated.
	x := e.createTask(ed, a.Id, "again")
	y := e.createTask(ed, a.Id, "again")
	if x.Id == y.Id {
		t.Fatal("keyless tasks deduplicated")
	}
}

func TestGetTaskLongPoll(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()
	a := e.createAgent(ed, "p1", "poller", "vm_p1")
	task := e.createTask(ed, a.Id, "wait for me")

	// Without wait_for_not the call answers at once with the current state.
	start := time.Now()
	if got := e.getTaskProto(ed, task.Id); got.State != agentdv1.TaskState_TASK_STATE_QUEUED {
		t.Fatalf("plain get: %v", got.State)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("plain get waited")
	}

	// wait_seconds 0 never blocks even when the state still matches.
	res, err := ed.Task.GetTask(ctx, connect.NewRequest(&agentdv1.GetTaskRequest{
		Id: task.Id, WaitForNot: agentdv1.TaskState_TASK_STATE_QUEUED, WaitSeconds: 0,
	}))
	if err != nil || res.Msg.State != agentdv1.TaskState_TASK_STATE_QUEUED {
		t.Fatalf("zero wait: %v %v", err, res)
	}

	// A worker moves the task to RUNNING shortly after the call starts; the long-poll returns then.
	go func() {
		time.Sleep(50 * time.Millisecond)
		e.setTaskState(task.Id, domain.TaskRunning)
	}()
	start = time.Now()
	res, err = ed.Task.GetTask(ctx, connect.NewRequest(&agentdv1.GetTaskRequest{
		Id: task.Id, WaitForNot: agentdv1.TaskState_TASK_STATE_QUEUED, WaitSeconds: 1,
	}))
	if err != nil || res.Msg.State != agentdv1.TaskState_TASK_STATE_RUNNING {
		t.Fatalf("long poll: %v %v", err, res)
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("long poll returned late: %v", elapsed)
	}

	// Nothing changes: the call gives up when its wait is over and returns the unchanged task.
	start = time.Now()
	res, err = ed.Task.GetTask(ctx, connect.NewRequest(&agentdv1.GetTaskRequest{
		Id: task.Id, WaitForNot: agentdv1.TaskState_TASK_STATE_RUNNING, WaitSeconds: 1,
	}))
	if err != nil || res.Msg.State != agentdv1.TaskState_TASK_STATE_RUNNING {
		t.Fatalf("expired wait: %v %v", err, res)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("expired wait took %v", elapsed)
	}
}

func TestCancelTaskSemantics(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()
	a := e.createAgent(ed, "p1", "canceller", "vm_p1")
	cancel := func(id string) (*agentdv1.Task, error) {
		res, err := ed.Task.CancelTask(ctx, connect.NewRequest(&agentdv1.CancelTaskRequest{Id: id}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}

	// Queued: cancelled at once, and repeating the call changes nothing.
	queued := e.createTask(ed, a.Id, "never started")
	c1, err := cancel(queued.Id)
	if err != nil {
		t.Fatal(err)
	}
	if c1.State != agentdv1.TaskState_TASK_STATE_CANCELLED || c1.FinishedAt == nil || c1.Result != "[cancelled]" {
		t.Fatalf("cancelled queued task: %+v", c1)
	}
	c2, err := cancel(queued.Id)
	if err != nil {
		t.Fatal(err)
	}
	if c2.State != agentdv1.TaskState_TASK_STATE_CANCELLED || !c2.FinishedAt.AsTime().Equal(c1.FinishedAt.AsTime()) {
		t.Fatalf("second cancel of a final task changed it: %+v", c2)
	}

	// Running: flagged for the worker, still RUNNING until it notices; repeating the call is harmless.
	running := e.createTask(ed, a.Id, "in progress")
	e.setTaskState(running.Id, domain.TaskRunning)
	r1, err := cancel(running.Id)
	if err != nil {
		t.Fatal(err)
	}
	if r1.State != agentdv1.TaskState_TASK_STATE_RUNNING || !r1.CancelRequested || r1.FinishedAt != nil {
		t.Fatalf("cancelled running task: %+v", r1)
	}
	r2, err := cancel(running.Id)
	if err != nil || !r2.CancelRequested || r2.State != agentdv1.TaskState_TASK_STATE_RUNNING {
		t.Fatalf("repeat cancel of running task: %v %+v", err, r2)
	}

	// Finished tasks come back unchanged.
	done := e.createTask(ed, a.Id, "already done")
	e.setTaskState(done.Id, domain.TaskSucceeded)
	d1, err := cancel(done.Id)
	if err != nil {
		t.Fatal(err)
	}
	if d1.State != agentdv1.TaskState_TASK_STATE_SUCCEEDED || d1.CancelRequested {
		t.Fatalf("cancel of finished task: %+v", d1)
	}

	if _, err := cancel("task_missing"); code(err) != connect.CodeNotFound {
		t.Fatalf("cancel unknown task: %v", code(err))
	}
}

func TestListTasksSelectorsFiltersAndPaging(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()
	a := e.createAgent(ed, "p1", "lister-a", "vm_p1")
	b := e.createAgent(ed, "p1", "lister-b", "vm_p1")
	foreign := e.createAgent(e.otherTenant(), "p2", "foreign", "vm_p2")

	var aTasks []string
	for range 3 {
		aTasks = append(aTasks, e.createTask(ed, a.Id, "a").Id)
	}
	bTask := e.createTask(ed, b.Id, "b")
	e.createTask(e.otherTenant(), foreign.Id, "foreign")
	e.setTaskState(aTasks[0], domain.TaskSucceeded)

	list := func(c *clients, req *agentdv1.ListTasksRequest) (*connect.Response[agentdv1.ListTasksResponse], error) {
		return c.Task.ListTasks(ctx, connect.NewRequest(req))
	}

	// Exactly one selector.
	if _, err := list(ed, &agentdv1.ListTasksRequest{}); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("no selector: %v", code(err))
	}
	if _, err := list(ed, &agentdv1.ListTasksRequest{AgentId: a.Id, ProjectId: "p1"}); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("both selectors: %v", code(err))
	}
	if _, err := list(ed, &agentdv1.ListTasksRequest{ProjectId: "*"}); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("wildcard project: %v", code(err))
	}

	// By agent, and by agent with a state filter.
	res, err := list(ed, &agentdv1.ListTasksRequest{AgentId: a.Id})
	if err != nil || len(res.Msg.Tasks) != 3 {
		t.Fatalf("agent a tasks: %v %d", err, len(res.Msg.GetTasks()))
	}
	res, err = list(ed, &agentdv1.ListTasksRequest{AgentId: a.Id, State: agentdv1.TaskState_TASK_STATE_QUEUED})
	if err != nil || len(res.Msg.Tasks) != 2 {
		t.Fatalf("agent a queued: %v %d", err, len(res.Msg.GetTasks()))
	}
	res, err = list(ed, &agentdv1.ListTasksRequest{AgentId: b.Id})
	if err != nil || len(res.Msg.Tasks) != 1 || res.Msg.Tasks[0].Id != bTask.Id {
		t.Fatalf("agent b tasks: %v %+v", err, res)
	}

	// By project, with a state filter.
	res, err = list(ed, &agentdv1.ListTasksRequest{ProjectId: "p1", State: agentdv1.TaskState_TASK_STATE_SUCCEEDED})
	if err != nil || len(res.Msg.Tasks) != 1 || res.Msg.Tasks[0].Id != aTasks[0] {
		t.Fatalf("project succeeded: %v %+v", err, res)
	}

	// Pages of 2 across the project's four tasks: complete, no duplicates.
	var seen []string
	token := ""
	for range 5 {
		res, err := list(ed, &agentdv1.ListTasksRequest{ProjectId: "p1", PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, tk := range res.Msg.Tasks {
			seen = append(seen, tk.Id)
		}
		token = res.Msg.NextPageToken
		if token == "" {
			break
		}
	}
	if len(seen) != 4 || token != "" {
		t.Fatalf("paged tasks = %v (token %q)", seen, token)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i-1] >= seen[i] {
			t.Fatalf("tasks not in ID order: %v", seen)
		}
	}
}

// recordingStore snapshots the agent's tasks just before the agent row is removed, so a test can see what
// DeleteAgent did to them before the cascade removed them.
type recordingStore struct {
	store.Store
	before []*domain.Task
}

func (r *recordingStore) DeleteAgent(ctx context.Context, id string) error {
	tasks, err := r.Store.ListTasks(ctx, id, "", "", "", 0)
	if err != nil {
		return err
	}
	r.before = tasks
	return r.Store.DeleteAgent(ctx, id)
}

func TestDeleteAgentCancelsItsTasks(t *testing.T) {
	var rs *recordingStore
	e := newEnv(t, func(d *service.Deps) {
		rs = &recordingStore{Store: d.Store}
		d.Store = rs
	})
	ed := e.editor()
	ctx := context.Background()
	a := e.createAgent(ed, "p1", "doomed", "vm_p1")
	queued := e.createTask(ed, a.Id, "queued")
	running := e.createTask(ed, a.Id, "running")
	e.setTaskState(running.Id, domain.TaskRunning)
	done := e.createTask(ed, a.Id, "done")
	e.setTaskState(done.Id, domain.TaskSucceeded)

	if _, err := ed.Agent.DeleteAgent(ctx, connect.NewRequest(&agentdv1.DeleteAgentRequest{Id: a.Id})); err != nil {
		t.Fatal(err)
	}
	seen := map[string]*domain.Task{}
	for _, tk := range rs.before {
		seen[tk.ID] = tk
	}
	if q := seen[queued.Id]; q == nil || q.State != domain.TaskCancelled || q.FinishedAt.IsZero() || q.Result != "[cancelled]" {
		t.Fatalf("queued task at delete: %+v", q)
	}
	if r := seen[running.Id]; r == nil || r.State != domain.TaskRunning || !r.CancelRequested {
		t.Fatalf("running task at delete: %+v", r)
	}
	if d := seen[done.Id]; d == nil || d.State != domain.TaskSucceeded || d.CancelRequested {
		t.Fatalf("finished task at delete: %+v", d)
	}
	// The tasks are gone with the agent.
	if _, err := ed.Task.GetTask(ctx, connect.NewRequest(&agentdv1.GetTaskRequest{Id: queued.Id})); code(err) != connect.CodeNotFound {
		t.Fatalf("task after agent delete: %v", code(err))
	}
}

func TestListStepsPaging(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()
	a := e.createAgent(ed, "p1", "stepper", "vm_p1")
	tk := e.createTask(ed, a.Id, "steps")
	for i := range 5 {
		exit := 0
		if _, err := e.st.AppendStep(ctx, &domain.Step{TaskID: tk.Id, Command: "cmd", Output: "out", ExitCode: &exit, At: time.Unix(int64(i), 0)}); err != nil {
			t.Fatal(err)
		}
	}
	list := func(afterSeq, limit int32) []*agentdv1.Step {
		t.Helper()
		res, err := ed.Task.ListSteps(ctx, connect.NewRequest(&agentdv1.ListStepsRequest{TaskId: tk.Id, AfterSeq: afterSeq, Limit: limit}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Steps
	}

	all := list(0, 0)
	if len(all) != 5 || all[0].Seq != 1 || all[4].Seq != 5 {
		t.Fatalf("default page: %d steps", len(all))
	}
	if all[0].ExitCode == nil || all[0].At == nil {
		t.Fatalf("step fields not converted: %+v", all[0])
	}
	if got := list(3, 0); len(got) != 2 || got[0].Seq != 4 {
		t.Fatalf("after_seq 3: %+v", got)
	}
	if got := list(-5, 0); len(got) != 5 {
		t.Fatalf("negative after_seq: %d steps", len(got))
	}
	if got := list(5, 0); len(got) != 0 {
		t.Fatalf("after last seq: %d steps", len(got))
	}
	if got := list(0, 100000); len(got) != 5 {
		t.Fatalf("huge limit: %d steps", len(got))
	}

	// Walk the task two steps at a time using after_seq.
	var seqs []int32
	for after := int32(0); ; {
		page := list(after, 2)
		if len(page) == 0 {
			break
		}
		for _, s := range page {
			seqs = append(seqs, s.Seq)
		}
		after = page[len(page)-1].Seq
	}
	if len(seqs) != 5 || seqs[4] != 5 {
		t.Fatalf("walked seqs = %v", seqs)
	}

	_, err := ed.Task.ListSteps(ctx, connect.NewRequest(&agentdv1.ListStepsRequest{TaskId: "task_missing"}))
	if code(err) != connect.CodeNotFound {
		t.Fatalf("steps of unknown task: %v", code(err))
	}
}
