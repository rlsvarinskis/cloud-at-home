// Package qemudriver implements node.Driver for a Linux host that runs virtual machines with
// QEMU/KVM. Images and disks are qcow2 files under Config.DataDir. Each VM is a
// qemu-system-x86_64 process in its own session, with a cloud-init seed ISO, a loopback SSH port
// forward and a 9p share. Guests are driven over ssh through Config.NewRunner.
//
// All durable state lives under DataDir:
//
//	images/<id>.qcow2      a symlink to a registered image, or a flattened snapshot
//	disks/<id>.qcow2       VM disks, optionally overlays on an image
//	shared/                the folder mounted at /shared in every guest
//	vms/<id>/spec.json     what the VM was started with
//	vms/<id>/runtime.json  pid, SSH port, start time, stop request, mount state
//	vms/<id>/seed.iso, user-data, meta-data, console.log, qemu.log
//
// The VM process is detached, so it survives a restart of the daemon. A new Driver built on the
// same DataDir adopts the VMs that are still running.
//
// Locking: each VM has a mutex that guards its runtime.json and its probe cache. It is never held
// across an ssh probe or a guest command. Each image and disk ID has its own lock. StartVM holds
// its disks' locks for the whole start, so a disk cannot be deleted between the check and the
// boot. The lock order is VM, then disks (sorted), then image.
package qemudriver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/guestexec"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/node"
)

var _ node.Driver = (*Driver)(nil)

// Commander runs host commands (qemu-img, genisoimage, iptables, chown).
type Commander interface {
	// Run runs name with args and returns its standard output. On failure the error includes
	// the command's stderr.
	Run(ctx context.Context, name string, args ...string) (stdout []byte, err error)
	// Start launches a long-running process detached in its own session, so it survives this
	// daemon. Its standard output and error are appended to logPath. It runs as the unprivileged
	// user runAs unless runAs is empty. It returns the process ID. The implementation reaps the
	// child when it exits.
	Start(name string, args []string, logPath, runAs string) (pid int, err error)
}

// Procs gives access to the process table.
type Procs interface {
	// Alive reports whether pid exists, is not a zombie, and its command line contains
	// cmdlineContains.
	Alive(pid int, cmdlineContains string) bool
	// Signal sends sig to pid. It returns syscall.ESRCH when there is no such process.
	Signal(pid int, sig syscall.Signal) error
}

// KeySource supplies the SSH public key that cloud-init installs for the guest user.
type KeySource interface {
	// PublicKeyLine returns one OpenSSH public key line, such as "ssh-ed25519 AAAA... comment".
	PublicKeyLine() (string, error)
}

// system is the part of the host that cannot be read through plain os calls in tests.
type system interface {
	// ReadFile reads a host file, such as /proc/meminfo.
	ReadFile(name string) ([]byte, error)
	// Statfs returns the total and the unprivileged-available bytes of the file system at path.
	Statfs(path string) (total, avail int64, err error)
	// NumCPU returns the number of CPUs usable by this process.
	NumCPU() int
	// FreePort returns a TCP port that is free on 127.0.0.1.
	FreePort() (int, error)
}

