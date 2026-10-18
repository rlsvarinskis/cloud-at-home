package promcollect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/fake"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/scheduler"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/service"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/store"
)

const gib = int64(1) << 30

// t0 is where the test clock starts: Unix time 1800000000.
var t0 = time.Unix(1_800_000_000, 0).UTC()

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testClock is a manually advanced clock. It is shared by the store, the fake nodes and the collector.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fixture is a two-node vmd with four VMs:
//
//	n1 (ready)    vm-1 "alpha/web" 8 vCPU running, vm-2 "beta/db" 2 vCPU running
//	n2 (draining) vm-3 "alpha/api" stopped, vm-4 "gamma/job" in error but desired running
type fixture struct {
	t     *testing.T
	ctx   context.Context
	st    store.Store
	clk   *testClock
	n1    *fake.Driver
	n2    *fake.Driver
	nodes *service.NodeManager
	c     *Collector
	reg   *prometheus.Registry
}

func newFixture(t *testing.T, tweak func(*Options)) *fixture {
	t.Helper()
	ctx := context.Background()
	clk := &testClock{t: t0}
	st := store.NewMemory()

	n1 := fake.New(fake.Config{CPUs: 16, MemoryMB: 32768, DiskBytesTotal: 100 * gib, DiskBytesFree: 40 * gib})
	n1.Now = clk.Now
	n2 := fake.New(fake.Config{CPUs: 8, MemoryMB: 16384, DiskBytesTotal: 50 * gib, DiskBytesFree: 10 * gib})
	n2.Now = clk.Now

	nodes := service.NewNodeManager(st, discard(), clk.Now)
	nodes.Add("n1", "192.0.2.1:7000", n1)
	nodes.Add("n2", "192.0.2.2:7000", n2)
	nodes.Poll(ctx)
	row, err := st.GetNode(ctx, "n2")
	if err != nil {
		t.Fatal(err)
	}
	row.State = domain.NodeDraining
	if err := st.UpsertNode(ctx, row); err != nil {
		t.Fatal(err)
	}

	opts := Options{SampleInterval: 30 * time.Second, SampleTimeout: 10 * time.Second, Concurrency: 4,
		Now: clk.Now, Log: discard()}
	if tweak != nil {
		tweak(&opts)
	}
	f := &fixture{t: t, ctx: ctx, st: st, clk: clk, n1: n1, n2: n2, nodes: nodes,
		c: New(st, nodes, scheduler.Config{}, opts), reg: prometheus.NewRegistry()}
	register(t, f.reg, f.c)

	f.createVM(&domain.VM{ID: "vm-1", ProjectID: "alpha", Name: "web", NodeID: "n1", CPUs: 8, MemoryMB: 4096,
		DiskIDs: []string{"vm-1-os", "vm-1-data"}, Desired: domain.PowerRunning, State: domain.VMRunning}, true)
	f.createVM(&domain.VM{ID: "vm-2", ProjectID: "beta", Name: "db", NodeID: "n1", CPUs: 2, MemoryMB: 2048,
		DiskIDs: []string{"vm-2-os"}, Desired: domain.PowerRunning, State: domain.VMRunning}, true)
	f.createVM(&domain.VM{ID: "vm-3", ProjectID: "alpha", Name: "api", NodeID: "n2", CPUs: 4, MemoryMB: 1024,
		Desired: domain.PowerStopped, State: domain.VMStopped}, false)
	f.createVM(&domain.VM{ID: "vm-4", ProjectID: "gamma", Name: "job", NodeID: "n2", CPUs: 4, MemoryMB: 1024,
		Desired: domain.PowerRunning, State: domain.VMError}, false)
	n1.SetMetrics("vm-1", vm1Metrics())
	n1.SetMetrics("vm-2", vm2Metrics())
	return f
}

// createVM stores row and, when running is true, creates its disks and boots it on its node's fake
// driver so that GetVMMetrics works for it.
func (f *fixture) createVM(row *domain.VM, running bool) {
	f.t.Helper()
	if err := f.st.CreateVM(f.ctx, row); err != nil {
		f.t.Fatal(err)
	}
	if !running {
		return
	}
	drv := f.n1
	if row.NodeID == "n2" {
		drv = f.n2
	}
	for _, id := range row.DiskIDs {
		if _, err := drv.CreateDisk(f.ctx, &nodev1.CreateDiskRequest{DiskId: id, SizeGb: 20}); err != nil {
			f.t.Fatal(err)
		}
	}
	if _, err := drv.StartVM(f.ctx, &nodev1.StartVMRequest{VmId: row.ID, Name: row.Name, Cpus: int32(row.CPUs),
		MemoryMb: row.MemoryMB, DiskIds: row.DiskIDs}); err != nil {
		f.t.Fatal(err)
	}
}

