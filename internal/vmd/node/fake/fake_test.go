package fake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	nodev1 "example.com/homecloud/gen/homecloud/node/v1"
	"example.com/homecloud/internal/errs"
)

// clock is a manually advanced clock for Driver.Now.
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

func newTestDriver(t *testing.T, cfg Config) (*Driver, *clock) {
	t.Helper()
	d := New(cfg)
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	d.Now = c.Now
	return d, c
}

var ctx = context.Background()

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		return
	}
	if !errors.Is(err, want) {
		t.Fatalf("got error %v, want %v", err, want)
	}
}

// setupDisk creates a disk for tests that need one, ignoring Exists.
func setupDisk(t *testing.T, d *Driver, id string) {
	t.Helper()
	_, err := d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: id, SizeGb: 10})
	if err != nil && !errors.Is(err, errs.ErrAlreadyExists) {
		t.Fatalf("CreateDisk(%s): %v", id, err)
	}
}

// startVM boots vmID with one disk "disk-<vmID>" and returns the StartVM response.
func startVM(t *testing.T, d *Driver, vmID string) *nodev1.StartVMResponse {
	t.Helper()
	disk := "disk-" + vmID
	setupDisk(t, d, disk)
	resp, err := d.StartVM(ctx, &nodev1.StartVMRequest{VmId: vmID, Name: vmID, Cpus: 2, MemoryMb: 512, DiskIds: []string{disk}})
	mustNoErr(t, err)
	return resp
}

// runningVM boots vmID and polls it until RUNNING.
func runningVM(t *testing.T, d *Driver, vmID string) {
	t.Helper()
	startVM(t, d, vmID)
	for i := 0; i < 100; i++ {
		r, err := d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: vmID})
		mustNoErr(t, err)
		if r.GetRuntime().GetState() == nodev1.RuntimeState_RUNTIME_STATE_RUNNING {
			return
		}
	}
	t.Fatalf("vm %s never reached RUNNING", vmID)
}

func poll(t *testing.T, d *Driver, vmID, jobID string, stdoutOff, stderrOff int64, max int32, tail bool) *nodev1.PollJobResponse {
	t.Helper()
	r, err := d.PollJob(ctx, &nodev1.PollJobRequest{
		VmId: vmID, JobId: jobID, StdoutOffset: stdoutOff, StderrOffset: stderrOff, MaxOutput: max, Tail: tail,
	})
	mustNoErr(t, err)
	return r
}

func startJob(d *Driver, vmID, jobID string, timeout int32) error {
	_, err := d.StartJob(ctx, &nodev1.StartJobRequest{
		VmId: vmID, Spec: &nodev1.JobSpec{JobId: jobID, Command: "true", TimeoutSeconds: timeout},
	})
	return err
}

func TestGetCapacity(t *testing.T) {
	d, _ := newTestDriver(t, Config{CPUs: 8, MemoryMB: 16384, DiskBytesTotal: 100, DiskBytesFree: 40})
	r, err := d.GetCapacity(ctx, &nodev1.GetCapacityRequest{})
	mustNoErr(t, err)
	c := r.GetCapacity()
	if c.GetCpusTotal() != 8 || c.GetMemoryMbTotal() != 16384 || c.GetMemoryMbAvailable() != 16384 ||
		c.GetDiskBytesTotal() != 100 || c.GetDiskBytesFree() != 40 {
		t.Fatalf("capacity = %+v", c)
	}
}

func TestImages(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	r, err := d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "img", Path: "/x"})
	mustNoErr(t, err)
	if r.GetSizeBytes() != imageSizeBytes {
		t.Fatalf("size = %d", r.GetSizeBytes())
	}
	_, err = d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "img"})
	wantErr(t, err, errs.ErrAlreadyExists)
	_, err = d.RegisterImage(ctx, &nodev1.RegisterImageRequest{})
	wantErr(t, err, errs.ErrInvalidArgument)

	_, err = d.DeleteImage(ctx, &nodev1.DeleteImageRequest{ImageId: "missing"})
	wantErr(t, err, errs.ErrNotFound)
	_, err = d.DeleteImage(ctx, &nodev1.DeleteImageRequest{ImageId: "img"})
	mustNoErr(t, err)
	_, err = d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "img"})
	mustNoErr(t, err)
}

func TestCreateDisk(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	_, err := d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "base"})
	mustNoErr(t, err)

	tests := []struct {
		name string
		req  *nodev1.CreateDiskRequest
		want error
	}{
		{"empty id", &nodev1.CreateDiskRequest{SizeGb: 1}, errs.ErrInvalidArgument},
		{"zero size", &nodev1.CreateDiskRequest{DiskId: "a", SizeGb: 0}, errs.ErrInvalidArgument},
		{"negative size", &nodev1.CreateDiskRequest{DiskId: "a", SizeGb: -1}, errs.ErrInvalidArgument},
		{"plain", &nodev1.CreateDiskRequest{DiskId: "a", SizeGb: 1}, nil},
		{"duplicate", &nodev1.CreateDiskRequest{DiskId: "a", SizeGb: 1}, errs.ErrAlreadyExists},
		{"missing backing", &nodev1.CreateDiskRequest{DiskId: "b", SizeGb: 1, BackingImageId: "nope"}, errs.ErrNotFound},
		{"with backing", &nodev1.CreateDiskRequest{DiskId: "b", SizeGb: 1, BackingImageId: "base"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.CreateDisk(ctx, tc.req)
			wantErr(t, err, tc.want)
		})
	}
}

