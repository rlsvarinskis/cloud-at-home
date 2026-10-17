// Package fake is an in-memory implementation of node.Driver for tests. All state (images, disks,
// VMs, jobs and guest files) lives in maps guarded by one mutex. Time comes from the injectable Now
// clock, so tests never sleep. Test helpers script failures (Crash, SetDown, FailNext) and inspect
// the calls made (Calls).
//
// Exec runs while the driver's lock is held, so it must not call back into the Driver.
package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	nodev1 "example.com/homecloud/gen/homecloud/node/v1"
	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/vmd/node"
)

var _ node.Driver = (*Driver)(nil)

const (
	gib              = 1 << 30
	imageSizeBytes   = gib // every registered image reports this size
	defaultMaxOutput = 8192
	maxOutputCap     = 65536
	defaultMaxFile   = 8 << 20
	exitTimeout      = 124
	exitCancelled    = 130
)

// Config describes the capacity the fake node reports and how it boots VMs.
type Config struct {
	CPUs           int
	MemoryMB       int64
	DiskBytesTotal int64
	DiskBytesFree  int64
	// BootPolls is how many GetVMRuntime calls report BOOTING after a start before RUNNING.
	// Zero means a started VM is RUNNING immediately.
	BootPolls int
}

// Driver is a fake node.Driver. Create one with New.
type Driver struct {
	// Exec decides what a started job does: its exit code, output, and how long it runs.
	// The default exits 0 immediately with stdout "ok\n". Set it before use.
	Exec func(vmID string, spec *nodev1.JobSpec) (exit int, stdout, stderr string, runFor time.Duration)
	// Now is the clock used for job timing. It defaults to time.Now. Set it before use.
	Now func() time.Time

	mu       sync.Mutex
	cfg      Config
	down     bool
	failNext map[string]error
	calls    []string
	images   map[string]int64
	disks    map[string]*disk
	vms      map[string]*vm
	metrics  map[string]*nodev1.GetVMMetricsResponse
}

type disk struct {
	sizeGB  int32
	backing string
}

func (d *disk) bytes() int64 { return int64(d.sizeGB) * gib }

type vm struct {
	name          string
	cpus          int32
	memMB         int64
	disks         []string
	exited        bool
	reason        string
	bootPollsLeft int
	startedAt     time.Time
	jobs          map[string]*job
	files         map[string]string
}

// state reports the runtime state without consuming a boot poll.
func (v *vm) state() nodev1.RuntimeState {
	switch {
	case v.exited:
		return nodev1.RuntimeState_RUNTIME_STATE_EXITED
	case v.bootPollsLeft > 0:
		return nodev1.RuntimeState_RUNTIME_STATE_BOOTING
	default:
		return nodev1.RuntimeState_RUNTIME_STATE_RUNNING
	}
}

func (v *vm) running() bool { return v.state() == nodev1.RuntimeState_RUNTIME_STATE_RUNNING }

// inUse reports whether the VM still holds its disks (booting or running).
func (v *vm) inUse() bool { return !v.exited }

func (v *vm) runtime() *nodev1.Runtime {
	rt := &nodev1.Runtime{State: v.state()}
	if v.exited {
		rt.Reason = v.reason
	}
	return rt
}

type job struct {
	start, end time.Time
	exit       int32
	finished   bool
	cancelled  bool
	lost       bool
	stdout     string
	stderr     string
}

// update marks the job finished once the clock has reached its end.
func (j *job) update(now time.Time) {
	if !j.finished && !now.Before(j.end) {
		j.finished = true
	}
}

func (j *job) elapsedSeconds(now time.Time) int64 {
	end := j.end
	if !j.finished {
		end = now
	}
	d := end.Sub(j.start)
	if d < 0 {
		return 0
	}
	return int64(d / time.Second)
}

// New returns a fake driver that reports the capacity in c.
func New(c Config) *Driver {
	return &Driver{
		Exec:     defaultExec,
		Now:      time.Now,
		cfg:      c,
		failNext: make(map[string]error),
		images:   make(map[string]int64),
		disks:    make(map[string]*disk),
		vms:      make(map[string]*vm),
		metrics:  make(map[string]*nodev1.GetVMMetricsResponse),
	}
}

func defaultExec(string, *nodev1.JobSpec) (int, string, string, time.Duration) {
	return 0, "ok\n", "", 0
}

