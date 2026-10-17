package e2e_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	agentdv1 "example.com/homecloud/gen/homecloud/agentd/v1"
	"example.com/homecloud/gen/homecloud/agentd/v1/agentdv1connect"
	nodev1 "example.com/homecloud/gen/homecloud/node/v1"
	vmdv1 "example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/gen/homecloud/vmd/v1/vmdv1connect"
	"example.com/homecloud/internal/agentd/llm"
	"example.com/homecloud/internal/agentd/runner"
	agentdsvc "example.com/homecloud/internal/agentd/service"
	agentdstore "example.com/homecloud/internal/agentd/store"
	agentdpg "example.com/homecloud/internal/agentd/store/postgres"
	"example.com/homecloud/internal/agentd/vmclient"
	"example.com/homecloud/internal/auth"
	"example.com/homecloud/internal/obs"
	"example.com/homecloud/internal/pg"
	"example.com/homecloud/internal/pg/pgtest"
	"example.com/homecloud/internal/vmd/node/fake"
	"example.com/homecloud/internal/vmd/promcollect"
	"example.com/homecloud/internal/vmd/scheduler"
	vmdsvc "example.com/homecloud/internal/vmd/service"
	vmdstore "example.com/homecloud/internal/vmd/store"
	vmdpg "example.com/homecloud/internal/vmd/store/postgres"
	"example.com/homecloud/internal/vmd/store/shard"
)

const (
	backendMemory   = "memory"
	backendPostgres = "postgres"
)

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// ---- clock ----

// clock is the one manually advanced clock shared by both servers, the fake node, the token issuers and the
// runner. Tests never sleep on it; they advance it explicitly.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)} }

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

// ---- scripted LLM ----

// llmHub is the agentd LLM backend for the tests: one llm.Fake script per model name, so that several agents
// can run concurrently without sharing one reply queue.
type llmHub struct {
	mu     sync.Mutex
	fakes  map[string]*llm.Fake
	models []llm.ModelInfo
	calls  map[string]int
	onChat func(model string, n int) // runs before each Chat, n is the 1-based call count for that model
}

func newHub() *llmHub {
	return &llmHub{fakes: map[string]*llm.Fake{}, calls: map[string]int{}}
}

// script installs model with its scripted replies and returns the fake, whose Calls record every request.
func (h *llmHub) script(model string, replies ...llm.FakeReply) *llm.Fake {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := &llm.Fake{Script: replies}
	h.fakes[model] = f
	h.models = append(h.models, llm.ModelInfo{Name: model, SizeBytes: 1 << 30})
	return f
}

func (h *llmHub) setOnChat(fn func(model string, n int)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onChat = fn
}

func (h *llmHub) Chat(ctx context.Context, model string, msgs []llm.Message, tools []llm.Tool, onChunk func(llm.Chunk)) (llm.Message, error) {
	h.mu.Lock()
	f, ok := h.fakes[model]
	h.calls[model]++
	n, hook := h.calls[model], h.onChat
	h.mu.Unlock()
	if !ok {
		return llm.Message{}, fmt.Errorf("e2e: no script for model %q", model)
	}
	if hook != nil {
		hook(model, n)
	}
	return f.Chat(ctx, model, msgs, tools, onChunk)
}

func (h *llmHub) ListModels(context.Context) ([]llm.ModelInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]llm.ModelInfo(nil), h.models...), nil
}

// ---- fake node execution log ----

// execRec is one command the fake node was asked to start.
type execRec struct {
	VMID, JobID, Command, Workdir string
}

type execFn = func(vmID string, spec *nodev1.JobSpec) (exit int, stdout, stderr string, runFor time.Duration)

func defaultExec(string, *nodev1.JobSpec) (int, string, string, time.Duration) {
	return 0, "ok\n", "", 0
}

// ---- world ----