// setVM changes a stored VM row through a compare-and-swap update.
func (f *fixture) setVM(id string, change func(*domain.VM)) {
	f.t.Helper()
	v, err := f.st.GetVM(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	change(v)
	if err := f.st.UpdateVM(f.ctx, v); err != nil {
		f.t.Fatal(err)
	}
}

// want checks the samples of one metric family as seen through the fixture's registry. Each line is
// the sample's labels and value, such as `{node="n1"} 1` or `16` for an unlabelled metric.
func (f *fixture) want(family string, lines ...string) {
	f.t.Helper()
	checkLines(f.t, family, gather(f.t, f.reg)[family], lines)
}

// register adds c to reg and fails the test if the registry refuses it.
func register(t *testing.T, reg prometheus.Registerer, c prometheus.Collector) {
	t.Helper()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
}

// gather returns, per gauge or counter family, its samples as sorted `{labels} value` strings. Labels
// are in name order; an unlabelled sample is written as " value". Histograms are left out.
func gather(t *testing.T, g prometheus.Gatherer) map[string][]string {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := make(map[string][]string)
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			var v float64
			switch {
			case m.GetGauge() != nil:
				v = m.GetGauge().GetValue()
			case m.GetCounter() != nil:
				v = m.GetCounter().GetValue()
			default:
				continue
			}
			var pairs []string
			for _, lp := range m.GetLabel() {
				pairs = append(pairs, fmt.Sprintf("%s=%q", lp.GetName(), lp.GetValue()))
			}
			labels := ""
			if len(pairs) > 0 {
				labels = "{" + strings.Join(pairs, ",") + "}"
			}
			line := labels + " " + strconv.FormatFloat(v, 'f', -1, 64)
			out[mf.GetName()] = append(out[mf.GetName()], line)
		}
	}
	for name := range out {
		slices.Sort(out[name])
	}
	return out
}

// checkLines compares the samples of family with want, in the normalised form gather produces.
func checkLines(t *testing.T, family string, got, want []string) {
	t.Helper()
	norm := make([]string, 0, len(want))
	for _, w := range want {
		if !strings.HasPrefix(w, "{") {
			w = " " + w
		}
		norm = append(norm, w)
	}
	slices.Sort(norm)
	if !slices.Equal(got, norm) {
		t.Errorf("%s:\n got: %q\nwant: %q", family, got, norm)
	}
}

// histogramStats returns the sample count and sum of a histogram family.
func histogramStats(t *testing.T, g prometheus.Gatherer, family string) (uint64, float64) {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == family && len(mf.GetMetric()) == 1 {
			h := mf.GetMetric()[0].GetHistogram()
			return h.GetSampleCount(), h.GetSampleSum()
		}
	}
	t.Fatalf("histogram %s not found", family)
	return 0, 0
}

func vm1Metrics() *nodev1.GetVMMetricsResponse {
	return &nodev1.GetVMMetricsResponse{
		CpuPercent: 812.5, CpuCores: 8, Load1: 3.25, Load5: 2.5, Load15: 1.75,
		MemTotalBytes: 4 * gib, MemUsedBytes: gib, MemAvailableBytes: 3 * gib,
		SwapTotalBytes: 2 * gib, SwapUsedBytes: gib / 4, UptimeSeconds: 86400,
		Filesystems: []*nodev1.GuestFilesystem{
			{DiskId: "vm-1-os", Device: "/dev/vda1", MountPoint: "/", SizeBytes: 20 * gib, UsedBytes: 8 * gib},
			{DiskId: "vm-1-data", Device: "/dev/vdb1", MountPoint: "/srv/data", SizeBytes: 100 * gib, UsedBytes: 50 * gib},
		},
		Disks: []*nodev1.DiskAllocation{
			{DiskId: "vm-1-os", VirtualSizeBytes: 20 * gib, AllocatedBytes: 8 * gib},
			{DiskId: "vm-1-data", VirtualSizeBytes: 100 * gib, AllocatedBytes: 50 * gib},
		},
	}
}

