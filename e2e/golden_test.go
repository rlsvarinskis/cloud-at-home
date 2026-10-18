package e2e_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"context"
	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1"
	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
)

// Scenario 1: the golden path, with a check command that passes and one that fails.
func TestGoldenPath(t *testing.T) {
	eachBackend(t, true, func(t *testing.T, backend string) {
		t.Run("check passes", func(t *testing.T) { goldenPath(t, backend, 0) })
		t.Run("check fails", func(t *testing.T) { goldenPath(t, backend, 1) })
	})
}

// Scenario 9 (part): the golden path on a project-sharded vmd.
func TestShardedVMD(t *testing.T) {
	t.Run("golden path", func(t *testing.T) { goldenPath(t, backendMemory, 0, withShards(3)) })
	t.Run("golden path check fails", func(t *testing.T) { goldenPath(t, backendMemory, 1, withShards(3)) })
	t.Run("many agents one VM", func(t *testing.T) { manyAgentsOneVM(t, backendMemory, withShards(3)) })
}

// Scenario 2: three agents share one VM and run their tasks concurrently.
func TestManyAgentsOneVM(t *testing.T) {
	eachBackend(t, true, func(t *testing.T, backend string) { manyAgentsOneVM(t, backend) })
}

func goldenPath(t *testing.T, backend string, checkExit int, opts ...worldOption) {
	t.Helper()
	w := newWorld(t, backend, opts...)
	vm := w.runningVM("web")
	ed := w.editor()

	const model = "scripted-golden"
	const cmd = "echo hello > out.txt"
	const check = "test -s out.txt"
	script := w.hub.script(model,
		llm.CallTool("bash", map[string]any{"command": cmd}),
		llm.Say("Done.\nVERDICT: SUCCESS"))
	w.setExec(func(_ string, spec *nodev1.JobSpec) (int, string, string, time.Duration) {
		if spec.GetCommand() == check {
			return checkExit, "", "check output\n", 0
		}
		return 0, "", "", 0
	})

	agent := w.createAgent(ed, "p1", "builder", vm.Id, model)
	workdir := "/home/agent/agents/" + agent.Id
	if agent.Workdir != workdir {
		t.Errorf("agent workdir %q, want the default %q", agent.Workdir, workdir)
	}
	if got := w.getVM(vm.Id).Holds; !slices.Equal(got, []string{"agentd/" + agent.Id}) {
		t.Errorf("VM holds %v, want [agentd/%s]", got, agent.Id)
	}

	task := w.createTask(ed, agent.Id, "write hello to out.txt", check)
	w.runTasks()

	got := w.getTask(ed, task.Id)
	wantState, wantCheckJob := agentdv1.TaskState_TASK_STATE_SUCCEEDED, vmdv1.JobState_JOB_STATE_SUCCEEDED
	if checkExit != 0 {
		wantState, wantCheckJob = agentdv1.TaskState_TASK_STATE_FAILED, vmdv1.JobState_JOB_STATE_FAILED
	}
	if got.State != wantState {
		t.Errorf("task state %v, want %v (verdict %q, result %q)", got.State, wantState, got.Verdict, got.Result)
	}
	if got.Verdict != "SUCCESS" {
		t.Errorf("verdict %q, want SUCCESS (the model's own verdict is recorded even when the check overrides it)", got.Verdict)
	}
	if checkExit != 0 && !strings.Contains(got.Result, "[note: the model reported SUCCESS but the check FAILED]") {
		t.Errorf("result does not explain the override: %q", got.Result)
	}

	steps, err := ed.Task.ListSteps(context.Background(), connect.NewRequest(&agentdv1.ListStepsRequest{TaskId: task.Id}))
	must(t, err)
	if n := len(steps.Msg.Steps); n != 1 {
		t.Fatalf("got %d steps, want 1", n)
	}
	if s := steps.Msg.Steps[0]; s.Command != cmd || s.GetExitCode() != 0 {
		t.Errorf("step = command %q exit %v, want %q exit 0", s.Command, s.GetExitCode(), cmd)
	}

	jobs := w.jobs(vm.Id)
	if j := jobFor(jobs, cmd); j == nil {
		t.Errorf("vmd has no job for %q", cmd)
	} else {
		if j.State != vmdv1.JobState_JOB_STATE_SUCCEEDED {
			t.Errorf("command job state %v, want SUCCEEDED", j.State)
		}
		if j.Workdir != workdir {
			t.Errorf("command job workdir %q, want %q", j.Workdir, workdir)
		}
	}
	if j := jobFor(jobs, check); j == nil {
		t.Errorf("vmd has no job for the check command %q", check)
	} else if j.State != wantCheckJob {
		t.Errorf("check job state %v, want %v", j.State, wantCheckJob)
	}

	if runs := w.commandRuns(cmd); len(runs) != 1 || runs[0].Workdir != workdir {
		t.Errorf("node ran %q %v times, want once in %s", cmd, runs, workdir)
	}
	if runs := w.commandRuns(check); len(runs) != 1 || runs[0].Workdir != workdir {
		t.Errorf("node ran the check %v times, want once in %s", runs, workdir)
	}

	if len(script.Calls) != 2 {
		t.Fatalf("model was called %d times, want 2", len(script.Calls))
	}
	last := script.Calls[1].Messages[len(script.Calls[1].Messages)-1]
	if last.Role != "tool" || !strings.Contains(last.Content, "[exit 0]") {
		t.Errorf("the model was not told the command's result: last message %+v", last)
	}
}

