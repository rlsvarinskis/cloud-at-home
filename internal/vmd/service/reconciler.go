package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	nodev1 "example.com/homecloud/gen/homecloud/node/v1"
	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/vmd/domain"
	"example.com/homecloud/internal/vmd/scheduler"
	"example.com/homecloud/internal/vmd/store"
)

// Reconciler drives images, disks and VMs towards their desired state. Any number of reconcilers (in
// one process or many) may run against the same store: VMs are claimed with a lease so each one is
// worked on by exactly one at a time, and every step is idempotent, so a crash only delays work.
type Reconciler struct {
	base
	owner string
	log   *slog.Logger
	Hooks Hooks
}

// Hooks lets metrics (or tests) observe reconciliation without the reconciler knowing about them.
type Hooks struct {
	OnVMReconciled func(vm *domain.VM, d time.Duration, err error)
	OnTick         func(claimed int, d time.Duration)
}

func NewReconciler(st store.Store, nodes Nodes, cfg Config, owner string) *Reconciler {
	cfg = cfg.withDefaults()
	return &Reconciler{base: base{st: st, nodes: nodes, cfg: cfg}, owner: owner, log: cfg.Log.With("component", "reconciler", "owner", owner)}
}

// Run ticks until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.cfg.ReconcileInterval)
	defer t.Stop()
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick does one pass: images, disks, then the VMs that are due. It returns how many VMs it worked on.
func (r *Reconciler) Tick(ctx context.Context) int {
	start := time.Now()
	r.reconcileImages(ctx)
	r.reconcileDisks(ctx)
	vms, err := r.st.ClaimVMs(ctx, r.owner, r.now(), r.cfg.LeaseTTL, r.cfg.BatchSize)
	if err != nil {
		r.log.Error("claim vms", "err", err, "claimed", len(vms)) // a sharded store may still have claimed some
	}
	sem := make(chan struct{}, 8) // bounded parallelism: one slow node must not stall the others for long
	var wg sync.WaitGroup
	for _, vm := range vms {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r.reconcileVM(ctx, vm)
		}()
	}
	wg.Wait()
	if r.Hooks.OnTick != nil {
		r.Hooks.OnTick(len(vms), time.Since(start))
	}
	return len(vms)
}

// transient reports whether retrying the same step later may succeed.
func transient(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errs.ErrNotFound), errors.Is(err, errs.ErrInvalidArgument),
		errors.Is(err, errs.ErrFailedPrecondition), errors.Is(err, errs.ErrAlreadyExists),
		errors.Is(err, errs.ErrPermissionDenied):
		return false
	}
	return true // unavailable, timeouts, and anything unknown
}

func backoff(failures int) time.Duration {
	if failures < 0 {
		failures = 0
	}
	d := time.Second << min(failures, 6)
	return min(d, 60*time.Second)
}

// ------------------------------------------------------------------------------------ images

func (r *Reconciler) reconcileImages(ctx context.Context) {
	creating, err := r.st.ListImagesByState(ctx, domain.ImageCreating, 20)
	if err != nil {
		r.log.Error("list creating images", "err", err)
		return
	}
	for _, img := range creating {
		r.createImage(ctx, img)
	}
	deleting, err := r.st.ListImagesByState(ctx, domain.ImageDeleting, 20)
	if err != nil {
		r.log.Error("list deleting images", "err", err)
		return
	}
	for _, img := range deleting {
		drv, err := r.nodes.Driver(img.NodeID)
		if err != nil {
			continue
		}
		if _, err := drv.DeleteImage(ctx, &nodev1.DeleteImageRequest{ImageId: img.ID}); err != nil && !errors.Is(err, errs.ErrNotFound) {
			r.log.Warn("delete image on node", "image", img.ID, "err", err)
			continue
		}
		_ = r.st.DeleteImage(ctx, img.ID)
	}
}