func TestResizeDisk(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	setupDisk(t, d, "disk") // 10 GB
	tests := []struct {
		name string
		id   string
		gb   int32
		want error
	}{
		{"empty id", "", 20, errs.ErrInvalidArgument},
		{"zero", "disk", 0, errs.ErrInvalidArgument},
		{"missing", "nope", 20, errs.ErrNotFound},
		{"shrink", "disk", 5, errs.ErrFailedPrecondition},
		{"same size", "disk", 10, nil},
		{"grow", "disk", 20, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: tc.id, SizeGb: tc.gb})
			wantErr(t, err, tc.want)
		})
	}
	// After growing to 20, shrinking to 15 must still be refused.
	_, err := d.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: "disk", SizeGb: 15})
	wantErr(t, err, errs.ErrFailedPrecondition)
}

func TestDeleteDisk(t *testing.T) {
	d, _ := newTestDriver(t, Config{BootPolls: 1})
	_, err := d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: "missing"})
	wantErr(t, err, errs.ErrNotFound)

	setupDisk(t, d, "free")
	_, err = d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: "free"})
	mustNoErr(t, err)

	// Booting VM holds its disk.
	startVM(t, d, "vm")
	_, err = d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: "disk-vm"})
	wantErr(t, err, errs.ErrFailedPrecondition)

	// Running VM holds its disk.
	d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm"})
	_, err = d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: "disk-vm"})
	wantErr(t, err, errs.ErrFailedPrecondition)

	// Stopped VM does not.
	_, err = d.StopVM(ctx, &nodev1.StopVMRequest{VmId: "vm"})
	mustNoErr(t, err)
	_, err = d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: "disk-vm"})
	mustNoErr(t, err)
}

func TestSnapshotDisk(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	_, err := d.SnapshotDisk(ctx, &nodev1.SnapshotDiskRequest{DiskId: "x", ImageId: ""})
	wantErr(t, err, errs.ErrInvalidArgument)
	_, err = d.SnapshotDisk(ctx, &nodev1.SnapshotDiskRequest{DiskId: "missing", ImageId: "img"})
	wantErr(t, err, errs.ErrNotFound)

	setupDisk(t, d, "disk") // 10 GB
	r, err := d.SnapshotDisk(ctx, &nodev1.SnapshotDiskRequest{DiskId: "disk", ImageId: "img"})
	mustNoErr(t, err)
	if want := int64(10) * gib; r.GetSizeBytes() != want {
		t.Fatalf("size = %d, want %d", r.GetSizeBytes(), want)
	}
	_, err = d.SnapshotDisk(ctx, &nodev1.SnapshotDiskRequest{DiskId: "disk", ImageId: "img"})
	wantErr(t, err, errs.ErrAlreadyExists)

	// The snapshot is a real image: usable as a backing image and deletable.
	_, err = d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "child", SizeGb: 1, BackingImageId: "img"})
	mustNoErr(t, err)
	_, err = d.DeleteImage(ctx, &nodev1.DeleteImageRequest{ImageId: "img"})
	mustNoErr(t, err)

	// A disk held by a started VM cannot be snapshotted.
	startVM(t, d, "vm")
	_, err = d.SnapshotDisk(ctx, &nodev1.SnapshotDiskRequest{DiskId: "disk-vm", ImageId: "img2"})
	wantErr(t, err, errs.ErrFailedPrecondition)
}

func TestStartVMValidation(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	setupDisk(t, d, "disk")
	tests := []struct {
		name string
		req  *nodev1.StartVMRequest
		want error
	}{
		{"empty id", &nodev1.StartVMRequest{Cpus: 1, MemoryMb: 1, DiskIds: []string{"disk"}}, errs.ErrInvalidArgument},
		{"zero cpus", &nodev1.StartVMRequest{VmId: "v", Cpus: 0, MemoryMb: 1, DiskIds: []string{"disk"}}, errs.ErrInvalidArgument},
		{"zero memory", &nodev1.StartVMRequest{VmId: "v", Cpus: 1, MemoryMb: 0, DiskIds: []string{"disk"}}, errs.ErrInvalidArgument},
		{"missing disk", &nodev1.StartVMRequest{VmId: "v", Cpus: 1, MemoryMb: 1, DiskIds: []string{"disk", "nope"}}, errs.ErrNotFound},
		{"ok", &nodev1.StartVMRequest{VmId: "v", Cpus: 1, MemoryMb: 1, DiskIds: []string{"disk"}}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.StartVM(ctx, tc.req)
			wantErr(t, err, tc.want)
		})
	}
}

func TestVMLifecycle(t *testing.T) {
	d, _ := newTestDriver(t, Config{})

	r, err := d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm"})
	mustNoErr(t, err)
	if r.GetRuntime().GetState() != nodev1.RuntimeState_RUNTIME_STATE_ABSENT {
		t.Fatalf("unknown vm state = %v", r.GetRuntime().GetState())
	}

	sr := startVM(t, d, "vm")
	if sr.GetRuntime().GetState() != nodev1.RuntimeState_RUNTIME_STATE_RUNNING {
		t.Fatalf("start state = %v, want RUNNING", sr.GetRuntime().GetState())
	}
	r, _ = d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm"})
	if r.GetRuntime().GetState() != nodev1.RuntimeState_RUNTIME_STATE_RUNNING {
		t.Fatalf("state = %v, want RUNNING", r.GetRuntime().GetState())
	}

	// Starting a running VM is a no-op success that keeps its jobs.
	mustNoErr(t, startJob(d, "vm", "j", 0))
	sr, err = d.StartVM(ctx, &nodev1.StartVMRequest{VmId: "vm", Cpus: 2, MemoryMb: 512, DiskIds: []string{"disk-vm"}})
	mustNoErr(t, err)
	if sr.GetRuntime().GetState() != nodev1.RuntimeState_RUNTIME_STATE_RUNNING {
		t.Fatalf("restart of running vm: state = %v", sr.GetRuntime().GetState())
	}
	if p := poll(t, d, "vm", "j", 0, 0, 0, false); p.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_EXITED {
		t.Fatalf("job lost by no-op start: state %v", p.GetState())
	}

	_, err = d.StopVM(ctx, &nodev1.StopVMRequest{VmId: "vm"})
	mustNoErr(t, err)
	r, _ = d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm"})
	if got := r.GetRuntime(); got.GetState() != nodev1.RuntimeState_RUNTIME_STATE_EXITED || got.GetReason() != "stopped" {
		t.Fatalf("after stop: %+v", got)
	}

	// Stop of an exited VM and of an absent VM are no-ops.
	_, err = d.StopVM(ctx, &nodev1.StopVMRequest{VmId: "vm"})
	mustNoErr(t, err)
	_, err = d.StopVM(ctx, &nodev1.StopVMRequest{VmId: "never"})
	mustNoErr(t, err)
	_, err = d.StopVM(ctx, &nodev1.StopVMRequest{})
	wantErr(t, err, errs.ErrInvalidArgument)

	// Restart an exited VM: it runs again with the old reason cleared.
	sr = startVM(t, d, "vm")
	if sr.GetRuntime().GetState() != nodev1.RuntimeState_RUNTIME_STATE_RUNNING || sr.GetRuntime().GetReason() != "" {
		t.Fatalf("restart: %+v", sr.GetRuntime())
	}

	// Force stop records a different reason.
	_, err = d.StopVM(ctx, &nodev1.StopVMRequest{VmId: "vm", Force: true})
	mustNoErr(t, err)
	r, _ = d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm"})
	if r.GetRuntime().GetReason() != "killed" {
		t.Fatalf("force reason = %q", r.GetRuntime().GetReason())
	}
}

