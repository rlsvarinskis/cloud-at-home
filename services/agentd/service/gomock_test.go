package service_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.uber.org/mock/gomock"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/agentd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/agentd/v1/agentdv1connect"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
	llmmock "github.com/rlsvarinskis/cloud-at-home/services/agentd/llm/mock"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/service"
	storemock "github.com/rlsvarinskis/cloud-at-home/services/agentd/store/mock"
	vmclientmock "github.com/rlsvarinskis/cloud-at-home/services/agentd/vmclient/mock"
)

// gmAgentSecret is a store failure text that must never reach an API client.
const gmAgentSecret = "pq: password authentication failed for user agentd_prod"

// gmAH is agentd served over HTTP with gomock store, VM client and LLM client.
type gmAH struct {
	t    *testing.T
	ctrl *gomock.Controller
	st   *storemock.MockStore
	vm   *vmclientmock.MockVM
	llm  *llmmock.MockClient
	srv  *httptest.Server
	iss  *authz.Issuer
}

type gmAClients struct {
	Agent agentdv1connect.AgentServiceClient
	Task  agentdv1connect.TaskServiceClient
}

func gmANew(t *testing.T) *gmAH {
	t.Helper()
	ctrl := gomock.NewController(t)
	h := &gmAH{t: t, ctrl: ctrl,
		st:  storemock.NewMockStore(ctrl),
		vm:  vmclientmock.NewMockVM(ctrl),
		llm: llmmock.NewMockClient(ctrl)}
	pub, priv, err := authz.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	h.iss = &authz.Issuer{KeyID: "gm", Key: priv}
	ver := &authz.Verifier{Keys: map[string]authz.TrustedKey{"gm": {Public: pub}}}
	cfg := service.Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h.srv = httptest.NewServer(service.NewHandler(service.Deps{
		Store: h.st, VM: h.vm, LLM: h.llm, Verifier: ver, Config: cfg}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *gmAH) as(role authz.Role, project string) gmAClients {
	h.t.Helper()
	tok, err := h.iss.Mint(authz.Principal{Subject: "user:gm-" + string(role), Type: authz.User,
		Bindings: []authz.Binding{{Project: project, Role: role}}}, time.Hour)
	if err != nil {
		h.t.Fatal(err)
	}
	opt := connect.WithInterceptors(authz.ClientInterceptor(authz.StaticToken(tok)))
	hc := h.srv.Client()
	return gmAClients{
		Agent: agentdv1connect.NewAgentServiceClient(hc, h.srv.URL, opt),
		Task:  agentdv1connect.NewTaskServiceClient(hc, h.srv.URL, opt),
	}
}

// gmAgent returns a fresh active agent in project p1 on VM vm_1.
func gmAgent() *domain.Agent {
	return &domain.Agent{ID: "agent_1", ProjectID: "p1", Name: "helper", VMID: "vm_1", Model: "llama3",
		Workdir: "/home/agent/agents/agent_1", State: domain.AgentActive}
}

func gmTask(id string, state domain.TaskState) *domain.Task {
	return &domain.Task{ID: id, AgentID: "agent_1", ProjectID: "p1", Prompt: "do it", MaxSteps: 40, State: state}
}

func gmInstalled() []llm.ModelInfo { return []llm.ModelInfo{{Name: "llama3"}} }

func gmAssertInternal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if c := connect.CodeOf(err); c != connect.CodeInternal {
		t.Fatalf("code = %v, want %v (err %v)", c, connect.CodeInternal, err)
	}
	if msg := err.Error(); !strings.Contains(msg, "internal error") || strings.Contains(msg, "agentd_prod") {
		t.Fatalf("client error %q is not the generic internal error", msg)
	}
}

// TestGMAgentStoreFailureIsGenericInternal: an arbitrary store failure surfaces as the generic Internal error.
func TestGMAgentStoreFailureIsGenericInternal(t *testing.T) {
	boom := errors.New(gmAgentSecret)
	cases := []struct {
		name    string
		arrange func(h *gmAH)
		call    func(c gmAClients) error
	}{
		{
			name:    "GetAgent",
			arrange: func(h *gmAH) { h.st.EXPECT().GetAgent(gomock.Any(), "agent_1").Return(nil, boom) },
			call: func(c gmAClients) error {
				_, err := c.Agent.GetAgent(context.Background(), connect.NewRequest(&agentdv1.GetAgentRequest{Id: "agent_1"}))
				return err
			},
		},
		{
			name: "ListAgents",
			arrange: func(h *gmAH) {
				h.st.EXPECT().ListAgents(gomock.Any(), "p1", gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, boom)
			},
			call: func(c gmAClients) error {
				_, err := c.Agent.ListAgents(context.Background(), connect.NewRequest(&agentdv1.ListAgentsRequest{ProjectId: "p1"}))
				return err
			},
		},
		{
			name: "CreateAgent",
			arrange: func(h *gmAH) {
				h.vm.EXPECT().GetVM(gomock.Any(), "vm_1").Return(&vmdv1.VM{Id: "vm_1", ProjectId: "p1",
					State: vmdv1.VMState_VM_STATE_RUNNING}, nil)
				h.llm.EXPECT().ListModels(gomock.Any()).Return(gmInstalled(), nil)
				h.st.EXPECT().CreateAgent(gomock.Any(), gomock.Any()).Return(boom)
			},
			call: func(c gmAClients) error {
				_, err := c.Agent.CreateAgent(context.Background(), connect.NewRequest(&agentdv1.CreateAgentRequest{
					ProjectId: "p1", Name: "helper", VmId: "vm_1", Model: "llama3"}))
				return err
			},
		},
		{
			name:    "GetTask",
			arrange: func(h *gmAH) { h.st.EXPECT().GetTask(gomock.Any(), "task_1").Return(nil, boom) },
			call: func(c gmAClients) error {
				_, err := c.Task.GetTask(context.Background(), connect.NewRequest(&agentdv1.GetTaskRequest{Id: "task_1"}))
				return err
			},
		},
		{
			name: "CreateTask",
			arrange: func(h *gmAH) {
				h.st.EXPECT().GetAgent(gomock.Any(), "agent_1").Return(gmAgent(), nil)
				h.st.EXPECT().CreateTask(gomock.Any(), gomock.Any()).Return(boom)
			},
			call: func(c gmAClients) error {
				_, err := c.Task.CreateTask(context.Background(), connect.NewRequest(&agentdv1.CreateTaskRequest{
					AgentId: "agent_1", Prompt: "do it"}))
				return err
			},
		},
		{
			name: "DeleteAgent",
			arrange: func(h *gmAH) {
				h.st.EXPECT().GetAgent(gomock.Any(), "agent_1").Return(gmAgent(), nil)
				h.st.EXPECT().ListTasks(gomock.Any(), "agent_1", gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, boom)
			},
			call: func(c gmAClients) error {
				_, err := c.Agent.DeleteAgent(context.Background(), connect.NewRequest(&agentdv1.DeleteAgentRequest{Id: "agent_1"}))
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := gmANew(t)
			tc.arrange(h)
			gmAssertInternal(t, tc.call(h.as(authz.RoleEditor, "p1")))
		})
	}
}

// TestGMCreateAgentOrderAndRollback: the VM is checked, the model validated, the agent stored, and only then
// the hold placed (agentd/<id>). If the hold fails the agent is deleted again, exactly once, even when that
// delete also fails; the caller still gets the error.
func TestGMCreateAgentOrderAndRollback(t *testing.T) {
	cases := []struct {
		name       string
		holdErr    error
		deleteErr  error
		wantDelete bool
	}{
		{name: "hold succeeds, no rollback"},
		{name: "hold fails, agent rolled back", holdErr: errors.New("vmd: connection reset"), wantDelete: true},
		{name: "hold fails and rollback fails too", holdErr: errors.New("vmd: connection reset"), deleteErr: errors.New("db down"), wantDelete: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := gmANew(t)
			var agentID string
			holdMatch := gomock.Cond(func(holder string) bool {
				return agentID != "" && holder == "agentd/"+agentID
			})
			var holdRet *vmdv1.VM
			if tc.holdErr == nil {
				holdRet = &vmdv1.VM{Id: "vm_1"}
			}
			seq := []any{
				h.vm.EXPECT().GetVM(gomock.Any(), "vm_1").Return(&vmdv1.VM{Id: "vm_1", ProjectId: "p1",
					State: vmdv1.VMState_VM_STATE_RUNNING}, nil),
				h.llm.EXPECT().ListModels(gomock.Any()).Return(gmInstalled(), nil),
				h.st.EXPECT().CreateAgent(gomock.Any(), gomock.Cond(func(a *domain.Agent) bool {
					return a.ProjectID == "p1" && a.VMID == "vm_1" && a.Model == "llama3" && a.State == domain.AgentActive &&
						a.Workdir == "/home/agent/agents/"+a.ID
				})).DoAndReturn(func(_ context.Context, a *domain.Agent) error { agentID = a.ID; return nil }),
				h.vm.EXPECT().AddHold(gomock.Any(), "vm_1", holdMatch).Return(holdRet, tc.holdErr),
			}
			if tc.wantDelete {
				seq = append(seq, h.st.EXPECT().DeleteAgent(gomock.Any(), gomock.Cond(func(id string) bool {
					return id == agentID && id != ""
				})).Return(tc.deleteErr).Times(1))
			}
			gomock.InOrder(seq...)

			_, err := h.as(authz.RoleEditor, "p1").Agent.CreateAgent(context.Background(), connect.NewRequest(&agentdv1.CreateAgentRequest{
				ProjectId: "p1", Name: "helper", VmId: "vm_1", Model: "llama3"}))
			if tc.holdErr != nil {
				if err == nil {
					t.Fatal("want an error when the hold cannot be placed")
				}
				if strings.Contains(err.Error(), "connection reset") {
					t.Fatalf("client error leaks downstream text: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateAgent: %v", err)
			}
		})
	}
}

// TestGMDeleteAgentOrderAndHoldRelease: running and queued tasks are cancelled (finished ones are left alone),
// the agent is deleted, and the VM hold released. A failed release does not fail the call.
func TestGMDeleteAgentOrderAndHoldRelease(t *testing.T) {
	cases := []struct {
		name    string
		holdErr error
	}{
		{name: "release succeeds"},
		{name: "release fails and is swallowed", holdErr: errors.New("vmd unavailable")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := gmANew(t)
			tasks := []*domain.Task{gmTask("task_q", domain.TaskQueued), gmTask("task_r", domain.TaskRunning), gmTask("task_done", domain.TaskSucceeded)}
			gomock.InOrder(
				h.st.EXPECT().GetAgent(gomock.Any(), "agent_1").Return(gmAgent(), nil),
				h.st.EXPECT().ListTasks(gomock.Any(), "agent_1", gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(tasks, nil),
				h.st.EXPECT().GetTask(gomock.Any(), "task_q").Return(gmTask("task_q", domain.TaskQueued), nil),
				h.st.EXPECT().UpdateTask(gomock.Any(), gomock.Cond(func(t *domain.Task) bool {
					return t.ID == "task_q" && t.State == domain.TaskCancelled && t.FinishedAt.IsZero() == false
				})).Return(nil),
				h.st.EXPECT().GetTask(gomock.Any(), "task_r").Return(gmTask("task_r", domain.TaskRunning), nil),
				h.st.EXPECT().UpdateTask(gomock.Any(), gomock.Cond(func(t *domain.Task) bool {
					return t.ID == "task_r" && t.State == domain.TaskRunning && t.CancelRequested
				})).Return(nil),
				h.st.EXPECT().DeleteAgent(gomock.Any(), "agent_1").Return(nil),
				h.vm.EXPECT().RemoveHold(gomock.Any(), "vm_1", "agentd/agent_1").Return(nil, tc.holdErr),
			)

			if _, err := h.as(authz.RoleEditor, "p1").Agent.DeleteAgent(context.Background(),
				connect.NewRequest(&agentdv1.DeleteAgentRequest{Id: "agent_1"})); err != nil {
				t.Fatalf("DeleteAgent: %v", err)
			}
		})
	}
}

// TestGMCreateTaskIdempotentReplay: a retried CreateTask with the same key returns the first task and never
// inserts a second row; a lost insert race (AlreadyExists) returns the winner's task.
func TestGMCreateTaskIdempotentReplay(t *testing.T) {
	cases := []struct {
		name string
		race bool // the insert loses to a concurrent request with the same key
	}{
		{name: "retry after success replays the stored task"},
		{name: "lost insert race returns the winner's task", race: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := gmANew(t)
			c := h.as(authz.RoleEditor, "p1")
			req := &agentdv1.CreateTaskRequest{AgentId: "agent_1", Prompt: "do it", IdempotencyKey: "key-1"}
			var created *domain.Task
			if tc.race {
				gomock.InOrder(
					h.st.EXPECT().GetAgent(gomock.Any(), "agent_1").Return(gmAgent(), nil),
					h.st.EXPECT().GetTaskByIdempotencyKey(gomock.Any(), "p1", "key-1").Return(nil, errs.ErrNotFound),
					h.st.EXPECT().CreateTask(gomock.Any(), gomock.Any()).Return(fmt.Errorf("insert: %w", errs.ErrAlreadyExists)),
					h.st.EXPECT().GetTaskByIdempotencyKey(gomock.Any(), "p1", "key-1").Return(gmTask("task_rival", domain.TaskQueued), nil),
				)
				resp, err := c.Task.CreateTask(context.Background(), connect.NewRequest(req))
				if err != nil {
					t.Fatalf("CreateTask: %v", err)
				}
				if resp.Msg.Id != "task_rival" {
					t.Fatalf("answered with %q, want the rival's task_rival", resp.Msg.Id)
				}
				return
			}
			gomock.InOrder(
				h.st.EXPECT().GetAgent(gomock.Any(), "agent_1").Return(gmAgent(), nil),
				h.st.EXPECT().GetTaskByIdempotencyKey(gomock.Any(), "p1", "key-1").Return(nil, errs.ErrNotFound),
				h.st.EXPECT().CreateTask(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, tk *domain.Task) error {
					created = tk
					return nil
				}),
				// The retry: the key now resolves to the stored task, and CreateTask must not run again.
				h.st.EXPECT().GetAgent(gomock.Any(), "agent_1").Return(gmAgent(), nil),
				h.st.EXPECT().GetTaskByIdempotencyKey(gomock.Any(), "p1", "key-1").DoAndReturn(
					func(context.Context, string, string) (*domain.Task, error) { return created, nil }),
			)
			first, err := c.Task.CreateTask(context.Background(), connect.NewRequest(req))
			if err != nil {
				t.Fatalf("first CreateTask: %v", err)
			}
			second, err := c.Task.CreateTask(context.Background(), connect.NewRequest(req))
			if err != nil {
				t.Fatalf("replayed CreateTask: %v", err)
			}
			if second.Msg.Id != first.Msg.Id {
				t.Fatalf("replay answered %q, want the first task %q", second.Msg.Id, first.Msg.Id)
			}
		})
	}
}
