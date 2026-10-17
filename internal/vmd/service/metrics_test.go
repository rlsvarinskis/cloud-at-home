package service_test

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"testing"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus/collectors"

	nodev1 "example.com/homecloud/gen/homecloud/node/v1"
	vmdv1 "example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/internal/auth"
	"example.com/homecloud/internal/vmd/domain"
	"example.com/homecloud/internal/vmd/node/fake"
	"example.com/homecloud/internal/vmd/service"
)

// metricsCapacity fetches GetCapacity as the given client.
func metricsCapacity(e *env, c *clients) *vmdv1.GetCapacityResponse {
	e.t.Helper()
	res, err := c.Metrics.GetCapacity(context.Background(), connect.NewRequest(&vmdv1.GetCapacityRequest{}))
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Msg
}

// metricsNode returns the row for one node in a capacity response.
func metricsNode(e *env, res *vmdv1.GetCapacityResponse, id string) *vmdv1.NodeCapacity {
	e.t.Helper()
	for _, n := range res.Nodes {
		if n.NodeId == id {
			return n
		}
	}
	e.t.Fatalf("node %s missing from capacity response", id)
	return nil
}

// metricsGet performs a plain HTTP GET against the vmd server, optionally with a bearer token.
func metricsGet(e *env, path, token string) (int, string) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, e.srv.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// ------------------------------------------------------------------------------------ GetVMMetrics

func TestMetricsVMSampleMappedFieldForField(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("met")
	disk := vm.DiskIds[0]
	e.node.SetMetrics(vm.Id, &nodev1.GetVMMetricsResponse{
		CpuPercent: 350, CpuCores: 4, Load1: 1.5, Load5: 1.25, Load15: 1.0,
		MemTotalBytes: 8 << 30, MemUsedBytes: 3 << 30, MemAvailableBytes: 5 << 30,
		SwapTotalBytes: 2 << 30, SwapUsedBytes: 1 << 20, UptimeSeconds: 777,
		Filesystems: []*nodev1.GuestFilesystem{{DiskId: disk, Device: "/dev/vda1", MountPoint: "/", SizeBytes: 10 << 30, UsedBytes: 4 << 30}},
		Disks:       []*nodev1.DiskAllocation{{DiskId: disk, VirtualSizeBytes: 10 << 30, AllocatedBytes: 2 << 30}},
	})

	at := e.clk.Now()
	res, err := e.editor().Metrics.GetVMMetrics(context.Background(), connect.NewRequest(&vmdv1.GetVMMetricsRequest{VmId: vm.Id}))
	if err != nil {
		t.Fatal(err)
	}
	m := res.Msg
	if m.VmId != vm.Id {
		t.Errorf("vm_id = %q", m.VmId)
	}
	if got := m.SampledAt.AsTime(); !got.Equal(at) {
		t.Errorf("sampled_at = %s, want %s", got, at)
	}
	if m.CpuPercent != 350 || m.CpuCores != 4 {
		t.Errorf("cpu = %v%% on %d cores, want 350%% on 4", m.CpuPercent, m.CpuCores)
	}
	if m.Load1 != 1.5 || m.Load5 != 1.25 || m.Load15 != 1.0 {
		t.Errorf("load = %v %v %v", m.Load1, m.Load5, m.Load15)
	}
	if m.MemTotalBytes != 8<<30 || m.MemUsedBytes != 3<<30 || m.MemAvailableBytes != 5<<30 {
		t.Errorf("memory = %d/%d/%d", m.MemTotalBytes, m.MemUsedBytes, m.MemAvailableBytes)
	}
	if m.SwapTotalBytes != 2<<30 || m.SwapUsedBytes != 1<<20 || m.UptimeSeconds != 777 {
		t.Errorf("swap/uptime = %d/%d/%d", m.SwapTotalBytes, m.SwapUsedBytes, m.UptimeSeconds)
	}
	if len(m.Filesystems) != 1 {
		t.Fatalf("filesystems = %d, want 1", len(m.Filesystems))
	}
	if f := m.Filesystems[0]; f.DiskId != disk || f.Device != "/dev/vda1" || f.MountPoint != "/" || f.SizeBytes != 10<<30 || f.UsedBytes != 4<<30 {
		t.Errorf("filesystem = %+v", f)
	}
	if len(m.Disks) != 1 {
		t.Fatalf("disk allocations = %d, want 1", len(m.Disks))
	}
	if d := m.Disks[0]; d.DiskId != disk || d.VirtualSizeBytes != 10<<30 || d.AllocatedBytes != 2<<30 {
		t.Errorf("disk allocation = %+v", d)
	}
}

