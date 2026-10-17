package e2e_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentdv1 "example.com/homecloud/gen/homecloud/agentd/v1"
	nodev1 "example.com/homecloud/gen/homecloud/node/v1"
	vmdv1 "example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/internal/agentd/llm"
)

// Scenario 5: failures in the middle of a task.
func TestFailureHandling(t *testing.T) {
	eachBackend(t, true, func(t *testing.T, backend string) {
		t.Run("node goes down mid-task", func(t *testing.T) { nodeDownMidTask(t, backend) })
		t.Run("VM crashes while a command runs", func(t *testing.T) { vmCrashMidCommand(t, backend) })
		t.Run("runner stopped mid-task is resumed by another runner", func(t *testing.T) { runnerStoppedMidTask(t, backend) })
	})
}

func nodeDownMidTask(t *testing.T, backend string) {
	w := newWorld(t, backend)
	ctx := context.Background()
	vm := w.runningVM("flaky")
	ed := w.editor()
	const model = "scripted-down"
	const cmd1, cmd2 = "echo one > a.txt", "echo two > b.txt"
	w.hub.script(model,
		llm.CallTool("bash", map[string]any{"command": cmd1}),
		llm.CallTool("bash", map[string]any{"command": cmd2}),
		llm.Say("Done.\nVERDICT: SUCCESS"))
	agent := w.createAgent(ed, "p1", "flaky-agent", vm.Id, model)
	task := w.createTask(ed, agent.Id, "two files", "")

	// The node dies after the first command has been recorded, just before the model asks for the second.
	w.hub.setOnChat(func(_ string, n int) {
		if n == 2 {
			w.node.SetDown(true)
		}
	})
	claimed, err := w.run.RunOnce(ctx)
	must(t, err)
	if !claimed {
		t.Fatal("RunOnce claimed nothing")
	}
	got := w.getTask(ed, task.Id)
	if got.State != agentdv1.TaskState_TASK_STATE_QUEUED {
		t.Fatalf("after the node went down: task state %v, want QUEUED (requeued)", got.State)
	}
	if got.Steps != 1 {
		t.Errorf("steps after requeue = %d, want 1", got.Steps)
	}

	w.node.SetDown(false)
	w.tickVMD(1)
	w.hub.setOnChat(nil)
	w.runTasks()

	got = w.getTask(ed, task.Id)
	if got.State != agentdv1.TaskState_TASK_STATE_SUCCEEDED {
		t.Errorf("final state %v (result %q), want SUCCEEDED", got.State, got.Result)
	}
	// Known bug candidate: the first StartJob that hit the dead node already created a vmd job under the same
	// idempotency key and marked it FAILED ("could not start"). The retry replays that record, so the command
	// never runs on the node.
	runs := w.commandRuns(cmd2)
	if len(runs) != 1 {
		t.Errorf("the second command ran %d times on the node after recovery, want 1", len(runs))
	}
	if j := jobFor(w.jobs(vm.Id), cmd2); j == nil || j.State != vmdv1.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("vmd job for %q: %+v, want SUCCEEDED", cmd2, j)
	}
	steps, err := ed.Task.ListSteps(ctx, connect.NewRequest(&agentdv1.ListStepsRequest{TaskId: task.Id}))
	must(t, err)
	if n := len(steps.Msg.Steps); n != 2 {
		t.Fatalf("got %d steps, want 2", n)
	}
	if s := steps.Msg.Steps[1]; s.ExitCode == nil || *s.ExitCode != 0 {
		t.Errorf("second step has exit code %v, want 0 from a command that really ran", s.ExitCode)
	}
}