func TestBootPolls(t *testing.T) {
	d, _ := newTestDriver(t, Config{BootPolls: 2})
	sr := startVM(t, d, "vm")
	if sr.GetRuntime().GetState() != nodev1.RuntimeState_RUNTIME_STATE_BOOTING {
		t.Fatalf("start state = %v, want BOOTING", sr.GetRuntime().GetState())
	}

	// A job cannot start while booting.
	wantErr(t, startJob(d, "vm", "j", 0), errs.ErrFailedPrecondition)

	// A second start while booting does not consume a poll.
	sr, err := d.StartVM(ctx, &nodev1.StartVMRequest{VmId: "vm", Cpus: 2, MemoryMb: 512, DiskIds: []string{"disk-vm"}})
	mustNoErr(t, err)
	if sr.GetRuntime().GetState() != nodev1.RuntimeState_RUNTIME_STATE_BOOTING {
		t.Fatalf("second start state = %v, want BOOTING", sr.GetRuntime().GetState())
	}

	want := []nodev1.RuntimeState{
		nodev1.RuntimeState_RUNTIME_STATE_BOOTING,
		nodev1.RuntimeState_RUNTIME_STATE_BOOTING,
		nodev1.RuntimeState_RUNTIME_STATE_RUNNING,
		nodev1.RuntimeState_RUNTIME_STATE_RUNNING,
	}
	for i, w := range want {
		r, err := d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm"})
		mustNoErr(t, err)
		if r.GetRuntime().GetState() != w {
			t.Fatalf("poll %d = %v, want %v", i+1, r.GetRuntime().GetState(), w)
		}
	}
	mustNoErr(t, startJob(d, "vm", "j", 0))
}

func TestBootPollsZeroIsRunningImmediately(t *testing.T) {
	d, _ := newTestDriver(t, Config{BootPolls: 0})
	sr := startVM(t, d, "vm")
	if sr.GetRuntime().GetState() != nodev1.RuntimeState_RUNTIME_STATE_RUNNING {
		t.Fatalf("start state = %v", sr.GetRuntime().GetState())
	}
}

func TestCrash(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	d.Crash("absent", "boom") // no-op
	runningVM(t, d, "vm")

	d.Exec = func(string, *nodev1.JobSpec) (int, string, string, time.Duration) {
		return 0, "", "", time.Hour
	}
	mustNoErr(t, startJob(d, "vm", "long", 0))
	d.Exec = nil // default: exits immediately
	mustNoErr(t, startJob(d, "vm", "done", 0))

	d.Crash("vm", "kernel panic")
	r, _ := d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm"})
	if got := r.GetRuntime(); got.GetState() != nodev1.RuntimeState_RUNTIME_STATE_EXITED || got.GetReason() != "kernel panic" {
		t.Fatalf("after crash: %+v", got)
	}
	p := poll(t, d, "vm", "long", 0, 0, 0, false)
	if p.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_LOST || p.ExitCode != nil {
		t.Fatalf("running job after crash: state %v exit %v", p.GetState(), p.ExitCode)
	}
	p = poll(t, d, "vm", "done", 0, 0, 0, false)
	if p.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_EXITED || p.GetExitCode() != 0 {
		t.Fatalf("finished job after crash: state %v", p.GetState())
	}

	// Crashing an exited VM keeps the first reason.
	d.Crash("vm", "second")
	r, _ = d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm"})
	if r.GetRuntime().GetReason() != "kernel panic" {
		t.Fatalf("reason changed to %q", r.GetRuntime().GetReason())
	}
}

func TestCrashWhileBooting(t *testing.T) {
	d, _ := newTestDriver(t, Config{BootPolls: 3})
	startVM(t, d, "vm")
	d.Crash("vm", "oom")
	r, _ := d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm"})
	if r.GetRuntime().GetState() != nodev1.RuntimeState_RUNTIME_STATE_EXITED {
		t.Fatalf("state = %v, want EXITED", r.GetRuntime().GetState())
	}
}

