package e2e_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/agentd/v1"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
)

// Scenario 4: authorization across the two services.
func TestAuthorizationAcrossServices(t *testing.T) {
	eachBackend(t, true, func(t *testing.T, backend string) {
		w := newWorld(t, backend)
		ctx := context.Background()
		vm := w.runningVM("p1vm")
		ed := w.editor()
		const model = "scripted-authz"
		w.hub.script(model, llm.Say("unused"))
		agent := w.createAgent(ed, "p1", "p1agent", vm.Id, model)
		task := w.createTask(ed, agent.Id, "a p1 task", "")

		t.Run("viewer reads but cannot write", func(t *testing.T) {
			v := w.viewer()
			_, err := v.Agent.CreateAgent(ctx, connect.NewRequest(&agentdv1.CreateAgentRequest{
				ProjectId: "p1", Name: "v-agent", VmId: vm.Id, Model: model}))
			if !codeIs(err, connect.CodePermissionDenied) {
				t.Errorf("viewer CreateAgent: got %v, want PermissionDenied", err)
			}
			_, err = v.Task.CreateTask(ctx, connect.NewRequest(&agentdv1.CreateTaskRequest{AgentId: agent.Id, Prompt: "x"}))
			if !codeIs(err, connect.CodePermissionDenied) {
				t.Errorf("viewer CreateTask: got %v, want PermissionDenied", err)
			}
			_, err = v.Agent.GetAgent(ctx, connect.NewRequest(&agentdv1.GetAgentRequest{Id: agent.Id}))
			must(t, err)
			_, err = v.Task.GetTask(ctx, connect.NewRequest(&agentdv1.GetTaskRequest{Id: task.Id}))
			must(t, err)
		})

		t.Run("another project sees nothing of p1", func(t *testing.T) {
			o := w.otherT()
			_, err := o.Agent.GetAgent(ctx, connect.NewRequest(&agentdv1.GetAgentRequest{Id: agent.Id}))
			if !codeIs(err, connect.CodeNotFound) {
				t.Errorf("GetAgent of p1's agent from p2: got %v, want NotFound", err)
			}
			_, err = o.Task.GetTask(ctx, connect.NewRequest(&agentdv1.GetTaskRequest{Id: task.Id}))
			if !codeIs(err, connect.CodeNotFound) {
				t.Errorf("GetTask from p2: got %v, want NotFound", err)
			}
			_, err = o.Task.ListTasks(ctx, connect.NewRequest(&agentdv1.ListTasksRequest{AgentId: agent.Id}))
			if !codeIs(err, connect.CodeNotFound) {
				t.Errorf("ListTasks of p1's agent from p2: got %v, want NotFound", err)
			}
			_, err = o.Task.CancelTask(ctx, connect.NewRequest(&agentdv1.CancelTaskRequest{Id: task.Id}))
			if !codeIs(err, connect.CodeNotFound) {
				t.Errorf("CancelTask from p2: got %v, want NotFound", err)
			}
			_, err = o.Task.CreateTask(ctx, connect.NewRequest(&agentdv1.CreateTaskRequest{AgentId: agent.Id, Prompt: "sneak"}))
			if !codeIs(err, connect.CodeNotFound) {
				t.Errorf("CreateTask on p1's agent from p2: got %v, want NotFound", err)
			}
			// Listing p1's agents by project name is a project-scoped request, so it is refused outright.
			_, err = o.Agent.ListAgents(ctx, connect.NewRequest(&agentdv1.ListAgentsRequest{ProjectId: "p1"}))
			if !codeIs(err, connect.CodePermissionDenied) {
				t.Errorf("ListAgents(p1) from p2: got %v, want PermissionDenied", err)
			}
		})

		t.Run("another project cannot see or attach to p1's VM", func(t *testing.T) {
			o := w.otherT()
			_, err := o.VM.GetVM(ctx, connect.NewRequest(&vmdv1.GetVMRequest{Id: vm.Id}))
			if !codeIs(err, connect.CodeNotFound) {
				t.Errorf("vmd GetVM from p2: got %v, want NotFound", err)
			}
			_, err = o.Agent.CreateAgent(ctx, connect.NewRequest(&agentdv1.CreateAgentRequest{
				ProjectId: "p2", Name: "thief", VmId: vm.Id, Model: model}))
			if !codeIs(err, connect.CodeNotFound) {
				t.Errorf("CreateAgent p2 on p1's VM: got %v, want NotFound", err)
			}
			if holds := w.getVM(vm.Id).Holds; len(holds) != 1 {
				t.Errorf("holds after refused attach: %v, want only the p1 agent", holds)
			}
		})

		t.Run("agentd service key is limited to the service role on vmd", func(t *testing.T) {
			svcTok, err := w.svc.Mint(authz.Principal{Subject: "svc:agentd", Type: authz.Service,
				Bindings: []authz.Binding{{Project: authz.AnyProject, Role: authz.RoleService}}}, time.Hour)
			must(t, err)
			svc := w.withToken(svcTok)

			_, err = svc.VM.GetVM(ctx, connect.NewRequest(&vmdv1.GetVMRequest{Id: vm.Id}))
			must(t, err)
			_, err = svc.VM.CreateVM(ctx, connect.NewRequest(&vmdv1.CreateVMRequest{ProjectId: "p1", Name: "rogue",
				Cpus: 1, MemoryMb: 512, DiskSizeGb: 5, Boot: &vmdv1.CreateVMRequest_ImageId{ImageId: "img_x"}}))
			if !codeIs(err, connect.CodePermissionDenied) {
				t.Errorf("service CreateVM: got %v, want PermissionDenied", err)
			}
			_, err = svc.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: vm.Id}))
			if !codeIs(err, connect.CodePermissionDenied) {
				t.Errorf("service DeleteVM: got %v, want PermissionDenied", err)
			}

			adminClaim, err := w.svc.Mint(authz.Principal{Subject: "svc:agentd", Type: authz.Service,
				Bindings: []authz.Binding{{Project: authz.AnyProject, Role: authz.RoleAdmin}}}, time.Hour)
			must(t, err)
			_, err = w.withToken(adminClaim).VM.ListVMs(ctx, connect.NewRequest(&vmdv1.ListVMsRequest{ProjectId: "p1"}))
			if !codeIs(err, connect.CodeUnauthenticated) {
				t.Errorf("agentd key claiming admin: got %v, want Unauthenticated", err)
			}
		})

		t.Run("no token is unauthenticated", func(t *testing.T) {
			anon := w.withToken("")
			_, err := anon.VM.GetVM(ctx, connect.NewRequest(&vmdv1.GetVMRequest{Id: vm.Id}))
			if !codeIs(err, connect.CodeUnauthenticated) {
				t.Errorf("vmd GetVM without token: got %v, want Unauthenticated", err)
			}
			_, err = anon.Agent.GetAgent(ctx, connect.NewRequest(&agentdv1.GetAgentRequest{Id: agent.Id}))
			if !codeIs(err, connect.CodeUnauthenticated) {
				t.Errorf("agentd GetAgent without token: got %v, want Unauthenticated", err)
			}
		})
	})
}