func (r *Reconciler) createImage(ctx context.Context, img *domain.Image) {
	drv, err := r.nodes.Driver(img.NodeID)
	if err != nil {
		r.failImage(ctx, img, fmt.Sprintf("node %s is not registered", img.NodeID), false)
		return
	}
	var size int64
	if img.SourceDiskID != "" {
		var res *nodev1.SnapshotDiskResponse
		res, err = drv.SnapshotDisk(ctx, &nodev1.SnapshotDiskRequest{DiskId: img.SourceDiskID, ImageId: img.ID})
		if err == nil {
			size = res.SizeBytes
		}
	} else {
		var res *nodev1.RegisterImageResponse
		res, err = drv.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: img.ID, Path: img.Path})
		if err == nil {
			size = res.SizeBytes
		}
	}
	if err != nil && !errors.Is(err, errs.ErrAlreadyExists) {
		r.failImage(ctx, img, err.Error(), transient(err))
		return
	}
	img.State, img.StateReason, img.SizeBytes, img.UpdatedAt = domain.ImageReady, "", size, r.now()
	if err := r.st.UpdateImage(ctx, img); err != nil && !errors.Is(err, errs.ErrConflict) {
		r.log.Error("update image", "image", img.ID, "err", err)
	}
}

func (r *Reconciler) failImage(ctx context.Context, img *domain.Image, reason string, retry bool) {
	if retry {
		r.log.Warn("image step failed, will retry", "image", img.ID, "reason", reason)
		return
	}
	img.State, img.StateReason, img.UpdatedAt = domain.ImageError, reason, r.now()
	_ = r.st.UpdateImage(ctx, img)
}

// ------------------------------------------------------------------------------------ disks

func (r *Reconciler) reconcileDisks(ctx context.Context) {
	creating, err := r.st.ListDisksByState(ctx, domain.DiskCreating, 20)
	if err != nil {
		r.log.Error("list creating disks", "err", err)
		return
	}
	for _, d := range creating {
		drv, err := r.nodes.Driver(d.NodeID)
		if err != nil {
			r.failDisk(ctx, d, fmt.Sprintf("node %s is not registered", d.NodeID), false)
			continue
		}
		_, err = drv.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: d.ID, BackingImageId: d.ImageID, SizeGb: int32(d.SizeGB)})
		if err != nil && !errors.Is(err, errs.ErrAlreadyExists) {
			r.failDisk(ctx, d, err.Error(), transient(err))
			continue
		}
		d.State, d.StateReason, d.UpdatedAt = domain.DiskReady, "", r.now()
		if err := r.st.UpdateDisk(ctx, d); err != nil && !errors.Is(err, errs.ErrConflict) {
			r.log.Error("update disk", "disk", d.ID, "err", err)
		}
	}
	deleting, err := r.st.ListDisksByState(ctx, domain.DiskDeleting, 20)
	if err != nil {
		r.log.Error("list deleting disks", "err", err)
		return
	}
	for _, d := range deleting {
		drv, err := r.nodes.Driver(d.NodeID)
		if err != nil {
			continue
		}
		if _, err := drv.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: d.ID}); err != nil && !errors.Is(err, errs.ErrNotFound) {
			r.log.Warn("delete disk on node", "disk", d.ID, "err", err)
			continue
		}
		_ = r.st.DeleteDisk(ctx, d.ID)
	}
}

func (r *Reconciler) failDisk(ctx context.Context, d *domain.Disk, reason string, retry bool) {
	if retry {
		r.log.Warn("disk step failed, will retry", "disk", d.ID, "reason", reason)
		return
	}
	d.State, d.StateReason, d.UpdatedAt = domain.DiskError, reason, r.now()
	_ = r.st.UpdateDisk(ctx, d)
}

// ------------------------------------------------------------------------------------ VMs

func (r *Reconciler) reconcileVM(ctx context.Context, vm *domain.VM) {
	start := time.Now()
	gone, err := r.step(ctx, vm)
	if r.Hooks.OnVMReconciled != nil {
		r.Hooks.OnVMReconciled(vm, time.Since(start), err)
	}
	if gone {
		return
	}
	now := r.now()
	if err != nil {
		vm.Failures++
		// state_reason is shown to API clients: say what is wrong, not where the node lives. Details go to the log.
		reason, hidden := errs.Sanitize("node "+vm.NodeID, err)
		msg := reason.Error()
		if hidden || errors.Is(err, errs.ErrUnavailable) {
			msg = "node unreachable: " + vm.NodeID
		}
		vm.StateReason = msg
		vm.NextReconcile = now.Add(backoff(vm.Failures))
		if !transient(err) {
			vm.State = domain.VMError
			vm.NextReconcile = now.Add(60 * time.Second) // keep checking: the cause may be fixed externally
		}
		r.log.Warn("reconcile failed", "vm", vm.ID, "failures", vm.Failures, "err", err)
	}
	vm.LeaseOwner, vm.LeaseUntil, vm.UpdatedAt = "", time.Time{}, now
	if err := r.st.UpdateVM(ctx, vm); err != nil {
		if errors.Is(err, errs.ErrConflict) {
			// An API call changed the VM while we worked. Keep its spec/desired state, just release our lease
			// so the next tick picks it up immediately instead of after the lease expires.
			r.releaseLease(ctx, vm.ID)
			return
		}
		r.log.Error("update vm", "vm", vm.ID, "err", err)
	}
}

