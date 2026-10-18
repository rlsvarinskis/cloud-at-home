package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

// A user whose disk or image went to ERROR must be able to see why, without asking an operator for the logs.
func TestFailureReasonsAreVisibleToClients(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ed := e.editor()

	e.node.FailNext("CreateDisk", errs.Invalid("backing image is corrupt"))
	d, err := ed.Disk.CreateDisk(ctx, connect.NewRequest(&vmdv1.CreateDiskRequest{ProjectId: "p1", Name: "doomed", SizeGb: 1}))
	if err != nil {
		t.Fatal(err)
	}
	e.tick(2)
	got, err := ed.Disk.GetDisk(ctx, connect.NewRequest(&vmdv1.GetDiskRequest{Id: d.Msg.Id}))
	if err != nil || got.Msg.State != vmdv1.DiskState_DISK_STATE_ERROR || !strings.Contains(got.Msg.StateReason, "backing image is corrupt") {
		t.Fatalf("disk: state %v reason %q err %v", got.Msg.State, got.Msg.StateReason, err)
	}

	e.node.FailNext("RegisterImage", errs.Invalid("not a qcow2 file"))
	img, err := e.root().Image.RegisterImage(ctx, connect.NewRequest(&vmdv1.RegisterImageRequest{Name: "bad", NodeId: "n1", Path: "/images/bad.qcow2"}))
	if err != nil {
		t.Fatal(err)
	}
	e.tick(2)
	gi, err := e.root().Image.GetImage(ctx, connect.NewRequest(&vmdv1.GetImageRequest{Id: img.Msg.Id}))
	if err != nil || gi.Msg.State != vmdv1.ImageState_IMAGE_STATE_ERROR || !strings.Contains(gi.Msg.StateReason, "not a qcow2 file") {
		t.Fatalf("image: state %v reason %q err %v", gi.Msg.State, gi.Msg.StateReason, err)
	}
}

// "node unreachable" means the network. A node that answered but failed (a broken host tool, a driver bug) must
// not be reported as unreachable, and its internals still must not reach the client.
func TestVMReasonDistinguishesUnreachableFromFailing(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("diagnosed")
	ed := e.editor()

	e.node.FailNext("GetVMRuntime", errs.Unavailable("dial tcp 198.51.100.3:9000: connect: connection refused"))
	e.clk.Advance(20e9)
	e.tick(1)
	if r := e.getVM(ed, vm.Id).StateReason; !strings.Contains(r, "node unreachable") || strings.Contains(r, "198.51.100.3") {
		t.Fatalf("a network failure: %q", r)
	}

	e.node.FailNext("GetVMRuntime", errors.New("genisoimage: exit status 1: /var/lib/attic/vms/x/seed.iso: No space left on device"))
	e.clk.Advance(120e9)
	e.tick(1)
	r := e.getVM(ed, vm.Id).StateReason
	if strings.Contains(r, "unreachable") || !strings.Contains(r, "failed to carry out the request") {
		t.Fatalf("a failing node is not an unreachable one: %q", r)
	}
	for _, leak := range []string{"genisoimage", "/var/lib", "No space left"} {
		if strings.Contains(r, leak) {
			t.Errorf("reason leaks %q: %s", leak, r)
		}
	}
}
