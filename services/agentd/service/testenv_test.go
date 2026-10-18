package service_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1/agentdv1connect"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/service"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/store"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/vmclient"
)

// clock is a manually advanced clock shared by the service and the token issuer, so tests never sleep for
// timestamps.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeVM is an in-memory stand-in for vmd: VMs with a project and a state, holds recorded on the VM, and
// switches that make individual calls fail.
type fakeVM struct {
	mu            sync.Mutex
	vms           map[string]*vmdv1.VM
	getErr        error
	addHoldErr    error
	removeHoldErr error
}

var _ vmclient.VM = (*fakeVM)(nil)

var errNotUsed = errors.New("fake vm: not used by agentd tests")

func newFakeVM() *fakeVM { return &fakeVM{vms: map[string]*vmdv1.VM{}} }

func (f *fakeVM) put(id, project string, state vmdv1.VMState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vms[id] = &vmdv1.VM{Id: id, ProjectId: project, State: state}
}

// holds returns the holders currently recorded on the VM.
func (f *fakeVM) holds(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.vms[id].GetHolds())
}

func (f *fakeVM) failGet(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = err
}

func (f *fakeVM) failAddHold(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addHoldErr = err
}

func (f *fakeVM) failRemoveHold(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeHoldErr = err
}

func (f *fakeVM) GetVM(_ context.Context, id string) (*vmdv1.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	v, ok := f.vms[id]
	if !ok {
		return nil, errs.NotFound("vm %s", id)
	}
	return proto.Clone(v).(*vmdv1.VM), nil
}

func (f *fakeVM) AddHold(_ context.Context, vmID, holder string) (*vmdv1.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addHoldErr != nil {
		return nil, f.addHoldErr
	}
	v, ok := f.vms[vmID]
	if !ok {
		return nil, errs.NotFound("vm %s", vmID)
	}
	if !slices.Contains(v.Holds, holder) {
		v.Holds = append(v.Holds, holder)
	}
	return proto.Clone(v).(*vmdv1.VM), nil
}

func (f *fakeVM) RemoveHold(_ context.Context, vmID, holder string) (*vmdv1.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeHoldErr != nil {
		return nil, f.removeHoldErr
	}
	v, ok := f.vms[vmID]
	if !ok {
		return nil, errs.NotFound("vm %s", vmID)
	}
	v.Holds = slices.DeleteFunc(v.Holds, func(h string) bool { return h == holder })
	return proto.Clone(v).(*vmdv1.VM), nil
}

func (f *fakeVM) StartJob(context.Context, *vmdv1.StartJobRequest) (*vmdv1.JobOutput, error) {
	return nil, errNotUsed
}

func (f *fakeVM) PollJob(context.Context, *vmdv1.PollJobRequest) (*vmdv1.JobOutput, error) {
	return nil, errNotUsed
}

func (f *fakeVM) CancelJob(context.Context, string) (*vmdv1.JobOutput, error) {
	return nil, errNotUsed
}

func (f *fakeVM) PutFile(context.Context, string, string, []byte, uint32) error {
	return errNotUsed
}

func (f *fakeVM) GetFile(context.Context, string, string, int64) ([]byte, bool, error) {
	return nil, false, errNotUsed
}

// flakyLLM is the scripted llm.Fake, with a switch that makes the backend unreachable for ListModels.
type flakyLLM struct {
	*llm.Fake
	down atomic.Bool
}

func (f *flakyLLM) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	if f.down.Load() {
		return nil, errors.New(`ollama list models: Get "http://198.51.100.7:11434/api/tags": dial tcp 198.51.100.7:11434: connect: connection refused`)
	}
	return f.Fake.ListModels(ctx)
}

var installedModels = []llm.ModelInfo{
	{Name: "llama3.2", SizeBytes: 2_000_000_000},
	{Name: "qwen2.5-coder:7b", SizeBytes: 4_700_000_000},
}

// env is a complete agentd: real HTTP handlers and auth, memory store, fake VM and LLM, manual clock.
type env struct {
	t   *testing.T
	st  store.Store
	vm  *fakeVM
	llm *flakyLLM
	clk *clock
	srv *httptest.Server
	iss *authz.Issuer
	reg *prometheus.Registry
}

