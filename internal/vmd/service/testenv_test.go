package service_test

import (
	"context"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"

	nodev1 "example.com/homecloud/gen/homecloud/node/v1"
	vmdv1 "example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/gen/homecloud/vmd/v1/vmdv1connect"
	"example.com/homecloud/internal/auth"
	"example.com/homecloud/internal/vmd/node/fake"
	"example.com/homecloud/internal/vmd/service"
	"example.com/homecloud/internal/vmd/store"
	"example.com/homecloud/internal/vmd/store/shard"
)

// clock is a manually advanced clock shared by the service, the reconciler and the fake node, so tests
// never sleep and time-dependent behaviour (leases, backoff, job timing) is deterministic.
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

// env is a complete vmd: real HTTP handlers and auth, memory store, fake node, reconciler driven by hand.
type env struct {
	t     *testing.T
	st    store.Store
	node  *fake.Driver
	nodes *service.NodeManager
	rec   *service.Reconciler
	clk   *clock
	srv   *httptest.Server
	iss   *auth.Issuer
	reg   *prometheus.Registry
	cfg   service.Config // the config every component of this env shares (incl. any sharding hooks)
}

// clients bundles every service client authenticated as one principal.
type clients struct {
	VM      vmdv1connect.VMServiceClient
	Disk    vmdv1connect.DiskServiceClient
	Image   vmdv1connect.ImageServiceClient
	Exec    vmdv1connect.ExecServiceClient
	Metrics vmdv1connect.MetricsServiceClient
	Token   string
}

type envOption func(*service.Config, *fake.Config)

func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	e := &env{t: t, clk: &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}}
	cfg := service.Config{Now: e.clk.Now, ResyncInterval: 10 * time.Second, LeaseTTL: 30 * time.Second}
	fcfg := fake.Config{CPUs: 16, MemoryMB: 65536, DiskBytesTotal: 2 << 40, DiskBytesFree: 1 << 40}
	for _, o := range opts {
		o(&cfg, &fcfg)
	}
	e.st = store.NewMemory()
	if n, _ := strconv.Atoi(os.Getenv("HOMECLOUD_TEST_SHARDS")); n > 1 {
		// Run the whole suite on a project-sharded store (HOMECLOUD_TEST_SHARDS=3 go test ./internal/vmd/service).
		shards := make([]store.Store, n)
		for i := range shards {
			shards[i] = store.NewMemory()
		}
		ss, err := shard.New(shards)
		if err != nil {
			t.Fatal(err)
		}
		e.st, cfg.NewID, cfg.ProjectContext = ss, ss.NewID, shard.WithProject
	}
	e.node = fake.New(fcfg)
	e.node.Now = e.clk.Now
	e.nodes = service.NewNodeManager(e.st, nil, e.clk.Now)
	e.nodes.Add("n1", "fake://n1", e.node)
	e.nodes.Poll(context.Background()) // registers the node row with its capacity
	e.cfg = cfg
	e.rec = service.NewReconciler(e.st, e.nodes, cfg, "test-reconciler")

	pub, priv, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	e.iss = &auth.Issuer{KeyID: "test", Key: priv, Now: e.clk.Now}
	e.reg = prometheus.NewRegistry()
	h := service.NewHandler(service.Deps{
		Store: e.st, Nodes: e.nodes, Config: cfg, Registry: e.reg,
		Verifier: &auth.Verifier{Keys: map[string]auth.TrustedKey{"test": {Public: pub}}, Now: e.clk.Now},
	})
	e.srv = httptest.NewServer(h)
	t.Cleanup(e.srv.Close)
	return e
}

