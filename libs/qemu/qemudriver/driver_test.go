package qemudriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/node"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/guestexec"
)

var _ node.Driver = (*Driver)(nil)

const runningState = nodev1.RuntimeState_RUNTIME_STATE_RUNNING
const bootingState = nodev1.RuntimeState_RUNTIME_STATE_BOOTING
const exitedState = nodev1.RuntimeState_RUNTIME_STATE_EXITED
const absentState = nodev1.RuntimeState_RUNTIME_STATE_ABSENT

// errKeys is a KeySource that always fails.
type errKeys struct{}

func (errKeys) PublicKeyLine() (string, error) { return "", errors.New("no key on disk") }

func TestNewValidatesConfig(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name string
		mut  func(*Config)
	}{
		{"relative DataDir", func(c *Config) { c.DataDir = "relative" }},
		{"missing NewRunner", func(c *Config) { c.NewRunner = nil }},
		{"missing key", func(c *Config) { c.Keys = nil }},
		{"relative SharedDir", func(c *Config) { c.SharedDir = "shared" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(h.config(tt.mut)); err == nil {
				t.Fatal("New succeeded, want an error")
			}
		})
	}
}

func TestNewCreatesLayoutAndHandsSharedToRunAs(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RunAs = "qemu" })
	for _, dir := range []string{"images", "disks", "vms", "shared"} {
		if fi, err := os.Stat(filepath.Join(h.dir, dir)); err != nil || !fi.IsDir() {
			t.Errorf("%s: not created: %v", dir, err)
		}
	}
	if got := h.cmd.commandsNamed("chown qemu " + filepath.Join(h.dir, "shared")); len(got) != 1 {
		t.Errorf("shared folder chown calls = %v, want 1", got)
	}
}

func TestNewAdoptsRunningVMs(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	port := h.port("vm1")

	// A second driver on the same DataDir, as after a daemon restart.
	d2 := h.open()
	if got := d2.ports[port]; got != "vm1" {
		t.Errorf("adopted port %d holder = %q, want vm1", port, got)
	}
	resp, err := d2.GetVMRuntime(context.Background(), &nodev1.GetVMRuntimeRequest{VmId: "vm1"})
	if err != nil {
		t.Fatalf("GetVMRuntime on adopting driver: %v", err)
	}
	if resp.GetRuntime().GetState() != bootingState {
		t.Errorf("state = %v, want BOOTING", resp.GetRuntime().GetState())
	}
}

func TestNewDoesNotAdoptDeadVMs(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	port := h.port("vm1")
	h.procs.crash(h.pid("vm1"))

	if d2 := h.open(); len(d2.ports) != 0 {
		t.Errorf("dead VM's port %d was adopted: %v", port, d2.ports)
	}
}

func TestGetCapacity(t *testing.T) {
	h := newHarness(t)
	resp, err := h.d.GetCapacity(context.Background(), &nodev1.GetCapacityRequest{})
	if err != nil {
		t.Fatalf("GetCapacity: %v", err)
	}
	want := &nodev1.Capacity{
		CpusTotal:         8,
		MemoryMbTotal:     16,
		MemoryMbAvailable: 8,
		DiskBytesTotal:    100 << 30,
		DiskBytesFree:     40 << 30,
	}
	if got := resp.GetCapacity(); got.String() != want.String() {
		t.Errorf("capacity = %v, want %v", got, want)
	}
}

func TestGetCapacityRejectsBadMeminfo(t *testing.T) {
	for _, meminfo := range []string{"", "MemTotal: 10 kB\n", "MemTotal: x kB\nMemAvailable: 1 kB\n"} {
		h := newHarness(t)
		h.fs.meminfo = meminfo
		if _, err := h.d.GetCapacity(context.Background(), &nodev1.GetCapacityRequest{}); err == nil {
			t.Errorf("meminfo %q: no error", meminfo)
		}
	}
}