// world is a complete deployment in one process: vmd and agentd behind real httptest servers, the
// agentd->vmd link made by the real vmclient with a service token, a fake node, and a runner.
type world struct {
	t       *testing.T
	backend string
	clk     *clock

	node     *fake.Driver
	nodes    *vmdsvc.NodeManager
	rec      *vmdsvc.Reconciler
	vmdStore vmdstore.Store
	vmdSrv   *httptest.Server
	vmdReg   *prometheus.Registry

	agentStore agentdstore.Store
	agentSrv   *httptest.Server
	agentReg   *prometheus.Registry

	admin *auth.Issuer // mints user tokens (trusted by both servers)
	svc   *auth.Issuer // agentd's service key (trusted by vmd for role service only)
	hub   *llmHub
	vm    vmclient.VM // the real vmclient, pointed at vmd
	run   *runner.Runner
	root  *clientSet

	mu     sync.Mutex
	onExec execFn
	execs  []execRec
}

type worldOption func(*worldOpts)

type worldOpts struct{ shards int }

// withShards makes vmd use n in-memory shards (memory backend only).
func withShards(n int) worldOption { return func(o *worldOpts) { o.shards = n } }

func newWorld(t *testing.T, backend string, opts ...worldOption) *world {
	t.Helper()
	var o worldOpts
	for _, f := range opts {
		f(&o)
	}
	ctx := context.Background()
	w := &world{t: t, backend: backend, clk: newClock(), hub: newHub(), onExec: defaultExec}

	vcfg := vmdsvc.Config{Now: w.clk.Now, ResyncInterval: 10 * time.Second, LeaseTTL: 30 * time.Second, Log: quietLog}
	switch {
	case o.shards > 1:
		if backend != backendMemory {
			t.Fatal("withShards is only used with the memory backend")
		}
		shards := make([]vmdstore.Store, o.shards)
		for i := range shards {
			shards[i] = vmdstore.NewMemory()
		}
		ss, err := shard.New(shards)
		must(t, err)
		w.vmdStore = ss
		vcfg.NewID, vcfg.ProjectContext = ss.NewID, shard.WithProject
	case backend == backendPostgres:
		pool := openPostgres(t)
		must(t, vmdpg.Migrate(ctx, pool))
		w.vmdStore = vmdpg.New(pool)
	default:
		w.vmdStore = vmdstore.NewMemory()
	}

	w.node = fake.New(fake.Config{CPUs: 16, MemoryMB: 65536, DiskBytesTotal: 2 << 40, DiskBytesFree: 1 << 40})
	w.node.Now = w.clk.Now
	w.node.Exec = w.execHook
	w.nodes = vmdsvc.NewNodeManager(w.vmdStore, quietLog, w.clk.Now)
	w.nodes.Add("n1", "fake://n1", w.node)
	w.nodes.Poll(ctx)
	w.rec = vmdsvc.NewReconciler(w.vmdStore, w.nodes, vcfg, "e2e-reconciler")

	adminPub, adminPriv, err := auth.GenerateKey()
	must(t, err)
	svcPub, svcPriv, err := auth.GenerateKey()
	must(t, err)
	w.admin = &auth.Issuer{KeyID: "admin", Key: adminPriv, Now: w.clk.Now}
	w.svc = &auth.Issuer{KeyID: "agentd", Key: svcPriv, Now: w.clk.Now}
	vmdVerifier := &auth.Verifier{Now: w.clk.Now, Keys: map[string]auth.TrustedKey{
		"admin":  {Public: adminPub},
		"agentd": {Public: svcPub, AllowedRoles: []auth.Role{auth.RoleService}},
	}}
	agentVerifier := &auth.Verifier{Now: w.clk.Now, Keys: map[string]auth.TrustedKey{"admin": {Public: adminPub}}}

	w.vmdReg = obs.NewRegistry()
	w.vmdReg.MustRegister(promcollect.New(w.vmdStore, w.nodes, scheduler.Config{}, promcollect.Options{Now: w.clk.Now, Log: quietLog}))
	w.vmdSrv = httptest.NewServer(vmdsvc.NewHandler(vmdsvc.Deps{Store: w.vmdStore, Nodes: w.nodes,
		Verifier: vmdVerifier, Config: vcfg, Registry: w.vmdReg}))
	t.Cleanup(w.vmdSrv.Close)

	if backend == backendPostgres {
		pool := openPostgres(t)
		must(t, agentdpg.Migrate(ctx, pool))
		w.agentStore = agentdpg.New(pool)
	} else {
		w.agentStore = agentdstore.NewMemory()
	}
	w.vm = vmclient.New(w.vmdSrv.Client(), w.vmdSrv.URL, auth.ServiceTokens(w.svc,
		auth.Principal{Subject: "svc:agentd", Type: auth.Service,
			Bindings: []auth.Binding{{Project: auth.AnyProject, Role: auth.RoleService}}}, time.Hour))
	w.agentReg = obs.NewRegistry()
	w.agentSrv = httptest.NewServer(agentdsvc.NewHandler(agentdsvc.Deps{Store: w.agentStore, VM: w.vm, LLM: w.hub,
		Verifier: agentVerifier, Config: agentdsvc.Config{Now: w.clk.Now, Log: quietLog}, Registry: w.agentReg}))
	t.Cleanup(w.agentSrv.Close)

	w.run = w.newRunner("w1", 2)
	w.root = w.as("user:root", bind(auth.AnyProject, auth.RoleAdmin))
	return w
}