// begin records the call and applies scripted failures. d.mu must be held.
func (d *Driver) begin(method string) error {
	d.calls = append(d.calls, method)
	if d.down {
		return errs.Unavailable("fake node is down (%s)", method)
	}
	if err, ok := d.failNext[method]; ok {
		delete(d.failNext, method)
		return err
	}
	return nil
}

// diskInUseLocked reports whether a booting or running VM uses the disk. d.mu must be held.
func (d *Driver) diskInUseLocked(diskID string) bool {
	for _, v := range d.vms {
		if !v.inUse() {
			continue
		}
		for _, id := range v.disks {
			if id == diskID {
				return true
			}
		}
	}
	return false
}

// GetCapacity reports the configured capacity.
func (d *Driver) GetCapacity(_ context.Context, _ *nodev1.GetCapacityRequest) (*nodev1.GetCapacityResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("GetCapacity"); err != nil {
		return nil, err
	}
	return &nodev1.GetCapacityResponse{Capacity: &nodev1.Capacity{
		CpusTotal:         int32(d.cfg.CPUs),
		MemoryMbTotal:     d.cfg.MemoryMB,
		DiskBytesFree:     d.cfg.DiskBytesFree,
		MemoryMbAvailable: d.cfg.MemoryMB,
		DiskBytesTotal:    d.cfg.DiskBytesTotal,
	}}, nil
}

// RegisterImage records an image. The path is ignored; every image reports imageSizeBytes.
func (d *Driver) RegisterImage(_ context.Context, r *nodev1.RegisterImageRequest) (*nodev1.RegisterImageResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("RegisterImage"); err != nil {
		return nil, err
	}
	id := r.GetImageId()
	if id == "" {
		return nil, errs.Invalid("image_id is required")
	}
	if _, ok := d.images[id]; ok {
		return nil, errs.Exists("image %q", id)
	}
	d.images[id] = imageSizeBytes
	return &nodev1.RegisterImageResponse{SizeBytes: imageSizeBytes}, nil
}

// DeleteImage removes an image. Disks backed by it are not checked.
func (d *Driver) DeleteImage(_ context.Context, r *nodev1.DeleteImageRequest) (*nodev1.DeleteImageResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("DeleteImage"); err != nil {
		return nil, err
	}
	id := r.GetImageId()
	if _, ok := d.images[id]; !ok {
		return nil, errs.NotFound("image %q", id)
	}
	delete(d.images, id)
	return &nodev1.DeleteImageResponse{}, nil
}

// CreateDisk creates a disk, optionally backed by an existing image.
func (d *Driver) CreateDisk(_ context.Context, r *nodev1.CreateDiskRequest) (*nodev1.CreateDiskResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("CreateDisk"); err != nil {
		return nil, err
	}
	id := r.GetDiskId()
	if id == "" {
		return nil, errs.Invalid("disk_id is required")
	}
	if r.GetSizeGb() < 1 {
		return nil, errs.Invalid("size_gb must be at least 1, got %d", r.GetSizeGb())
	}
	if _, ok := d.disks[id]; ok {
		return nil, errs.Exists("disk %q", id)
	}
	backing := r.GetBackingImageId()
	if backing != "" {
		if _, ok := d.images[backing]; !ok {
			return nil, errs.NotFound("backing image %q", backing)
		}
	}
	d.disks[id] = &disk{sizeGB: r.GetSizeGb(), backing: backing}
	return &nodev1.CreateDiskResponse{}, nil
}

// ResizeDisk grows a disk. Shrinking is a precondition failure; the same size is a no-op.
func (d *Driver) ResizeDisk(_ context.Context, r *nodev1.ResizeDiskRequest) (*nodev1.ResizeDiskResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("ResizeDisk"); err != nil {
		return nil, err
	}
	id := r.GetDiskId()
	if id == "" {
		return nil, errs.Invalid("disk_id is required")
	}
	if r.GetSizeGb() < 1 {
		return nil, errs.Invalid("size_gb must be at least 1, got %d", r.GetSizeGb())
	}
	dk, ok := d.disks[id]
	if !ok {
		return nil, errs.NotFound("disk %q", id)
	}
	switch {
	case r.GetSizeGb() < dk.sizeGB:
		return nil, errs.Precondition("disk %q cannot shrink from %d to %d GB", id, dk.sizeGB, r.GetSizeGb())
	case r.GetSizeGb() > dk.sizeGB:
		dk.sizeGB = r.GetSizeGb()
	}
	return &nodev1.ResizeDiskResponse{}, nil
}