func TestStartVMBuildsQemuCommandAndSeed(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.KVM = true })
	h.mkDisk("root")
	h.mkDisk("data")
	resp := h.start("vm1", "root", "data")
	if resp.GetRuntime().GetState() != bootingState {
		t.Fatalf("StartVM state = %v, want BOOTING", resp.GetRuntime().GetState())
	}

	starts := h.cmd.startCalls()
	if len(starts) != 1 {
		t.Fatalf("Start calls = %d, want 1", len(starts))
	}
	st := starts[0]
	dir := filepath.Join(h.dir, "vms", "vm1")
	args := strings.Join(st.args, " ")
	port := h.port("vm1")
	for _, want := range []string{
		"-enable-kvm",
		"file=" + filepath.Join(h.dir, "disks", "root.qcow2") + ",if=virtio,format=qcow2",
		"file=" + filepath.Join(h.dir, "disks", "data.qcow2") + ",if=virtio,format=qcow2",
		"file=" + filepath.Join(dir, "seed.iso") + ",if=virtio,format=raw,readonly=on",
		"-serial file:" + filepath.Join(dir, "console.log"),
		"path=" + filepath.Join(h.dir, "shared") + ",security_model=mapped-xattr",
		fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:22", port),
	} {
		if !strings.Contains(args, want) {
			t.Errorf("qemu args missing %q\nargs: %s", want, args)
		}
	}
	if st.logPath != filepath.Join(dir, "qemu.log") {
		t.Errorf("log path = %q", st.logPath)
	}
	if rec, _ := h.d.readRuntime("vm1"); rec == nil || rec.PID != st.pid || rec.SSHPort != port {
		t.Errorf("runtime.json = %+v, want pid %d port %d", rec, st.pid, port)
	}

	userData, err := os.ReadFile(filepath.Join(dir, "user-data"))
	if err != nil || !strings.Contains(string(userData), "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest agent@test") {
		t.Errorf("user-data does not carry the key: %v\n%s", err, userData)
	}
	metaData, _ := os.ReadFile(filepath.Join(dir, "meta-data"))
	if !strings.Contains(string(metaData), "instance-id: vm1-") || !strings.Contains(string(metaData), "local-hostname: vm1") {
		t.Errorf("meta-data = %q", metaData)
	}
	if got := h.cmd.commandsNamed("genisoimage"); len(got) != 1 || !strings.Contains(got[0], "-volid cidata") {
		t.Errorf("genisoimage calls = %v", got)
	}
	var spec specRec
	b, _ := os.ReadFile(filepath.Join(dir, "spec.json"))
	if err := json.Unmarshal(b, &spec); err != nil || spec.Name != "vm1" || spec.CPUs != 2 || len(spec.DiskIDs) != 2 {
		t.Errorf("spec.json = %s (%v)", b, err)
	}
}

func TestStartVMInstanceIDChangesEachStart(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	first, _ := os.ReadFile(filepath.Join(h.dir, "vms", "vm1", "meta-data"))
	h.procs.crash(h.pid("vm1"))
	h.start("vm1", "d1")
	second, _ := os.ReadFile(filepath.Join(h.dir, "vms", "vm1", "meta-data"))
	if string(first) == string(second) {
		t.Errorf("instance-id did not change across starts: %q", first)
	}
}

func TestStartVMIsIdempotentWhileAlive(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	h.bootGuest("vm1")
	resp, err := h.d.StartVM(context.Background(), &nodev1.StartVMRequest{VmId: "vm1", Name: "vm1", Cpus: 2, MemoryMb: 1024, DiskIds: []string{"d1"}})
	if err != nil {
		t.Fatalf("second StartVM: %v", err)
	}
	if resp.GetRuntime().GetState() != runningState {
		t.Errorf("second StartVM state = %v, want RUNNING", resp.GetRuntime().GetState())
	}
	if n := len(h.cmd.startCalls()); n != 1 {
		t.Errorf("Start calls = %d, want 1", n)
	}
}