func TestForgetVM(t *testing.T) {
	d, _ := newTestDriver(t, Config{BootPolls: 1})
	_, err := d.ForgetVM(ctx, &nodev1.ForgetVMRequest{VmId: "unknown"})
	mustNoErr(t, err)

	startVM(t, d, "vm")
	_, err = d.ForgetVM(ctx, &nodev1.ForgetVMRequest{VmId: "vm"})
	wantErr(t, err, errs.ErrFailedPrecondition) // booting counts as running

	d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm"})
	_, err = d.ForgetVM(ctx, &nodev1.ForgetVMRequest{VmId: "vm"})
	wantErr(t, err, errs.ErrFailedPrecondition)

	_, err = d.PutFile(ctx, &nodev1.PutFileRequest{VmId: "vm", Path: "/f", Content: []byte("x")})
	mustNoErr(t, err)
	mustNoErr(t, startJob(d, "vm", "j", 0))

	_, err = d.StopVM(ctx, &nodev1.StopVMRequest{VmId: "vm"})
	mustNoErr(t, err)
	_, err = d.ForgetVM(ctx, &nodev1.ForgetVMRequest{VmId: "vm"})
	mustNoErr(t, err)

	r, _ := d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm"})
	if r.GetRuntime().GetState() != nodev1.RuntimeState_RUNTIME_STATE_ABSENT {
		t.Fatalf("after forget state = %v", r.GetRuntime().GetState())
	}
	if p := poll(t, d, "vm", "j", 0, 0, 0, false); p.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_MISSING {
		t.Fatalf("job after forget = %v", p.GetState())
	}
	// Files are gone too: GetFile needs a running VM, so restart and check.
	startVM(t, d, "vm")
	d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm"})
	_, err = d.GetFile(ctx, &nodev1.GetFileRequest{VmId: "vm", Path: "/f"})
	wantErr(t, err, errs.ErrNotFound)
}

func TestSetDown(t *testing.T) {
	calls := map[string]func(d *Driver) error{
		"GetCapacity": func(d *Driver) error {
			_, err := d.GetCapacity(ctx, &nodev1.GetCapacityRequest{})
			return err
		},
		"RegisterImage": func(d *Driver) error {
			_, err := d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "i"})
			return err
		},
		"DeleteImage": func(d *Driver) error {
			_, err := d.DeleteImage(ctx, &nodev1.DeleteImageRequest{ImageId: "i"})
			return err
		},
		"CreateDisk": func(d *Driver) error {
			_, err := d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "d", SizeGb: 1})
			return err
		},
		"ResizeDisk": func(d *Driver) error {
			_, err := d.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: "d", SizeGb: 2})
			return err
		},
		"DeleteDisk": func(d *Driver) error {
			_, err := d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: "d"})
			return err
		},
		"SnapshotDisk": func(d *Driver) error {
			_, err := d.SnapshotDisk(ctx, &nodev1.SnapshotDiskRequest{DiskId: "d", ImageId: "i"})
			return err
		},
		"StartVM": func(d *Driver) error {
			_, err := d.StartVM(ctx, &nodev1.StartVMRequest{VmId: "v", Cpus: 1, MemoryMb: 1})
			return err
		},
		"StopVM": func(d *Driver) error {
			_, err := d.StopVM(ctx, &nodev1.StopVMRequest{VmId: "v"})
			return err
		},
		"GetVMRuntime": func(d *Driver) error {
			_, err := d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "v"})
			return err
		},
		"ForgetVM": func(d *Driver) error {
			_, err := d.ForgetVM(ctx, &nodev1.ForgetVMRequest{VmId: "v"})
			return err
		},
		"StartJob": func(d *Driver) error {
			return startJob(d, "v", "j", 0)
		},
		"PollJob": func(d *Driver) error {
			_, err := d.PollJob(ctx, &nodev1.PollJobRequest{VmId: "v", JobId: "j"})
			return err
		},
		"CancelJob": func(d *Driver) error {
			_, err := d.CancelJob(ctx, &nodev1.CancelJobRequest{VmId: "v", JobId: "j"})
			return err
		},
		"PutFile": func(d *Driver) error {
			_, err := d.PutFile(ctx, &nodev1.PutFileRequest{VmId: "v", Path: "/f"})
			return err
		},
		"GetFile": func(d *Driver) error {
			_, err := d.GetFile(ctx, &nodev1.GetFileRequest{VmId: "v", Path: "/f"})
			return err
		},
		"GetVMMetrics": func(d *Driver) error {
			_, err := d.GetVMMetrics(ctx, &nodev1.GetVMMetricsRequest{VmId: "v"})
			return err
		},
	}
	if len(calls) != 17 {
		t.Fatalf("test covers %d methods, want 17", len(calls))
	}

	d, _ := newTestDriver(t, Config{})
	d.SetDown(true)
	for name, call := range calls {
		if err := call(d); !errors.Is(err, errs.ErrUnavailable) {
			t.Fatalf("%s while down: got %v, want Unavailable", name, err)
		}
	}
	d.SetDown(false)
	for name, call := range calls {
		// Without a VM these fail with their own errors, but never Unavailable.
		if err := call(d); errors.Is(err, errs.ErrUnavailable) {
			t.Fatalf("%s still unavailable after SetDown(false)", name)
		}
	}
	// Every method is still recorded while down.
	d.SetDown(true)
	before := len(d.Calls())
	calls["GetCapacity"](d)
	if got := len(d.Calls()); got != before+1 {
		t.Fatalf("down call not recorded: %d -> %d", before, got)
	}
}