// DeleteDisk removes a disk. It is refused while a booting or running VM uses the disk.
func (d *Driver) DeleteDisk(_ context.Context, r *nodev1.DeleteDiskRequest) (*nodev1.DeleteDiskResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("DeleteDisk"); err != nil {
		return nil, err
	}
	id := r.GetDiskId()
	if _, ok := d.disks[id]; !ok {
		return nil, errs.NotFound("disk %q", id)
	}
	if d.diskInUseLocked(id) {
		return nil, errs.Precondition("disk %q is in use by a VM", id)
	}
	delete(d.disks, id)
	return &nodev1.DeleteDiskResponse{}, nil
}

// SnapshotDisk copies a disk into a new image of the same size. The disk must not be in use.
func (d *Driver) SnapshotDisk(_ context.Context, r *nodev1.SnapshotDiskRequest) (*nodev1.SnapshotDiskResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("SnapshotDisk"); err != nil {
		return nil, err
	}
	imageID := r.GetImageId()
	if imageID == "" {
		return nil, errs.Invalid("image_id is required")
	}
	dk, ok := d.disks[r.GetDiskId()]
	if !ok {
		return nil, errs.NotFound("disk %q", r.GetDiskId())
	}
	if _, ok := d.images[imageID]; ok {
		return nil, errs.Exists("image %q", imageID)
	}
	if d.diskInUseLocked(r.GetDiskId()) {
		return nil, errs.Precondition("disk %q is in use by a VM", r.GetDiskId())
	}
	d.images[imageID] = dk.bytes()
	return &nodev1.SnapshotDiskResponse{SizeBytes: dk.bytes()}, nil
}

// StartVM boots a VM. Starting a VM that is already booting or running is a no-op. Restarting an
// exited VM keeps its jobs and files.
func (d *Driver) StartVM(_ context.Context, r *nodev1.StartVMRequest) (*nodev1.StartVMResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("StartVM"); err != nil {
		return nil, err
	}
	id := r.GetVmId()
	if id == "" {
		return nil, errs.Invalid("vm_id is required")
	}
	if r.GetCpus() < 1 {
		return nil, errs.Invalid("cpus must be at least 1, got %d", r.GetCpus())
	}
	if r.GetMemoryMb() < 1 {
		return nil, errs.Invalid("memory_mb must be at least 1, got %d", r.GetMemoryMb())
	}
	for _, diskID := range r.GetDiskIds() {
		if _, ok := d.disks[diskID]; !ok {
			return nil, errs.NotFound("disk %q", diskID)
		}
	}
	v := d.vms[id]
	if v != nil && !v.exited {
		return &nodev1.StartVMResponse{Runtime: v.runtime()}, nil
	}
	if v == nil {
		v = &vm{jobs: make(map[string]*job), files: make(map[string]string)}
		d.vms[id] = v
	}
	v.name = r.GetName()
	v.cpus = r.GetCpus()
	v.memMB = r.GetMemoryMb()
	v.disks = append([]string(nil), r.GetDiskIds()...)
	v.exited = false
	v.reason = ""
	v.bootPollsLeft = d.cfg.BootPolls
	v.startedAt = d.now()
	return &nodev1.StartVMResponse{Runtime: v.runtime()}, nil
}

// StopVM marks a VM exited. Stopping an absent or already exited VM is a no-op.
func (d *Driver) StopVM(_ context.Context, r *nodev1.StopVMRequest) (*nodev1.StopVMResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("StopVM"); err != nil {
		return nil, err
	}
	if id := r.GetVmId(); id == "" {
		return nil, errs.Invalid("vm_id is required")
	}
	v := d.vms[r.GetVmId()]
	if v != nil && !v.exited {
		reason := "stopped"
		if r.GetForce() {
			reason = "killed"
		}
		d.terminateLocked(v, reason)
	}
	return &nodev1.StopVMResponse{}, nil
}

// GetVMRuntime reports the VM's state. Each call while booting consumes one of the BootPolls.
func (d *Driver) GetVMRuntime(_ context.Context, r *nodev1.GetVMRuntimeRequest) (*nodev1.GetVMRuntimeResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("GetVMRuntime"); err != nil {
		return nil, err
	}
	v := d.vms[r.GetVmId()]
	if v == nil {
		return &nodev1.GetVMRuntimeResponse{Runtime: &nodev1.Runtime{State: nodev1.RuntimeState_RUNTIME_STATE_ABSENT}}, nil
	}
	if !v.exited && v.bootPollsLeft > 0 {
		v.bootPollsLeft--
		return &nodev1.GetVMRuntimeResponse{Runtime: &nodev1.Runtime{State: nodev1.RuntimeState_RUNTIME_STATE_BOOTING}}, nil
	}
	return &nodev1.GetVMRuntimeResponse{Runtime: v.runtime()}, nil
}