func TestStartVMRestartsAfterCrash(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	oldPID := h.pid("vm1")
	h.procs.crash(oldPID)
	if rt := h.runtimeOf("vm1"); rt.GetState() != exitedState {
		t.Fatalf("after crash state = %v, want EXITED", rt.GetState())
	}
	h.start("vm1", "d1")
	if h.pid("vm1") == oldPID {
		t.Error("restart reused the old pid")
	}
	if rt := h.runtimeOf("vm1"); rt.GetState() != bootingState || rt.GetReason() != "" {
		t.Errorf("after restart runtime = %v, want BOOTING with no reason", rt)
	}
}

func TestStartVMRejectsInvalidRequestsBeforeWriting(t *testing.T) {
	tests := []struct {
		name string
		req  *nodev1.StartVMRequest
	}{
		{"bad vm id", &nodev1.StartVMRequest{VmId: "../x", Cpus: 1, MemoryMb: 512, DiskIds: []string{"d1"}}},
		{"bad disk id", &nodev1.StartVMRequest{VmId: "vm1", Cpus: 1, MemoryMb: 512, DiskIds: []string{"a/b"}}},
		{"name with space", &nodev1.StartVMRequest{VmId: "vm1", Name: "bad name", Cpus: 1, MemoryMb: 512, DiskIds: []string{"d1"}}},
		{"zero cpus", &nodev1.StartVMRequest{VmId: "vm1", Cpus: 0, MemoryMb: 512, DiskIds: []string{"d1"}}},
		{"zero memory", &nodev1.StartVMRequest{VmId: "vm1", Cpus: 1, MemoryMb: 0, DiskIds: []string{"d1"}}},
		{"memory below QEMU minimum", &nodev1.StartVMRequest{VmId: "vm1", Cpus: 1, MemoryMb: 100, DiskIds: []string{"d1"}}},
		{"no disks", &nodev1.StartVMRequest{VmId: "vm1", Cpus: 1, MemoryMb: 512}},
		{"disk twice", &nodev1.StartVMRequest{VmId: "vm1", Cpus: 1, MemoryMb: 512, DiskIds: []string{"d1", "d1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.mkDisk("d1")
			_, err := h.d.StartVM(context.Background(), tt.req)
			if !errors.Is(err, errs.ErrInvalidArgument) {
				t.Fatalf("err = %v, want invalid argument", err)
			}
			if n := len(h.cmd.startCalls()); n != 0 {
				t.Errorf("Start calls = %d, want 0", n)
			}
			if _, statErr := os.Stat(filepath.Join(h.dir, "vms", tt.req.GetVmId())); statErr == nil {
				t.Error("a VM directory was written for a rejected request")
			}
		})
	}
}

func TestStartVMMissingDiskIsNotFound(t *testing.T) {
	h := newHarness(t)
	_, err := h.d.StartVM(context.Background(), &nodev1.StartVMRequest{VmId: "vm1", Cpus: 1, MemoryMb: 512, DiskIds: []string{"nope"}})
	if !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("err = %v, want not found", err)
	}
}

func TestStartVMFailureLeavesNoRuntimeAndNoPort(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.cmd.failStart = true
	_, err := h.d.StartVM(context.Background(), &nodev1.StartVMRequest{VmId: "vm1", Cpus: 1, MemoryMb: 512, DiskIds: []string{"d1"}})
	if !errors.Is(err, errs.ErrUnavailable) {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if rec, _ := h.d.readRuntime("vm1"); rec != nil {
		t.Errorf("runtime.json written after a failed start: %+v", rec)
	}
	if len(h.d.ports) != 0 {
		t.Errorf("ports still reserved after a failed start: %v", h.d.ports)
	}
}

func TestStartVMKeyFailure(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Keys = errKeys{} })
	h.mkDisk("d1")
	_, err := h.d.StartVM(context.Background(), &nodev1.StartVMRequest{VmId: "vm1", Cpus: 1, MemoryMb: 512, DiskIds: []string{"d1"}})
	if !errors.Is(err, errs.ErrUnavailable) {
		t.Errorf("err = %v, want unavailable", err)
	}
	if n := len(h.cmd.startCalls()); n != 0 {
		t.Errorf("Start calls = %d, want 0", n)
	}
}

func TestSSHKeyIsReadFromPublicKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	pub := "ssh-ed25519 AAAAC3NzaFromFile host@test\n"
	if err := os.WriteFile(keyPath+".pub", []byte(pub), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *Config) {
		c.Keys = nil
		c.SSHKeyPath = keyPath
	})
	h.mkDisk("d1")
	h.start("vm1", "d1")
	userData, _ := os.ReadFile(filepath.Join(h.dir, "vms", "vm1", "user-data"))
	if !strings.Contains(string(userData), "ssh-ed25519 AAAAC3NzaFromFile host@test") {
		t.Errorf("user-data = %s", userData)
	}
}

func TestRuntimeStates(t *testing.T) {
	h := newHarness(t)
	if rt := h.runtimeOf("vm1"); rt.GetState() != absentState {
		t.Errorf("never started: state = %v, want ABSENT", rt.GetState())
	}
	h.mkDisk("d1")
	h.start("vm1", "d1")
	if rt := h.runtimeOf("vm1"); rt.GetState() != bootingState || rt.GetReason() != "" {
		t.Errorf("unreachable guest: %v, want BOOTING with no reason", rt)
	}
	h.clock.Advance(6 * time.Minute)
	if rt := h.runtimeOf("vm1"); rt.GetState() != bootingState || rt.GetReason() != reasonSlowBoot {
		t.Errorf("after BootTimeout: %v, want BOOTING with %q", rt, reasonSlowBoot)
	}
	h.bootGuest("vm1")
	h.clock.Advance(2 * time.Second) // the failed probe is cached for one second
	if rt := h.runtimeOf("vm1"); rt.GetState() != runningState || rt.GetReason() != "" {
		t.Errorf("reachable guest: %v, want RUNNING", rt)
	}
}

func TestGuestMountRunsOncePerBoot(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	if rt := h.runtimeOf("vm1"); rt.GetState() != bootingState {
		t.Fatalf("state = %v, want BOOTING", rt.GetState())
	}
	h.bootGuest("vm1")
	for range 4 {
		h.clock.Advance(2 * time.Second)
		if rt := h.runtimeOf("vm1"); rt.GetState() != runningState {
			t.Fatalf("state = %v, want RUNNING", rt.GetState())
		}
	}
	if m := h.guest("vm1").mounts; m != 1 {
		t.Errorf("mount ran %d times, want 1", m)
	}
	if rec, _ := h.d.readRuntime("vm1"); rec == nil || !rec.Mounted {
		t.Errorf("runtime.json not marked mounted: %+v", rec)
	}
}

func TestFailedMountIsRetried(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	h.bootGuest("vm1")
	g := h.guest("vm1")
	g.mountFails = true
	if rt := h.runtimeOf("vm1"); rt.GetState() != runningState {
		t.Fatalf("state = %v, want RUNNING even though the mount failed", rt.GetState())
	}
	if rec, _ := h.d.readRuntime("vm1"); rec == nil || rec.Mounted {
		t.Fatalf("a failed mount was recorded as done: %+v", rec)
	}
	g.mountFails = false
	h.clock.Advance(2 * time.Second)
	h.runtimeOf("vm1")
	if g.mounts != 1 {
		t.Errorf("successful mounts = %d, want 1 after the retry", g.mounts)
	}
}

func TestProbeIsCachedForOneSecond(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	g := h.guest("vm1")
	for range 3 {
		h.runtimeOf("vm1")
	}
	if g.probes != 1 {
		t.Errorf("probes = %d, want 1 within the cache window", g.probes)
	}
	h.clock.Advance(time.Second)
	h.runtimeOf("vm1")
	if g.probes != 2 {
		t.Errorf("probes = %d, want 2 after the cache expired", g.probes)
	}
}

