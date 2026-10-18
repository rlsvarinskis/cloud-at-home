package e2e_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
)

// Scenario 3: a VM that agents hold cannot be deleted until its agents are gone; a force delete orphans them.
func TestHoldProtection(t *testing.T) {
	eachBackend(t, true, func(t *testing.T, backend string) {
		w := newWorld(t, backend)
		ed := w.editor()
		ctx := context.Background()
		const model = "scripted-holds"
		w.hub.script(model)

		t.Run("delete refused while held, allowed after the agent is deleted", func(t *testing.T) {
			vm := w.runningVM("held")
			agent := w.createAgent(ed, "p1", "keeper", vm.Id, model)

			_, err := ed.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: vm.Id}))
			if !codeIs(err, connect.CodeFailedPrecondition) {
				t.Fatalf("DeleteVM while held: got %v, want FailedPrecondition", err)
			}
			if !strings.Contains(err.Error(), "agentd/"+agent.Id) {
				t.Errorf("error does not name the holder agentd/%s: %v", agent.Id, err)
			}
			if got := w.getVM(vm.Id); got.State != vmdv1.VMState_VM_STATE_RUNNING {
				t.Errorf("refused delete changed the VM: state %v", got.State)
			}

			_, err = ed.Agent.DeleteAgent(ctx, connect.NewRequest(&agentdv1.DeleteAgentRequest{Id: agent.Id}))
			must(t, err)
			if holds := w.getVM(vm.Id).Holds; len(holds) != 0 {
				t.Errorf("holds after DeleteAgent: %v, want none", holds)
			}
			_, err = ed.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: vm.Id}))
			must(t, err)
			w.until(20, "vm gone", func() bool {
				_, err := w.root.VM.GetVM(ctx, connect.NewRequest(&vmdv1.GetVMRequest{Id: vm.Id}))
				return codeIs(err, connect.CodeNotFound)
			})
		})

		t.Run("force delete orphans the agent", func(t *testing.T) {
			vm := w.runningVM("doomed")
			agent := w.createAgent(ed, "p1", "orphan-me", vm.Id, model)

			_, err := ed.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: vm.Id, Force: true}))
			must(t, err)
			w.until(20, "vm gone", func() bool {
				_, err := w.root.VM.GetVM(ctx, connect.NewRequest(&vmdv1.GetVMRequest{Id: vm.Id}))
				return codeIs(err, connect.CodeNotFound)
			})

			task := w.createTask(ed, agent.Id, "still there?", "")
			w.runTasks()

			got := w.getTask(ed, task.Id)
			if got.State != agentdv1.TaskState_TASK_STATE_ERROR {
				t.Errorf("task state %v, want ERROR", got.State)
			}
			if !strings.Contains(got.Result, "the VM no longer exists") {
				t.Errorf("task result %q does not say the VM is gone", got.Result)
			}
			a, err := ed.Agent.GetAgent(ctx, connect.NewRequest(&agentdv1.GetAgentRequest{Id: agent.Id}))
			must(t, err)
			if a.Msg.State != agentdv1.AgentState_AGENT_STATE_ORPHANED {
				t.Errorf("agent state %v, want ORPHANED", a.Msg.State)
			}
		})
	})
}