func manyAgentsOneVM(t *testing.T, backend string, opts ...worldOption) {
	t.Helper()
	w := newWorld(t, backend, opts...)
	vm := w.runningVM("shared")
	ed := w.editor()

	type agentRun struct {
		agent *agentdv1.Agent
		task  *agentdv1.Task
		cmd   string
	}
	var runs []agentRun
	var taskIDs []string
	for i := 1; i <= 3; i++ {
		model := fmt.Sprintf("scripted-%d", i)
		cmd := fmt.Sprintf("echo agent-%d > out.txt", i)
		w.hub.script(model,
			llm.CallTool("bash", map[string]any{"command": cmd}),
			llm.Say("Done.\nVERDICT: SUCCESS"))
		agent := w.createAgent(ed, "p1", fmt.Sprintf("worker-%d", i), vm.Id, model)
		task := w.createTask(ed, agent.Id, "write my marker", "")
		runs = append(runs, agentRun{agent: agent, task: task, cmd: cmd})
		taskIDs = append(taskIDs, task.Id)
	}

	w.runWorkers(3, taskIDs...)

	for _, r := range runs {
		if got := w.getTask(ed, r.task.Id); got.State != agentdv1.TaskState_TASK_STATE_SUCCEEDED {
			t.Errorf("task of %s: state %v (result %q), want SUCCEEDED", r.agent.Name, got.State, got.Result)
		}
		execs := w.commandRuns(r.cmd)
		if len(execs) != 1 {
			t.Errorf("%q ran %d times, want once", r.cmd, len(execs))
			continue
		}
		if execs[0].Workdir != r.agent.Workdir {
			t.Errorf("%q ran in %q, want its own workdir %q", r.cmd, execs[0].Workdir, r.agent.Workdir)
		}
	}

	res, err := ed.VM.ListVMs(context.Background(), connect.NewRequest(&vmdv1.ListVMsRequest{ProjectId: "p1"}))
	must(t, err)
	var holds []string
	for _, v := range res.Msg.Vms {
		if v.Id == vm.Id {
			holds = v.Holds
		}
	}
	want := []string{}
	for _, r := range runs {
		want = append(want, "agentd/"+r.agent.Id)
	}
	if !slices.Equal(sortedCopy(holds), sortedCopy(want)) {
		t.Errorf("ListVMs holds %v, want %v", holds, want)
	}
}