func TestCrashReportsLastQemuLogLines(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	logPath := filepath.Join(h.dir, "vms", "vm1", "qemu.log")
	if err := os.WriteFile(logPath, []byte("early line\nfatal: cannot lock\nkvm: no\nqemu: exit 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.procs.crash(h.pid("vm1"))
	rt := h.runtimeOf("vm1")
	want := "QEMU exited unexpectedly: fatal: cannot lock | kvm: no | qemu: exit 1"
	if rt.GetState() != exitedState || rt.GetReason() != want {
		t.Errorf("runtime = %v, want EXITED %q", rt, want)
	}
}

func TestCrashWithoutLogGivesGenericReason(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	h.procs.crash(h.pid("vm1"))
	if rt := h.runtimeOf("vm1"); rt.GetReason() != "QEMU exited unexpectedly" {
		t.Errorf("reason = %q", rt.GetReason())
	}
}

func TestGracefulStopReportsStopped(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	h.bootGuest("vm1")
	h.guest("vm1").poweroffWorks = true
	if _, err := h.d.StopVM(context.Background(), &nodev1.StopVMRequest{VmId: "vm1"}); err != nil {
		t.Fatalf("StopVM: %v", err)
	}
	if n := h.guest("vm1").poweroffs; n != 1 {
		t.Errorf("poweroff requests = %d, want 1", n)
	}
	if rt := h.runtimeOf("vm1"); rt.GetState() != exitedState || rt.GetReason() != reasonStopped {
		t.Errorf("runtime = %v, want EXITED stopped", rt)
	}
	if len(h.procs.sentSignals()) != 0 {
		t.Errorf("a graceful stop sent signals: %v", h.procs.sentSignals())
	}
}

func TestGracefulStopEscalatesToKillAfterGrace(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	h.bootGuest("vm1") // reachable, but it ignores the poweroff request
	if _, err := h.d.StopVM(context.Background(), &nodev1.StopVMRequest{VmId: "vm1"}); err != nil {
		t.Fatalf("StopVM: %v", err)
	}
	if rt := h.runtimeOf("vm1"); rt.GetState() != runningState {
		t.Fatalf("within grace: state = %v, want RUNNING", rt.GetState())
	}
	h.clock.Advance(61 * time.Second)
	rt := h.runtimeOf("vm1")
	if rt.GetState() != exitedState || rt.GetReason() != reasonTimedOut {
		t.Fatalf("after grace: runtime = %v, want EXITED %q", rt, reasonTimedOut)
	}
	if sigs := h.procs.sentSignals(); len(sigs) != 1 || sigs[0] != syscall.SIGKILL {
		t.Errorf("signals = %v, want one SIGKILL", sigs)
	}
	if again := h.runtimeOf("vm1"); again.GetReason() != reasonTimedOut {
		t.Errorf("reason after kill = %q, want it kept", again.GetReason())
	}
}

func TestForceStopKillsAtOnce(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	h.bootGuest("vm1")
	if _, err := h.d.StopVM(context.Background(), &nodev1.StopVMRequest{VmId: "vm1", Force: true}); err != nil {
		t.Fatalf("StopVM force: %v", err)
	}
	if sigs := h.procs.sentSignals(); len(sigs) != 1 || sigs[0] != syscall.SIGKILL {
		t.Errorf("signals = %v, want one SIGKILL", sigs)
	}
	if n := h.guest("vm1").poweroffs; n != 0 {
		t.Errorf("force stop asked the guest to power off %d times", n)
	}
	if rt := h.runtimeOf("vm1"); rt.GetState() != exitedState || rt.GetReason() != reasonKilled {
		t.Errorf("runtime = %v, want EXITED killed", rt)
	}
}

func TestStopOfAbsentOrExitedVMIsNoOp(t *testing.T) {
	h := newHarness(t)
	if _, err := h.d.StopVM(context.Background(), &nodev1.StopVMRequest{VmId: "nobody"}); err != nil {
		t.Errorf("stop of absent VM: %v", err)
	}
	h.mkDisk("d1")
	h.start("vm1", "d1")
	h.procs.crash(h.pid("vm1"))
	for _, force := range []bool{false, true} {
		if _, err := h.d.StopVM(context.Background(), &nodev1.StopVMRequest{VmId: "vm1", Force: force}); err != nil {
			t.Errorf("stop of exited VM (force=%v): %v", force, err)
		}
	}
	if n := h.guest("vm1").poweroffs; n != 0 {
		t.Errorf("poweroff sent to an exited VM %d times", n)
	}
	if len(h.procs.sentSignals()) != 0 {
		t.Errorf("signals sent to an exited VM: %v", h.procs.sentSignals())
	}
	if rt := h.runtimeOf("vm1"); rt.GetReason() != "QEMU exited unexpectedly" {
		t.Errorf("a stop of a crashed VM changed its reason: %q", rt.GetReason())
	}
}

func TestForgetVM(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	if _, err := h.d.ForgetVM(context.Background(), &nodev1.ForgetVMRequest{VmId: "vm1"}); !errors.Is(err, errs.ErrFailedPrecondition) {
		t.Fatalf("ForgetVM of a running VM: err = %v, want precondition", err)
	}
	if _, err := h.d.StopVM(context.Background(), &nodev1.StopVMRequest{VmId: "vm1", Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.ForgetVM(context.Background(), &nodev1.ForgetVMRequest{VmId: "vm1"}); err != nil {
		t.Fatalf("ForgetVM: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "vms", "vm1")); !os.IsNotExist(err) {
		t.Errorf("VM directory still exists: %v", err)
	}
	if rt := h.runtimeOf("vm1"); rt.GetState() != absentState {
		t.Errorf("after forget: state = %v, want ABSENT", rt.GetState())
	}
	if _, err := os.Stat(filepath.Join(h.dir, "disks", "d1.qcow2")); err != nil {
		t.Errorf("forgetting a VM removed its disk: %v", err)
	}
	if _, err := h.d.ForgetVM(context.Background(), &nodev1.ForgetVMRequest{VmId: "nobody"}); err != nil {
		t.Errorf("ForgetVM of unknown VM: %v", err)
	}
	h.start("vm1", "d1") // the VM can be created again, and it gets a port
}

func TestRejectsTraversalAndBadIDs(t *testing.T) {
	bad := []string{"", ".", "..", "../x", "a/b", "a.b", " x", strings.Repeat("a", 65)}
	for _, id := range bad {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			calls := map[string]func() error{
				"StartVM": func() error {
					_, err := h.d.StartVM(ctx, &nodev1.StartVMRequest{VmId: id, Cpus: 1, MemoryMb: 512, DiskIds: []string{"d1"}})
					return err
				},
				"GetVMRuntime": func() error {
					_, err := h.d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: id})
					return err
				},
				"StopVM": func() error {
					_, err := h.d.StopVM(ctx, &nodev1.StopVMRequest{VmId: id})
					return err
				},
				"ForgetVM": func() error {
					_, err := h.d.ForgetVM(ctx, &nodev1.ForgetVMRequest{VmId: id})
					return err
				},
				"CreateDisk": func() error {
					_, err := h.d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: id, SizeGb: 1})
					return err
				},
				"RegisterImage": func() error {
					_, err := h.d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: id, Path: "/x.qcow2"})
					return err
				},
				"DeleteDisk": func() error {
					_, err := h.d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: id})
					return err
				},
				"StartJob": func() error {
					_, err := h.d.StartJob(ctx, &nodev1.StartJobRequest{VmId: "vm1", Spec: &nodev1.JobSpec{JobId: id}})
					return err
				},
				"PutFile": func() error {
					_, err := h.d.PutFile(ctx, &nodev1.PutFileRequest{VmId: id, Path: "/x"})
					return err
				},
			}
			for name, call := range calls {
				if err := call(); !errors.Is(err, errs.ErrInvalidArgument) {
					t.Errorf("%s(%q): err = %v, want invalid argument", name, id, err)
				}
			}
			entries, _ := os.ReadDir(h.dir)
			for _, e := range entries {
				if e.Name() != "images" && e.Name() != "disks" && e.Name() != "vms" && e.Name() != "shared" {
					t.Errorf("unexpected entry %s in DataDir", e.Name())
				}
			}
		})
	}
}