// as returns clients authenticated with the given bindings.
func (e *env) as(subject string, bindings ...auth.Binding) *clients {
	e.t.Helper()
	tok, err := e.iss.Mint(auth.Principal{Subject: subject, Type: auth.User, Bindings: bindings}, time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.withToken(tok)
}

func (e *env) withToken(tok string) *clients {
	opt := connect.WithInterceptors(auth.ClientInterceptor(auth.StaticToken(tok)))
	hc := e.srv.Client()
	return &clients{
		VM: vmdv1connect.NewVMServiceClient(hc, e.srv.URL, opt), Disk: vmdv1connect.NewDiskServiceClient(hc, e.srv.URL, opt),
		Image: vmdv1connect.NewImageServiceClient(hc, e.srv.URL, opt), Exec: vmdv1connect.NewExecServiceClient(hc, e.srv.URL, opt),
		Metrics: vmdv1connect.NewMetricsServiceClient(hc, e.srv.URL, opt), Token: tok,
	}
}

func bind(project string, role auth.Role) auth.Binding {
	return auth.Binding{Project: project, Role: role}
}

// Principals used throughout the tests.
func (e *env) admin() *clients       { return e.as("user:admin-p1", bind("p1", auth.RoleAdmin)) }
func (e *env) editor() *clients      { return e.as("user:editor-p1", bind("p1", auth.RoleEditor)) }
func (e *env) viewer() *clients      { return e.as("user:viewer-p1", bind("p1", auth.RoleViewer)) }
func (e *env) otherTenant() *clients { return e.as("user:editor-p2", bind("p2", auth.RoleEditor)) }
func (e *env) root() *clients        { return e.as("user:root", bind(auth.AnyProject, auth.RoleAdmin)) }
func (e *env) operator() *clients    { return e.as("user:ops", bind(auth.AnyProject, auth.RoleOperator)) }
func (e *env) service() *clients     { return e.as("svc:agentd", bind(auth.AnyProject, auth.RoleService)) }

// tick runs the reconciler n times, advancing the clock one second per tick.
func (e *env) tick(n int) {
	e.t.Helper()
	for range n {
		e.rec.Tick(context.Background())
		e.clk.Advance(time.Second)
	}
}

// until ticks the reconciler (up to max times) until cond is true, failing the test otherwise.
func (e *env) until(max int, what string, cond func() bool) {
	e.t.Helper()
	for range max {
		if cond() {
			return
		}
		e.tick(1)
	}
	if !cond() {
		e.t.Fatalf("condition not reached after %d ticks: %s", max, what)
	}
}

// sharedImage registers a shared image as a global admin and waits until it is ready.
func (e *env) sharedImage(name string) *vmdv1.Image {
	e.t.Helper()
	img, err := e.root().Image.RegisterImage(context.Background(), connect.NewRequest(&vmdv1.RegisterImageRequest{
		Name: name, NodeId: "n1", Path: "/images/" + name + ".qcow2"}))
	if err != nil {
		e.t.Fatal(err)
	}
	e.until(5, "image ready", func() bool {
		i, err := e.root().Image.GetImage(context.Background(), connect.NewRequest(&vmdv1.GetImageRequest{Id: img.Msg.Id}))
		return err == nil && i.Msg.State == vmdv1.ImageState_IMAGE_STATE_READY
	})
	return img.Msg
}

// createVM creates a VM in project p1 from the image, optionally starting it.
func (e *env) createVM(c *clients, project, name, imageID string, start bool) *vmdv1.VM {
	e.t.Helper()
	vm, err := c.VM.CreateVM(context.Background(), connect.NewRequest(&vmdv1.CreateVMRequest{
		ProjectId: project, Name: name, Cpus: 2, MemoryMb: 2048, DiskSizeGb: 10, Start: start,
		Boot: &vmdv1.CreateVMRequest_ImageId{ImageId: imageID}}))
	if err != nil {
		e.t.Fatal(err)
	}
	return vm.Msg
}

func (e *env) getVM(c *clients, id string) *vmdv1.VM {
	e.t.Helper()
	vm, err := c.VM.GetVM(context.Background(), connect.NewRequest(&vmdv1.GetVMRequest{Id: id}))
	if err != nil {
		e.t.Fatal(err)
	}
	return vm.Msg
}

// runningVM creates a started VM and ticks until it is RUNNING.
func (e *env) runningVM(name string) *vmdv1.VM {
	e.t.Helper()
	img := e.sharedImage("base-" + name)
	vm := e.createVM(e.editor(), "p1", name, img.Id, true)
	e.until(10, "vm running", func() bool { return e.getVM(e.editor(), vm.Id).State == vmdv1.VMState_VM_STATE_RUNNING })
	return e.getVM(e.editor(), vm.Id)
}

func code(err error) connect.Code { return connect.CodeOf(err) }

var _ = nodev1.RuntimeState_RUNTIME_STATE_ABSENT