// newEnv builds the service. opts may adjust the Deps before the handler is assembled. Two VMs exist:
// vm_p1 in project p1 and vm_p2 in project p2, both running.
func newEnv(t *testing.T, opts ...func(*service.Deps)) *env {
	t.Helper()
	e := &env{
		t:   t,
		vm:  newFakeVM(),
		llm: &flakyLLM{Fake: &llm.Fake{Models: installedModels}},
		clk: &clock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)},
		reg: prometheus.NewRegistry(),
	}
	e.vm.put("vm_p1", "p1", vmdv1.VMState_VM_STATE_RUNNING)
	e.vm.put("vm_p2", "p2", vmdv1.VMState_VM_STATE_RUNNING)

	pub, priv, err := authz.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	e.iss = &authz.Issuer{KeyID: "test", Key: priv, Now: e.clk.Now}
	d := service.Deps{
		Store:    store.NewMemory(),
		VM:       e.vm,
		LLM:      e.llm,
		Verifier: &authz.Verifier{Keys: map[string]authz.TrustedKey{"test": {Public: pub}}, Now: e.clk.Now},
		Config: service.Config{
			Now: e.clk.Now,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		Registry: e.reg,
	}
	for _, o := range opts {
		o(&d)
	}
	e.st = d.Store
	e.srv = httptest.NewServer(service.NewHandler(d))
	t.Cleanup(e.srv.Close)
	return e
}

// clients bundles the agentd service clients, authenticated as one principal (or anonymously).
type clients struct {
	Agent agentdv1connect.AgentServiceClient
	Task  agentdv1connect.TaskServiceClient
	Model agentdv1connect.ModelServiceClient
	Token string // the bearer token, empty for anonymous clients
}

func (e *env) client(token string) *clients {
	hc := e.srv.Client()
	var opts []connect.ClientOption
	if token != "" {
		opts = append(opts, connect.WithInterceptors(authz.ClientInterceptor(authz.StaticToken(token))))
	}
	return &clients{
		Agent: agentdv1connect.NewAgentServiceClient(hc, e.srv.URL, opts...),
		Task:  agentdv1connect.NewTaskServiceClient(hc, e.srv.URL, opts...),
		Model: agentdv1connect.NewModelServiceClient(hc, e.srv.URL, opts...),
		Token: token,
	}
}

// as returns clients for a principal holding the given bindings.
func (e *env) as(subject string, bindings ...authz.Binding) *clients {
	e.t.Helper()
	tok, err := e.iss.Mint(authz.Principal{Subject: subject, Type: authz.User, Bindings: bindings}, time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.client(tok)
}

func bind(project string, role authz.Role) authz.Binding {
	return authz.Binding{Project: project, Role: role}
}

// Principals used throughout the tests.
func (e *env) editor() *clients      { return e.as("user:editor-p1", bind("p1", authz.RoleEditor)) }
func (e *env) viewer() *clients      { return e.as("user:viewer-p1", bind("p1", authz.RoleViewer)) }
func (e *env) otherTenant() *clients { return e.as("user:editor-p2", bind("p2", authz.RoleEditor)) }
func (e *env) service() *clients {
	return e.as("svc:agentd", bind(authz.AnyProject, authz.RoleService))
}
func (e *env) operator() *clients {
	return e.as("user:ops", bind(authz.AnyProject, authz.RoleOperator))
}
func (e *env) anonymous() *clients { return e.client("") }

// createAgent creates an agent as c and fails the test on error.
func (e *env) createAgent(c *clients, project, name, vmID string) *agentdv1.Agent {
	e.t.Helper()
	a, err := c.Agent.CreateAgent(context.Background(), connect.NewRequest(&agentdv1.CreateAgentRequest{
		ProjectId: project, Name: name, VmId: vmID, Model: "llama3.2",
	}))
	if err != nil {
		e.t.Fatalf("create agent %s: %v", name, err)
	}
	return a.Msg
}

// createTask queues a task for agentID as c and fails the test on error.
func (e *env) createTask(c *clients, agentID, prompt string) *agentdv1.Task {
	e.t.Helper()
	task, err := c.Task.CreateTask(context.Background(), connect.NewRequest(&agentdv1.CreateTaskRequest{
		AgentId: agentID, Prompt: prompt,
	}))
	if err != nil {
		e.t.Fatalf("create task: %v", err)
	}
	return task.Msg
}

// code is the Connect code of err, or Unknown for nil.
func code(err error) connect.Code { return connect.CodeOf(err) }
