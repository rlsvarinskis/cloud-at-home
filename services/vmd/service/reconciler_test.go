package service_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/fake"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/node"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/service"
)

// ------------------------------------------------------------------------------------ helpers

// rectCfg is the reconciler config used by the harness, so extra reconcilers behave the same way.
func rectCfg(e *env) service.Config {
	return e.cfg // same clock and sharding hooks as the env's own reconciler
}

// rectVM reads the stored VM.
func rectVM(e *env, id string) *domain.VM {
	e.t.Helper()
	v, err := e.st.GetVM(context.Background(), id)
	if err != nil {
		e.t.Fatalf("get vm %s: %v", id, err)
	}
	return v
}

// rectDue advances the clock to when the VM is next due (if it is not due already) and runs one pass of rr.
// Resync and backoff intervals are long compared with the one-second ticks of e.tick, so tests use this.
func rectDue(e *env, rr *service.Reconciler, id string) {
	e.t.Helper()
	if v := rectVM(e, id); v.NextReconcile.After(e.clk.Now()) {
		e.clk.Advance(v.NextReconcile.Sub(e.clk.Now()))
	}
	rr.Tick(context.Background())
}

// rectRun runs rectDue until cond holds for the stored VM, failing the test after max passes.
func rectRun(e *env, rr *service.Reconciler, id string, max int, what string, cond func(*domain.VM) bool) *domain.VM {
	e.t.Helper()
	for range max {
		if v := rectVM(e, id); cond(v) {
			return v
		}
		rectDue(e, rr, id)
	}
	v := rectVM(e, id)
	if !cond(v) {
		e.t.Fatalf("%s not reached after %d passes (state=%s reason=%q)", what, max, v.State, v.StateReason)
	}
	return v
}

// rectGone runs rectDue until the VM row no longer exists.
func rectGone(e *env, rr *service.Reconciler, id string, max int) {
	e.t.Helper()
	for range max {
		if _, err := e.st.GetVM(context.Background(), id); err != nil {
			return
		}
		rectDue(e, rr, id)
	}
	e.t.Fatalf("vm %s still exists after %d passes (state=%s reason=%q)", id, max, rectVM(e, id).State, rectVM(e, id).StateReason)
}

// rectCalls counts how many times the fake node received method.
func rectCalls(e *env, method string) int {
	n := 0
	for _, c := range e.node.Calls() {
		if c == method {
			n++
		}
	}
	return n
}

func rectStop(e *env, id string, force bool) {
	e.t.Helper()
	if _, err := e.editor().VM.StopVM(context.Background(), connect.NewRequest(&vmdv1.StopVMRequest{Id: id, Force: force})); err != nil {
		e.t.Fatal(err)
	}
}

func rectDelete(e *env, id string, deleteDisks bool) {
	e.t.Helper()
	if _, err := e.editor().VM.DeleteVM(context.Background(), connect.NewRequest(&vmdv1.DeleteVMRequest{Id: id, DeleteDisks: deleteDisks})); err != nil {
		e.t.Fatal(err)
	}
}

// rectGate holds one call open: the driver closes entered and then waits for release.
type rectGate struct {
	entered chan struct{}
	release chan struct{}
}

// rectDriver wraps the fake node. It can hold the next GetVMRuntime open, and can make ForgetVM fail.
type rectDriver struct {
	node.Driver
	mu        sync.Mutex
	gate      *rectGate
	forgetErr error
	forgets   int
}

func (d *rectDriver) GetVMRuntime(ctx context.Context, r *nodev1.GetVMRuntimeRequest) (*nodev1.GetVMRuntimeResponse, error) {
	d.mu.Lock()
	g := d.gate
	d.gate = nil
	d.mu.Unlock()
	if g != nil {
		close(g.entered)
		<-g.release
	}
	return d.Driver.GetVMRuntime(ctx, r)
}

