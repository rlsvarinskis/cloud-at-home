package qemudriver

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/qemu"
)

// mountScript mounts the shared folder at /shared in the guest. It is idempotent, and it does
// not fail when the mount cannot be made.
const mountScript = `
sudo mkdir -p /shared
mountpoint -q /shared || sudo mount -t 9p -o trans=virtio,version=9p2000.L,msize=1048576 shared /shared
grep -q ' /shared ' /etc/fstab || echo 'shared /shared 9p trans=virtio,version=9p2000.L,msize=1048576,nofail 0 0' | sudo tee -a /etc/fstab >/dev/null
[ "$(stat -c %U /shared)" = agent ] || sudo chown agent:agent /shared
exit 0
`

// specRec is spec.json: what a VM was started with.
type specRec struct {
	Name     string   `json:"name"`
	CPUs     int      `json:"cpus"`
	MemoryMB int64    `json:"memory_mb"`
	DiskIDs  []string `json:"disk_ids"`
}

// runtimeRec is runtime.json: the process of the current boot of a VM.
type runtimeRec struct {
	PID       int       `json:"pid"`
	SSHPort   int       `json:"ssh_port"`
	StartedAt time.Time `json:"started_at"`
	// StopRequestedAt is set by a graceful or forced stop.
	StopRequestedAt time.Time `json:"stop_requested_at,omitzero"`
	// Mounted records that the guest's shared folder mount has been run for this boot.
	Mounted bool `json:"mounted"`
	// KillReason is set when the driver killed the process. It is the reason reported after exit.
	KillReason string `json:"kill_reason,omitempty"`
}

// observation is the runtime of a VM at one moment.
type observation struct {
	state  nodev1.RuntimeState
	reason string
	// rec is the runtime record, or nil when the VM is ABSENT.
	rec *runtimeRec
}

func (o observation) runtime() *nodev1.Runtime {
	return &nodev1.Runtime{State: o.state, Reason: o.reason}
}

// readRuntime returns the runtime record of VM id, or nil when there is none.
func (d *Driver) readRuntime(id string) (*runtimeRec, error) {
	var rec runtimeRec
	ok, err := readJSON(d.runtimePath(id), &rec)
	if err != nil || !ok {
		return nil, err
	}
	return &rec, nil
}

// alive reports whether the VM process of rec is still running.
func (d *Driver) alive(rec *runtimeRec) bool {
	return d.cfg.Procs.Alive(rec.PID, qemu.Binary())
}

// readJSON decodes the JSON file at path into v. It reports false, with no error, when the file does
// not exist.
func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	return true, nil
}

// writeJSON replaces the file at path with v encoded as JSON. The write is atomic: a temporary file
// in the same directory is renamed over the target.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(name)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// lastLines returns the last n lines of the file at path, joined with " | ". It reads at most the
// final 16 KiB, and it returns "" when the file is missing or empty.
func lastLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const window = 16 << 10
	if fi, err := f.Stat(); err == nil && fi.Size() > window {
		if _, err := f.Seek(-window, io.SeekEnd); err != nil {
			return ""
		}
	}
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// exitReason explains why the process of rec is gone.
func (d *Driver) exitReason(id string, rec *runtimeRec) string {
	switch {
	case rec.KillReason != "":
		return rec.KillReason
	case !rec.StopRequestedAt.IsZero():
		return reasonStopped
	}
	tail := lastLines(filepath.Join(d.vmDir(id), "qemu.log"), qemuLogTailLines)
	if tail == "" {
		return "QEMU exited unexpectedly"
	}
	return "QEMU exited unexpectedly: " + tail
}

