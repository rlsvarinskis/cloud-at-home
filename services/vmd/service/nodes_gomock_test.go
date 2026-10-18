package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	nodemock "github.com/rlsvarinskis/cloud-at-home/libs/qemu/node/mock"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/service"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/store"
)

// gmNodeClock is a clock the test moves by hand; polls run sequentially so there is no race on it.
type gmNodeClock struct{ t time.Time }

func (c *gmNodeClock) Now() time.Time          { return c.t }
func (c *gmNodeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func gmCapacity() *nodev1.GetCapacityResponse {
	return &nodev1.GetCapacityResponse{Capacity: &nodev1.Capacity{
		CpusTotal: 16, MemoryMbTotal: 65536, DiskBytesFree: 1 << 40, DiskBytesTotal: 2 << 40}}
}

func gmNodeFixture(t *testing.T) (*service.NodeManager, *nodemock.MockDriver, store.Store, *gmNodeClock) {
	t.Helper()
	ctrl := gomock.NewController(t)
	drv := nodemock.NewMockDriver(ctrl)
	st := store.NewMemory()
	clk := &gmNodeClock{t: gmNow}
	m := service.NewNodeManager(st, nil, clk.Now)
	m.Add("n1", "gm://n1", drv)
	return m, drv, st, clk
}

// TestGMPollMarksReadyWithCapacity: a successful GetCapacity makes the node ready and records its capacity
// and the heartbeat time from the injected clock.
func TestGMPollMarksReadyWithCapacity(t *testing.T) {
	m, drv, st, clk := gmNodeFixture(t)
	drv.EXPECT().GetCapacity(gomock.Any(), gomock.Any()).Return(gmCapacity(), nil)

	m.Poll(context.Background())

	n, err := st.GetNode(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Node{State: domain.NodeReady, CPUs: 16, MemoryMB: 65536, DiskBytesFree: 1 << 40, DiskBytesTotal: 2 << 40,
		LastHeartbeat: clk.Now()}
	if n.State != want.State || n.CPUs != want.CPUs || n.MemoryMB != want.MemoryMB ||
		n.DiskBytesFree != want.DiskBytesFree || n.DiskBytesTotal != want.DiskBytesTotal || !n.LastHeartbeat.Equal(want.LastHeartbeat) {
		t.Fatalf("node row = %+v, want state %s with capacity %d/%d and heartbeat %v", n, want.State, want.CPUs, want.MemoryMB, want.LastHeartbeat)
	}
}

// TestGMNodeDownOnlyAfterSilence: a failing GetCapacity marks the node down only once its last heartbeat is
// more than 15 s old on the injected clock (exactly 15 s is still up), and the next success recovers it.
func TestGMNodeDownOnlyAfterSilence(t *testing.T) {
	steps := []struct {
		name    string
		advance time.Duration
		fail    bool
		want    domain.NodeState
	}{
		{name: "healthy poll at t0", advance: 0, fail: false, want: domain.NodeReady},
		{name: "silent for 10s is still ready", advance: 10 * time.Second, fail: true, want: domain.NodeReady},
		{name: "silent for exactly 15s is still ready", advance: 5 * time.Second, fail: true, want: domain.NodeReady},
		{name: "silent for 15s+1ms is down", advance: time.Millisecond, fail: true, want: domain.NodeDown},
		{name: "recovered poll is ready", advance: time.Second, fail: false, want: domain.NodeReady},
		{name: "failure right after recovery is ready again", advance: 0, fail: true, want: domain.NodeReady},
	}
	m, drv, st, clk := gmNodeFixture(t)
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			clk.Advance(s.advance)
			if s.fail {
				drv.EXPECT().GetCapacity(gomock.Any(), gomock.Any()).Return(nil, errors.New("dial tcp 192.0.2.5:7000: refused"))
			} else {
				drv.EXPECT().GetCapacity(gomock.Any(), gomock.Any()).Return(gmCapacity(), nil)
			}
			m.Poll(context.Background())
			n, err := st.GetNode(context.Background(), "n1")
			if err != nil {
				t.Fatal(err)
			}
			if n.State != s.want {
				t.Fatalf("state = %s, want %s", n.State, s.want)
			}
		})
	}
}