func (d *rectDriver) ForgetVM(ctx context.Context, r *nodev1.ForgetVMRequest) (*nodev1.ForgetVMResponse, error) {
	d.mu.Lock()
	d.forgets++
	err := d.forgetErr
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return d.Driver.ForgetVM(ctx, r)
}

// rectReconciler builds a reconciler for the harness store whose node "n1" is driven by d.
func rectReconciler(e *env, d node.Driver, owner string) *service.Reconciler {
	nm := service.NewNodeManager(e.st, nil, e.clk.Now)
	nm.Add("n1", "fake://n1", d)
	return service.NewReconciler(e.st, nm, rectCfg(e), owner)
}

// ------------------------------------------------------------------------------------ 1. crashes

func TestReconCrashIsRestartedAndReasonKept(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("crash1")

	e.node.Crash(vm.Id, "oom")
	rectDue(e, e.rec, vm.Id)

	got := rectVM(e, vm.Id)
	if got.State != domain.VMStarting {
		t.Fatalf("after crash and one pass: state %s, want starting", got.State)
	}
	if !strings.Contains(got.StateReason, "VM exited unexpectedly") || !strings.Contains(got.StateReason, "oom") {
		t.Errorf("state reason %q does not mention the exit", got.StateReason)
	}
	if n := rectCalls(e, "StartVM"); n != 2 { // the first start plus the restart
		t.Errorf("StartVM calls = %d, want 2", n)
	}

	rectRun(e, e.rec, vm.Id, 5, "running after restart", func(v *domain.VM) bool { return v.State == domain.VMRunning })
	got = rectVM(e, vm.Id)
	if got.StateReason != "" || got.Failures != 0 {
		t.Errorf("after recovery: reason %q failures %d, want empty and 0", got.StateReason, got.Failures)
	}
}

func TestReconCrashLoopGoesToErrorWithBackoff(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("loop")

	for i := 1; i <= 3; i++ {
		e.node.Crash(vm.Id, "oom")
		rectDue(e, e.rec, vm.Id)
		got := rectVM(e, vm.Id)
		if got.State != domain.VMStarting || got.Crashes != i {
			t.Fatalf("crash %d: state %s crashes %d, want starting and %d", i, got.State, got.Crashes, i)
		}
	}

	// The fourth crash is over the limit: the VM goes to ERROR and is retried after the backoff for 4 failures (16 s).
	e.node.Crash(vm.Id, "oom")
	rectDue(e, e.rec, vm.Id)
	got := rectVM(e, vm.Id)
	if got.State != domain.VMError {
		t.Fatalf("crash loop: state %s, want error", got.State)
	}
	if !strings.Contains(got.StateReason, "VM exited unexpectedly") {
		t.Errorf("crash loop: reason %q, want it to say the VM exited unexpectedly", got.StateReason)
	}
	if d := got.NextReconcile.Sub(e.clk.Now()); d != 16*time.Second {
		t.Errorf("crash loop: next reconcile in %s, want 16s (backoff for 4 failures)", d)
	}

	// Once the loop stops (the fake VM stays down until the reconciler restarts it), the VM runs again.
	rectRun(e, e.rec, vm.Id, 5, "running again", func(v *domain.VM) bool { return v.State == domain.VMRunning })
	got = rectVM(e, vm.Id)
	if got.Failures != 0 || got.StateReason != "" {
		t.Errorf("after recovery: failures %d reason %q, want 0 and empty", got.Failures, got.StateReason)
	}
	// Coming back up does not erase the crash history: it is forgiven one crash per resync interval of
	// continuous health (10 s here), so a VM that boots and crashes again keeps backing off.
	if got.Crashes != 4 {
		t.Errorf("right after recovery: crashes = %d, want 4 (not yet forgiven)", got.Crashes)
	}
	e.tick(45)
	if got = rectVM(e, vm.Id); got.Crashes != 0 || got.State != domain.VMRunning {
		t.Errorf("after 45 s healthy: crashes = %d state %s, want 0 and running", got.Crashes, got.State)
	}
}