// Config configures a Driver. DataDir, NewRunner and a key (Keys or SSHKeyPath) are required. Every
// other zero value selects the production behaviour.
type Config struct {
	// NodeID identifies this host. It is informational only.
	NodeID string
	// DataDir is the absolute root of all state.
	DataDir string
	// SharedDir is the absolute folder exported to every guest at /shared. Empty means DataDir/shared.
	SharedDir string
	// SSHKeyPath is the client private key. The public key is read from SSHKeyPath+".pub". It is
	// used only when Keys is nil.
	SSHKeyPath string
	// Keys supplies the public key line for cloud-init. It takes precedence over SSHKeyPath.
	Keys KeySource
	// KVM selects hardware acceleration for guests.
	KVM bool
	// RunAs is the unprivileged user that qemu processes and the shared folder belong to. Empty
	// means the current user.
	RunAs string
	// GuestPackages are installed by cloud-init in every guest.
	GuestPackages []string
	// StopGrace is how long a VM may take to power off after a graceful stop before it is killed.
	// Zero means 60 seconds.
	StopGrace time.Duration
	// BootTimeout is how long a VM may stay unreachable over ssh before it is reported as
	// "slow boot". Zero means 5 minutes.
	BootTimeout time.Duration
	// Cmd runs host commands. Nil means os/exec.
	Cmd Commander
	// Procs inspects and signals processes. Nil means /proc and kill(2).
	Procs Procs
	// NewRunner returns a runner for the guest whose SSH port is sshPort on 127.0.0.1. Required.
	NewRunner func(sshPort int) guestexec.Runner
	// FS gives access to host facts. Nil means the real host.
	FS system
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

const (
	gib = 1 << 30
	mib = 1 << 20

	// probeTimeout bounds one ssh reachability probe.
	probeTimeout = 3 * time.Second
	// probeCacheTTL is how long a probe result is reused.
	probeCacheTTL = time.Second
	// mountTimeout bounds the one-time guest mount.
	mountTimeout = 30 * time.Second
	// poweroffTimeout bounds the request to power off a guest.
	poweroffTimeout = 10 * time.Second
	// metricsTimeout bounds one metrics collection.
	metricsTimeout = 30 * time.Second
	// qemuLogTailLines is how many trailing qemu.log lines explain a crash.
	qemuLogTailLines = 3

	// minUserPort is the lowest loopback SSH port the QEMU argument validation accepts.
	minUserPort = 1024

	reasonStopped  = "stopped"
	reasonKilled   = "killed"
	reasonTimedOut = "stop timed out, killed"
	reasonSlowBoot = "slow boot"
)

// Driver is the QEMU node driver. Create one with New. It is safe for concurrent use.
type Driver struct {
	cfg       Config
	dataDir   string
	sharedDir string

	// mu guards vms and locks.
	mu    sync.Mutex
	vms   map[string]*vmState    // per-VM state, keyed by VM ID
	locks map[string]*sync.Mutex // per image or disk lock, keyed by imageKey or diskKey

	// portMu guards ports.
	portMu sync.Mutex
	ports  map[int]string // loopback SSH port -> the VM holding it
}

var idRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// checkID rejects IDs that are not safe as file names. kind names the ID in the error.
func checkID(kind, id string) error {
	if !idRE.MatchString(id) {
		return errs.Invalid("%s %q must match ^[a-zA-Z0-9_-]{1,64}$", kind, id)
	}
	return nil
}

// New returns a Driver for the host described by c. It creates the directory layout, fixes the
// ownership of the shared folder when RunAs is set, and adopts the VMs whose processes are still
// alive. It does not start or stop anything.
func New(c Config) (*Driver, error) {
	if !filepath.IsAbs(c.DataDir) {
		return nil, fmt.Errorf("qemudriver: DataDir must be an absolute path, got %q", c.DataDir)
	}
	c.DataDir = filepath.Clean(c.DataDir)
	if c.NewRunner == nil {
		return nil, errors.New("qemudriver: NewRunner is required")
	}
	if c.Keys == nil {
		if c.SSHKeyPath == "" {
			return nil, errors.New("qemudriver: an SSH key is required (Keys or SSHKeyPath)")
		}
		c.Keys = fileKeySource{path: c.SSHKeyPath + ".pub"}
	}
	if c.SharedDir == "" {
		c.SharedDir = filepath.Join(c.DataDir, "shared")
	}
	if !filepath.IsAbs(c.SharedDir) {
		return nil, fmt.Errorf("qemudriver: SharedDir must be an absolute path, got %q", c.SharedDir)
	}
	c.SharedDir = filepath.Clean(c.SharedDir)
	if c.StopGrace <= 0 {
		c.StopGrace = 60 * time.Second
	}
	if c.BootTimeout <= 0 {
		c.BootTimeout = 5 * time.Minute
	}
	if c.Cmd == nil {
		c.Cmd = osCommander{}
	}
	if c.Procs == nil {
		c.Procs = osProcs{}
	}
	if c.FS == nil {
		c.FS = osSystem{}
	}
	if c.Now == nil {
		c.Now = time.Now
	}

	d := &Driver{
		cfg:       c,
		dataDir:   c.DataDir,
		sharedDir: c.SharedDir,
		vms:       make(map[string]*vmState),
		locks:     make(map[string]*sync.Mutex),
		ports:     make(map[int]string),
	}
	for _, dir := range []string{d.imagesDir(), d.disksDir(), d.vmsDir(), d.sharedDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("qemudriver: create %s: %w", dir, err)
		}
	}
	if c.RunAs != "" {
		if _, err := c.Cmd.Run(context.Background(), "chown", c.RunAs, c.SharedDir); err != nil {
			return nil, fmt.Errorf("qemudriver: give %s to %s: %w", c.SharedDir, c.RunAs, err)
		}
	}
	if err := d.adopt(); err != nil {
		return nil, err
	}
	return d, nil
}