// ForgetVM drops all state for a VM: its jobs and files. It is refused while the VM is booting or
// running. Forgetting an unknown VM is a no-op.
func (d *Driver) ForgetVM(_ context.Context, r *nodev1.ForgetVMRequest) (*nodev1.ForgetVMResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("ForgetVM"); err != nil {
		return nil, err
	}
	id := r.GetVmId()
	v := d.vms[id]
	if v == nil {
		return &nodev1.ForgetVMResponse{}, nil
	}
	if v.inUse() {
		return nil, errs.Precondition("vm %q is still running", id)
	}
	delete(d.vms, id)
	return &nodev1.ForgetVMResponse{}, nil
}

// StartJob starts a guest job by calling Exec. The VM must be RUNNING.
func (d *Driver) StartJob(_ context.Context, r *nodev1.StartJobRequest) (*nodev1.StartJobResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("StartJob"); err != nil {
		return nil, err
	}
	spec := r.GetSpec()
	if spec.GetJobId() == "" {
		return nil, errs.Invalid("job_id is required")
	}
	v := d.vms[r.GetVmId()]
	if v == nil || !v.running() {
		return nil, errs.Precondition("vm %q is not running", r.GetVmId())
	}
	if _, ok := v.jobs[spec.GetJobId()]; ok {
		return nil, errs.Exists("job %q", spec.GetJobId())
	}
	exec := d.Exec
	if exec == nil {
		exec = defaultExec
	}
	exit, stdout, stderr, runFor := exec(r.GetVmId(), spec)
	if runFor < 0 {
		runFor = 0
	}
	now := d.now()
	j := &job{start: now, end: now.Add(runFor), exit: int32(exit), stdout: stdout, stderr: stderr}
	if t := spec.GetTimeoutSeconds(); t > 0 {
		limit := time.Duration(t) * time.Second
		if runFor > limit {
			j.end = now.Add(limit)
			j.exit = exitTimeout
		}
	}
	v.jobs[spec.GetJobId()] = j
	return &nodev1.StartJobResponse{}, nil
}

// PollJob reports a job's state and a page of its output. Unknown VMs and jobs are MISSING.
// While the job is RUNNING its output is empty. A LOST job has no exit code.
func (d *Driver) PollJob(_ context.Context, r *nodev1.PollJobRequest) (*nodev1.PollJobResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("PollJob"); err != nil {
		return nil, err
	}
	if r.GetStdoutOffset() < 0 || r.GetStderrOffset() < 0 {
		return nil, errs.Invalid("offsets must not be negative")
	}
	if r.GetMaxOutput() < 0 {
		return nil, errs.Invalid("max_output must not be negative")
	}
	limit := int(r.GetMaxOutput())
	if limit == 0 {
		limit = defaultMaxOutput
	}
	limit = min(limit, maxOutputCap)

	v := d.vms[r.GetVmId()]
	var j *job
	if v != nil {
		j = v.jobs[r.GetJobId()]
	}
	if j == nil {
		return &nodev1.PollJobResponse{State: nodev1.GuestJobState_GUEST_JOB_STATE_MISSING}, nil
	}
	now := d.now()
	j.update(now)

	stdout, stderr := j.stdout, j.stderr
	if !j.finished {
		stdout, stderr = "", ""
	}
	out, outFrom, outNext, outMore, outTrunc := page(stdout, r.GetStdoutOffset(), limit, r.GetTail())
	errOut, errFrom, errNext, errMore, errTrunc := page(stderr, r.GetStderrOffset(), limit, r.GetTail())

	resp := &nodev1.PollJobResponse{
		ElapsedSeconds: j.elapsedSeconds(now),
		Cancelled:      j.cancelled,
		Stdout:         []byte(out),
		Stderr:         []byte(errOut),
		StdoutFrom:     outFrom,
		StderrFrom:     errFrom,
		StdoutOffset:   outNext,
		StderrOffset:   errNext,
		More:           outMore || errMore,
		Truncated:      outTrunc || errTrunc,
	}
	switch {
	case !j.finished:
		resp.State = nodev1.GuestJobState_GUEST_JOB_STATE_RUNNING
	case j.lost:
		resp.State = nodev1.GuestJobState_GUEST_JOB_STATE_LOST
	default:
		resp.State = nodev1.GuestJobState_GUEST_JOB_STATE_EXITED
		resp.ExitCode = ptrTo(j.exit)
	}
	return resp, nil
}