// Each crash loop backs off further than the last: a recovery that merely reaches RUNNING does not erase the history.
func TestReconCrashLoopBackoffGrowsAcrossCycles(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("loop2")

	var intervals []time.Duration
	for cycle := range 2 {
		// Crash the VM every time the reconciler (re)starts it, until it gives up and reports ERROR.
		for i := 0; ; i++ {
			if i > 12 {
				t.Fatalf("cycle %d: never reached ERROR", cycle)
			}
			e.node.Crash(vm.Id, "oom")
			rectDue(e, e.rec, vm.Id)
			if rectVM(e, vm.Id).State == domain.VMError {
				break
			}
		}
		got := rectVM(e, vm.Id)
		intervals = append(intervals, got.NextReconcile.Sub(e.clk.Now()))
		rectRun(e, e.rec, vm.Id, 5, "running again", func(v *domain.VM) bool { return v.State == domain.VMRunning })
	}
	if intervals[1] <= intervals[0] {
		t.Errorf("crash-loop retry interval did not grow: first cycle %s, second cycle %s", intervals[0], intervals[1])
	}
}

// ------------------------------------------------------------------------------------ 2. node down

func TestReconNodeDownBacksOffAndRecovers(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("down")

	e.node.SetDown(true)
	// backoff(n) = min(1s << min(n, 6), 60s)
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range want {
		rectDue(e, e.rec, vm.Id)
		got := rectVM(e, vm.Id)
		if got.State != domain.VMRunning {
			t.Fatalf("node down, failure %d: state %s, want running (state is kept)", i+1, got.State)
		}
		if !strings.Contains(got.StateReason, "node unreachable") {
			t.Errorf("node down, failure %d: reason %q, want it to contain \"node unreachable\"", i+1, got.StateReason)
		}
		if got.Failures != i+1 {
			t.Errorf("node down, failure %d: failures = %d", i+1, got.Failures)
		}
		if d := got.NextReconcile.Sub(e.clk.Now()); d != w*time.Second {
			t.Errorf("node down, failure %d: next reconcile in %s, want %s", i+1, d, w*time.Second)
		}
	}

	e.node.SetDown(false)
	// The VM was never left RUNNING, so wait for a pass that clears the failure bookkeeping instead.
	got := rectRun(e, e.rec, vm.Id, 5, "node recovery reconciled", func(v *domain.VM) bool { return v.Failures == 0 })
	if got.State != domain.VMRunning || got.StateReason != "" {
		t.Errorf("after node recovery: state %s reason %q, want running and empty", got.State, got.StateReason)
	}
}

func TestReconPermanentErrorGoesToErrorAndRetriesEveryMinute(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base-perm")
	vm := e.createVM(e.editor(), "p1", "perm", img.Id, false)
	rectDue(e, e.rec, vm.Id) // stopped, nothing to do yet
	if got := rectVM(e, vm.Id); got.State != domain.VMStopped {
		t.Fatalf("setup: state %s, want stopped", got.State)
	}

	if _, err := e.editor().VM.StartVM(context.Background(), connect.NewRequest(&vmdv1.StartVMRequest{Id: vm.Id})); err != nil {
		t.Fatal(err)
	}
	e.node.FailNext("StartVM", errs.Invalid("bad spec"))
	rectDue(e, e.rec, vm.Id)

	got := rectVM(e, vm.Id)
	if got.State != domain.VMError {
		t.Fatalf("permanent failure: state %s, want error", got.State)
	}
	if !strings.Contains(got.StateReason, "bad spec") {
		t.Errorf("permanent failure: reason %q does not carry the driver error", got.StateReason)
	}
	if d := got.NextReconcile.Sub(e.clk.Now()); d != 60*time.Second {
		t.Errorf("permanent failure: next reconcile in %s, want 60s", d)
	}

	// Still broken one minute later: stays in ERROR, again retried after 60 s.
	e.node.FailNext("StartVM", errs.Invalid("bad spec"))
	rectDue(e, e.rec, vm.Id)
	got = rectVM(e, vm.Id)
	if got.State != domain.VMError {
		t.Fatalf("second permanent failure: state %s, want error", got.State)
	}
	if d := got.NextReconcile.Sub(e.clk.Now()); d != 60*time.Second {
		t.Errorf("second permanent failure: next reconcile in %s, want 60s", d)
	}

	// Fixed externally: the next retry starts the VM.
	rectRun(e, e.rec, vm.Id, 5, "running after the fault clears", func(v *domain.VM) bool { return v.State == domain.VMRunning })
	if n := rectCalls(e, "StartVM"); n != 3 {
		t.Errorf("StartVM calls = %d, want 3 (two failures, one success)", n)
	}
}

