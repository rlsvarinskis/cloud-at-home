package qemudriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/guestexec"
)

// fakeClock is a clock that moves only when a test advances it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
}

// Now returns the current fake time.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward by d.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeProcs is a process table. Signal(SIGKILL) removes the process, as a real kill would.
type fakeProcs struct {
	mu      sync.Mutex
	alive   map[int]string
	signals []syscall.Signal
}

func newFakeProcs() *fakeProcs {
	return &fakeProcs{alive: make(map[int]string)}
}

// Alive implements Procs.
func (p *fakeProcs) Alive(pid int, contains string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	cmd, ok := p.alive[pid]
	return ok && strings.Contains(cmd, contains)
}

// Signal implements Procs. A missing process yields ESRCH.
func (p *fakeProcs) Signal(pid int, sig syscall.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.signals = append(p.signals, sig)
	if _, ok := p.alive[pid]; !ok {
		return syscall.ESRCH
	}
	if sig == syscall.SIGKILL {
		delete(p.alive, pid)
	}
	return nil
}

// crash makes the process vanish, as a QEMU crash would.
func (p *fakeProcs) crash(pid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.alive, pid)
}

// sentSignals returns the signals sent so far.
func (p *fakeProcs) sentSignals() []syscall.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]syscall.Signal(nil), p.signals...)
}

// fakeGuest is one guest reachable on a loopback SSH port.
type fakeGuest struct {
	up            bool // ssh answers
	mountFails    bool // the guest mount command fails in transport
	poweroffWorks bool // a poweroff request stops the VM process
	probes        int
	mounts        int
	poweroffs     int
	metrics       string // output of the metrics script
	metricsCode   int    // exit code of the metrics script
	pollOut       string // stdout of the job poll script
	getOut        string // content returned by the file read script
	getCode       int    // exit code of the file read script
	lastPut       []byte // stdin of the last file write
	cmds          []string
}

// fakeGuests is the set of guests, keyed by SSH port.
type fakeGuests struct {
	mu     sync.Mutex
	byPort map[int]*fakeGuest
	pidOf  map[int]int // SSH port -> pid of the VM process that forwards it
	procs  *fakeProcs
}

func newFakeGuests(procs *fakeProcs) *fakeGuests {
	return &fakeGuests{byPort: make(map[int]*fakeGuest), pidOf: make(map[int]int), procs: procs}
}

// guest returns the guest on port, creating it (down) on first use.
func (g *fakeGuests) guest(port int) *fakeGuest {
	g.mu.Lock()
	defer g.mu.Unlock()
	if gs := g.byPort[port]; gs != nil {
		return gs
	}
	gs := &fakeGuest{metrics: defaultMetricsOutput, pollOut: "running - 0 5 0 0 0 0\n\n\n"}
	g.byPort[port] = gs
	return gs
}

// setUp makes the guest on port answer ssh (or stop answering).
func (g *fakeGuests) setUp(port int, up bool) {
	gs := g.guest(port)
	g.mu.Lock()
	defer g.mu.Unlock()
	gs.up = up
}

// bind records which VM process forwards port.
func (g *fakeGuests) bind(port, pid int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pidOf[port] = pid
}

// fakeRunner runs guest commands against one fakeGuest.
type fakeRunner struct {
	g    *fakeGuests
	port int
}

var _ guestexec.Runner = fakeRunner{}