func TestFailNext(t *testing.T) {
	boom := errors.New("boom")
	d, _ := newTestDriver(t, Config{})
	setupDisk(t, d, "disk")

	d.FailNext("StartVM", boom)
	_, err := d.StartVM(ctx, &nodev1.StartVMRequest{VmId: "v", Cpus: 1, MemoryMb: 1, DiskIds: []string{"disk"}})
	if !errors.Is(err, boom) {
		t.Fatalf("first call err = %v, want boom", err)
	}
	// One-shot: the next call succeeds.
	_, err = d.StartVM(ctx, &nodev1.StartVMRequest{VmId: "v", Cpus: 1, MemoryMb: 1, DiskIds: []string{"disk"}})
	mustNoErr(t, err)

	// Failures are scoped to their method name.
	d.FailNext("StopVM", boom)
	_, err = d.GetCapacity(ctx, &nodev1.GetCapacityRequest{})
	mustNoErr(t, err)
	_, err = d.StopVM(ctx, &nodev1.StopVMRequest{VmId: "v"})
	wantErr(t, err, boom)
	_, err = d.StopVM(ctx, &nodev1.StopVMRequest{VmId: "v"})
	mustNoErr(t, err)

	// A nil error cancels a pending failure.
	d.FailNext("GetCapacity", boom)
	d.FailNext("GetCapacity", nil)
	_, err = d.GetCapacity(ctx, &nodev1.GetCapacityRequest{})
	mustNoErr(t, err)

	// Down wins over a pending failure, and the failure is not consumed.
	d.FailNext("GetCapacity", boom)
	d.SetDown(true)
	_, err = d.GetCapacity(ctx, &nodev1.GetCapacityRequest{})
	wantErr(t, err, errs.ErrUnavailable)
	d.SetDown(false)
	_, err = d.GetCapacity(ctx, &nodev1.GetCapacityRequest{})
	wantErr(t, err, boom)
}

func TestCallsRecordsInOrder(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	d.FailNext("RegisterImage", errors.New("x"))
	d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "i"}) // fails, still recorded
	d.GetCapacity(ctx, &nodev1.GetCapacityRequest{})
	d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "i"})
	d.DeleteImage(ctx, &nodev1.DeleteImageRequest{ImageId: "i"})
	d.Crash("x", "y") // not a Driver method: not recorded

	got := d.Calls()
	want := []string{"RegisterImage", "GetCapacity", "RegisterImage", "DeleteImage"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	// The returned slice is a copy.
	got[0] = "tampered"
	if d.Calls()[0] != "RegisterImage" {
		t.Fatal("Calls returned shared state")
	}
}

func TestPollJobStates(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	// Unknown VM and unknown job are MISSING, not errors.
	p := poll(t, d, "nope", "j", 0, 0, 0, false)
	if p.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_MISSING {
		t.Fatalf("unknown vm state = %v", p.GetState())
	}
	runningVM(t, d, "vm")
	p = poll(t, d, "vm", "j", 0, 0, 0, false)
	if p.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_MISSING {
		t.Fatalf("unknown job state = %v", p.GetState())
	}

	mustNoErr(t, startJob(d, "vm", "j", 0))
	wantErr(t, startJob(d, "vm", "j", 0), errs.ErrAlreadyExists)

	p = poll(t, d, "vm", "j", 0, 0, 0, false)
	if p.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_EXITED || p.GetExitCode() != 0 {
		t.Fatalf("default job: state %v exit %v", p.GetState(), p.ExitCode)
	}
	if string(p.GetStdout()) != "ok\n" || len(p.GetStderr()) != 0 || p.GetMore() || p.GetTruncated() {
		t.Fatalf("default output: %q %q more=%v trunc=%v", p.GetStdout(), p.GetStderr(), p.GetMore(), p.GetTruncated())
	}
}

func TestJobRequiresRunningVM(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	wantErr(t, startJob(d, "absent", "j", 0), errs.ErrFailedPrecondition)
	_, err := d.StartJob(ctx, &nodev1.StartJobRequest{VmId: "absent"})
	wantErr(t, err, errs.ErrInvalidArgument) // nil spec has no job id

	runningVM(t, d, "vm")
	_, err = d.StopVM(ctx, &nodev1.StopVMRequest{VmId: "vm"})
	mustNoErr(t, err)
	wantErr(t, startJob(d, "vm", "j", 0), errs.ErrFailedPrecondition)
}

func TestJobRunForWithClock(t *testing.T) {
	d, clk := newTestDriver(t, Config{})
	d.Exec = func(string, *nodev1.JobSpec) (int, string, string, time.Duration) {
		return 3, "out", "err", 10 * time.Second
	}
	runningVM(t, d, "vm")
	mustNoErr(t, startJob(d, "vm", "j", 0))

	p := poll(t, d, "vm", "j", 0, 0, 0, false)
	if p.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_RUNNING || p.ExitCode != nil || p.GetElapsedSeconds() != 0 {
		t.Fatalf("at t0: state %v exit %v elapsed %d", p.GetState(), p.ExitCode, p.GetElapsedSeconds())
	}
	if len(p.GetStdout()) != 0 || len(p.GetStderr()) != 0 {
		t.Fatalf("running job has output %q %q", p.GetStdout(), p.GetStderr())
	}

	clk.Advance(4 * time.Second)
	p = poll(t, d, "vm", "j", 0, 0, 0, false)
	if p.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_RUNNING || p.GetElapsedSeconds() != 4 {
		t.Fatalf("at 4s: state %v elapsed %d", p.GetState(), p.GetElapsedSeconds())
	}

	clk.Advance(6 * time.Second)
	p = poll(t, d, "vm", "j", 0, 0, 0, false)
	if p.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_EXITED || p.GetExitCode() != 3 || p.GetElapsedSeconds() != 10 {
		t.Fatalf("at 10s: state %v exit %v elapsed %d", p.GetState(), p.ExitCode, p.GetElapsedSeconds())
	}
	if string(p.GetStdout()) != "out" || string(p.GetStderr()) != "err" {
		t.Fatalf("finished output %q %q", p.GetStdout(), p.GetStderr())
	}

	clk.Advance(time.Hour)
	p = poll(t, d, "vm", "j", 0, 0, 0, false)
	if p.GetElapsedSeconds() != 10 {
		t.Fatalf("elapsed grew after finish: %d", p.GetElapsedSeconds())
	}
}