func TestMetricsVMSampleErrors(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("merr")
	req := func() *connect.Request[vmdv1.GetVMMetricsRequest] {
		return connect.NewRequest(&vmdv1.GetVMMetricsRequest{VmId: vm.Id})
	}

	if _, err := e.otherTenant().Metrics.GetVMMetrics(context.Background(), req()); code(err) != connect.CodeNotFound {
		t.Errorf("other tenant: code %v, want not_found", code(err))
	}
	if _, err := e.viewer().Metrics.GetVMMetrics(context.Background(), req()); err != nil {
		t.Errorf("viewer (vm.metrics): %v", err)
	}
	if _, err := e.withToken("").Metrics.GetVMMetrics(context.Background(), req()); code(err) != connect.CodeUnauthenticated {
		t.Errorf("no token: code %v, want unauthenticated", code(err))
	}
	if _, err := e.withToken("not-a-token").Metrics.GetVMMetrics(context.Background(), req()); code(err) != connect.CodeUnauthenticated {
		t.Errorf("bad token: code %v, want unauthenticated", code(err))
	}

	rectStop(e, vm.Id, false)
	e.until(10, "stopped", func() bool { return e.getVM(e.editor(), vm.Id).State == vmdv1.VMState_VM_STATE_STOPPED })
	if _, err := e.editor().Metrics.GetVMMetrics(context.Background(), req()); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("stopped VM: code %v, want failed_precondition", code(err))
	}
}

// ------------------------------------------------------------------------------------ GetCapacity