// observe works out the runtime of VM id. It may probe the guest over ssh, and it may run the
// guest mount once per boot. It holds the VM's lock only while reading or writing its state.
func (d *Driver) observe(ctx context.Context, id string) (observation, error) {
	if err := checkID("vm id", id); err != nil {
		return observation{}, err
	}
	st := d.vmStateOf(id)
	st.mu.Lock()
	rec, err := d.readRuntime(id)
	if err != nil {
		st.mu.Unlock()
		return observation{}, err
	}
	if rec == nil {
		st.mu.Unlock()
		return observation{state: nodev1.RuntimeState_RUNTIME_STATE_ABSENT}, nil
	}
	now := d.cfg.Now()
	if !d.alive(rec) {
		obs := observation{state: nodev1.RuntimeState_RUNTIME_STATE_EXITED, reason: d.exitReason(id, rec), rec: rec}
		st.mu.Unlock()
		return obs, nil
	}
	if rec.KillReason != "" {
		st.mu.Unlock()
		return observation{state: nodev1.RuntimeState_RUNTIME_STATE_EXITED, reason: rec.KillReason, rec: rec}, nil
	}
	if !rec.StopRequestedAt.IsZero() && now.Sub(rec.StopRequestedAt) > d.cfg.StopGrace {
		if err := d.cfg.Procs.Signal(rec.PID, syscall.SIGKILL); err != nil && !errNotSignalled(err) {
			st.mu.Unlock()
			return observation{}, fmt.Errorf("kill vm %s (pid %d): %w", id, rec.PID, err)
		}
		rec.KillReason = reasonTimedOut
		if err := writeJSON(d.runtimePath(id), rec); err != nil {
			st.mu.Unlock()
			return observation{}, err
		}
		st.mu.Unlock()
		return observation{state: nodev1.RuntimeState_RUNTIME_STATE_EXITED, reason: reasonTimedOut, rec: rec}, nil
	}
	ok, cached := cachedProbe(st, rec.PID, now)
	st.mu.Unlock()

	if !cached {
		ok = d.probe(ctx, rec.SSHPort)
		if err := ctx.Err(); err != nil {
			return observation{}, err
		}
		st.mu.Lock()
		st.probePID, st.probeAt, st.probeOK = rec.PID, d.cfg.Now(), ok
		st.mu.Unlock()
	}
	if !ok {
		reason := ""
		if now.Sub(rec.StartedAt) > d.cfg.BootTimeout {
			reason = reasonSlowBoot
		}
		return observation{state: nodev1.RuntimeState_RUNTIME_STATE_BOOTING, reason: reason, rec: rec}, nil
	}
	return d.ensureMounted(ctx, id, st, rec)
}

// cachedProbe returns the last probe result for pid when it is recent enough. st.mu must be held.
func cachedProbe(st *vmState, pid int, now time.Time) (ok, cached bool) {
	if st.probePID != pid {
		return false, false
	}
	age := now.Sub(st.probeAt)
	if age < 0 || age >= probeCacheTTL {
		return false, false
	}
	return st.probeOK, true
}

// probe reports whether the guest answers a trivial ssh command.
func (d *Driver) probe(ctx context.Context, sshPort int) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	_, _, code, err := d.cfg.NewRunner(sshPort).Run(ctx, "true", nil)
	return err == nil && code == 0
}

// ensureMounted runs the shared folder mount once per boot, after the guest first answers ssh, and
// then reports RUNNING. A failed mount is not fatal: it is retried on a later call. The guest mount
// runs without st.mu held, and st.mounting keeps a second caller from starting another.
func (d *Driver) ensureMounted(ctx context.Context, id string, st *vmState, rec *runtimeRec) (observation, error) {
	st.mu.Lock()
	cur, err := d.readRuntime(id)
	if err != nil {
		st.mu.Unlock()
		return observation{}, err
	}
	if cur == nil {
		st.mu.Unlock()
		return observation{state: nodev1.RuntimeState_RUNTIME_STATE_ABSENT}, nil
	}
	if cur.PID != rec.PID {
		// The VM was restarted while it was probed. The new process has not been probed yet.
		st.mu.Unlock()
		return observation{state: nodev1.RuntimeState_RUNTIME_STATE_BOOTING, rec: cur}, nil
	}
	if cur.Mounted || st.mounting {
		st.mu.Unlock()
		return observation{state: nodev1.RuntimeState_RUNTIME_STATE_RUNNING, rec: cur}, nil
	}
	st.mounting = true
	st.mu.Unlock()

	mctx, cancel := context.WithTimeout(ctx, mountTimeout)
	_, _, _, merr := d.cfg.NewRunner(cur.SSHPort).Run(mctx, bashCommand(mountScript), nil)
	cancel()

	st.mu.Lock()
	defer st.mu.Unlock()
	st.mounting = false
	if merr == nil {
		cur.Mounted = true
		if err := writeJSON(d.runtimePath(id), cur); err != nil {
			return observation{}, err
		}
	}
	return observation{state: nodev1.RuntimeState_RUNTIME_STATE_RUNNING, rec: cur}, nil
}