// ------------------------------------------------------------------------------------ 3. capacity

func TestReconCapacityWaitsForRoom(t *testing.T) {
	e := newEnv(t, func(_ *service.Config, f *fake.Config) { f.CPUs = 2 })
	a := e.runningVM("cap-a")
	img := e.sharedImage("base-cap-b")
	b := e.createVM(e.editor(), "p1", "cap-b", img.Id, true)

	rectRun(e, e.rec, b.Id, 3, "second VM waiting for capacity", func(v *domain.VM) bool {
		return v.State == domain.VMPending && strings.HasPrefix(v.StateReason, "waiting for capacity")
	})
	for range 3 {
		rectDue(e, e.rec, b.Id)
		if got := rectVM(e, b.Id); got.State != domain.VMPending || !strings.HasPrefix(got.StateReason, "waiting for capacity") {
			t.Fatalf("second VM: state %s reason %q, want pending waiting for capacity", got.State, got.StateReason)
		}
	}
	if n := rectCalls(e, "StartVM"); n != 1 {
		t.Errorf("StartVM calls while full = %d, want 1 (only the first VM)", n)
	}

	t0 := e.clk.Now()
	rectStop(e, a.Id, false)
	rectRun(e, e.rec, b.Id, 30, "second VM running", func(v *domain.VM) bool { return v.State == domain.VMRunning })
	// The waiting VM is re-checked every 3 s of clock time, so it starts shortly after the stop completes.
	// (The test clock only jumps to the waiting VM's own retry time, so this includes up to two 3 s
	// re-checks while the first VM finishes stopping.)
	if waited := e.clk.Now().Sub(t0); waited > 12*time.Second {
		t.Errorf("second VM started %s after capacity was freed, want within 12 s", waited)
	}
	if got := rectVM(e, b.Id); got.StateReason != "" {
		t.Errorf("running VM reason = %q, want empty", got.StateReason)
	}
}

func TestReconCapacityOvercommitLetsBothRun(t *testing.T) {
	e := newEnv(t, func(c *service.Config, f *fake.Config) {
		f.CPUs = 2
		c.Scheduler.CPUOvercommit = 2
	})
	a := e.runningVM("oc-a")
	b := e.runningVM("oc-b")
	for _, id := range []string{a.Id, b.Id} {
		if st := e.getVM(e.editor(), id).State; st != vmdv1.VMState_VM_STATE_RUNNING {
			t.Errorf("VM %s state %s, want running with 2x CPU overcommit on a 2-thread node", id, st)
		}
	}
}

// Expected to FAIL with the current code: see the report.
func TestReconDrainingNodeKeepsRunningVMsButRefusesNewStarts(t *testing.T) {
	e := newEnv(t)
	a := e.runningVM("dr-a")

	n, err := e.st.GetNode(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	n.State = domain.NodeDraining
	if err := e.st.UpsertNode(context.Background(), n); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		rectDue(e, e.rec, a.Id)
		if got := rectVM(e, a.Id); got.State != domain.VMRunning {
			t.Fatalf("running VM on draining node: state %s, want running", got.State)
		}
	}

	img := e.sharedImage("base-dr-b")
	b := e.createVM(e.editor(), "p1", "dr-b", img.Id, true)
	for range 2 {
		rectDue(e, e.rec, b.Id)
	}
	got := rectVM(e, b.Id)
	if got.State != domain.VMPending || !strings.HasPrefix(got.StateReason, "waiting for capacity") {
		t.Errorf("new VM on draining node: state %s reason %q, want pending waiting for capacity", got.State, got.StateReason)
	}
}