func TestJobTimeout(t *testing.T) {
	tests := []struct {
		name        string
		runFor      time.Duration
		timeout     int32
		wantExit    int32
		wantElapsed int64
	}{
		{"no timeout", 30 * time.Second, 0, 0, 30},
		{"finishes before timeout", 5 * time.Second, 10, 0, 5},
		{"exactly at timeout is not a timeout", 10 * time.Second, 10, 0, 10},
		{"exceeds timeout", 30 * time.Second, 10, exitTimeout, 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, clk := newTestDriver(t, Config{})
			d.Exec = func(string, *nodev1.JobSpec) (int, string, string, time.Duration) {
				return 0, "", "", tc.runFor
			}
			runningVM(t, d, "vm")
			mustNoErr(t, startJob(d, "vm", "j", tc.timeout))

			clk.Advance(time.Minute)
			p := poll(t, d, "vm", "j", 0, 0, 0, false)
			if p.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_EXITED {
				t.Fatalf("state %v", p.GetState())
			}
			if p.GetExitCode() != tc.wantExit {
				t.Fatalf("exit = %d, want %d", p.GetExitCode(), tc.wantExit)
			}
			if p.GetElapsedSeconds() != tc.wantElapsed {
				t.Fatalf("elapsed = %d, want %d", p.GetElapsedSeconds(), tc.wantElapsed)
			}
		})
	}

	// Timed-out job stays RUNNING until the timeout, not until the Exec runtime.
	d, clk := newTestDriver(t, Config{})
	d.Exec = func(string, *nodev1.JobSpec) (int, string, string, time.Duration) {
		return 0, "", "", time.Minute
	}
	runningVM(t, d, "vm")
	mustNoErr(t, startJob(d, "vm", "j", 10))
	clk.Advance(9 * time.Second)
	if p := poll(t, d, "vm", "j", 0, 0, 0, false); p.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_RUNNING {
		t.Fatalf("at 9s with 10s timeout: state %v", p.GetState())
	}
}

func TestCancelJob(t *testing.T) {
	d, clk := newTestDriver(t, Config{})
	_, err := d.CancelJob(ctx, &nodev1.CancelJobRequest{VmId: "vm", JobId: "j"})
	wantErr(t, err, errs.ErrNotFound) // unknown VM

	runningVM(t, d, "vm")
	_, err = d.CancelJob(ctx, &nodev1.CancelJobRequest{VmId: "vm", JobId: "j"})
	wantErr(t, err, errs.ErrNotFound) // unknown job

	d.Exec = func(string, *nodev1.JobSpec) (int, string, string, time.Duration) {
		return 0, "partial", "", time.Hour
	}
	mustNoErr(t, startJob(d, "vm", "run", 0))
	_, err = d.CancelJob(ctx, &nodev1.CancelJobRequest{VmId: "vm", JobId: "run"})
	mustNoErr(t, err)
	clk.Advance(5 * time.Second)
	p := poll(t, d, "vm", "run", 0, 0, 0, false)
	if p.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_EXITED || p.GetExitCode() != 130 || !p.GetCancelled() {
		t.Fatalf("cancelled job: state %v exit %v cancelled %v", p.GetState(), p.ExitCode, p.GetCancelled())
	}
	if p.GetElapsedSeconds() != 0 {
		t.Fatalf("elapsed after cancel at once = %d, want 0", p.GetElapsedSeconds())
	}

	// Cancelling a finished job is a no-op.
	d.Exec = nil
	mustNoErr(t, startJob(d, "vm", "done", 0))
	_, err = d.CancelJob(ctx, &nodev1.CancelJobRequest{VmId: "vm", JobId: "done"})
	mustNoErr(t, err)
	p = poll(t, d, "vm", "done", 0, 0, 0, false)
	if p.GetExitCode() != 0 || p.GetCancelled() {
		t.Fatalf("cancel of finished job changed it: exit %v cancelled %v", p.ExitCode, p.GetCancelled())
	}

	// A job whose runtime has passed but was not yet polled is finished, so cancel is a no-op.
	d.Exec = func(string, *nodev1.JobSpec) (int, string, string, time.Duration) {
		return 7, "", "", time.Second
	}
	mustNoErr(t, startJob(d, "vm", "timed", 0))
	clk.Advance(2 * time.Second)
	_, err = d.CancelJob(ctx, &nodev1.CancelJobRequest{VmId: "vm", JobId: "timed"})
	mustNoErr(t, err)
	p = poll(t, d, "vm", "timed", 0, 0, 0, false)
	if p.GetExitCode() != 7 || p.GetCancelled() {
		t.Fatalf("late cancel: exit %v cancelled %v, want 7 false", p.ExitCode, p.GetCancelled())
	}
}