func vm2Metrics() *nodev1.GetVMMetricsResponse {
	return &nodev1.GetVMMetricsResponse{
		CpuPercent: 12.5, CpuCores: 2, Load1: 0.5, Load5: 0.4, Load15: 0.3,
		MemTotalBytes: 2 * gib, MemUsedBytes: gib / 2, MemAvailableBytes: 3 * gib / 2,
		UptimeSeconds: 3600,
		Filesystems: []*nodev1.GuestFilesystem{
			{DiskId: "vm-2-os", Device: "/dev/vda1", MountPoint: "/", SizeBytes: 10 * gib, UsedBytes: gib},
		},
		Disks: []*nodev1.DiskAllocation{
			{DiskId: "vm-2-os", VirtualSizeBytes: 10 * gib, AllocatedBytes: 2 * gib},
		},
	}
}

func TestOptionsDefaults(t *testing.T) {
	c := New(store.NewMemory(), nil, scheduler.Config{}, Options{})
	if c.opts.SampleInterval != 30*time.Second || c.opts.SampleTimeout != 10*time.Second || c.opts.Concurrency != 8 {
		t.Errorf("defaults: interval %v timeout %v concurrency %d", c.opts.SampleInterval, c.opts.SampleTimeout, c.opts.Concurrency)
	}
	if c.opts.Now == nil || c.opts.Log == nil {
		t.Error("Now and Log must default to non-nil")
	}
}

func TestNodeAndClusterCapacity(t *testing.T) {
	f := newFixture(t, nil)
	// n2 is draining, so it is excluded from the cluster totals, and a draining node hands out nothing.
	f.want("homecloud_node_up", `{node="n1"} 1`, `{node="n2"} 0`)
	f.want("homecloud_node_draining", `{node="n1"} 0`, `{node="n2"} 1`)
	f.want("homecloud_node_cpu_threads", `{node="n1"} 16`, `{node="n2"} 8`)
	f.want("homecloud_node_cpu_allocated_vcpus", `{node="n1"} 10`, `{node="n2"} 4`)
	f.want("homecloud_node_cpu_available_vcpus", `{node="n1"} 6`, `{node="n2"} 0`)
	f.want("homecloud_node_memory_total_bytes", `{node="n1"} 34359738368`, `{node="n2"} 17179869184`)
	f.want("homecloud_node_memory_allocated_bytes", `{node="n1"} 6442450944`, `{node="n2"} 1073741824`)
	f.want("homecloud_node_memory_available_bytes", `{node="n1"} 27917287424`, `{node="n2"} 0`)
	f.want("homecloud_node_disk_total_bytes", `{node="n1"} 107374182400`, `{node="n2"} 53687091200`)
	f.want("homecloud_node_disk_free_bytes", `{node="n1"} 42949672960`, `{node="n2"} 10737418240`)
	f.want("homecloud_node_vms", `{node="n1"} 2`, `{node="n2"} 2`)
	f.want("homecloud_node_vms_running", `{node="n1"} 2`, `{node="n2"} 0`)
	f.want("homecloud_node_last_heartbeat_timestamp_seconds", `{node="n1"} 1800000000`, `{node="n2"} 1800000000`)

	f.want("homecloud_cluster_cpu_threads", "16")
	f.want("homecloud_cluster_cpu_allocated_vcpus", "10")
	f.want("homecloud_cluster_cpu_available_vcpus", "6")
	f.want("homecloud_cluster_memory_total_bytes", "34359738368")
	f.want("homecloud_cluster_memory_allocated_bytes", "6442450944")
	f.want("homecloud_cluster_memory_available_bytes", "27917287424")
	f.want("homecloud_cluster_disk_total_bytes", "107374182400")
	f.want("homecloud_cluster_disk_free_bytes", "42949672960")
	f.want("homecloud_cluster_vms", "2")
	f.want("homecloud_cluster_vms_running", "2")
	f.want("homecloud_cluster_nodes_ready", "1")
}