func TestMetricsCapacityOperatorOnly(t *testing.T) {
	e := newEnv(t)
	denied := map[string]*clients{
		"editor":           e.editor(),
		"admin":            e.admin(),
		"root admin (*)":   e.root(),
		"service":          e.service(),
		"viewer":           e.viewer(),
		"project operator": e.as("user:ops-p1", bind("p1", auth.RoleOperator)),
	}
	for name, c := range denied {
		_, err := c.Metrics.GetCapacity(context.Background(), connect.NewRequest(&vmdv1.GetCapacityRequest{}))
		if code(err) != connect.CodePermissionDenied {
			t.Errorf("%s: code %v, want permission_denied", name, code(err))
		}
	}
	if _, err := e.operator().Metrics.GetCapacity(context.Background(), connect.NewRequest(&vmdv1.GetCapacityRequest{})); err != nil {
		t.Errorf("operator: %v", err)
	}
	if _, err := e.withToken("").Metrics.GetCapacity(context.Background(), connect.NewRequest(&vmdv1.GetCapacityRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Errorf("no token: code %v, want unauthenticated", code(err))
	}
}

func TestMetricsCapacityNumbers(t *testing.T) {
	e := newEnv(t)
	e.runningVM("cap1")
	e.runningVM("cap2")

	res := metricsCapacity(e, e.operator())
	want := &vmdv1.NodeCapacity{
		NodeId: "n1", State: "ready", CpusTotal: 16, CpusAllocated: 4, CpusAvailable: 12,
		MemoryMbTotal: 65536, MemoryMbAllocated: 4096, MemoryMbAvailable: 61440,
		DiskBytesTotal: 2 << 40, DiskBytesFree: 1 << 40, VmsTotal: 2, VmsRunning: 2,
	}
	check := func(what string, got *vmdv1.NodeCapacity) {
		t.Helper()
		if got.State != want.State || got.CpusTotal != want.CpusTotal || got.CpusAllocated != want.CpusAllocated ||
			got.CpusAvailable != want.CpusAvailable || got.MemoryMbTotal != want.MemoryMbTotal ||
			got.MemoryMbAllocated != want.MemoryMbAllocated || got.MemoryMbAvailable != want.MemoryMbAvailable ||
			got.DiskBytesTotal != want.DiskBytesTotal || got.DiskBytesFree != want.DiskBytesFree ||
			got.VmsTotal != want.VmsTotal || got.VmsRunning != want.VmsRunning {
			t.Errorf("%s = %+v, want %+v", what, got, want)
		}
	}
	check("node n1", metricsNode(e, res, "n1"))
	want.NodeId = "*"
	check("total", res.Total)
	if res.Total.NodeId != "*" {
		t.Errorf("total node_id = %q, want \"*\"", res.Total.NodeId)
	}
}

func TestMetricsCapacityExcludesStoppedVMsAndNonReadyNodesFromTotals(t *testing.T) {
	e := newEnv(t)
	e.runningVM("ex1")
	stopped := e.runningVM("ex2")

	ctx := context.Background()
	if err := e.st.UpsertNode(ctx, &domain.Node{ID: "n2", Address: "fake://n2", State: domain.NodeDown,
		CPUs: 32, MemoryMB: 131072, DiskBytesTotal: 1 << 40, DiskBytesFree: 1 << 39}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertNode(ctx, &domain.Node{ID: "n3", Address: "fake://n3", State: domain.NodeDraining,
		CPUs: 8, MemoryMB: 16384, DiskBytesTotal: 1 << 40, DiskBytesFree: 1 << 39}); err != nil {
		t.Fatal(err)
	}

	rectStop(e, stopped.Id, false)
	e.until(10, "second VM stopped", func() bool { return e.getVM(e.editor(), stopped.Id).State == vmdv1.VMState_VM_STATE_STOPPED })

	res := metricsCapacity(e, e.operator())
	if len(res.Nodes) != 3 {
		t.Fatalf("nodes listed = %d, want 3 (down and draining nodes are listed)", len(res.Nodes))
	}
	n1 := metricsNode(e, res, "n1")
	if n1.CpusAllocated != 2 || n1.MemoryMbAllocated != 2048 || n1.VmsRunning != 1 || n1.VmsTotal != 2 {
		t.Errorf("n1 after stop: allocated cpus %d mem %d running %d total %d, want 2/2048/1/2",
			n1.CpusAllocated, n1.MemoryMbAllocated, n1.VmsRunning, n1.VmsTotal)
	}
	if n2 := metricsNode(e, res, "n2"); n2.State != "down" || n2.CpusTotal != 32 {
		t.Errorf("n2 row = state %q cpus %d, want down/32", n2.State, n2.CpusTotal)
	}
	if n3 := metricsNode(e, res, "n3"); n3.State != "draining" || n3.CpusTotal != 8 {
		t.Errorf("n3 row = state %q cpus %d, want draining/8", n3.State, n3.CpusTotal)
	}

	tot := res.Total
	if tot.CpusTotal != 16 || tot.MemoryMbTotal != 65536 || tot.DiskBytesTotal != 2<<40 {
		t.Errorf("total capacity = cpus %d mem %d disk %d, want only ready n1 (16/65536/2^41)",
			tot.CpusTotal, tot.MemoryMbTotal, tot.DiskBytesTotal)
	}
	if tot.CpusAllocated != 2 || tot.MemoryMbAllocated != 2048 || tot.VmsRunning != 1 {
		t.Errorf("total allocation = cpus %d mem %d running %d, want 2/2048/1", tot.CpusAllocated, tot.MemoryMbAllocated, tot.VmsRunning)
	}
	if tot.VmsTotal != 2 {
		t.Errorf("total vms = %d, want 2 (both on n1)", tot.VmsTotal)
	}
}

func TestMetricsCapacityOvercommitChangesAvailable(t *testing.T) {
	e := newEnv(t, func(c *service.Config, _ *fake.Config) { c.Scheduler.CPUOvercommit = 2 })
	e.runningVM("oc1")
	e.runningVM("oc2")

	res := metricsCapacity(e, e.operator())
	n1 := metricsNode(e, res, "n1")
	if n1.CpusTotal != 16 || n1.CpusAllocated != 4 || n1.CpusAvailable != 28 {
		t.Errorf("n1 with 2x overcommit: total %d allocated %d available %d, want 16/4/28", n1.CpusTotal, n1.CpusAllocated, n1.CpusAvailable)
	}
	if res.Total.CpusAvailable != 28 {
		t.Errorf("total cpus available = %d, want 28", res.Total.CpusAvailable)
	}
	if n1.MemoryMbAvailable != 61440 {
		t.Errorf("memory available = %d, want 61440 (no memory overcommit)", n1.MemoryMbAvailable)
	}
}

// Expected to FAIL with the current code: see the report. The field is documented as "what the scheduler
// would still hand out", and the scheduler hands out nothing from a down node.
func TestMetricsCapacityDownNodeOffersNoAvailable(t *testing.T) {
	e := newEnv(t)
	if err := e.st.UpsertNode(context.Background(), &domain.Node{ID: "n2", Address: "fake://n2", State: domain.NodeDown,
		CPUs: 32, MemoryMB: 131072, DiskBytesTotal: 1 << 40, DiskBytesFree: 1 << 39}); err != nil {
		t.Fatal(err)
	}
	n2 := metricsNode(e, metricsCapacity(e, e.operator()), "n2")
	if n2.CpusAvailable != 0 || n2.MemoryMbAvailable != 0 {
		t.Errorf("down node reports available cpus %d mem %d, want 0/0 (scheduler would place nothing there)", n2.CpusAvailable, n2.MemoryMbAvailable)
	}
}

// ------------------------------------------------------------------------------------ Prometheus, health

func TestMetricsEndpointAuthAndSeries(t *testing.T) {
	e := newEnv(t)
	// The harness registry starts empty. Production builds it with obs.NewRegistry, which adds the Go and
	// process collectors, so add them the same way here.
	e.reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	img := e.sharedImage("base-metrics")
	e.createVM(e.editor(), "p1", "mx", img.Id, false)                                                    // CreateVM ok
	if _, err := e.viewer().VM.CreateVM(context.Background(), connect.NewRequest(&vmdv1.CreateVMRequest{ // permission_denied
		ProjectId: "p1", Name: "mx2", Cpus: 2, MemoryMb: 2048, DiskSizeGb: 10,
		Boot: &vmdv1.CreateVMRequest_ImageId{ImageId: img.Id}})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer CreateVM: code %v, want permission_denied", code(err))
	}
	if _, err := e.editor().VM.GetVM(context.Background(), connect.NewRequest(&vmdv1.GetVMRequest{Id: "vm-missing"})); code(err) != connect.CodeNotFound { // not_found
		t.Fatalf("GetVM missing: code %v, want not_found", code(err))
	}

	if status, _ := metricsGet(e, "/metrics", ""); status != http.StatusUnauthorized {
		t.Errorf("/metrics without token: status %d, want 401", status)
	}
	if status, _ := metricsGet(e, "/metrics", e.editor().Token); status != http.StatusForbidden {
		t.Errorf("/metrics with editor token: status %d, want 403", status)
	}
	status, body := metricsGet(e, "/metrics", e.operator().Token)
	if status != http.StatusOK {
		t.Fatalf("/metrics with operator token: status %d, want 200", status)
	}

	const svc = `service="homecloud.vmd.v1.VMService"`
	expect := []string{
		`(?m)^homecloud_rpc_requests_total\{code="ok",method="CreateVM",server="vmd",` + svc + `\} 1$`,
		`(?m)^homecloud_rpc_requests_total\{code="permission_denied",method="CreateVM",server="vmd",` + svc + `\} 1$`,
		`(?m)^homecloud_rpc_requests_total\{code="not_found",method="GetVM",server="vmd",` + svc + `\} 1$`,
		`(?m)^homecloud_rpc_duration_seconds_count\{method="CreateVM",server="vmd",` + svc + `\} 2$`,
		`(?m)^homecloud_rpc_duration_seconds_bucket\{`,
		`(?m)^go_goroutines `,
	}
	for _, pat := range expect {
		if !regexp.MustCompile(pat).MatchString(body) {
			t.Errorf("/metrics body has no series matching %s", pat)
		}
	}
}

func TestMetricsHealthAndReadyNeedNoToken(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		if status, _ := metricsGet(e, path, ""); status != http.StatusOK {
			t.Errorf("%s without token: status %d, want 200", path, status)
		}
	}
}