func TestOutputPaging(t *testing.T) {
	const size = 20000
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	d, _ := newTestDriver(t, Config{})
	d.Exec = func(string, *nodev1.JobSpec) (int, string, string, time.Duration) {
		return 0, string(payload), "err\n", 0
	}
	runningVM(t, d, "vm")
	mustNoErr(t, startJob(d, "vm", "j", 0))

	t.Run("offset walk default page", func(t *testing.T) {
		var got []byte
		var off int64
		var pages []int
		for {
			p := poll(t, d, "vm", "j", off, 0, 0, false)
			if p.GetStdoutFrom() != off {
				t.Fatalf("StdoutFrom = %d, want %d", p.GetStdoutFrom(), off)
			}
			if p.GetStdoutOffset() != off+int64(len(p.GetStdout())) {
				t.Fatalf("StdoutOffset = %d, want %d", p.GetStdoutOffset(), off+int64(len(p.GetStdout())))
			}
			pages = append(pages, len(p.GetStdout()))
			got = append(got, p.GetStdout()...)
			off = p.GetStdoutOffset()
			if !p.GetMore() {
				break
			}
			if len(pages) > 10 {
				t.Fatal("paging did not terminate")
			}
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("reassembled output differs (len %d)", len(got))
		}
		if fmt.Sprint(pages) != "[8192 8192 3616]" {
			t.Fatalf("page sizes = %v", pages)
		}
	})

	offsetTests := []struct {
		name     string
		max      int32
		wantLen  int
		wantMore bool
	}{
		{"zero means default", 0, 8192, true},
		{"explicit small", 100, 100, true},
		{"capped at 65536", 100000, 20000, false},
	}
	for _, tc := range offsetTests {
		t.Run("offset "+tc.name, func(t *testing.T) {
			p := poll(t, d, "vm", "j", 0, 0, tc.max, false)
			if len(p.GetStdout()) != tc.wantLen || p.GetMore() != tc.wantMore {
				t.Fatalf("len %d more %v, want len %d more %v", len(p.GetStdout()), p.GetMore(), tc.wantLen, tc.wantMore)
			}
			if !bytes.Equal(p.GetStdout(), payload[:tc.wantLen]) {
				t.Fatal("wrong bytes")
			}
		})
	}

	t.Run("offset past end is empty and keeps offset", func(t *testing.T) {
		p := poll(t, d, "vm", "j", 25000, 0, 0, false)
		if len(p.GetStdout()) != 0 || p.GetMore() || p.GetStdoutFrom() != 25000 || p.GetStdoutOffset() != 25000 {
			t.Fatalf("got %q more=%v from=%d off=%d", p.GetStdout(), p.GetMore(), p.GetStdoutFrom(), p.GetStdoutOffset())
		}
	})

	t.Run("stderr pages independently", func(t *testing.T) {
		p := poll(t, d, "vm", "j", 0, 2, 0, false)
		if string(p.GetStderr()) != "r\n" || p.GetStderrFrom() != 2 || p.GetStderrOffset() != 4 {
			t.Fatalf("stderr %q from %d off %d", p.GetStderr(), p.GetStderrFrom(), p.GetStderrOffset())
		}
		if !p.GetMore() {
			t.Fatal("more should be set: stdout still has bytes")
		}
	})

	tailTests := []struct {
		name          string
		max           int32
		wantFrom      int64
		wantLen       int
		wantTruncated bool
	}{
		{"last 1000", 1000, 19000, 1000, true},
		{"zero means default", 0, 20000 - 8192, 8192, true},
		{"larger than output", 30000, 0, 20000, false},
		{"cap", 1 << 20, 0, 20000, false},
	}
	for _, tc := range tailTests {
		t.Run("tail "+tc.name, func(t *testing.T) {
			p := poll(t, d, "vm", "j", 0, 0, tc.max, true)
			if p.GetStdoutFrom() != tc.wantFrom || len(p.GetStdout()) != tc.wantLen || p.GetTruncated() != tc.wantTruncated {
				t.Fatalf("from %d len %d trunc %v", p.GetStdoutFrom(), len(p.GetStdout()), p.GetTruncated())
			}
			if !bytes.Equal(p.GetStdout(), payload[tc.wantFrom:]) {
				t.Fatal("tail bytes wrong")
			}
			if p.GetStdoutOffset() != size {
				t.Fatalf("tail StdoutOffset = %d, want %d", p.GetStdoutOffset(), size)
			}
			if p.GetMore() {
				t.Fatal("tail mode should not report More")
			}
		})
	}

	t.Run("negative inputs are invalid", func(t *testing.T) {
		_, err := d.PollJob(ctx, &nodev1.PollJobRequest{VmId: "vm", JobId: "j", StdoutOffset: -1})
		wantErr(t, err, errs.ErrInvalidArgument)
		_, err = d.PollJob(ctx, &nodev1.PollJobRequest{VmId: "vm", JobId: "j", MaxOutput: -1})
		wantErr(t, err, errs.ErrInvalidArgument)
	})
}

func TestPutGetFile(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	_, err := d.PutFile(ctx, &nodev1.PutFileRequest{VmId: "vm", Path: "/f", Content: []byte("x")})
	wantErr(t, err, errs.ErrFailedPrecondition) // VM absent

	runningVM(t, d, "vm")
	_, err = d.PutFile(ctx, &nodev1.PutFileRequest{VmId: "vm", Path: "", Content: []byte("x")})
	wantErr(t, err, errs.ErrInvalidArgument)

	content := []byte("hello world")
	_, err = d.PutFile(ctx, &nodev1.PutFileRequest{VmId: "vm", Path: "/f", Content: content, Mode: 0o600})
	mustNoErr(t, err)
	content[0] = 'J' // caller mutation must not reach the stored file

	r, err := d.GetFile(ctx, &nodev1.GetFileRequest{VmId: "vm", Path: "/f"})
	mustNoErr(t, err)
	if string(r.GetContent()) != "hello world" || r.GetTruncated() {
		t.Fatalf("got %q truncated=%v", r.GetContent(), r.GetTruncated())
	}

	r, err = d.GetFile(ctx, &nodev1.GetFileRequest{VmId: "vm", Path: "/f", MaxBytes: 5})
	mustNoErr(t, err)
	if string(r.GetContent()) != "hello" || !r.GetTruncated() {
		t.Fatalf("max 5: got %q truncated=%v", r.GetContent(), r.GetTruncated())
	}

	_, err = d.GetFile(ctx, &nodev1.GetFileRequest{VmId: "vm", Path: "/missing"})
	wantErr(t, err, errs.ErrNotFound)
	_, err = d.GetFile(ctx, &nodev1.GetFileRequest{VmId: "vm", Path: ""})
	wantErr(t, err, errs.ErrInvalidArgument)

	// Big file: the default cap is 8 MiB even when asked for more.
	big := bytes.Repeat([]byte{'z'}, 9<<20)
	_, err = d.PutFile(ctx, &nodev1.PutFileRequest{VmId: "vm", Path: "/big", Content: big})
	mustNoErr(t, err)
	for _, max := range []int64{0, 1 << 30} {
		r, err = d.GetFile(ctx, &nodev1.GetFileRequest{VmId: "vm", Path: "/big", MaxBytes: max})
		mustNoErr(t, err)
		if len(r.GetContent()) != 8<<20 || !r.GetTruncated() {
			t.Fatalf("max %d: len %d truncated %v", max, len(r.GetContent()), r.GetTruncated())
		}
	}

	_, err = d.StopVM(ctx, &nodev1.StopVMRequest{VmId: "vm"})
	mustNoErr(t, err)
	_, err = d.GetFile(ctx, &nodev1.GetFileRequest{VmId: "vm", Path: "/f"})
	wantErr(t, err, errs.ErrFailedPrecondition)
}