// requireRunning returns the SSH port of VM id, or a precondition error when the VM is not RUNNING.
func (d *Driver) requireRunning(ctx context.Context, id string) (int, error) {
	obs, err := d.observe(ctx, id)
	if err != nil {
		return 0, err
	}
	if obs.state != nodev1.RuntimeState_RUNTIME_STATE_RUNNING {
		name := strings.ToLower(strings.TrimPrefix(obs.state.String(), "RUNTIME_STATE_"))
		return 0, errs.Precondition("vm %q is %s, not running", id, name)
	}
	return obs.rec.SSHPort, nil
}

// StartVM boots VM r.VmId. If the VM is already booting or running, it does nothing and returns its
// current runtime. Otherwise it writes the spec and a fresh cloud-init seed, reserves a loopback SSH
// port and starts QEMU. The result is BOOTING; poll GetVMRuntime for RUNNING.
func (d *Driver) StartVM(ctx context.Context, r *nodev1.StartVMRequest) (*nodev1.StartVMResponse, error) {
	id := r.GetVmId()
	if err := checkID("vm id", id); err != nil {
		return nil, err
	}
	name := r.GetName()
	if name == "" {
		name = id
	}
	if _, err := qemu.MetaData(id, name); err != nil {
		return nil, errs.Invalid("guest name %q: %v", name, err)
	}
	if r.GetCpus() < 1 {
		return nil, errs.Invalid("cpus must be at least 1, got %d", r.GetCpus())
	}
	if r.GetMemoryMb() < 1 {
		return nil, errs.Invalid("memory_mb must be at least 1, got %d", r.GetMemoryMb())
	}
	diskIDs := r.GetDiskIds()
	if len(diskIDs) == 0 {
		return nil, errs.Invalid("a VM needs at least one disk")
	}
	seen := make(map[string]bool, len(diskIDs))
	diskPaths := make([]string, 0, len(diskIDs))
	for _, dk := range diskIDs {
		if err := checkID("disk id", dk); err != nil {
			return nil, err
		}
		if seen[dk] {
			return nil, errs.Invalid("disk %q is attached more than once", dk)
		}
		seen[dk] = true
		diskPaths = append(diskPaths, d.diskPath(dk))
	}
	// Reject a bad request before anything is written. The port is a placeholder: the real one is
	// reserved later and is validated the same way.
	check := qemu.VMSpec{Name: name, CPUs: int(r.GetCpus()), MemoryMB: r.GetMemoryMb(),
		DiskPaths: diskPaths, SSHPort: minUserPort}
	if err := check.Validate(); err != nil {
		return nil, errs.Invalid("%v", err)
	}

	st := d.vmStateOf(id)
	st.mu.Lock()
	rec, err := d.readRuntime(id)
	if err != nil {
		st.mu.Unlock()
		return nil, err
	}
	if rec != nil && d.alive(rec) {
		st.mu.Unlock()
		obs, err := d.observe(ctx, id)
		if err != nil {
			return nil, err
		}
		return &nodev1.StartVMResponse{Runtime: obs.runtime()}, nil
	}

	keys := make([]string, 0, len(diskIDs))
	for _, dk := range diskIDs {
		keys = append(keys, diskKey(dk))
	}
	unlockDisks := d.lockKeys(keys...)
	rt, err := d.startLocked(ctx, id, name, r.GetCpus(), r.GetMemoryMb(), diskIDs)
	unlockDisks()
	st.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &nodev1.StartVMResponse{Runtime: rt}, nil
}