// adopt reserves the SSH ports of VMs whose processes are still alive, so that new VMs never pick
// one of them. A VM with unreadable runtime state is skipped: it is reported when it is queried.
func (d *Driver) adopt() error {
	entries, err := os.ReadDir(d.vmsDir())
	if err != nil {
		return fmt.Errorf("qemudriver: list VMs: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || checkID("vm id", e.Name()) != nil {
			continue
		}
		rec, err := d.readRuntime(e.Name())
		if err != nil || rec == nil || !d.alive(rec) {
			continue
		}
		d.ports[rec.SSHPort] = e.Name()
	}
	return nil
}

// Paths. Callers must validate IDs with checkID first.

func (d *Driver) imagesDir() string { return filepath.Join(d.dataDir, "images") }
func (d *Driver) disksDir() string  { return filepath.Join(d.dataDir, "disks") }
func (d *Driver) vmsDir() string    { return filepath.Join(d.dataDir, "vms") }

func (d *Driver) imagePath(id string) string {
	return filepath.Join(d.imagesDir(), id+".qcow2")
}

func (d *Driver) diskPath(id string) string {
	return filepath.Join(d.disksDir(), id+".qcow2")
}

func (d *Driver) vmDir(id string) string {
	return filepath.Join(d.vmsDir(), id)
}

func (d *Driver) runtimePath(id string) string {
	return filepath.Join(d.vmDir(id), "runtime.json")
}

// vmState is the in-memory state of one VM. Its mutex guards the VM's runtime.json and the
// fields below. The mutex is never held across an ssh probe or a guest command.
type vmState struct {
	mu sync.Mutex
	// probePID, probeAt and probeOK hold the last ssh probe of the process probePID.
	probePID int
	probeAt  time.Time
	probeOK  bool
	// mounting is set while a guest mount is in progress, so that only one runs at a time.
	mounting bool
}

// vmStateOf returns the state of VM id, creating it on first use.
func (d *Driver) vmStateOf(id string) *vmState {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.vms[id]
	if st == nil {
		st = &vmState{}
		d.vms[id] = st
	}
	return st
}

// lockKey locks the lock named key and returns the function that unlocks it. Keys are
// "image:<id>" and "disk:<id>".
func (d *Driver) lockKey(key string) func() {
	d.mu.Lock()
	m := d.locks[key]
	if m == nil {
		m = new(sync.Mutex)
		d.locks[key] = m
	}
	d.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// lockKeys locks the named keys in sorted order, so that concurrent callers cannot deadlock. It
// returns the function that unlocks them all.
func (d *Driver) lockKeys(keys ...string) func() {
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	unlocks := make([]func(), 0, len(sorted))
	for _, k := range sorted {
		unlocks = append(unlocks, d.lockKey(k))
	}
	return func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
}

func imageKey(id string) string { return "image:" + id }
func diskKey(id string) string  { return "disk:" + id }

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// bashCommand returns a guest command that runs script with bash -c.
func bashCommand(script string) string {
	return "bash -c " + shellQuote(script)
}

// exec returns the guest job runner for the guest on sshPort.
func (d *Driver) exec(sshPort int) *guestexec.Exec {
	return &guestexec.Exec{R: d.cfg.NewRunner(sshPort)}
}

// fileKeySource reads a public key line from a file.
type fileKeySource struct {
	path string
}

// PublicKeyLine implements KeySource.
func (f fileKeySource) PublicKeyLine() (string, error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return "", fmt.Errorf("read SSH public key: %w", err)
	}
	line := strings.TrimSpace(string(b))
	if line == "" {
		return "", fmt.Errorf("SSH public key %s is empty", f.path)
	}
	return line, nil
}

// errNotSignalled reports whether err means the process was already gone.
func errNotSignalled(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}
