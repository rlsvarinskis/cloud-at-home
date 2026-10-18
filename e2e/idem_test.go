package e2e_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/agentd/v1"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
)

// Scenario 7: retried requests create one resource, and a retried StartJob does not run a command twice.
func TestIdempotency(t *testing.T) {
	eachBackend(t, true, func(t *testing.T, backend string) {
		w := newWorld(t, backend)
		ctx := context.Background()
		vm := w.runningVM("idem")
		ed := w.editor()
		const model = "scripted-idem"
		w.hub.script(model, llm.Say("unused"))

		a1, err := ed.Agent.CreateAgent(ctx, connect.NewRequest(&agentdv1.CreateAgentRequest{
			ProjectId: "p1", Name: "twice", VmId: vm.Id, Model: model, IdempotencyKey: "agent-key"}))
		must(t, err)
		a2, err := ed.Agent.CreateAgent(ctx, connect.NewRequest(&agentdv1.CreateAgentRequest{
			ProjectId: "p1", Name: "twice", VmId: vm.Id, Model: model, IdempotencyKey: "agent-key"}))
		must(t, err)
		if a1.Msg.Id != a2.Msg.Id {
			t.Errorf("retried CreateAgent made a second agent: %s and %s", a1.Msg.Id, a2.Msg.Id)
		}
		list, err := ed.Agent.ListAgents(ctx, connect.NewRequest(&agentdv1.ListAgentsRequest{ProjectId: "p1"}))
		must(t, err)
		if len(list.Msg.Agents) != 1 {
			t.Errorf("agents in p1 = %d, want 1", len(list.Msg.Agents))
		}
		if holds := w.getVM(vm.Id).Holds; len(holds) != 1 || holds[0] != "agentd/"+a1.Msg.Id {
			t.Errorf("holds = %v, want exactly [agentd/%s]", holds, a1.Msg.Id)
		}

		t1, err := ed.Task.CreateTask(ctx, connect.NewRequest(&agentdv1.CreateTaskRequest{
			AgentId: a1.Msg.Id, Prompt: "once", IdempotencyKey: "task-key"}))
		must(t, err)
		t2, err := ed.Task.CreateTask(ctx, connect.NewRequest(&agentdv1.CreateTaskRequest{
			AgentId: a1.Msg.Id, Prompt: "once", IdempotencyKey: "task-key"}))
		must(t, err)
		if t1.Msg.Id != t2.Msg.Id {
			t.Errorf("retried CreateTask made a second task: %s and %s", t1.Msg.Id, t2.Msg.Id)
		}
		tasks, err := ed.Task.ListTasks(ctx, connect.NewRequest(&agentdv1.ListTasksRequest{AgentId: a1.Msg.Id}))
		must(t, err)
		if len(tasks.Msg.Tasks) != 1 {
			t.Errorf("tasks of the agent = %d, want 1", len(tasks.Msg.Tasks))
		}

		// A retried StartJob (same key, same request) returns the first job and never runs the command again.
		req := &vmdv1.StartJobRequest{VmId: vm.Id, Command: "echo once", Workdir: "/home/agent", WaitSeconds: 1,
			IdempotencyKey: "job-key"}
		j1, err := w.vm.StartJob(ctx, req)
		must(t, err)
		j2, err := w.vm.StartJob(ctx, req)
		must(t, err)
		if j1.GetJob().GetId() == "" || j1.GetJob().GetId() != j2.GetJob().GetId() {
			t.Errorf("retried StartJob returned job %q, first was %q", j2.GetJob().GetId(), j1.GetJob().GetId())
		}
		if n := len(w.commandRuns("echo once")); n != 1 {
			t.Errorf("command ran %d times on the node, want 1", n)
		}
	})
}