// startLocked boots VM id. The caller holds the VM's lock and the locks of its disks.
func (d *Driver) startLocked(ctx context.Context, id, name string, cpus int32, memMB int64, diskIDs []string) (*nodev1.Runtime, error) {
	paths := make([]string, 0, len(diskIDs))
	for _, dk := range diskIDs {
		p := d.diskPath(dk)
		if _, err := os.Stat(p); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, errs.NotFound("disk %q", dk)
			}
			return nil, fmt.Errorf("stat disk %q: %w", dk, err)
		}
		paths = append(paths, p)
	}

	dir := d.vmDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	spec := specRec{Name: name, CPUs: int(cpus), MemoryMB: memMB, DiskIDs: diskIDs}
	if err := writeJSON(filepath.Join(dir, "spec.json"), spec); err != nil {
		return nil, err
	}
	if err := d.writeSeed(ctx, id, dir, name); err != nil {
		return nil, err
	}

	if d.cfg.RunAs != "" {
		// The qemu user must write its console log in the VM directory and open each disk read-write.
		if _, err := d.cfg.Cmd.Run(ctx, "chown", d.cfg.RunAs, dir); err != nil {
			return nil, errs.Unavailable("give %s to %s: %v", dir, d.cfg.RunAs, err)
		}
		for _, p := range paths {
			if _, err := d.cfg.Cmd.Run(ctx, "chown", d.cfg.RunAs, p); err != nil {
				return nil, errs.Unavailable("give %s to %s: %v", p, d.cfg.RunAs, err)
			}
		}
	}

	port, err := d.reservePort(id)
	if err != nil {
		return nil, err
	}
	args, err := qemu.Args(qemu.VMSpec{
		Name:        name,
		CPUs:        int(cpus),
		MemoryMB:    memMB,
		DiskPaths:   paths,
		SeedISO:     filepath.Join(dir, "seed.iso"),
		SSHPort:     port,
		ConsolePath: filepath.Join(dir, "console.log"),
		SharedDir:   d.sharedDir,
		KVM:         d.cfg.KVM,
	})
	if err != nil {
		d.releasePort(id)
		return nil, errs.Invalid("%v", err)
	}
	pid, err := d.cfg.Cmd.Start(qemu.Binary(), args, filepath.Join(dir, "qemu.log"), d.cfg.RunAs)
	if err != nil {
		d.releasePort(id)
		return nil, errs.Unavailable("start %s: %v", qemu.Binary(), err)
	}
	rec := &runtimeRec{PID: pid, SSHPort: port, StartedAt: d.cfg.Now()}
	if err := writeJSON(d.runtimePath(id), rec); err != nil {
		// Without runtime state nothing could find the process again, so stop it now.
		_ = d.cfg.Procs.Signal(pid, syscall.SIGKILL)
		d.releasePort(id)
		return nil, err
	}
	return &nodev1.Runtime{State: nodev1.RuntimeState_RUNTIME_STATE_BOOTING}, nil
}

// writeSeed writes user-data and meta-data for VM id, and packs them into dir/seed.iso. Each start
// gets a new instance ID, so that cloud-init treats it as a new instance.
func (d *Driver) writeSeed(ctx context.Context, id, dir, name string) error {
	key, err := d.cfg.Keys.PublicKeyLine()
	if err != nil {
		return errs.Unavailable("%v", err)
	}
	userData, err := qemu.UserData(key, d.cfg.GuestPackages)
	if err != nil {
		return fmt.Errorf("build user-data: %w", err)
	}
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("instance id: %w", err)
	}
	metaData, err := qemu.MetaData(id+"-"+hex.EncodeToString(nonce[:]), name)
	if err != nil {
		return fmt.Errorf("build meta-data: %w", err)
	}
	userPath := filepath.Join(dir, "user-data")
	metaPath := filepath.Join(dir, "meta-data")
	if err := os.WriteFile(userPath, []byte(userData), 0o644); err != nil {
		return fmt.Errorf("write user-data: %w", err)
	}
	if err := os.WriteFile(metaPath, []byte(metaData), 0o644); err != nil {
		return fmt.Errorf("write meta-data: %w", err)
	}
	iso := filepath.Join(dir, "seed.iso")
	_, err = d.cfg.Cmd.Run(ctx, "genisoimage", "-quiet", "-output", iso, "-volid", "cidata",
		"-joliet", "-rock", userPath, metaPath)
	if err != nil {
		return errs.Unavailable("build seed ISO: %v", err)
	}
	return nil
}

// reservePort picks a loopback SSH port that no other VM holds, and records it for VM id. A VM
// that already holds a port gives it up first.
func (d *Driver) reservePort(id string) (int, error) {
	d.portMu.Lock()
	defer d.portMu.Unlock()
	d.releasePortLocked(id)
	for range 32 {
		p, err := d.cfg.FS.FreePort()
		if err != nil {
			return 0, errs.Unavailable("find a free port: %v", err)
		}
		if _, taken := d.ports[p]; !taken {
			d.ports[p] = id
			return p, nil
		}
	}
	return 0, errs.Unavailable("no free loopback port for vm %q", id)
}

// releasePort gives up the SSH port of VM id.
func (d *Driver) releasePort(id string) {
	d.portMu.Lock()
	defer d.portMu.Unlock()
	d.releasePortLocked(id)
}

func (d *Driver) releasePortLocked(id string) {
	for port, holder := range d.ports {
		if holder == id {
			delete(d.ports, port)
		}
	}
}