func (r *Reconciler) releaseLease(ctx context.Context, id string) {
	for range 5 {
		v, err := r.st.GetVM(ctx, id)
		if err != nil || v.LeaseOwner != r.owner {
			return
		}
		v.LeaseOwner, v.LeaseUntil = "", time.Time{}
		if v.NextReconcile.IsZero() || v.NextReconcile.After(r.now()) {
			v.NextReconcile = r.now()
		}
		if err := r.st.UpdateVM(ctx, v); err == nil || !errors.Is(err, errs.ErrConflict) {
			return
		}
	}
}

// step performs one reconciliation of vm in memory (the caller persists it). It returns gone=true when the
// VM row was deleted. It sets vm.NextReconcile to when it wants to look again (zero = nothing more to do).
func (r *Reconciler) step(ctx context.Context, vm *domain.VM) (gone bool, err error) {
	now := r.now()
	drv, err := r.nodes.Driver(vm.NodeID)
	if err != nil {
		return false, errs.Unavailable("node %s is not registered", vm.NodeID)
	}
	if vm.Desired == domain.PowerDeleted {
		return r.stepDelete(ctx, vm, drv)
	}

	if vm.State == domain.VMPending {
		ready, reason, err := r.disksReady(ctx, vm)
		if err != nil {
			vm.State, vm.StateReason, vm.NextReconcile = domain.VMError, err.Error(), now.Add(30*time.Second)
			return false, nil
		}
		if !ready {
			vm.StateReason, vm.NextReconcile = reason, now.Add(time.Second)
			return false, nil
		}
		vm.State, vm.StateReason = domain.VMStopped, ""
	}

	res, err := drv.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: vm.ID})
	if err != nil {
		return false, err
	}
	vm.Failures = 0 // the node answered: whatever was failing before is over
	rt := res.GetRuntime()
	state := rt.GetState()
	alive := state == nodev1.RuntimeState_RUNTIME_STATE_RUNNING || state == nodev1.RuntimeState_RUNTIME_STATE_BOOTING

	switch vm.Desired {
	case domain.PowerRunning:
		switch {
		case state == nodev1.RuntimeState_RUNTIME_STATE_RUNNING:
			// Forgive one crash per resync interval of continuous health. A VM that was only just seen
			// booting (state Starting) earns nothing, so a boot-then-crash loop keeps backing off.
			if vm.State == domain.VMRunning && vm.Crashes > 0 {
				vm.Crashes--
			}
			vm.State, vm.StateReason = domain.VMRunning, ""
			vm.NextReconcile = now.Add(r.cfg.ResyncInterval)
		case state == nodev1.RuntimeState_RUNTIME_STATE_BOOTING:
			vm.State, vm.StateReason = domain.VMStarting, rt.GetReason()
			vm.NextReconcile = now.Add(time.Second)
		default: // absent or exited: (re)start
			if state == nodev1.RuntimeState_RUNTIME_STATE_EXITED && (vm.State == domain.VMRunning || vm.State == domain.VMStarting) {
				vm.Crashes++
				vm.StateReason = "VM exited unexpectedly: " + rt.GetReason()
				if vm.Crashes > 3 { // crash loop: stop hammering, retry ever more slowly
					vm.State, vm.NextReconcile = domain.VMError, now.Add(backoff(vm.Crashes))
					return false, nil
				}
			}
			if ok, why := r.fits(ctx, vm); !ok {
				vm.State, vm.StateReason, vm.NextReconcile = domain.VMPending, "waiting for capacity: "+why, now.Add(3*time.Second)
				return false, nil
			}
			if _, err := drv.StartVM(ctx, &nodev1.StartVMRequest{VmId: vm.ID, Name: vm.Name, Cpus: int32(vm.CPUs), MemoryMb: vm.MemoryMB, DiskIds: vm.DiskIDs}); err != nil {
				return false, err
			}
			vm.State = domain.VMStarting
			vm.NextReconcile = now.Add(time.Second)
		}
	case domain.PowerStopped:
		switch {
		case alive:
			if vm.State != domain.VMStopping || vm.StopForce {
				if _, err := drv.StopVM(ctx, &nodev1.StopVMRequest{VmId: vm.ID, Force: vm.StopForce}); err != nil {
					return false, err
				}
			}
			vm.State, vm.NextReconcile = domain.VMStopping, now.Add(time.Second)
		default:
			vm.State, vm.StateReason, vm.Crashes = domain.VMStopped, stopReason(vm, rt), 0
			vm.NextReconcile = time.Time{} // nothing more to do until the API changes the VM
		}
	}
	return false, nil
}

