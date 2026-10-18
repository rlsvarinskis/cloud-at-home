package service_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
)

// The reference scenario for the harness: create from a shared image, run, stop, start, delete.
func TestVMLifecycleEndToEnd(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()

	img := e.sharedImage("debian")
	vm := e.createVM(ed, "p1", "web", img.Id, true)
	if vm.State != vmdv1.VMState_VM_STATE_PENDING || vm.DesiredState != vmdv1.PowerState_POWER_STATE_RUNNING || len(vm.DiskIds) != 1 {
		t.Fatalf("fresh VM: %+v", vm)
	}

	e.until(10, "running", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_RUNNING })

	if _, err := ed.VM.StopVM(ctx, connect.NewRequest(&vmdv1.StopVMRequest{Id: vm.Id})); err != nil {
		t.Fatal(err)
	}
	e.until(10, "stopped", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_STOPPED })

	if _, err := ed.VM.StartVM(ctx, connect.NewRequest(&vmdv1.StartVMRequest{Id: vm.Id})); err != nil {
		t.Fatal(err)
	}
	e.until(10, "running again", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_RUNNING })

	if _, err := ed.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: vm.Id, DeleteDisks: true})); err != nil {
		t.Fatal(err)
	}
	e.until(10, "gone", func() bool {
		_, err := ed.VM.GetVM(ctx, connect.NewRequest(&vmdv1.GetVMRequest{Id: vm.Id}))
		return code(err) == connect.CodeNotFound
	})
	e.until(10, "disk gone", func() bool {
		l, err := ed.Disk.ListDisks(ctx, connect.NewRequest(&vmdv1.ListDisksRequest{ProjectId: "p1"}))
		return err == nil && len(l.Msg.Disks) == 0
	})
}