// Expected to FAIL with the current code: see the report. Two VMs wanting to run on a node with room for one
// must not both wait forever; one of them should start.
func TestReconCapacityTwoPendingVMsOneSlotStartsOne(t *testing.T) {
	e := newEnv(t, func(_ *service.Config, f *fake.Config) { f.CPUs = 2 })
	img := e.sharedImage("base-slot")
	a := e.createVM(e.editor(), "p1", "slot-a", img.Id, true)
	b := e.createVM(e.editor(), "p1", "slot-b", img.Id, true)

	for range 5 {
		e.tick(1)
	}
	running := 0
	for _, id := range []string{a.Id, b.Id} {
		if e.getVM(e.editor(), id).State == vmdv1.VMState_VM_STATE_RUNNING {
			running++
		}
	}
	if running != 1 {
		t.Errorf("VMs running = %d, want exactly 1 on a node with room for one (both desire to run, neither starts)", running)
	}
}

// ------------------------------------------------------------------------------------ 4. concurrency

func TestReconTwoReconcilersNeverDoubleStart(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base-two")
	var ids []string
	for i := range 3 {
		ids = append(ids, e.createVM(e.editor(), "p1", fmt.Sprintf("two-%d", i), img.Id, true).Id)
	}
	ra := service.NewReconciler(e.st, e.nodes, rectCfg(e), "rect-a")
	rb := service.NewReconciler(e.st, e.nodes, rectCfg(e), "rect-b")

	for range 10 { // alternate
		ra.Tick(context.Background())
		e.clk.Advance(time.Second)
		rb.Tick(context.Background())
		e.clk.Advance(time.Second)
	}
	for range 20 { // then overlap
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); ra.Tick(context.Background()) }()
		go func() { defer wg.Done(); rb.Tick(context.Background()) }()
		wg.Wait()
		e.clk.Advance(time.Second)
	}

	for _, id := range ids {
		if st := e.getVM(e.editor(), id).State; st != vmdv1.VMState_VM_STATE_RUNNING {
			t.Errorf("VM %s state %s, want running", id, st)
		}
	}
	if n := rectCalls(e, "StartVM"); n != len(ids) {
		t.Errorf("StartVM calls = %d, want exactly one per VM (%d)", n, len(ids))
	}
}

func TestReconLostCASReleasesLeaseForNextTick(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("cas")

	g := &rectGate{entered: make(chan struct{}), release: make(chan struct{})}
	d := &rectDriver{Driver: e.node, gate: g}
	rr := rectReconciler(e, d, "rect-gated")

	e.clk.Advance(10 * time.Second) // the running VM is due for its resync
	done := make(chan struct{})
	go func() {
		rr.Tick(context.Background())
		close(done)
	}()
	<-g.entered // the reconciler holds the VM's lease and is inside a node call
	rectStop(e, vm.Id, false)
	close(g.release)
	<-done

	got := rectVM(e, vm.Id)
	if got.Desired != domain.PowerStopped {
		t.Fatalf("API stop was overwritten: desired %s", got.Desired)
	}
	if got.LeaseOwner != "" {
		t.Errorf("lease still held by %q after the lost CAS, want released", got.LeaseOwner)
	}

	// Same instant, no clock advance: the next pass must claim the VM and act on the stop.
	before := e.clk.Now()
	rr.Tick(context.Background())
	if e.clk.Now() != before {
		t.Fatal("clock moved during tick")
	}
	got = rectVM(e, vm.Id)
	if got.State != domain.VMStopping {
		t.Errorf("next pass: state %s, want stopping (the stop was seen immediately)", got.State)
	}
	if n := rectCalls(e, "StopVM"); n != 1 {
		t.Errorf("StopVM calls = %d, want 1", n)
	}
}