func TestStartVMChownsForRunAs(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RunAs = "qemu" })
	h.mkDisk("d1")
	h.start("vm1", "d1")
	if got := h.cmd.startCalls()[0].runAs; got != "qemu" {
		t.Errorf("runAs = %q, want qemu", got)
	}
	for _, want := range []string{
		"chown qemu " + filepath.Join(h.dir, "vms", "vm1"),
		"chown qemu " + filepath.Join(h.dir, "disks", "d1.qcow2"),
	} {
		if got := h.cmd.commandsNamed(want); len(got) != 1 {
			t.Errorf("command %q ran %d times, want 1", want, len(got))
		}
	}
}

func TestConcurrentStartOfDistinctVMs(t *testing.T) {
	h := newHarness(t)
	const n = 8
	for i := range n {
		h.mkDisk(fmt.Sprintf("d%d", i))
	}
	var wg sync.WaitGroup
	errc := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("vm%d", i)
			_, err := h.d.StartVM(context.Background(), &nodev1.StartVMRequest{
				VmId: id, Name: id, Cpus: 1, MemoryMb: 512, DiskIds: []string{fmt.Sprintf("d%d", i)},
			})
			errc <- err
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			t.Errorf("StartVM: %v", err)
		}
	}
	if got := len(h.cmd.startCalls()); got != n {
		t.Errorf("Start calls = %d, want %d", got, n)
	}
	h.d.portMu.Lock()
	if len(h.d.ports) != n {
		t.Errorf("reserved ports = %d, want %d (all distinct)", len(h.d.ports), n)
	}
	h.d.portMu.Unlock()
}