// Run implements guestexec.Runner. It recognises the scripts the driver sends by their content.
func (r fakeRunner) Run(_ context.Context, command string, stdin []byte) ([]byte, []byte, int, error) {
	gs := r.g.guest(r.port)
	r.g.mu.Lock()
	defer r.g.mu.Unlock()
	gs.cmds = append(gs.cmds, command)
	switch {
	case command == "true":
		gs.probes++
		if !gs.up {
			return nil, nil, 255, errors.New("ssh: connect to host 127.0.0.1: connection refused")
		}
		return nil, nil, 0, nil
	case strings.Contains(command, "mountpoint -q /shared"):
		if !gs.up || gs.mountFails {
			return nil, nil, 255, errors.New("ssh: connection refused")
		}
		gs.mounts++
		return nil, nil, 0, nil
	case command == "sudo poweroff":
		gs.poweroffs++
		if gs.poweroffWorks {
			if pid, ok := r.g.pidOf[r.port]; ok {
				r.g.procs.crash(pid)
			}
		}
		return nil, nil, 0, nil
	case strings.Contains(command, "===STAT1==="):
		if gs.metricsCode != 0 {
			return nil, []byte("metrics broke"), gs.metricsCode, nil
		}
		return []byte(gs.metrics), nil, 0, nil
	case strings.Contains(command, "setsid"):
		return nil, nil, 0, nil
	case strings.Contains(command, "pkill"):
		return nil, nil, 0, nil
	case strings.Contains(command, "stat -c %s"):
		return []byte(gs.pollOut), nil, 0, nil
	case strings.Contains(command, "mktemp"):
		gs.lastPut = append([]byte(nil), stdin...)
		return nil, nil, 0, nil
	case strings.Contains(command, "head -c"):
		if gs.getCode != 0 {
			return nil, nil, gs.getCode, nil
		}
		return []byte(gs.getOut), nil, 0, nil
	}
	return nil, nil, 0, nil
}

// startCall records one Start on the fakeCmd.
type startCall struct {
	name    string
	args    []string
	logPath string
	runAs   string
	pid     int
}

// fakeCmd records host commands and emulates their file effects. qemu-img info answers from sizes
// (default 4 GiB), keyed by the last two path elements such as "disks/d1.qcow2". Start creates a
// process in procs and binds its SSH port to a guest.
type fakeCmd struct {
	mu        sync.Mutex
	procs     *fakeProcs
	guests    *fakeGuests
	nextPID   int
	calls     []string
	sizes     map[string]int64
	badImages map[string]bool
	formats   map[string]string
	fail      map[string]error // substring of "name args..." -> error
	failStart bool
	chains    map[string]bool
	rules     map[string]bool
	starts    []startCall
}

func newFakeCmd(procs *fakeProcs, guests *fakeGuests) *fakeCmd {
	return &fakeCmd{
		procs:     procs,
		guests:    guests,
		nextPID:   1000,
		sizes:     make(map[string]int64),
		badImages: make(map[string]bool),
		formats:   make(map[string]string),
		fail:      make(map[string]error),
		chains:    make(map[string]bool),
		rules:     make(map[string]bool),
	}
}

// keyOf names a path for the size and format tables.
func keyOf(path string) string {
	return filepath.Base(filepath.Dir(path)) + "/" + filepath.Base(path)
}

// commandsNamed returns the recorded commands whose line starts with prefix.
func (c *fakeCmd) commandsNamed(prefix string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, line := range c.calls {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return out
}

// Run implements Commander.
func (c *fakeCmd) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, line)
	for sub, err := range c.fail {
		if strings.Contains(line, sub) {
			return nil, err
		}
	}
	switch name {
	case "qemu-img":
		return c.qemuImg(args)
	case "genisoimage":
		for i, a := range args {
			if a == "-output" && i+1 < len(args) {
				return nil, os.WriteFile(args[i+1], []byte("iso"), 0o644)
			}
		}
	case "iptables", "ip6tables":
		return nil, c.iptables(name, args)
	}
	return nil, nil
}

// qemuImg emulates qemu-img info, create, convert and resize.
func (c *fakeCmd) qemuImg(args []string) ([]byte, error) {
	switch args[0] {
	case "info":
		key := keyOf(args[len(args)-1])
		if c.badImages[key] {
			return nil, fmt.Errorf("qemu-img: Could not open: not a disk image")
		}
		size, ok := c.sizes[key]
		if !ok {
			size = 4 << 30
		}
		format := c.formats[key]
		if format == "" {
			format = "qcow2"
		}
		return json.Marshal(map[string]any{"virtual-size": size, "actual-size": size / 4, "format": format})
	case "create", "convert":
		path := args[len(args)-1]
		if args[0] == "create" && strings.HasSuffix(path, "G") {
			path = args[len(args)-2]
		}
		return nil, os.WriteFile(path, []byte("fake image"), 0o644)
	case "resize":
		return nil, nil
	}
	return nil, nil
}