// GetVMRuntime reports the runtime of VM r.VmId. See Driver.observe for how the states are found.
func (d *Driver) GetVMRuntime(ctx context.Context, r *nodev1.GetVMRuntimeRequest) (*nodev1.GetVMRuntimeResponse, error) {
	obs, err := d.observe(ctx, r.GetVmId())
	if err != nil {
		return nil, err
	}
	return &nodev1.GetVMRuntimeResponse{Runtime: obs.runtime()}, nil
}

// StopVM stops VM r.VmId and returns without waiting for it to exit. With Force the process is
// killed at once. Otherwise the guest is asked to power off, and it is killed by GetVMRuntime once
// Config.StopGrace has passed. Stopping an absent or exited VM does nothing.
func (d *Driver) StopVM(ctx context.Context, r *nodev1.StopVMRequest) (*nodev1.StopVMResponse, error) {
	id := r.GetVmId()
	if err := checkID("vm id", id); err != nil {
		return nil, err
	}
	st := d.vmStateOf(id)
	st.mu.Lock()
	rec, err := d.readRuntime(id)
	if err != nil {
		st.mu.Unlock()
		return nil, err
	}
	if rec == nil || !d.alive(rec) {
		st.mu.Unlock()
		return &nodev1.StopVMResponse{}, nil
	}
	now := d.cfg.Now()
	if rec.StopRequestedAt.IsZero() {
		rec.StopRequestedAt = now
	}
	if r.GetForce() {
		if err := d.cfg.Procs.Signal(rec.PID, syscall.SIGKILL); err != nil && !errNotSignalled(err) {
			st.mu.Unlock()
			return nil, fmt.Errorf("kill vm %s (pid %d): %w", id, rec.PID, err)
		}
		if rec.KillReason == "" {
			rec.KillReason = reasonKilled
		}
	}
	if err := writeJSON(d.runtimePath(id), rec); err != nil {
		st.mu.Unlock()
		return nil, err
	}
	st.mu.Unlock()

	if !r.GetForce() {
		// The request is best effort: a guest that cannot be reached is killed after StopGrace.
		pctx, cancel := context.WithTimeout(ctx, poweroffTimeout)
		defer cancel()
		_, _, _, _ = d.cfg.NewRunner(rec.SSHPort).Run(pctx, "sudo poweroff", nil)
	}
	return &nodev1.StopVMResponse{}, nil
}

// ForgetVM removes the per-VM files of VM r.VmId: spec, runtime state, seed and logs. Disks are kept.
// It is refused while the VM process is alive. Forgetting an unknown VM does nothing.
func (d *Driver) ForgetVM(_ context.Context, r *nodev1.ForgetVMRequest) (*nodev1.ForgetVMResponse, error) {
	id := r.GetVmId()
	if err := checkID("vm id", id); err != nil {
		return nil, err
	}
	st := d.vmStateOf(id)
	st.mu.Lock()
	defer st.mu.Unlock()
	rec, err := d.readRuntime(id)
	if err != nil {
		return nil, err
	}
	if rec != nil && d.alive(rec) {
		return nil, errs.Precondition("vm %q is still running", id)
	}
	if err := os.RemoveAll(d.vmDir(id)); err != nil {
		return nil, fmt.Errorf("remove vm %s: %w", id, err)
	}
	d.releasePort(id)
	return &nodev1.ForgetVMResponse{}, nil
}

// readSpec returns spec.json of VM id, or nil when there is none.
func (d *Driver) readSpec(id string) (*specRec, error) {
	var spec specRec
	ok, err := readJSON(filepath.Join(d.vmDir(id), "spec.json"), &spec)
	if err != nil || !ok {
		return nil, err
	}
	return &spec, nil
}

// diskInUse reports whether a VM with a live process has disk id attached.
func (d *Driver) diskInUse(id string) (bool, error) {
	entries, err := os.ReadDir(d.vmsDir())
	if err != nil {
		return false, fmt.Errorf("list VMs: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || checkID("vm id", e.Name()) != nil {
			continue
		}
		spec, err := d.readSpec(e.Name())
		if err != nil {
			return false, err
		}
		if spec == nil || !slices.Contains(spec.DiskIDs, id) {
			continue
		}
		rec, err := d.readRuntime(e.Name())
		if err != nil {
			return false, err
		}
		if rec != nil && d.alive(rec) {
			return true, nil
		}
	}
	return false, nil
}

// fileExists reports whether path exists, without following a symlink.
func fileExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