// page returns the slice of data a poll asks for. In tail mode it is the last max bytes; otherwise
// it is up to max bytes starting at from. It returns the slice, the offset it starts at, the offset
// to continue from, whether bytes remain after it, and whether earlier bytes were omitted.
func page(data string, from int64, limit int, tail bool) (out string, start, next int64, more, truncated bool) {
	n := int64(len(data))
	if tail {
		if len(data) > limit {
			start = n - int64(limit)
			return data[start:], start, n, false, true
		}
		return data, 0, n, false, false
	}
	if from >= n {
		return "", from, from, false, false
	}
	end := min(n, from+int64(limit))
	return data[from:end], from, end, end < n, false
}

// CancelJob ends a running job with exit code 130 and Cancelled set. Cancelling a finished job is a
// no-op. An unknown VM or job is NotFound.
func (d *Driver) CancelJob(_ context.Context, r *nodev1.CancelJobRequest) (*nodev1.CancelJobResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("CancelJob"); err != nil {
		return nil, err
	}
	var j *job
	if v := d.vms[r.GetVmId()]; v != nil {
		j = v.jobs[r.GetJobId()]
	}
	if j == nil {
		return nil, errs.NotFound("job %q on vm %q", r.GetJobId(), r.GetVmId())
	}
	now := d.now()
	j.update(now)
	if !j.finished {
		j.finished = true
		j.cancelled = true
		j.exit = exitCancelled
		j.end = now
	}
	return &nodev1.CancelJobResponse{}, nil
}

// PutFile stores a file in a running VM. The content is copied.
func (d *Driver) PutFile(_ context.Context, r *nodev1.PutFileRequest) (*nodev1.PutFileResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("PutFile"); err != nil {
		return nil, err
	}
	if r.GetPath() == "" {
		return nil, errs.Invalid("path is required")
	}
	v := d.vms[r.GetVmId()]
	if v == nil || !v.running() {
		return nil, errs.Precondition("vm %q is not running", r.GetVmId())
	}
	v.files[r.GetPath()] = string(r.GetContent())
	return &nodev1.PutFileResponse{}, nil
}

// GetFile returns a file from a running VM, up to max_bytes (default and cap 8 MiB).
func (d *Driver) GetFile(_ context.Context, r *nodev1.GetFileRequest) (*nodev1.GetFileResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("GetFile"); err != nil {
		return nil, err
	}
	if r.GetPath() == "" {
		return nil, errs.Invalid("path is required")
	}
	v := d.vms[r.GetVmId()]
	if v == nil || !v.running() {
		return nil, errs.Precondition("vm %q is not running", r.GetVmId())
	}
	content, ok := v.files[r.GetPath()]
	if !ok {
		return nil, errs.NotFound("file %q on vm %q", r.GetPath(), r.GetVmId())
	}
	limit := r.GetMaxBytes()
	if limit <= 0 || limit > defaultMaxFile {
		limit = defaultMaxFile
	}
	truncated := int64(len(content)) > limit
	if truncated {
		content = content[:limit]
	}
	return &nodev1.GetFileResponse{Content: []byte(content), Truncated: truncated}, nil
}

// GetVMMetrics reports metrics for a running VM. It returns the value set by SetMetrics if there is
// one; otherwise it builds plausible defaults with one filesystem and allocation per disk ID.
func (d *Driver) GetVMMetrics(_ context.Context, r *nodev1.GetVMMetricsRequest) (*nodev1.GetVMMetricsResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.begin("GetVMMetrics"); err != nil {
		return nil, err
	}
	id := r.GetVmId()
	v := d.vms[id]
	if v == nil || !v.running() {
		return nil, errs.Precondition("vm %q is not running", id)
	}
	if m, ok := d.metrics[id]; ok {
		return copyMetrics(m), nil
	}
	m := &nodev1.GetVMMetricsResponse{
		CpuPercent:        1.5,
		CpuCores:          v.cpus,
		Load1:             0.1,
		Load5:             0.2,
		Load15:            0.15,
		MemTotalBytes:     gib,
		MemUsedBytes:      gib / 4,
		MemAvailableBytes: gib - gib/4,
		SwapTotalBytes:    0,
		SwapUsedBytes:     0,
		UptimeSeconds:     max(0, int64(d.now().Sub(v.startedAt)/time.Second)),
	}
	for i, diskID := range r.GetDiskIds() {
		dk, ok := d.disks[diskID]
		if !ok {
			return nil, errs.NotFound("disk %q", diskID)
		}
		mount := "/"
		if i > 0 {
			mount = "/mnt/vd" + devLetter(i)
		}
		m.Filesystems = append(m.Filesystems, &nodev1.GuestFilesystem{
			DiskId:     diskID,
			Device:     "/dev/vd" + devLetter(i) + "1",
			MountPoint: mount,
			SizeBytes:  dk.bytes(),
			UsedBytes:  dk.bytes() / 4,
		})
		m.Disks = append(m.Disks, &nodev1.DiskAllocation{
			DiskId:           diskID,
			VirtualSizeBytes: dk.bytes(),
			AllocatedBytes:   dk.bytes() / 4,
		})
	}
	return m, nil
}