// iptables emulates the chain and rule commands the firewall uses.
func (c *fakeCmd) iptables(tool string, args []string) error {
	key := func(a []string) string { return tool + " " + strings.Join(a, " ") }
	switch args[0] {
	case "-nL":
		if !c.chains[args[1]] {
			return errors.New("iptables: No chain/target/match by that name")
		}
	case "-N":
		c.chains[args[1]] = true
	case "-F":
		if !c.chains[args[1]] {
			return errors.New("iptables: no such chain")
		}
		for r := range c.rules {
			if strings.HasPrefix(r, tool+" -A "+args[1]+" ") {
				delete(c.rules, r)
			}
		}
	case "-A":
		c.rules[key(args)] = true
	case "-C":
		if !c.rules[key(append([]string{"-A"}, args[1:]...))] {
			return errors.New("iptables: Bad rule (does a matching rule exist in that chain?)")
		}
	}
	return nil
}

// Start implements Commander. It creates a live process whose command line names the binary, and
// binds the SSH port from the -netdev argument to it.
func (c *fakeCmd) Start(name string, args []string, logPath, runAs string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failStart {
		return 0, errors.New("exec: no such file")
	}
	pid := c.nextPID
	c.nextPID++
	cmdline := name + " " + strings.Join(args, " ")
	c.procs.mu.Lock()
	c.procs.alive[pid] = cmdline
	c.procs.mu.Unlock()

	if m := hostfwdRE.FindStringSubmatch(strings.Join(args, " ")); m != nil {
		var port int
		fmt.Sscan(m[1], &port)
		c.guests.bind(port, pid)
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	f.Close()
	c.starts = append(c.starts, startCall{name: name, args: args, logPath: logPath, runAs: runAs, pid: pid})
	return pid, nil
}

var hostfwdRE = regexp.MustCompile(`hostfwd=tcp:127\.0\.0\.1:(\d+)-:22`)

// startCalls returns the recorded Start calls.
func (c *fakeCmd) startCalls() []startCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]startCall(nil), c.starts...)
}

// fakeFS answers host facts from fixed values.
type fakeFS struct {
	mu       sync.Mutex
	meminfo  string
	total    int64
	avail    int64
	cpus     int
	nextPort int
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		meminfo:  "MemTotal:       16384 kB\nMemFree:         1024 kB\nMemAvailable:    8192 kB\n",
		total:    100 << 30,
		avail:    40 << 30,
		cpus:     8,
		nextPort: 40000,
	}
}

// ReadFile implements system.
func (f *fakeFS) ReadFile(name string) ([]byte, error) {
	if name != memInfoPath {
		return nil, fmt.Errorf("fakefs: no %s", name)
	}
	return []byte(f.meminfo), nil
}

// Statfs implements system.
func (f *fakeFS) Statfs(string) (int64, int64, error) {
	return f.total, f.avail, nil
}

// NumCPU implements system.
func (f *fakeFS) NumCPU() int { return f.cpus }

// FreePort implements system. Ports count up from 40000.
func (f *fakeFS) FreePort() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.nextPort
	f.nextPort++
	return p, nil
}

// fixedKeys is a KeySource that returns one key line.
type fixedKeys string

// PublicKeyLine implements KeySource.
func (k fixedKeys) PublicKeyLine() (string, error) { return string(k), nil }

// harness wires a Driver to fakes and a temp DataDir.
type harness struct {
	t      *testing.T
	dir    string
	clock  *fakeClock
	procs  *fakeProcs
	guests *fakeGuests
	cmd    *fakeCmd
	fs     *fakeFS
	d      *Driver
}

// newHarness returns a harness with a driver built from the default test config.
func newHarness(t *testing.T, opts ...func(*Config)) *harness {
	t.Helper()
	procs := newFakeProcs()
	guests := newFakeGuests(procs)
	h := &harness{
		t:      t,
		dir:    t.TempDir(),
		clock:  newFakeClock(),
		procs:  procs,
		guests: guests,
		cmd:    newFakeCmd(procs, guests),
		fs:     newFakeFS(),
	}
	h.d = h.open(opts...)
	return h
}