func TestGetVMMetricsDefaults(t *testing.T) {
	d, clk := newTestDriver(t, Config{})
	_, err := d.GetVMMetrics(ctx, &nodev1.GetVMMetricsRequest{VmId: "vm"})
	wantErr(t, err, errs.ErrFailedPrecondition)

	setupDisk(t, d, "root") // 10 GB
	setupDisk(t, d, "data") // 10 GB
	_, err = d.StartVM(ctx, &nodev1.StartVMRequest{VmId: "vm", Cpus: 4, MemoryMb: 1024, DiskIds: []string{"root", "data"}})
	mustNoErr(t, err)
	clk.Advance(30 * time.Second)

	m, err := d.GetVMMetrics(ctx, &nodev1.GetVMMetricsRequest{VmId: "vm", DiskIds: []string{"root", "data"}})
	mustNoErr(t, err)
	if m.GetCpuCores() != 4 || m.GetCpuPercent() != 1.5 || m.GetMemTotalBytes() != gib {
		t.Fatalf("defaults: cores %d cpu %v mem %d", m.GetCpuCores(), m.GetCpuPercent(), m.GetMemTotalBytes())
	}
	if m.GetUptimeSeconds() != 30 {
		t.Fatalf("uptime = %d, want 30", m.GetUptimeSeconds())
	}
	if len(m.GetFilesystems()) != 2 || len(m.GetDisks()) != 2 {
		t.Fatalf("filesystems %d disks %d, want 2 each", len(m.GetFilesystems()), len(m.GetDisks()))
	}
	fs0, fs1 := m.GetFilesystems()[0], m.GetFilesystems()[1]
	if fs0.GetDevice() != "/dev/vda1" || fs0.GetDiskId() != "root" || fs0.GetMountPoint() != "/" {
		t.Fatalf("fs0 = %+v", fs0)
	}
	if fs1.GetDevice() != "/dev/vdb1" || fs1.GetDiskId() != "data" {
		t.Fatalf("fs1 = %+v", fs1)
	}
	if m.GetDisks()[1].GetDiskId() != "data" || m.GetDisks()[1].GetVirtualSizeBytes() != 10*gib {
		t.Fatalf("alloc1 = %+v", m.GetDisks()[1])
	}

	_, err = d.GetVMMetrics(ctx, &nodev1.GetVMMetricsRequest{VmId: "vm", DiskIds: []string{"nope"}})
	wantErr(t, err, errs.ErrNotFound)
}

func TestSetMetrics(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	runningVM(t, d, "vm")
	setupDisk(t, d, "x")

	custom := &nodev1.GetVMMetricsResponse{CpuPercent: 99, CpuCores: 7, Filesystems: []*nodev1.GuestFilesystem{{DiskId: "x"}}}
	d.SetMetrics("vm", custom)
	custom.CpuPercent = 1 // caller mutation must not leak in

	m, err := d.GetVMMetrics(ctx, &nodev1.GetVMMetricsRequest{VmId: "vm"})
	mustNoErr(t, err)
	if m.GetCpuPercent() != 99 || m.GetCpuCores() != 7 || len(m.GetFilesystems()) != 1 {
		t.Fatalf("override = %+v", m)
	}
	m.CpuPercent = 0 // caller mutation must not leak out
	m, _ = d.GetVMMetrics(ctx, &nodev1.GetVMMetricsRequest{VmId: "vm"})
	if m.GetCpuPercent() != 99 {
		t.Fatal("returned metrics alias driver state")
	}

	d.SetMetrics("vm", nil)
	m, _ = d.GetVMMetrics(ctx, &nodev1.GetVMMetricsRequest{VmId: "vm"})
	if m.GetCpuPercent() != 1.5 {
		t.Fatalf("after clearing override cpu = %v", m.GetCpuPercent())
	}
}

func TestConcurrentUse(t *testing.T) {
	d, _ := newTestDriver(t, Config{BootPolls: 1})
	setupDisk(t, d, "disk")
	if _, err := d.StartVM(ctx, &nodev1.StartVMRequest{VmId: "vm", Cpus: 1, MemoryMb: 1, DiskIds: []string{"disk"}}); err != nil {
		t.Fatal(err)
	}
	d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm"}) // now RUNNING
	before := len(d.Calls())

	const workers, iters = 8, 100
	var wg sync.WaitGroup
	errc := make(chan error, workers*iters)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				jobID := fmt.Sprintf("j-%d-%d", w, i)
				if err := startJob(d, "vm", jobID, 0); err != nil {
					errc <- err
				}
				if _, err := d.PollJob(ctx, &nodev1.PollJobRequest{VmId: "vm", JobId: jobID}); err != nil {
					errc <- err
				}
				if _, err := d.CancelJob(ctx, &nodev1.CancelJobRequest{VmId: "vm", JobId: jobID}); err != nil {
					errc <- err
				}
				if _, err := d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm"}); err != nil {
					errc <- err
				}
				_ = d.Calls()
			}
		}(w)
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Errorf("concurrent call failed: %v", err)
	}
	// Each iteration makes four Driver calls (Calls itself is not recorded).
	if got, want := len(d.Calls())-before, workers*iters*4; got != want {
		t.Fatalf("recorded %d calls, want %d", got, want)
	}
}

func TestDriverImplementsNodeDriver(t *testing.T) {
	// The compile-time assertion in fake.go does the work; this checks New returns a usable value.
	d := New(Config{CPUs: 1})
	if d == nil || d.Now == nil || d.Exec == nil {
		t.Fatal("New did not set defaults")
	}
}