func TestReconLeaseExpiryHandsVMToAnotherReconciler(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base-lease")
	vm := e.createVM(e.editor(), "p1", "lease", img.Id, true)

	claimed, err := e.st.ClaimVMs(context.Background(), "dead", e.clk.Now(), 30*time.Second, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != vm.Id {
		t.Fatalf("claimed %d VMs, want the one VM", len(claimed))
	}
	t0 := e.clk.Now()

	for range 25 {
		e.tick(1)
	}
	if n := rectCalls(e, "StartVM"); n != 0 {
		t.Fatalf("StartVM called %d times while another owner holds the lease", n)
	}

	e.until(20, "running after lease expiry", func() bool { return e.getVM(e.editor(), vm.Id).State == vmdv1.VMState_VM_STATE_RUNNING })
	if waited := e.clk.Now().Sub(t0); waited < 30*time.Second {
		t.Errorf("VM taken over after %s, before the 30 s lease expired", waited)
	}
}

// ------------------------------------------------------------------------------------ 5. delete flows

func TestReconDeletePendingVM(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base-delp")
	vm := e.createVM(e.editor(), "p1", "delp", img.Id, false)
	rectDelete(e, vm.Id, true)

	e.until(10, "vm gone", func() bool {
		_, err := e.editor().VM.GetVM(context.Background(), connect.NewRequest(&vmdv1.GetVMRequest{Id: vm.Id}))
		return code(err) == connect.CodeNotFound
	})
	e.until(10, "disk gone", func() bool {
		l, err := e.editor().Disk.ListDisks(context.Background(), connect.NewRequest(&vmdv1.ListDisksRequest{ProjectId: "p1"}))
		return err == nil && len(l.Msg.Disks) == 0
	})
	if n := rectCalls(e, "StartVM"); n != 0 {
		t.Errorf("StartVM called %d times for a deleted VM", n)
	}
}

func TestReconDeleteWaitsForNodeRecovery(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("deld")
	e.node.SetDown(true)
	rectDelete(e, vm.Id, true)

	rectDue(e, e.rec, vm.Id)
	got := rectVM(e, vm.Id)
	if got.State != domain.VMDeleting {
		t.Fatalf("node down: state %s, want deleting", got.State)
	}
	if !strings.Contains(got.StateReason, "node unreachable") {
		t.Errorf("node down: reason %q, want it to contain \"node unreachable\"", got.StateReason)
	}

	e.node.SetDown(false)
	rectGone(e, e.rec, vm.Id, 10)
	if n := rectCalls(e, "ForgetVM"); n != 1 {
		t.Errorf("ForgetVM calls = %d, want 1", n)
	}
	if n := rectCalls(e, "StopVM"); n < 1 {
		t.Errorf("StopVM not called before the forget")
	}
	e.until(10, "disk gone", func() bool {
		l, err := e.editor().Disk.ListDisks(context.Background(), connect.NewRequest(&vmdv1.ListDisksRequest{ProjectId: "p1"}))
		return err == nil && len(l.Msg.Disks) == 0
	})
}

func TestReconForceStopRecordsKilledAndGracefulStopRecordsStopped(t *testing.T) {
	e := newEnv(t)
	forced := e.runningVM("fs-a")
	rectStop(e, forced.Id, true)
	e.until(10, "forced stop complete", func() bool { return e.getVM(e.editor(), forced.Id).State == vmdv1.VMState_VM_STATE_STOPPED })
	if r := rectVM(e, forced.Id).StateReason; r != "killed" {
		t.Errorf("forced stop reason = %q, want \"killed\"", r)
	}

	graceful := e.runningVM("fs-b")
	rectStop(e, graceful.Id, false)
	e.until(10, "graceful stop complete", func() bool { return e.getVM(e.editor(), graceful.Id).State == vmdv1.VMState_VM_STATE_STOPPED })
	if r := rectVM(e, graceful.Id).StateReason; r != "stopped" {
		t.Errorf("graceful stop reason = %q, want \"stopped\"", r)
	}
}

func TestReconDeleteToleratesForgetVMNotFound(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base-fnf")
	vm := e.createVM(e.editor(), "p1", "fnf", img.Id, false)
	d := &rectDriver{Driver: e.node, forgetErr: errs.NotFound("vm %s", vm.Id)}
	rr := rectReconciler(e, d, "rect-fnf")

	rectDelete(e, vm.Id, false)
	rectGone(e, rr, vm.Id, 10)

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.forgets != 1 {
		t.Errorf("ForgetVM called %d times, want 1", d.forgets)
	}
}

// ------------------------------------------------------------------------------------ 6. hooks

func TestReconHooksSeeSuccessAndFailure(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("hooks")

	var mu sync.Mutex
	var reconciled []error
	var ticks []int
	e.rec.Hooks = service.Hooks{
		OnVMReconciled: func(v *domain.VM, _ time.Duration, err error) {
			mu.Lock()
			defer mu.Unlock()
			if v.ID != vm.Id {
				e.t.Errorf("hook saw VM %s, want %s", v.ID, vm.Id)
			}
			reconciled = append(reconciled, err)
		},
		OnTick: func(claimed int, _ time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			ticks = append(ticks, claimed)
		},
	}

	e.rec.Tick(context.Background()) // nothing due yet
	e.clk.Advance(10 * time.Second)
	e.rec.Tick(context.Background()) // the VM is due
	e.node.SetDown(true)
	e.clk.Advance(10 * time.Second)
	e.rec.Tick(context.Background())
	e.node.SetDown(false)

	mu.Lock()
	defer mu.Unlock()
	if len(ticks) != 3 || ticks[0] != 0 || ticks[1] != 1 || ticks[2] != 1 {
		t.Errorf("OnTick claimed counts = %v, want [0 1 1]", ticks)
	}
	if len(reconciled) != 2 {
		t.Fatalf("OnVMReconciled calls = %d, want 2", len(reconciled))
	}
	if reconciled[0] != nil {
		t.Errorf("first reconcile hook error = %v, want nil", reconciled[0])
	}
	if !errors.Is(reconciled[1], errs.ErrUnavailable) {
		t.Errorf("failed reconcile hook error = %v, want an unavailable error", reconciled[1])
	}
}

// ------------------------------------------------------------------------------------ 7. unknown node

func TestReconUnknownNodeBacksOffThenRecovers(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base-ghost")
	vm := e.createVM(e.editor(), "p1", "ghost", img.Id, true)

	v := rectVM(e, vm.Id)
	v.NodeID = "ghost"
	if err := e.st.UpdateVM(context.Background(), v); err != nil {
		t.Fatal(err)
	}

	for i, w := range []time.Duration{2, 4, 8} {
		rectDue(e, e.rec, vm.Id)
		got := rectVM(e, vm.Id)
		if !strings.Contains(got.StateReason, "node unreachable") || !strings.Contains(got.StateReason, "ghost") {
			t.Errorf("unknown node, failure %d: reason %q, want node unreachable mentioning ghost", i+1, got.StateReason)
		}
		if got.Failures != i+1 {
			t.Errorf("unknown node, failure %d: failures = %d", i+1, got.Failures)
		}
		if d := got.NextReconcile.Sub(e.clk.Now()); d != w*time.Second {
			t.Errorf("unknown node, failure %d: next reconcile in %s, want %s", i+1, d, w*time.Second)
		}
	}
	if n := rectCalls(e, "StartVM"); n != 0 {
		t.Errorf("StartVM calls = %d, want 0", n)
	}

	v = rectVM(e, vm.Id)
	v.NodeID = "n1"
	if err := e.st.UpdateVM(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	rectRun(e, e.rec, vm.Id, 5, "running once the node is known again", func(v *domain.VM) bool { return v.State == domain.VMRunning })
}