// config returns the default test configuration.
func (h *harness) config(opts ...func(*Config)) Config {
	c := Config{
		DataDir:     h.dir,
		Keys:        fixedKeys("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest agent@test"),
		Cmd:         h.cmd,
		Procs:       h.procs,
		FS:          h.fs,
		Now:         h.clock.Now,
		NewRunner:   func(port int) guestexec.Runner { return fakeRunner{g: h.guests, port: port} },
		StopGrace:   60 * time.Second,
		BootTimeout: 5 * time.Minute,
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// open builds a Driver on h's DataDir. It may be called again to simulate a daemon restart.
func (h *harness) open(opts ...func(*Config)) *Driver {
	h.t.Helper()
	d, err := New(h.config(opts...))
	if err != nil {
		h.t.Fatalf("New: %v", err)
	}
	return d
}

// mkDisk creates a disk and fails the test on error.
func (h *harness) mkDisk(id string) {
	h.t.Helper()
	if _, err := h.d.CreateDisk(context.Background(), &nodev1.CreateDiskRequest{DiskId: id, SizeGb: 10}); err != nil {
		h.t.Fatalf("CreateDisk %s: %v", id, err)
	}
}

// start boots VM id with the given disks, 2 vCPUs and 1024 MB.
func (h *harness) start(id string, disks ...string) *nodev1.StartVMResponse {
	h.t.Helper()
	resp, err := h.d.StartVM(context.Background(), &nodev1.StartVMRequest{
		VmId: id, Name: id, Cpus: 2, MemoryMb: 1024, DiskIds: disks,
	})
	if err != nil {
		h.t.Fatalf("StartVM %s: %v", id, err)
	}
	return resp
}

// runtimeOf returns the driver's view of VM id.
func (h *harness) runtimeOf(id string) *nodev1.Runtime {
	h.t.Helper()
	resp, err := h.d.GetVMRuntime(context.Background(), &nodev1.GetVMRuntimeRequest{VmId: id})
	if err != nil {
		h.t.Fatalf("GetVMRuntime %s: %v", id, err)
	}
	return resp.GetRuntime()
}

// port returns the SSH port of VM id from its runtime file.
func (h *harness) port(id string) int {
	h.t.Helper()
	rec, err := h.d.readRuntime(id)
	if err != nil || rec == nil {
		h.t.Fatalf("no runtime for %s: %v", id, err)
	}
	return rec.SSHPort
}

// pid returns the process ID of VM id from its runtime file.
func (h *harness) pid(id string) int {
	h.t.Helper()
	rec, err := h.d.readRuntime(id)
	if err != nil || rec == nil {
		h.t.Fatalf("no runtime for %s: %v", id, err)
	}
	return rec.PID
}

// guest returns the fake guest of VM id.
func (h *harness) guest(id string) *fakeGuest {
	h.t.Helper()
	return h.guests.guest(h.port(id))
}

// bootGuest makes the guest of VM id answer ssh. It also advances the clock past the probe cache, so
// that the next observation probes again instead of reusing a failed probe.
func (h *harness) bootGuest(id string) {
	h.guests.setUp(h.port(id), true)
	h.clock.Advance(2 * time.Second)
}

// defaultMetricsOutput is a realistic guest metrics script output: 8 vCPUs and two disks.
const defaultMetricsOutput = `===STAT1===
cpu  840102 510 493115 89766099 512124 0 64833 0 0 0
===STAT2===
cpu  840602 510 493215 89766999 512124 0 64853 0 0 0
===MEMINFO===
MemTotal:       16318020 kB
MemFree:         2214448 kB
MemAvailable:   11925468 kB
SwapTotal:       2097148 kB
SwapFree:        2097148 kB
===LOADAVG===
0.52 0.58 0.59 2/345 12345
===UPTIME===
123456.78 987654.32
===NPROC===
8
===DF===
Filesystem        1B-blocks        Used Mounted on
/dev/vda1      10726932480  4294967296 /
/dev/vdb1      21474836480  1073741824 /srv/data
===END===
`