func TestVMCountsIncludeZeroStates(t *testing.T) {
	f := newFixture(t, nil)
	f.want("homecloud_vms",
		`{state="deleting"} 0`, `{state="error"} 1`, `{state="pending"} 0`, `{state="running"} 2`,
		`{state="starting"} 0`, `{state="stopped"} 1`, `{state="stopping"} 0`)
	f.want("homecloud_vms_desired_running", "3") // vm-1, vm-2 and vm-4

	for _, id := range []string{"vm-1", "vm-2", "vm-3", "vm-4"} {
		if err := f.st.DeleteVM(f.ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	f.want("homecloud_vms",
		`{state="deleting"} 0`, `{state="error"} 0`, `{state="pending"} 0`, `{state="running"} 0`,
		`{state="starting"} 0`, `{state="stopped"} 0`, `{state="stopping"} 0`)
	f.want("homecloud_vms_desired_running", "0")
}

func TestExpositionFormat(t *testing.T) {
	f := newFixture(t, nil)
	want := `# HELP homecloud_vms VMs per state across all nodes.
# TYPE homecloud_vms gauge
homecloud_vms{state="deleting"} 0
homecloud_vms{state="error"} 1
homecloud_vms{state="pending"} 0
homecloud_vms{state="running"} 2
homecloud_vms{state="starting"} 0
homecloud_vms{state="stopped"} 1
homecloud_vms{state="stopping"} 0
# HELP homecloud_vms_desired_running VMs whose desired power state is running.
# TYPE homecloud_vms_desired_running gauge
homecloud_vms_desired_running 3
`
	if err := testutil.GatherAndCompare(f.reg, strings.NewReader(want), "homecloud_vms", "homecloud_vms_desired_running"); err != nil {
		t.Error(err)
	}
}

func TestPerVMSeriesAfterSample(t *testing.T) {
	f := newFixture(t, nil)
	f.want("homecloud_vm_cpu_percent") // nothing until the first sampling pass
	f.c.SampleOnce(f.ctx)

	// vm-1 is an 8-vCPU guest busy on every vCPU: 800 plus its 12.5 of other work reads 812.5.
	f.want("homecloud_vm_cpu_percent",
		`{name="web",node="n1",project="alpha",vm_id="vm-1"} 812.5`,
		`{name="db",node="n1",project="beta",vm_id="vm-2"} 12.5`)
	f.want("homecloud_vm_cpu_cores",
		`{name="web",node="n1",project="alpha",vm_id="vm-1"} 8`,
		`{name="db",node="n1",project="beta",vm_id="vm-2"} 2`)
	f.want("homecloud_vm_memory_used_bytes",
		`{name="web",node="n1",project="alpha",vm_id="vm-1"} 1073741824`,
		`{name="db",node="n1",project="beta",vm_id="vm-2"} 536870912`)
	f.want("homecloud_vm_load1",
		`{name="web",node="n1",project="alpha",vm_id="vm-1"} 3.25`,
		`{name="db",node="n1",project="beta",vm_id="vm-2"} 0.5`)
	f.want("homecloud_vm_metrics_age_seconds",
		`{name="web",node="n1",project="alpha",vm_id="vm-1"} 0`,
		`{name="db",node="n1",project="beta",vm_id="vm-2"} 0`)
	f.want("homecloud_vm_filesystem_size_bytes",
		`{device="/dev/vda1",disk_id="vm-1-os",mount_point="/",name="web",node="n1",project="alpha",vm_id="vm-1"} 21474836480`,
		`{device="/dev/vdb1",disk_id="vm-1-data",mount_point="/srv/data",name="web",node="n1",project="alpha",vm_id="vm-1"} 107374182400`,
		`{device="/dev/vda1",disk_id="vm-2-os",mount_point="/",name="db",node="n1",project="beta",vm_id="vm-2"} 10737418240`)
	f.want("homecloud_vm_filesystem_used_bytes",
		`{device="/dev/vda1",disk_id="vm-1-os",mount_point="/",name="web",node="n1",project="alpha",vm_id="vm-1"} 8589934592`,
		`{device="/dev/vdb1",disk_id="vm-1-data",mount_point="/srv/data",name="web",node="n1",project="alpha",vm_id="vm-1"} 53687091200`,
		`{device="/dev/vda1",disk_id="vm-2-os",mount_point="/",name="db",node="n1",project="beta",vm_id="vm-2"} 1073741824`)
	f.want("homecloud_vm_disk_virtual_size_bytes",
		`{disk_id="vm-1-os",name="web",node="n1",project="alpha",vm_id="vm-1"} 21474836480`,
		`{disk_id="vm-1-data",name="web",node="n1",project="alpha",vm_id="vm-1"} 107374182400`,
		`{disk_id="vm-2-os",name="db",node="n1",project="beta",vm_id="vm-2"} 10737418240`)
	f.want("homecloud_vm_disk_allocated_bytes",
		`{disk_id="vm-1-os",name="web",node="n1",project="alpha",vm_id="vm-1"} 8589934592`,
		`{disk_id="vm-1-data",name="web",node="n1",project="alpha",vm_id="vm-1"} 53687091200`,
		`{disk_id="vm-2-os",name="db",node="n1",project="beta",vm_id="vm-2"} 2147483648`)

	f.want("homecloud_vm_sample_errors_total", "0")
	f.want("homecloud_vm_samples_last_timestamp_seconds", strconv.FormatInt(t0.Unix(), 10))
}

func TestStoppedVMSeriesVanish(t *testing.T) {
	f := newFixture(t, nil)
	f.c.SampleOnce(f.ctx)

	// Stop vm-2 on its node and in the store: its series go at the next pass.
	if _, err := f.n1.StopVM(f.ctx, &nodev1.StopVMRequest{VmId: "vm-2"}); err != nil {
		t.Fatal(err)
	}
	f.setVM("vm-2", func(v *domain.VM) { v.State, v.Desired = domain.VMStopped, domain.PowerStopped })
	f.c.SampleOnce(f.ctx)

	f.want("homecloud_vm_cpu_percent", `{name="web",node="n1",project="alpha",vm_id="vm-1"} 812.5`)
	f.want("homecloud_vm_disk_allocated_bytes",
		`{disk_id="vm-1-os",name="web",node="n1",project="alpha",vm_id="vm-1"} 8589934592`,
		`{disk_id="vm-1-data",name="web",node="n1",project="alpha",vm_id="vm-1"} 53687091200`)
	f.want("homecloud_vms", `{state="deleting"} 0`, `{state="error"} 1`, `{state="pending"} 0`,
		`{state="running"} 1`, `{state="starting"} 0`, `{state="stopped"} 2`, `{state="stopping"} 0`)
}

func TestSamplesExpireAfterThreeIntervals(t *testing.T) {
	f := newFixture(t, nil) // SampleInterval is 30s, so a sample expires after 90s without success
	f.c.SampleOnce(f.ctx)

	f.n1.SetDown(true)
	f.clk.Advance(30 * time.Second)
	f.c.SampleOnce(f.ctx) // both samples fail; the old ones are kept
	f.want("homecloud_vm_sample_errors_total", "2")
	f.want("homecloud_vm_metrics_age_seconds",
		`{name="web",node="n1",project="alpha",vm_id="vm-1"} 30`,
		`{name="db",node="n1",project="beta",vm_id="vm-2"} 30`)

	f.clk.Advance(60 * time.Second) // age 90: still exported
	f.want("homecloud_vm_metrics_age_seconds",
		`{name="web",node="n1",project="alpha",vm_id="vm-1"} 90`,
		`{name="db",node="n1",project="beta",vm_id="vm-2"} 90`)

	f.clk.Advance(time.Second) // age 91: dropped
	f.want("homecloud_vm_cpu_percent")

	f.n1.SetDown(false)
	f.c.SampleOnce(f.ctx)
	f.want("homecloud_vm_cpu_percent",
		`{name="web",node="n1",project="alpha",vm_id="vm-1"} 812.5`,
		`{name="db",node="n1",project="beta",vm_id="vm-2"} 12.5`)
}

func TestSampleFailureKeepsNoNewSample(t *testing.T) {
	f := newFixture(t, nil)
	f.n1.FailNext("GetVMMetrics", errors.New("guest agent timeout"))
	f.c.SampleOnce(f.ctx)

	// One VM was sampled, the other failed and had no earlier sample to keep.
	f.want("homecloud_vm_sample_errors_total", "1")
	if got := testutil.CollectAndCount(f.c, "homecloud_vm_cpu_percent"); got != 1 {
		t.Errorf("cpu series after one failed sample: got %d, want 1", got)
	}
}

func TestCapacityErrorIsCountedAndSkipped(t *testing.T) {
	f := newFixture(t, nil)
	broken := New(failingListNodes{f.st}, f.nodes, scheduler.Config{}, Options{Now: f.clk.Now, Log: discard()})
	reg := prometheus.NewRegistry()
	register(t, reg, broken)

	got := gather(t, reg) // one gather, so the error counter moves exactly once
	checkLines(t, "homecloud_vmd_scrape_errors_total", got["homecloud_vmd_scrape_errors_total"],
		[]string{`{source="capacity"} 1`})
	for _, family := range []string{"homecloud_node_up", "homecloud_cluster_cpu_threads", "homecloud_vms"} {
		checkLines(t, family, got[family], nil)
	}
}

// failingListNodes is a store whose node listing fails, as a database outage would.
type failingListNodes struct{ store.Store }

func (failingListNodes) ListNodes(context.Context) ([]*domain.Node, error) {
	return nil, errors.New("store unavailable")
}

func TestHooksCountReconciliations(t *testing.T) {
	f := newFixture(t, nil)
	h := f.c.Hooks()
	vm := &domain.VM{ID: "vm-1"}
	h.OnVMReconciled(vm, 250*time.Millisecond, nil)
	h.OnVMReconciled(vm, 3*time.Second, errors.New("boom"))
	h.OnTick(2, 500*time.Millisecond)
	h.OnTick(0, 2*time.Second)

	f.want("homecloud_reconcile_vm_total", `{outcome="error"} 1`, `{outcome="ok"} 1`)
	f.want("homecloud_reconcile_ticks_total", "2")
	f.want("homecloud_reconcile_last_tick_claimed", "0")
	if n, sum := histogramStats(t, f.reg, "homecloud_reconcile_vm_duration_seconds"); n != 2 || sum != 3.25 {
		t.Errorf("vm duration: count %d sum %v, want 2 and 3.25", n, sum)
	}
	if n, sum := histogramStats(t, f.reg, "homecloud_reconcile_tick_duration_seconds"); n != 2 || sum != 2.5 {
		t.Errorf("tick duration: count %d sum %v, want 2 and 2.5", n, sum)
	}
}

func TestDescribeMatchesCollect(t *testing.T) {
	f := newFixture(t, nil)
	f.c.SampleOnce(f.ctx)

	// A pedantic registry rejects Collect output that Describe did not announce.
	reg := prometheus.NewPedanticRegistry()
	register(t, reg, f.c)
	if _, err := reg.Gather(); err != nil {
		t.Fatal(err)
	}
	problems, err := testutil.CollectAndLint(f.c)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("lint: %s: %s", p.Metric, p.Text)
	}
}

func TestRegisterTwiceFailsCleanly(t *testing.T) {
	f := newFixture(t, nil) // already registered once
	err := f.reg.Register(f.c)
	var already prometheus.AlreadyRegisteredError
	if !errors.As(err, &already) {
		t.Fatalf("second register: got %v, want AlreadyRegisteredError", err)
	}
	// A second collector with the same metric names conflicts with the first.
	other := New(f.st, f.nodes, scheduler.Config{}, Options{Log: discard()})
	if err := f.reg.Register(other); err == nil {
		t.Error("registering a second collector with the same names succeeded")
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	f := newFixture(t, nil)
	// Project IDs are free text in the store, so they can hold quotes, backslashes and newlines.
	f.createVM(&domain.VM{ID: "vm-5", ProjectID: "we\"ird\\proj\nx", Name: "odd", NodeID: "n1", CPUs: 1, MemoryMB: 512,
		Desired: domain.PowerRunning, State: domain.VMRunning}, true)
	f.n1.SetMetrics("vm-5", &nodev1.GetVMMetricsResponse{CpuCores: 1})
	f.c.SampleOnce(f.ctx)

	f.want("homecloud_vm_cpu_cores",
		`{name="web",node="n1",project="alpha",vm_id="vm-1"} 8`,
		`{name="db",node="n1",project="beta",vm_id="vm-2"} 2`,
		`{name="odd",node="n1",project="we\"ird\\proj\nx",vm_id="vm-5"} 1`)
}

func TestRunSamplesThenReturnsOnCancel(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	// Run samples once before it looks at the context, then returns.
	f.c.Run(ctx)
	if got := testutil.CollectAndCount(f.c, "homecloud_vm_cpu_percent"); got != 2 {
		t.Errorf("series after Run with a cancelled context: got %d, want 2", got)
	}
}

func TestScrapeWhileSampling(t *testing.T) {
	f := newFixture(t, nil)
	h := f.c.Hooks()
	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	run(func() {
		for range 30 {
			f.c.SampleOnce(f.ctx)
		}
	})
	run(func() {
		for range 30 {
			f.clk.Advance(time.Second)
		}
	})
	run(func() {
		for range 30 {
			if _, err := f.reg.Gather(); err != nil {
				t.Error(err)
			}
		}
	})
	run(func() {
		for range 30 {
			h.OnVMReconciled(nil, time.Millisecond, nil)
			h.OnTick(1, time.Millisecond)
		}
	})
	wg.Wait()
}