// TestGMDrainingSurvivesOutage: the operator's drain decision is stored apart from health, so ordinary polls, a
// short failure and a long outage followed by recovery all leave the node draining.
func TestGMDrainingSurvivesOutage(t *testing.T) {
	m, drv, st, clk := gmNodeFixture(t)
	ctx := context.Background()
	drv.EXPECT().GetCapacity(gomock.Any(), gomock.Any()).Return(gmCapacity(), nil)
	m.Poll(ctx)
	if err := st.SetNodeDrain(ctx, "n1", true); err != nil {
		t.Fatal(err)
	}
	effective := func() domain.NodeState {
		n, err := st.GetNode(ctx, "n1")
		if err != nil {
			t.Fatal(err)
		}
		return n.Effective()
	}

	// Ordinary polls and a short failure: still draining.
	drv.EXPECT().GetCapacity(gomock.Any(), gomock.Any()).Return(gmCapacity(), nil)
	m.Poll(ctx)
	clk.Advance(5 * time.Second)
	drv.EXPECT().GetCapacity(gomock.Any(), gomock.Any()).Return(nil, errors.New("timeout"))
	m.Poll(ctx)
	if got := effective(); got != domain.NodeDraining {
		t.Fatalf("after a short failure the node is %s, want draining", got)
	}

	// Long outage: the node is down (down beats draining), and after recovery the drain is still in force.
	clk.Advance(20 * time.Second)
	drv.EXPECT().GetCapacity(gomock.Any(), gomock.Any()).Return(nil, errors.New("timeout"))
	m.Poll(ctx)
	if got := effective(); got != domain.NodeDown {
		t.Fatalf("during a long outage the node is %s, want down", got)
	}
	drv.EXPECT().GetCapacity(gomock.Any(), gomock.Any()).Return(gmCapacity(), nil)
	m.Poll(ctx)
	if got := effective(); got != domain.NodeDraining {
		t.Fatalf("draining node came back as %s after an outage; the operator's drain was lost", got)
	}
}

// Rows written before drain had its own column stored the decision as State "draining". Polling migrates them.
func TestGMLegacyDrainingStateIsMigrated(t *testing.T) {
	m, drv, st, _ := gmNodeFixture(t)
	ctx := context.Background()
	drv.EXPECT().GetCapacity(gomock.Any(), gomock.Any()).Return(gmCapacity(), nil).Times(2)
	m.Poll(ctx)
	n, _ := st.GetNode(ctx, "n1")
	n.State = domain.NodeDraining
	if err := st.UpsertNode(ctx, n); err != nil {
		t.Fatal(err)
	}
	m.Poll(ctx)
	got, _ := st.GetNode(ctx, "n1")
	if got.State != domain.NodeReady || !got.Drain || got.Effective() != domain.NodeDraining {
		t.Fatalf("legacy draining row not migrated: state=%s drain=%v", got.State, got.Drain)
	}
}

// TestGMInfoSumsDesiredRunningOnly: Info counts the CPU and memory of VMs that should be running, per node,
// and ignores stopped, deleting and wrong-node VMs.
func TestGMInfoSumsDesiredRunningOnly(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	drv1, drv2 := nodemock.NewMockDriver(ctrl), nodemock.NewMockDriver(ctrl)
	st := store.NewMemory()
	clk := &gmNodeClock{t: gmNow}
	m := service.NewNodeManager(st, nil, clk.Now)
	m.Add("n1", "gm://n1", drv1)
	m.Add("n2", "gm://n2", drv2)
	drv1.EXPECT().GetCapacity(gomock.Any(), gomock.Any()).Return(gmCapacity(), nil)
	drv2.EXPECT().GetCapacity(gomock.Any(), gomock.Any()).Return(gmCapacity(), nil)
	m.Poll(ctx)

	vms := []*domain.VM{
		{ID: "vm_run", ProjectID: "p1", Name: "run", NodeID: "n1", CPUs: 2, MemoryMB: 2048, Desired: domain.PowerRunning, State: domain.VMRunning},
		{ID: "vm_err", ProjectID: "p1", Name: "err", NodeID: "n1", CPUs: 1, MemoryMB: 512, Desired: domain.PowerRunning, State: domain.VMError},
		{ID: "vm_stopped", ProjectID: "p1", Name: "stopped", NodeID: "n1", CPUs: 4, MemoryMB: 4096, Desired: domain.PowerStopped, State: domain.VMRunning},
		{ID: "vm_deleted", ProjectID: "p1", Name: "deleted", NodeID: "n1", CPUs: 16, MemoryMB: 8192, Desired: domain.PowerDeleted, State: domain.VMDeleting},
		{ID: "vm_other", ProjectID: "p1", Name: "other", NodeID: "n2", CPUs: 8, MemoryMB: 1024, Desired: domain.PowerRunning, State: domain.VMRunning},
	}
	for _, v := range vms {
		if err := st.CreateVM(ctx, v); err != nil {
			t.Fatal(err)
		}
	}

	infos, err := m.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]int64{}
	for _, ni := range infos {
		got[ni.ID] = [2]int64{int64(ni.AllocatedCPUs), ni.AllocatedMemoryMB}
		if ni.CPUs != 16 {
			t.Errorf("node %s CPUs = %d, want 16", ni.ID, ni.CPUs)
		}
	}
	want := map[string][2]int64{"n1": {3, 2560}, "n2": {8, 1024}}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("node %s allocated (cpus, mem) = %v, want %v", id, got[id], w)
		}
	}
}
