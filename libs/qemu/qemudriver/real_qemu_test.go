package qemudriver

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/guestexec"
)

// isoCommander runs real host commands but builds ISOs with xorriso when genisoimage is not installed
// (`xorriso -as mkisofs` takes the same options).
type isoCommander struct{ Commander }

func (c isoCommander) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "genisoimage" {
		if _, err := exec.LookPath("genisoimage"); err != nil {
			return c.Commander.Run(ctx, "xorriso", append([]string{"-as", "mkisofs"}, args...)...)
		}
	}
	return c.Commander.Run(ctx, name, args...)
}

// noGuest is what an SSH runner does when nothing answers on the forwarded port.
type noGuest struct{}

func (noGuest) Run(context.Context, string, []byte) ([]byte, []byte, int, error) {
	return nil, nil, 0, errs.Unavailable("no guest is answering")
}

func kvmUsable() bool {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// Real QEMU with KVM must accept the driver's generated command line and the driver must supervise the process
// correctly. There is no operating system on the disk, so the guest never answers over SSH: the VM stays BOOTING.
// What this proves is everything on the host side of a boot. Skipped where qemu, KVM or an ISO builder is missing.
func TestRealQEMUAcceptsTheGeneratedCommandLine(t *testing.T) {
	for _, tool := range []string{"qemu-system-x86_64", "qemu-img"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	if _, err := exec.LookPath("genisoimage"); err != nil {
		if _, err := exec.LookPath("xorriso"); err != nil {
			t.Skip("neither genisoimage nor xorriso is installed")
		}
	}
	if !kvmUsable() {
		t.Skip("/dev/kvm is not usable by this user")
	}
	ctx := context.Background()
	data := t.TempDir()
	d, err := New(Config{
		DataDir: data, KVM: true, Keys: fixedKeys("ssh-ed25519 AAAATEST test"),
		Cmd:       isoCommander{OSCommander()},
		NewRunner: func(int) guestexec.Runner { return noGuest{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "boot", SizeGb: 1}); err != nil {
		t.Fatal(err)
	}

	res, err := d.StartVM(ctx, &nodev1.StartVMRequest{VmId: "vm1", Name: "realvm", Cpus: 2, MemoryMb: 256, DiskIds: []string{"boot"}})
	if err != nil {
		t.Fatalf("StartVM: %v", err)
	}
	if st := res.Runtime.State; st != nodev1.RuntimeState_RUNTIME_STATE_BOOTING {
		t.Fatalf("a fresh start is BOOTING, got %v", st)
	}
	t.Cleanup(func() { d.StopVM(ctx, &nodev1.StopVMRequest{VmId: "vm1", Force: true}) })

	// Give QEMU a moment: if it rejected any option it would exit immediately.
	time.Sleep(1500 * time.Millisecond)
	rt, err := d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm1"})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Runtime.State != nodev1.RuntimeState_RUNTIME_STATE_BOOTING {
		qlog, _ := os.ReadFile(filepath.Join(data, "vms", "vm1", "qemu.log"))
		t.Fatalf("QEMU did not stay up: state %v reason %q\nqemu.log:\n%s", rt.Runtime.State, rt.Runtime.Reason, qlog)
	}

	// The SSH port forward is real: QEMU's user-mode network listens on 127.0.0.1:<port> even with no guest behind it.
	rec, err := d.readRuntime("vm1")
	if err != nil || rec == nil {
		t.Fatalf("runtime record: %v %v", rec, err)
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", rec.SSHPort), 3*time.Second)
	if err != nil {
		t.Fatalf("the forwarded SSH port %d is not listening: %v", rec.SSHPort, err)
	}
	conn.Close()
	// ...and only on loopback.
	if out, err := exec.Command("ss", "-ltn").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, fmt.Sprintf(":%d ", rec.SSHPort)) && !strings.Contains(line, "127.0.0.1") {
				t.Errorf("the SSH forward must listen on loopback only: %s", line)
			}
		}
	}

	// The cloud-init seed is a real ISO labelled cidata holding user-data and meta-data.
	listing, err := exec.Command("xorriso", "-indev", filepath.Join(data, "vms", "vm1", "seed.iso"), "-ls").CombinedOutput()
	if err == nil {
		for _, want := range []string{"user-data", "meta-data"} {
			if !strings.Contains(string(listing), want) {
				t.Errorf("seed.iso lacks %s:\n%s", want, listing)
			}
		}
	}

	// The VM survives the driver: a second driver on the same data directory adopts the running process.
	d2, err := New(Config{DataDir: data, KVM: true, Keys: fixedKeys("ssh-ed25519 AAAATEST test"),
		Cmd: isoCommander{OSCommander()}, NewRunner: func(int) guestexec.Runner { return noGuest{} }})
	if err != nil {
		t.Fatal(err)
	}
	rt2, err := d2.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm1"})
	if err != nil || rt2.Runtime.State != nodev1.RuntimeState_RUNTIME_STATE_BOOTING {
		t.Fatalf("a restarted daemon must re-adopt the running VM: %v %v", rt2, err)
	}

	// Force stop kills the process for real.
	if _, err := d2.StopVM(ctx, &nodev1.StopVMRequest{VmId: "vm1", Force: true}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := d2.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm1"})
		if err == nil && r.Runtime.State == nodev1.RuntimeState_RUNTIME_STATE_EXITED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the VM did not stop: %v %v", r, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if out, _ := exec.Command("pgrep", "-f", filepath.Join(data, "disks", "boot.qcow2")).Output(); len(strings.TrimSpace(string(out))) > 0 {
		t.Errorf("a qemu process is still running after a forced stop: %s", out)
	}
}