// execHook records every command the fake node starts, then runs the scripted behaviour. It runs under the
// node's lock, so it must not call the node.
func (w *world) execHook(vmID string, spec *nodev1.JobSpec) (int, string, string, time.Duration) {
	w.mu.Lock()
	w.execs = append(w.execs, execRec{VMID: vmID, JobID: spec.GetJobId(), Command: spec.GetCommand(), Workdir: spec.GetWorkdir()})
	f := w.onExec
	w.mu.Unlock()
	return f(vmID, spec)
}

// setExec replaces the scripted command behaviour.
func (w *world) setExec(f execFn) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onExec = f
}

// commandRuns returns the commands the node started with exactly this command line.
func (w *world) commandRuns(command string) []execRec {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []execRec
	for _, e := range w.execs {
		if e.Command == command {
			out = append(out, e)
		}
	}
	return out
}

// newRunner returns a runner with short real intervals and the shared clock. Leases expire only when the
// test advances the clock.
func (w *world) newRunner(owner string, workers int) *runner.Runner {
	return runner.New(runner.Config{
		Store: w.agentStore, LLM: w.hub, VM: w.vm, Owner: owner, Workers: workers,
		LeaseTTL: 30 * time.Second, PollInterval: 2 * time.Millisecond, CancelPoll: 5 * time.Millisecond,
		Now: w.clk.Now, Log: quietLog,
	})
}

// runTasks runs the default runner's RunOnce until no task is claimable.
func (w *world) runTasks() {
	w.t.Helper()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		claimed, err := w.run.RunOnce(ctx)
		if err != nil {
			w.t.Fatalf("RunOnce: %v", err)
		}
		if !claimed {
			return
		}
	}
	w.t.Fatal("runTasks: still claiming after 50 tasks")
}

