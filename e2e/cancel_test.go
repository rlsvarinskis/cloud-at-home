package e2e_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/agentd/v1"
	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/node/v1"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
)

// Scenario 6: cancelling a task, both while the model is thinking and while a command runs in the VM.
func TestCancel(t *testing.T) {
	eachBackend(t, true, func(t *testing.T, backend string) {
		t.Run("while the model call is blocked", func(t *testing.T) { cancelWhileModelBlocked(t, backend) })
		t.Run("while a long command runs", func(t *testing.T) { cancelWhileJobRuns(t, backend) })
	})
}

func cancelWhileModelBlocked(t *testing.T, backend string) {
	w := newWorld(t, backend)
	ctx := context.Background()
	vm := w.runningVM("thinker")
	ed := w.editor()
	const model = "scripted-cancel-llm"
	blocked := make(chan struct{}) // never released
	w.hub.script(model, llm.FakeReply{Block: blocked})
	agent := w.createAgent(ed, "p1", "thinker-agent", vm.Id, model)
	task := w.createTask(ed, agent.Id, "think forever", "")

	entered := make(chan struct{}, 1)
	w.hub.setOnChat(func(string, int) {
		select {
		case entered <- struct{}{}:
		default:
		}
	})
	done := runAsync(ctx, w.run)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the model was never called")
	}
	_, err := ed.Task.CancelTask(ctx, connect.NewRequest(&agentdv1.CancelTaskRequest{Id: task.Id}))
	must(t, err)

	if err := awaitErr(t, done, "RunOnce"); err != nil {
		t.Fatalf("RunOnce after cancel: %v", err)
	}
	if got := w.getTask(ed, task.Id); got.State != agentdv1.TaskState_TASK_STATE_CANCELLED {
		t.Errorf("task state %v, want CANCELLED", got.State)
	}
}

func cancelWhileJobRuns(t *testing.T, backend string) {
	w := newWorld(t, backend)
	ctx := context.Background()
	vm := w.runningVM("worker")
	ed := w.editor()
	const model = "scripted-cancel-job"
	const cmd = "sleep 3600"
	w.hub.script(model, llm.CallTool("bash", map[string]any{"command": cmd}), llm.Say("Done.\nVERDICT: SUCCESS"))
	started := make(chan struct{}, 1)
	w.setExec(func(_ string, spec *nodev1.JobSpec) (int, string, string, time.Duration) {
		if spec.GetCommand() == cmd {
			select {
			case started <- struct{}{}:
			default:
			}
			return 0, "", "", time.Hour // runs until cancelled
		}
		return 0, "", "", 0
	})
	agent := w.createAgent(ed, "p1", "long-agent", vm.Id, model)
	task := w.createTask(ed, agent.Id, "sleep for an hour", "")

	done := runAsync(ctx, w.run)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the command never started")
	}
	_, err := ed.Task.CancelTask(ctx, connect.NewRequest(&agentdv1.CancelTaskRequest{Id: task.Id}))
	must(t, err)

	if err := awaitErr(t, done, "RunOnce"); err != nil {
		t.Fatalf("RunOnce after cancel: %v", err)
	}
	if got := w.getTask(ed, task.Id); got.State != agentdv1.TaskState_TASK_STATE_CANCELLED {
		t.Errorf("task state %v, want CANCELLED", got.State)
	}
	// The job lives in the VM, so cancelling the task must also cancel it there.
	j := jobFor(w.jobs(vm.Id), cmd)
	if j == nil {
		t.Fatalf("no vmd job for %q", cmd)
	}
	if j.State != vmdv1.JobState_JOB_STATE_CANCELLED {
		t.Errorf("vmd job state %v, want CANCELLED (the runner must cancel the command it started)", j.State)
	}
}