func vmCrashMidCommand(t *testing.T, backend string) {
	w := newWorld(t, backend)
	ctx := context.Background()
	vm := w.runningVM("crashy")
	ed := w.editor()
	const model = "scripted-crash"
	const cmd = "sleep 3600 && echo never"
	fake := w.hub.script(model,
		llm.CallTool("bash", map[string]any{"command": cmd}),
		llm.Say("Done.\nVERDICT: SUCCESS"))
	started := make(chan struct{}, 1)
	w.setExec(func(_ string, spec *nodev1.JobSpec) (int, string, string, time.Duration) {
		if spec.GetCommand() == cmd {
			select {
			case started <- struct{}{}:
			default:
			}
			return 0, "", "", time.Hour // never finishes on the test clock
		}
		return 0, "", "", 0
	})
	agent := w.createAgent(ed, "p1", "crash-agent", vm.Id, model)
	task := w.createTask(ed, agent.Id, "run a long thing", "")

	done := runAsync(ctx, w.run)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the command never started on the node")
	}
	w.node.Crash(vm.Id, "killed by the test")

	if err := awaitErr(t, done, "RunOnce"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := w.getTask(ed, task.Id); got.State != agentdv1.TaskState_TASK_STATE_SUCCEEDED {
		t.Errorf("task state %v (result %q), want SUCCEEDED after the crash", got.State, got.Result)
	}
	if len(fake.Calls) != 2 {
		t.Fatalf("model called %d times, want 2", len(fake.Calls))
	}
	last := fake.Calls[1].Messages[len(fake.Calls[1].Messages)-1]
	if last.Role != "tool" || !strings.Contains(last.Content, "lost") {
		t.Errorf("the model was not told the command was lost: %+v", last)
	}
	jobs := w.jobs(vm.Id)
	if j := jobFor(jobs, cmd); j == nil || j.State != vmdv1.JobState_JOB_STATE_LOST {
		t.Errorf("crashed command job: %+v, want state LOST", j)
	}
}

func runnerStoppedMidTask(t *testing.T, backend string) {
	w := newWorld(t, backend)
	ctx := context.Background()
	vm := w.runningVM("resume")
	ed := w.editor()
	const model = "scripted-resume"
	const cmd = "echo first > one.txt"
	blocked := make(chan struct{}) // never released: the first runner is stopped while the model is thinking
	fake := w.hub.script(model,
		llm.CallTool("bash", map[string]any{"command": cmd}),
		llm.FakeReply{Block: blocked},
		llm.Say("Done.\nVERDICT: SUCCESS"))
	agent := w.createAgent(ed, "p1", "resume-agent", vm.Id, model)
	task := w.createTask(ed, agent.Id, "first then second", "")

	entered := make(chan struct{}, 1)
	w.hub.setOnChat(func(_ string, n int) {
		if n == 2 {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
	})
	stopCtx, stop := context.WithCancel(ctx)
	first := w.newRunner("worker-1", 1)
	done := runAsync(stopCtx, first)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first runner never reached the second model call")
	}
	stop()
	if err := awaitErr(t, done, "first RunOnce"); err != nil {
		t.Fatalf("stopped runner returned an error: %v", err)
	}
	if got := w.getTask(ed, task.Id); got.State != agentdv1.TaskState_TASK_STATE_RUNNING {
		t.Fatalf("after stopping the runner: state %v, want RUNNING (lease held, not finished)", got.State)
	}

	// The first worker's lease expires; a second worker takes the task over and resumes from the stored conversation.
	w.clk.Advance(time.Minute)
	second := w.newRunner("worker-2", 1)
	claimed, err := second.RunOnce(ctx)
	must(t, err)
	if !claimed {
		t.Fatal("second runner did not claim the expired task")
	}
	got := w.getTask(ed, task.Id)
	if got.State != agentdv1.TaskState_TASK_STATE_SUCCEEDED {
		t.Errorf("final state %v (result %q), want SUCCEEDED", got.State, got.Result)
	}
	if runs := w.commandRuns(cmd); len(runs) != 1 {
		t.Errorf("the finished command ran %d times on the node, want exactly once", len(runs))
	}
	if n := w.nodeStartJobs(); n != 1 {
		t.Errorf("node StartJob calls = %d, want 1", n)
	}
	if len(fake.Calls) != 3 {
		t.Errorf("model called %d times, want 3 (the stopped call is retried by the second worker)", len(fake.Calls))
	}
}

// nodeStartJobs counts StartJob calls that reached the fake node.
func (w *world) nodeStartJobs() int {
	n := 0
	for _, c := range w.node.Calls() {
		if c == "StartJob" {
			n++
		}
	}
	return n
}