// runWorkers runs a runner with the given number of workers until every listed task is final.
func (w *world) runWorkers(workers int, taskIDs ...string) {
	w.t.Helper()
	r := w.newRunner("workers", workers)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		allFinal := true
		for _, id := range taskIDs {
			if !isFinalTask(w.getTask(w.root, id).GetState()) {
				allFinal = false
				break
			}
		}
		if allFinal {
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatal("runWorkers: tasks not final after 20s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// tickVMD runs the vmd reconciler n times, advancing the shared clock one second per tick.
func (w *world) tickVMD(n int) {
	for range n {
		w.rec.Tick(context.Background())
		w.clk.Advance(time.Second)
	}
}

// until ticks the reconciler (up to max times) until cond holds.
func (w *world) until(max int, what string, cond func() bool) {
	w.t.Helper()
	for range max {
		if cond() {
			return
		}
		w.tickVMD(1)
	}
	if !cond() {
		w.t.Fatalf("condition not reached after %d ticks: %s", max, what)
	}
}

// ---- clients ----

// clientSet is every service client authenticated as one principal.
type clientSet struct {
	VM    vmdv1connect.VMServiceClient
	Image vmdv1connect.ImageServiceClient
	Exec  vmdv1connect.ExecServiceClient
	Agent agentdv1connect.AgentServiceClient
	Task  agentdv1connect.TaskServiceClient
	Model agentdv1connect.ModelServiceClient
	Token string
}

func bind(project string, role auth.Role) auth.Binding {
	return auth.Binding{Project: project, Role: role}
}

// as mints a user token signed by the admin key and returns clients for it.
func (w *world) as(subject string, bindings ...auth.Binding) *clientSet {
	w.t.Helper()
	tok, err := w.admin.Mint(auth.Principal{Subject: subject, Type: auth.User, Bindings: bindings}, time.Hour)
	must(w.t, err)
	return w.withToken(tok)
}

// withToken returns clients for an arbitrary bearer token ("" sends an empty bearer).
func (w *world) withToken(tok string) *clientSet {
	opt := connect.WithInterceptors(auth.ClientInterceptor(auth.StaticToken(tok)))
	vh, ah := w.vmdSrv.Client(), w.agentSrv.Client()
	return &clientSet{
		VM:    vmdv1connect.NewVMServiceClient(vh, w.vmdSrv.URL, opt),
		Image: vmdv1connect.NewImageServiceClient(vh, w.vmdSrv.URL, opt),
		Exec:  vmdv1connect.NewExecServiceClient(vh, w.vmdSrv.URL, opt),
		Agent: agentdv1connect.NewAgentServiceClient(ah, w.agentSrv.URL, opt),
		Task:  agentdv1connect.NewTaskServiceClient(ah, w.agentSrv.URL, opt),
		Model: agentdv1connect.NewModelServiceClient(ah, w.agentSrv.URL, opt),
		Token: tok,
	}
}

func (w *world) editor() *clientSet { return w.as("user:editor-p1", bind("p1", auth.RoleEditor)) }
func (w *world) viewer() *clientSet { return w.as("user:viewer-p1", bind("p1", auth.RoleViewer)) }
func (w *world) otherT() *clientSet { return w.as("user:editor-p2", bind("p2", auth.RoleEditor)) }
func (w *world) operator() *clientSet {
	return w.as("user:ops", bind(auth.AnyProject, auth.RoleOperator))
}

// ---- resource helpers (as root unless noted) ----

// sharedImage registers a shared image and waits until it is ready.
func (w *world) sharedImage(name string) *vmdv1.Image {
	w.t.Helper()
	img, err := w.root.Image.RegisterImage(context.Background(), connect.NewRequest(&vmdv1.RegisterImageRequest{
		Name: name, NodeId: "n1", Path: "/images/" + name + ".qcow2"}))
	must(w.t, err)
	w.until(5, "image ready", func() bool {
		i, err := w.root.Image.GetImage(context.Background(), connect.NewRequest(&vmdv1.GetImageRequest{Id: img.Msg.Id}))
		return err == nil && i.Msg.State == vmdv1.ImageState_IMAGE_STATE_READY
	})
	return img.Msg
}

func (w *world) createVM(c *clientSet, project, name, imageID string, start bool) *vmdv1.VM {
	w.t.Helper()
	vm, err := c.VM.CreateVM(context.Background(), connect.NewRequest(&vmdv1.CreateVMRequest{
		ProjectId: project, Name: name, Cpus: 2, MemoryMb: 2048, DiskSizeGb: 10, Start: start,
		Boot: &vmdv1.CreateVMRequest_ImageId{ImageId: imageID}}))
	must(w.t, err)
	return vm.Msg
}

// getVM reads a VM as root.
func (w *world) getVM(id string) *vmdv1.VM {
	w.t.Helper()
	vm, err := w.root.VM.GetVM(context.Background(), connect.NewRequest(&vmdv1.GetVMRequest{Id: id}))
	must(w.t, err)
	return vm.Msg
}

// runningVM creates a started VM from a fresh shared image and waits until it is RUNNING.
func (w *world) runningVM(name string) *vmdv1.VM {
	w.t.Helper()
	img := w.sharedImage("base-" + name)
	vm := w.createVM(w.editor(), "p1", name, img.Id, true)
	w.until(20, "vm running", func() bool { return w.getVM(vm.Id).State == vmdv1.VMState_VM_STATE_RUNNING })
	return w.getVM(vm.Id)
}

func (w *world) createAgent(c *clientSet, project, name, vmID, model string) *agentdv1.Agent {
	w.t.Helper()
	a, err := c.Agent.CreateAgent(context.Background(), connect.NewRequest(&agentdv1.CreateAgentRequest{
		ProjectId: project, Name: name, VmId: vmID, Model: model}))
	must(w.t, err)
	return a.Msg
}

func (w *world) createTask(c *clientSet, agentID, prompt, check string) *agentdv1.Task {
	w.t.Helper()
	task, err := c.Task.CreateTask(context.Background(), connect.NewRequest(&agentdv1.CreateTaskRequest{
		AgentId: agentID, Prompt: prompt, CheckCommand: check}))
	must(w.t, err)
	return task.Msg
}

func (w *world) getTask(c *clientSet, id string) *agentdv1.Task {
	w.t.Helper()
	task, err := c.Task.GetTask(context.Background(), connect.NewRequest(&agentdv1.GetTaskRequest{Id: id}))
	must(w.t, err)
	return task.Msg
}

// jobs lists the vmd jobs of a VM, oldest first as vmd returns them.
func (w *world) jobs(vmID string) []*vmdv1.Job {
	w.t.Helper()
	res, err := w.root.Exec.ListJobs(context.Background(), connect.NewRequest(&vmdv1.ListJobsRequest{VmId: vmID}))
	must(w.t, err)
	return res.Msg.Jobs
}

// jobFor returns the vmd job that ran the given command line, or nil.
func jobFor(jobs []*vmdv1.Job, command string) *vmdv1.Job {
	for _, j := range jobs {
		if j.Command == command {
			return j
		}
	}
	return nil
}

// ---- small utilities ----

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// openPostgres returns a pool on a fresh, empty database, closed when the test ends.
func openPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pg.Open(context.Background(), pgtest.DSN(t))
	must(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// eachBackend runs fn once on the memory store and, when pgToo is set, once on Postgres.
func eachBackend(t *testing.T, pgToo bool, fn func(t *testing.T, backend string)) {
	t.Run(backendMemory, func(t *testing.T) { fn(t, backendMemory) })
	if pgToo {
		t.Run(backendPostgres, func(t *testing.T) { fn(t, backendPostgres) })
	}
}

// sortedCopy returns a sorted copy of s.
func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	sort.Strings(out)
	return out
}

// runAsync runs RunOnce in a goroutine and returns the channel that receives its error.
func runAsync(ctx context.Context, r *runner.Runner) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := r.RunOnce(ctx)
		done <- err
	}()
	return done
}

// awaitErr waits for a runner goroutine to return, failing after 10 s.
func awaitErr(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return", what)
		return nil
	}
}

func codeIs(err error, want connect.Code) bool { return err != nil && connect.CodeOf(err) == want }

// isFinalTask reports whether a task state is terminal.
func isFinalTask(s agentdv1.TaskState) bool {
	switch s {
	case agentdv1.TaskState_TASK_STATE_QUEUED, agentdv1.TaskState_TASK_STATE_RUNNING, agentdv1.TaskState_TASK_STATE_UNSPECIFIED:
		return false
	}
	return true
}