func TestConcurrentObservationOfOneVM(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	h.bootGuest("vm1")
	var wg sync.WaitGroup
	states := make(chan nodev1.RuntimeState, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := h.d.GetVMRuntime(context.Background(), &nodev1.GetVMRuntimeRequest{VmId: "vm1"})
			if err != nil {
				t.Errorf("GetVMRuntime: %v", err)
				return
			}
			states <- resp.GetRuntime().GetState()
		}()
	}
	wg.Wait()
	close(states)
	for s := range states {
		if s != runningState {
			t.Errorf("state = %v, want RUNNING", s)
		}
	}
	if m := h.guest("vm1").mounts; m != 1 {
		t.Errorf("mount ran %d times under concurrency, want 1", m)
	}
}

func TestLastLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log")
	if got := lastLines(p, 3); got != "" {
		t.Errorf("missing file: %q", got)
	}
	if err := os.WriteFile(p, []byte("a\n\n b \nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lastLines(p, 2); got != "c | d" {
		t.Errorf("lastLines = %q, want %q", got, "c | d")
	}
}

func TestShellQuote(t *testing.T) {
	tests := map[string]string{
		"plain":   "'plain'",
		"":        "''",
		"it's":    `'it'"'"'s'`,
		"a b; rm": "'a b; rm'",
	}
	for in, want := range tests {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestParseMeminfo(t *testing.T) {
	total, avail, err := parseMeminfo([]byte("MemTotal: 2048 kB\nMemFree: 1 kB\nMemAvailable: 1024 kB\n"))
	if err != nil || total != 2048*1024 || avail != 1024*1024 {
		t.Errorf("parseMeminfo = %d, %d, %v", total, avail, err)
	}
}

// blockingRunner never answers until its context ends.
type blockingRunner struct{}

// Run implements guestexec.Runner.
func (blockingRunner) Run(ctx context.Context, _ string, _ []byte) ([]byte, []byte, int, error) {
	<-ctx.Done()
	return nil, nil, 0, ctx.Err()
}

func TestProbeIsBounded(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.NewRunner = func(int) guestexec.Runner { return blockingRunner{} }
	})
	start := time.Now()
	if h.d.probe(context.Background(), 1) {
		t.Fatal("probe of a hung guest reported reachable")
	}
	if elapsed := time.Since(start); elapsed > 2*probeTimeout {
		t.Errorf("probe took %v, want about %v", elapsed, probeTimeout)
	}
}