// stopReason keeps the reason a VM stopped (e.g. "stopped" / "killed") visible after the fact.
func stopReason(vm *domain.VM, rt *nodev1.Runtime) string {
	if rt.GetReason() != "" && vm.State != domain.VMStopped {
		return rt.GetReason()
	}
	if vm.State == domain.VMStopped {
		return vm.StateReason
	}
	return ""
}

func (r *Reconciler) stepDelete(ctx context.Context, vm *domain.VM, drv nodeDriver) (bool, error) {
	now := r.now()
	res, err := drv.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: vm.ID})
	if err != nil {
		return false, err
	}
	switch res.GetRuntime().GetState() {
	case nodev1.RuntimeState_RUNTIME_STATE_RUNNING, nodev1.RuntimeState_RUNTIME_STATE_BOOTING:
		if _, err := drv.StopVM(ctx, &nodev1.StopVMRequest{VmId: vm.ID, Force: true}); err != nil {
			return false, err
		}
		vm.NextReconcile = now.Add(time.Second)
		return false, nil
	}
	if _, err := drv.ForgetVM(ctx, &nodev1.ForgetVMRequest{VmId: vm.ID}); err != nil && !errors.Is(err, errs.ErrNotFound) {
		return false, err
	}
	err = r.st.Tx(r.txCtx(ctx, vm.ProjectID), func(tx store.Store) error {
		for _, id := range vm.DiskIDs {
			d, err := tx.GetDisk(ctx, id)
			if errors.Is(err, errs.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			d.AttachedVMID, d.UpdatedAt = "", now
			if vm.DeleteDisks {
				d.State = domain.DiskDeleting
			}
			if err := tx.UpdateDisk(ctx, d); err != nil {
				return err
			}
		}
		return tx.DeleteVM(ctx, vm.ID)
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *Reconciler) disksReady(ctx context.Context, vm *domain.VM) (ready bool, reason string, err error) {
	for _, id := range vm.DiskIDs {
		d, err := r.st.GetDisk(ctx, id)
		if err != nil {
			return false, "", fmt.Errorf("disk %s: %w", id, err)
		}
		switch d.State {
		case domain.DiskReady:
		case domain.DiskError:
			return false, "", fmt.Errorf("disk %s failed: %s", id, d.StateReason)
		default:
			return false, "waiting for disk " + id, nil
		}
	}
	return true, "", nil
}

// fits checks node capacity for starting vm. Only VMs that actually hold resources right now (starting,
// running or stopping) are counted, not VMs that merely want to run: otherwise two pending VMs competing for
// one slot would each see the other as taking it and neither would ever start. Whoever the reconciler reaches
// first starts and holds the slot; the other waits. A draining node takes no new starts.
func (r *Reconciler) fits(ctx context.Context, vm *domain.VM) (bool, string) {
	rows, err := r.st.ListNodes(ctx)
	if err != nil {
		return true, "" // cannot tell: let the node decide
	}
	for _, n := range rows {
		if n.ID != vm.NodeID {
			continue
		}
		vms, err := r.st.ListVMsByNode(ctx, n.ID)
		if err != nil {
			return true, ""
		}
		info := scheduler.NodeInfo{ID: n.ID, State: n.State, CPUs: n.CPUs, MemoryMB: n.MemoryMB, DiskBytesFree: n.DiskBytesFree}
		for _, o := range vms {
			if o.ID == vm.ID {
				continue
			}
			switch o.State {
			case domain.VMStarting, domain.VMRunning, domain.VMStopping:
				info.AllocatedCPUs += o.CPUs
				info.AllocatedMemoryMB += o.MemoryMB
			}
		}
		return scheduler.Fits(info, scheduler.Request{CPUs: vm.CPUs, MemoryMB: vm.MemoryMB}, r.cfg.Scheduler)
	}
	return true, ""
}