// Crash kills a VM as if its process died: it becomes EXITED with reason, and its running jobs
// become LOST. Crashing a VM that is not booting or running does nothing.
func (d *Driver) Crash(vmID string, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if v := d.vms[vmID]; v != nil && !v.exited {
		d.terminateLocked(v, reason)
	}
}

// SetDown makes every Driver method return errs.Unavailable while down is true.
func (d *Driver) SetDown(down bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.down = down
}

// FailNext makes the next call to the named method (for example "StartVM") return err instead of
// running. It is one-shot. A nil err cancels a pending failure for that method.
func (d *Driver) FailNext(method string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		delete(d.failNext, method)
		return
	}
	d.failNext[method] = err
}

// Calls returns the names of the Driver methods called so far, in order. Calls that fail are
// included.
func (d *Driver) Calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

// SetMetrics overrides what GetVMMetrics reports for a VM. A nil m removes the override.
func (d *Driver) SetMetrics(vmID string, m *nodev1.GetVMMetricsResponse) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if m == nil {
		delete(d.metrics, vmID)
		return
	}
	d.metrics[vmID] = copyMetrics(m)
}

// terminateLocked moves a VM to EXITED and loses its unfinished jobs. d.mu must be held.
func (d *Driver) terminateLocked(v *vm, reason string) {
	now := d.now()
	v.exited = true
	v.reason = reason
	v.bootPollsLeft = 0
	for _, j := range v.jobs {
		j.update(now)
		if !j.finished {
			j.finished = true
			j.lost = true
			j.end = now
		}
	}
}

// now reads the clock, falling back to time.Now. d.mu must be held.
func (d *Driver) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

// devLetter names the virtio device for the i-th disk: 0 is a, 1 is b, and so on.
func devLetter(i int) string {
	if i < 26 {
		return string(rune('a' + i))
	}
	return fmt.Sprint(i)
}

// copyMetrics returns a deep copy so callers and the driver never share mutable state.
func copyMetrics(m *nodev1.GetVMMetricsResponse) *nodev1.GetVMMetricsResponse {
	c := &nodev1.GetVMMetricsResponse{
		CpuPercent:        m.GetCpuPercent(),
		CpuCores:          m.GetCpuCores(),
		Load1:             m.GetLoad1(),
		Load5:             m.GetLoad5(),
		Load15:            m.GetLoad15(),
		MemTotalBytes:     m.GetMemTotalBytes(),
		MemUsedBytes:      m.GetMemUsedBytes(),
		MemAvailableBytes: m.GetMemAvailableBytes(),
		SwapTotalBytes:    m.GetSwapTotalBytes(),
		SwapUsedBytes:     m.GetSwapUsedBytes(),
		UptimeSeconds:     m.GetUptimeSeconds(),
	}
	for _, f := range m.GetFilesystems() {
		c.Filesystems = append(c.Filesystems, &nodev1.GuestFilesystem{
			DiskId: f.GetDiskId(), Device: f.GetDevice(), MountPoint: f.GetMountPoint(),
			SizeBytes: f.GetSizeBytes(), UsedBytes: f.GetUsedBytes(),
		})
	}
	for _, a := range m.GetDisks() {
		c.Disks = append(c.Disks, &nodev1.DiskAllocation{
			DiskId: a.GetDiskId(), VirtualSizeBytes: a.GetVirtualSizeBytes(), AllocatedBytes: a.GetAllocatedBytes(),
		})
	}
	return c
}

func ptrTo[T any](v T) *T { return &v }
