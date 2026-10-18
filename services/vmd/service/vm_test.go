package service_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1/vmdv1connect"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/fake"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/service"
)

// vmtAllowed marks an authorization-matrix cell where the call must succeed. connect has no CodeOK,
// and 0 is not a valid connect.Code, so it cannot collide with a real error code.
const vmtAllowed connect.Code = 0

// ------------------------------------------------------------------------------------ helpers

func vmtCreateReq(project, name, imageID string) *vmdv1.CreateVMRequest {
	return &vmdv1.CreateVMRequest{ProjectId: project, Name: name, Cpus: 2, MemoryMb: 2048, DiskSizeGb: 10,
		Boot: &vmdv1.CreateVMRequest_ImageId{ImageId: imageID}}
}

func vmtInt32(v int32) *int32 { return &v }
func vmtInt64(v int64) *int64 { return &v }

func vmtMustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func vmtWantCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want code %v", want)
	}
	if got := code(err); got != want {
		t.Fatalf("code = %v (%v), want %v", got, err, want)
	}
}

func vmtCreate(t *testing.T, c *clients, req *vmdv1.CreateVMRequest) *vmdv1.VM {
	t.Helper()
	res, err := c.VM.CreateVM(context.Background(), connect.NewRequest(req))
	vmtMustOK(t, err)
	return res.Msg
}

func vmtDisk(t *testing.T, c *clients, id string) *vmdv1.Disk {
	t.Helper()
	res, err := c.Disk.GetDisk(context.Background(), connect.NewRequest(&vmdv1.GetDiskRequest{Id: id}))
	vmtMustOK(t, err)
	return res.Msg
}

func vmtListDisks(t *testing.T, c *clients, project string) []*vmdv1.Disk {
	t.Helper()
	res, err := c.Disk.ListDisks(context.Background(), connect.NewRequest(&vmdv1.ListDisksRequest{ProjectId: project}))
	vmtMustOK(t, err)
	return res.Msg.Disks
}

// vmtListAll follows next_page_token until the listing ends.
func vmtListAll(t *testing.T, c *clients, project string, sel map[string]string) []*vmdv1.VM {
	t.Helper()
	var out []*vmdv1.VM
	token := ""
	for range 100 {
		res, err := c.VM.ListVMs(context.Background(), connect.NewRequest(&vmdv1.ListVMsRequest{
			ProjectId: project, PageToken: token, LabelSelector: sel}))
		vmtMustOK(t, err)
		out = append(out, res.Msg.Vms...)
		if res.Msg.NextPageToken == "" {
			return out
		}
		token = res.Msg.NextPageToken
	}
	t.Fatal("listing did not terminate")
	return nil
}

// vmtDrive runs the reconciler in the background (for long-poll tests) until stop is closed.
func vmtDrive(e *env, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		e.rec.Tick(context.Background())
		e.clk.Advance(time.Second)
		time.Sleep(20 * time.Millisecond)
	}
}

// ------------------------------------------------------------------------------------ validation

func TestVMValidationName(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	bad := map[string]string{
		"uppercase":       "Web",
		"leading digit":   "1web",
		"underscore":      "web_1",
		"leading hyphen":  "-web",
		"trailing hyphen": "web-",
		"dot":             "web.1",
		"too long (64)":   strings.Repeat("a", 64),
		"empty":           "",
	}
	for name, n := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := ed.VM.CreateVM(ctx, connect.NewRequest(vmtCreateReq("p1", n, img.Id)))
			vmtWantCode(t, err, connect.CodeInvalidArgument)
		})
	}
	for _, n := range []string{"a", "web-1", strings.Repeat("a", 63)} {
		t.Run(fmt.Sprintf("valid length %d", len(n)), func(t *testing.T) {
			vm := vmtCreate(t, ed, vmtCreateReq("p1", n, img.Id))
			if vm.Name != n {
				t.Fatalf("name = %q, want %q", vm.Name, n)
			}
		})
	}
}

func TestVMValidationSpec(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	seq := 0
	try := func(cpus int32, mem int64) error {
		seq++
		req := vmtCreateReq("p1", fmt.Sprintf("spec-%d", seq), img.Id)
		req.Cpus, req.MemoryMb = cpus, mem
		_, err := ed.VM.CreateVM(ctx, connect.NewRequest(req))
		return err
	}
	for _, cpus := range []int32{0, -1, 65} {
		vmtWantCode(t, try(cpus, 2048), connect.CodeInvalidArgument)
	}
	for _, cpus := range []int32{1, 64} { // default MaxVMCPUs is 64
		vmtMustOK(t, try(cpus, 2048))
	}
	for _, mem := range []int64{0, 255, (1 << 20) + 1} { // default range is [256, 1 MiB]
		vmtWantCode(t, try(2, mem), connect.CodeInvalidArgument)
	}
	for _, mem := range []int64{256, 1 << 20} {
		vmtMustOK(t, try(2, mem))
	}
}

func TestVMValidationProjectAndBoot(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	for _, p := range []string{"", "*"} {
		_, err := ed.VM.CreateVM(ctx, connect.NewRequest(vmtCreateReq(p, "web", img.Id)))
		vmtWantCode(t, err, connect.CodeInvalidArgument)
	}

	noBoot := &vmdv1.CreateVMRequest{ProjectId: "p1", Name: "noboot", Cpus: 2, MemoryMb: 2048}
	_, err := ed.VM.CreateVM(ctx, connect.NewRequest(noBoot))
	vmtWantCode(t, err, connect.CodeInvalidArgument)

	unknown := vmtCreateReq("p1", "ghost", "img_does_not_exist")
	_, err = ed.VM.CreateVM(ctx, connect.NewRequest(unknown))
	vmtWantCode(t, err, connect.CodeNotFound)
}

func TestVMValidationImageVisibility(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p2admin := e.as("user:admin-p2", bind("p2", authz.RoleAdmin))

	// A project-owned image of p2, made ready so the only reason p1 can be refused is the project boundary.
	img, err := p2admin.Image.RegisterImage(ctx, connect.NewRequest(&vmdv1.RegisterImageRequest{
		ProjectId: "p2", Name: "p2img", NodeId: "n1", Path: "/images/p2img.qcow2"}))
	vmtMustOK(t, err)
	e.until(5, "p2 image ready", func() bool {
		i, err := p2admin.Image.GetImage(ctx, connect.NewRequest(&vmdv1.GetImageRequest{Id: img.Msg.Id}))
		return err == nil && i.Msg.State == vmdv1.ImageState_IMAGE_STATE_READY
	})

	_, err = e.editor().VM.CreateVM(ctx, connect.NewRequest(vmtCreateReq("p1", "steal", img.Msg.Id)))
	vmtWantCode(t, err, connect.CodeNotFound)

	// The owning project can use it.
	own := vmtCreate(t, p2admin, vmtCreateReq("p2", "own", img.Msg.Id))
	if own.ProjectId != "p2" {
		t.Fatalf("project = %q, want p2", own.ProjectId)
	}
}

func TestVMValidationImageNotReady(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Registered but never reconciled (no ticks): the image is still CREATING.
	img, err := e.root().Image.RegisterImage(ctx, connect.NewRequest(&vmdv1.RegisterImageRequest{
		Name: "pending", NodeId: "n1", Path: "/images/pending.qcow2"}))
	vmtMustOK(t, err)
	_, err = e.editor().VM.CreateVM(ctx, connect.NewRequest(vmtCreateReq("p1", "early", img.Msg.Id)))
	vmtWantCode(t, err, connect.CodeFailedPrecondition)
}

func TestVMValidationDiskSize(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	seq := 0
	try := func(size int32) (*vmdv1.VM, error) {
		seq++
		req := vmtCreateReq("p1", fmt.Sprintf("disk-%d", seq), img.Id)
		req.DiskSizeGb = size
		res, err := ed.VM.CreateVM(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	for _, size := range []int32{-1, 4097} { // default MaxDiskSizeGB is 4096
		_, err := try(size)
		vmtWantCode(t, err, connect.CodeInvalidArgument)
	}
	for _, size := range []int32{1, 4096} {
		vm, err := try(size)
		vmtMustOK(t, err)
		d := vmtDisk(t, ed, vm.DiskIds[0])
		if d.SizeGb != size {
			t.Fatalf("boot disk size = %d, want %d", d.SizeGb, size)
		}
	}
	// 0 means "use the default" (DefaultDiskGB, 20 GB).
	vm, err := try(0)
	vmtMustOK(t, err)
	if d := vmtDisk(t, ed, vm.DiskIds[0]); d.SizeGb != 20 {
		t.Fatalf("default boot disk size = %d, want 20", d.SizeGb)
	}
}

func TestVMBootFromDisk(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	d, err := ed.Disk.CreateDisk(ctx, connect.NewRequest(&vmdv1.CreateDiskRequest{ProjectId: "p1", Name: "data", ImageId: img.Id, SizeGb: 5}))
	vmtMustOK(t, err)
	disk := d.Msg
	e.until(10, "disk ready", func() bool { return vmtDisk(t, ed, disk.Id).State == vmdv1.DiskState_DISK_STATE_READY })

	bootFrom := func(c *clients, project, name, diskID string) (*vmdv1.VM, error) {
		res, err := c.VM.CreateVM(ctx, connect.NewRequest(&vmdv1.CreateVMRequest{ProjectId: project, Name: name,
			Cpus: 2, MemoryMb: 2048, Boot: &vmdv1.CreateVMRequest_DiskId{DiskId: diskID}}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}

	vm, err := bootFrom(ed, "p1", "from-disk", disk.Id)
	vmtMustOK(t, err)
	if len(vm.DiskIds) != 1 || vm.DiskIds[0] != disk.Id {
		t.Fatalf("disk_ids = %v, want [%s]", vm.DiskIds, disk.Id)
	}
	if got := vmtDisk(t, ed, disk.Id).AttachedVmId; got != vm.Id {
		t.Fatalf("disk attached_vm_id = %q, want %q", got, vm.Id)
	}

	_, err = bootFrom(ed, "p1", "second", disk.Id)
	vmtWantCode(t, err, connect.CodeFailedPrecondition) // already attached

	_, err = bootFrom(ed, "p1", "missing", "disk_does_not_exist")
	vmtWantCode(t, err, connect.CodeNotFound)

	p2d, err := e.otherTenant().Disk.CreateDisk(ctx, connect.NewRequest(&vmdv1.CreateDiskRequest{ProjectId: "p2", Name: "theirs", ImageId: img.Id, SizeGb: 5}))
	vmtMustOK(t, err)
	_, err = bootFrom(ed, "p1", "steal", p2d.Msg.Id)
	vmtWantCode(t, err, connect.CodeNotFound)
}

// ------------------------------------------------------------------------------------ authorization

type vmtOp struct {
	name string
	call func(ctx context.Context, c *clients, vmID string) error
}

// vmtOps returns one call per VMService RPC. Every call targets project p1 and, except CreateVM and
// ListVMs, the VM whose ID is passed in.
func vmtOps(imageID string) []vmtOp {
	seq := 0
	return []vmtOp{
		{"GetVM", func(ctx context.Context, c *clients, id string) error {
			_, err := c.VM.GetVM(ctx, connect.NewRequest(&vmdv1.GetVMRequest{Id: id}))
			return err
		}},
		{"ListVMs", func(ctx context.Context, c *clients, _ string) error {
			_, err := c.VM.ListVMs(ctx, connect.NewRequest(&vmdv1.ListVMsRequest{ProjectId: "p1"}))
			return err
		}},
		{"CreateVM", func(ctx context.Context, c *clients, _ string) error {
			seq++
			_, err := c.VM.CreateVM(ctx, connect.NewRequest(vmtCreateReq("p1", fmt.Sprintf("mx-%d", seq), imageID)))
			return err
		}},
		{"UpdateVM", func(ctx context.Context, c *clients, id string) error {
			_, err := c.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: id, Labels: map[string]string{"k": "v"}}))
			return err
		}},
		{"StartVM", func(ctx context.Context, c *clients, id string) error {
			_, err := c.VM.StartVM(ctx, connect.NewRequest(&vmdv1.StartVMRequest{Id: id}))
			return err
		}},
		{"StopVM", func(ctx context.Context, c *clients, id string) error {
			_, err := c.VM.StopVM(ctx, connect.NewRequest(&vmdv1.StopVMRequest{Id: id}))
			return err
		}},
		{"DeleteVM", func(ctx context.Context, c *clients, id string) error {
			_, err := c.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: id}))
			return err
		}},
		{"AddHold", func(ctx context.Context, c *clients, id string) error {
			_, err := c.VM.AddHold(ctx, connect.NewRequest(&vmdv1.HoldRequest{Id: id, Holder: "agentd/m"}))
			return err
		}},
		{"RemoveHold", func(ctx context.Context, c *clients, id string) error {
			_, err := c.VM.RemoveHold(ctx, connect.NewRequest(&vmdv1.HoldRequest{Id: id, Holder: "agentd/m"}))
			return err
		}},
	}
}

// TestVMAuthorizationMatrix checks every VMService RPC against every kind of caller. Columns follow vmtOps:
// Get, List, Create, Update, Start, Stop, Delete, AddHold, RemoveHold.
func TestVMAuthorizationMatrix(t *testing.T) {
	const (
		A  = vmtAllowed
		PD = connect.CodePermissionDenied
		NF = connect.CodeNotFound
		UN = connect.CodeUnauthenticated
	)
	type row struct {
		name string
		who  func(e *env) *clients
		want []connect.Code
	}
	rows := []row{
		{"root (* admin)", func(e *env) *clients { return e.root() }, []connect.Code{A, A, A, A, A, A, A, A, A}},
		{"admin p1", func(e *env) *clients { return e.admin() }, []connect.Code{A, A, A, A, A, A, A, A, A}},
		{"editor p1", func(e *env) *clients { return e.editor() }, []connect.Code{A, A, A, A, A, A, A, PD, PD}},
		{"viewer p1", func(e *env) *clients { return e.viewer() }, []connect.Code{A, A, PD, PD, PD, PD, PD, PD, PD}},
		{"editor p2 (other tenant)", func(e *env) *clients { return e.otherTenant() }, []connect.Code{NF, PD, PD, NF, NF, NF, NF, NF, NF}},
		{"operator (* operator)", func(e *env) *clients { return e.operator() }, []connect.Code{PD, PD, PD, PD, PD, PD, PD, PD, PD}},
		{"service (* service)", func(e *env) *clients { return e.service() }, []connect.Code{A, A, PD, PD, PD, PD, PD, A, A}},
		{"no token", func(e *env) *clients {
			return &clients{VM: vmdv1connect.NewVMServiceClient(e.srv.Client(), e.srv.URL)}
		}, []connect.Code{UN, UN, UN, UN, UN, UN, UN, UN, UN}},
		{"garbage token", func(e *env) *clients { return e.withToken("not.a.token") }, []connect.Code{UN, UN, UN, UN, UN, UN, UN, UN, UN}},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			e := newEnv(t)
			img := e.sharedImage("base")
			ops := vmtOps(img.Id)
			if len(ops) != len(r.want) {
				t.Fatalf("matrix has %d columns, ops has %d", len(r.want), len(ops))
			}
			for i, op := range ops {
				// A fresh VM per cell, so one call's side effects cannot change the next cell's outcome.
				vm := e.createVM(e.admin(), "p1", fmt.Sprintf("cell-%d", i), img.Id, false)
				err := op.call(context.Background(), r.who(e), vm.Id)
				want := r.want[i]
				switch {
				case want == vmtAllowed && err != nil:
					t.Errorf("%s: unexpected error: %v", op.name, err)
				case want != vmtAllowed && code(err) != want:
					t.Errorf("%s: code = %v (%v), want %v", op.name, code(err), err, want)
				}
			}
		})
	}
}

// TestVMCrossTenantLeaksNothing checks that another tenant's attempts fail as "not found" without
// revealing the VM, and do not change it.
func TestVMCrossTenantLeaksNothing(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ctx := context.Background()
	vm := e.createVM(e.editor(), "p1", "secret-web", img.Id, false)
	mal := e.otherTenant()

	_, err := mal.VM.GetVM(ctx, connect.NewRequest(&vmdv1.GetVMRequest{Id: vm.Id}))
	vmtWantCode(t, err, connect.CodeNotFound)
	if strings.Contains(err.Error(), "secret-web") {
		t.Fatalf("not-found error leaks the VM name: %v", err)
	}

	_, err = mal.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id, Labels: map[string]string{"hacked": "1"}}))
	vmtWantCode(t, err, connect.CodeNotFound)
	_, err = mal.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: vm.Id, Force: true}))
	vmtWantCode(t, err, connect.CodeNotFound)

	after := e.getVM(e.editor(), vm.Id)
	if _, ok := after.Labels["hacked"]; ok {
		t.Fatalf("other tenant changed labels: %v", after.Labels)
	}
	if after.DesiredState != vmdv1.PowerState_POWER_STATE_STOPPED || after.Generation != 1 {
		t.Fatalf("other tenant changed the VM: %+v", after)
	}

	// The other tenant's listing must not show p1's VM, and p1 must not see p2's.
	theirs, err := mal.VM.CreateVM(ctx, connect.NewRequest(vmtCreateReq("p2", "their-vm", img.Id)))
	vmtMustOK(t, err)
	for _, v := range vmtListAll(t, e.admin(), "p1", nil) {
		if v.Id == theirs.Msg.Id {
			t.Fatalf("p1 listing shows p2 VM %s", v.Id)
		}
	}
	for _, v := range vmtListAll(t, mal, "p2", nil) {
		if v.ProjectId != "p2" {
			t.Fatalf("p2 listing contains %s from project %s", v.Id, v.ProjectId)
		}
	}
	_, err = mal.VM.ListVMs(ctx, connect.NewRequest(&vmdv1.ListVMsRequest{ProjectId: "p1"}))
	vmtWantCode(t, err, connect.CodePermissionDenied)
}

// ------------------------------------------------------------------------------------ idempotency

func TestVMCreateIdempotency(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	req := vmtCreateReq("p1", "idem", img.Id)
	req.IdempotencyKey = "create-1"
	first := vmtCreate(t, ed, req)
	second := vmtCreate(t, ed, req)
	if first.Id != second.Id {
		t.Fatalf("replay returned %s, want the original %s", second.Id, first.Id)
	}
	if n := len(vmtListDisks(t, ed, "p1")); n != 1 {
		t.Fatalf("p1 has %d disks after a replayed create, want 1", n)
	}
	if n := len(vmtListAll(t, ed, "p1", nil)); n != 1 {
		t.Fatalf("p1 has %d VMs after a replayed create, want 1", n)
	}

	// Same name without a key is a genuine conflict.
	_, err := ed.VM.CreateVM(ctx, connect.NewRequest(vmtCreateReq("p1", "idem", img.Id)))
	vmtWantCode(t, err, connect.CodeAlreadyExists)

	// Same name in another project is fine.
	other := vmtCreate(t, e.otherTenant(), vmtCreateReq("p2", "idem", img.Id))
	if other.Id == first.Id || other.ProjectId != "p2" {
		t.Fatalf("p2 VM = %+v, want a new VM in p2", other)
	}

	// Keys are scoped per project: the same key in p2 creates a separate VM, not p1's.
	keyed := vmtCreateReq("p2", "idem-2", img.Id)
	keyed.IdempotencyKey = "create-1"
	p2keyed := vmtCreate(t, e.otherTenant(), keyed)
	if p2keyed.Id == first.Id || p2keyed.ProjectId != "p2" {
		t.Fatalf("same key in p2 returned %+v, want a new p2 VM", p2keyed)
	}
}

// ------------------------------------------------------------------------------------ quota

func TestVMQuotaMaxVMs(t *testing.T) {
	e := newEnv(t, func(c *service.Config, _ *fake.Config) { c.Quota = service.Quota{MaxVMs: 2} })
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	a := e.createVM(ed, "p1", "qa", img.Id, false)
	e.createVM(ed, "p1", "qb", img.Id, false)
	_, err := ed.VM.CreateVM(ctx, connect.NewRequest(vmtCreateReq("p1", "qc", img.Id)))
	vmtWantCode(t, err, connect.CodeResourceExhausted)

	// A deleted VM frees its slot once the reconciler has removed it.
	_, err = ed.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: a.Id}))
	vmtMustOK(t, err)
	e.until(10, "a gone", func() bool {
		_, err := ed.VM.GetVM(ctx, connect.NewRequest(&vmdv1.GetVMRequest{Id: a.Id}))
		return code(err) == connect.CodeNotFound
	})
	e.createVM(ed, "p1", "qc", img.Id, false)
}

func TestVMQuotaCPUsCountOnlyRunningVMs(t *testing.T) {
	e := newEnv(t, func(c *service.Config, _ *fake.Config) { c.Quota = service.Quota{MaxCPUs: 4} })
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	e.createVM(ed, "p1", "run-a", img.Id, true)   // 2 running vCPUs
	e.createVM(ed, "p1", "idle-b", img.Id, false) // stopped: not counted
	e.createVM(ed, "p1", "run-c", img.Id, true)   // 4 running vCPUs, exactly at the limit

	d := vmtCreateReq("p1", "run-d", img.Id)
	d.Start = true
	_, err := ed.VM.CreateVM(ctx, connect.NewRequest(d))
	vmtWantCode(t, err, connect.CodeResourceExhausted)

	idle := e.createVM(ed, "p1", "idle-e", img.Id, false)
	_, err = ed.VM.StartVM(ctx, connect.NewRequest(&vmdv1.StartVMRequest{Id: idle.Id}))
	vmtWantCode(t, err, connect.CodeResourceExhausted)

	// Stopping one running VM frees its vCPUs for the stopped VM.
	runA := vmtListAll(t, ed, "p1", map[string]string{})
	var runAID string
	for _, v := range runA {
		if v.Name == "run-a" {
			runAID = v.Id
		}
	}
	_, err = ed.VM.StopVM(ctx, connect.NewRequest(&vmdv1.StopVMRequest{Id: runAID}))
	vmtMustOK(t, err)
	_, err = ed.VM.StartVM(ctx, connect.NewRequest(&vmdv1.StartVMRequest{Id: idle.Id}))
	vmtMustOK(t, err)
}

func TestVMQuotaMemoryCountsOnlyRunningVMs(t *testing.T) {
	e := newEnv(t, func(c *service.Config, _ *fake.Config) { c.Quota = service.Quota{MaxMemoryMB: 3000} })
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	mk := func(name string, start bool) *vmdv1.VM {
		req := vmtCreateReq("p1", name, img.Id)
		req.Cpus, req.MemoryMb, req.Start = 1, 2048, start
		return vmtCreate(t, ed, req)
	}
	a := mk("mem-a", true)
	_, err := ed.VM.CreateVM(ctx, connect.NewRequest(func() *vmdv1.CreateVMRequest {
		r := vmtCreateReq("p1", "mem-b", img.Id)
		r.Cpus, r.MemoryMb, r.Start = 1, 2048, true
		return r
	}()))
	vmtWantCode(t, err, connect.CodeResourceExhausted)

	b := mk("mem-c", false)
	_, err = ed.VM.StartVM(ctx, connect.NewRequest(&vmdv1.StartVMRequest{Id: b.Id}))
	vmtWantCode(t, err, connect.CodeResourceExhausted)

	_, err = ed.VM.StopVM(ctx, connect.NewRequest(&vmdv1.StopVMRequest{Id: a.Id}))
	vmtMustOK(t, err)
	_, err = ed.VM.StartVM(ctx, connect.NewRequest(&vmdv1.StartVMRequest{Id: b.Id}))
	vmtMustOK(t, err)
}

func TestVMQuotaDiskCountsStoppedVMs(t *testing.T) {
	e := newEnv(t, func(c *service.Config, _ *fake.Config) { c.Quota = service.Quota{MaxDiskGB: 15} })
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	e.createVM(ed, "p1", "disk-a", img.Id, false) // 10 GB, stopped: disks always count
	_, err := ed.VM.CreateVM(ctx, connect.NewRequest(vmtCreateReq("p1", "disk-b", img.Id)))
	vmtWantCode(t, err, connect.CodeResourceExhausted)

	small := vmtCreateReq("p1", "disk-c", img.Id)
	small.DiskSizeGb = 5 // 10 + 5 == 15, exactly at the limit
	vmtCreate(t, ed, small)
}

// ------------------------------------------------------------------------------------ update

func TestVMUpdateLabels(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	req := vmtCreateReq("p1", "labels", img.Id)
	req.Labels = map[string]string{"a": "1", "b": "2"}
	vm := vmtCreate(t, ed, req)
	e.clk.Advance(time.Minute)

	res, err := ed.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id, Labels: map[string]string{"b": "3", "c": "4"}}))
	vmtMustOK(t, err)
	if want := map[string]string{"a": "1", "b": "3", "c": "4"}; !maps.Equal(res.Msg.Labels, want) {
		t.Fatalf("merged labels = %v, want %v", res.Msg.Labels, want)
	}
	if res.Msg.Generation != 1 {
		t.Fatalf("label-only update bumped generation to %d", res.Msg.Generation)
	}

	res, err = ed.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id, ReplaceLabels: true, Labels: map[string]string{"z": "9"}}))
	vmtMustOK(t, err)
	if want := map[string]string{"z": "9"}; !maps.Equal(res.Msg.Labels, want) {
		t.Fatalf("replaced labels = %v, want %v", res.Msg.Labels, want)
	}

	res, err = ed.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id, ReplaceLabels: true}))
	vmtMustOK(t, err)
	if len(res.Msg.Labels) != 0 {
		t.Fatalf("replace with no labels left %v", res.Msg.Labels)
	}
	if res.Msg.Generation != 1 {
		t.Fatalf("label-only updates bumped generation to %d", res.Msg.Generation)
	}
	if !res.Msg.UpdatedAt.AsTime().After(vm.UpdatedAt.AsTime()) {
		t.Fatalf("updated_at did not advance: before %v, after %v", vm.UpdatedAt.AsTime(), res.Msg.UpdatedAt.AsTime())
	}
	if !res.Msg.CreatedAt.AsTime().Equal(vm.CreatedAt.AsTime()) {
		t.Fatalf("created_at changed on update")
	}
}

func TestVMUpdateResize(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	vm := e.createVM(ed, "p1", "resize", img.Id, false)
	e.until(10, "stopped", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_STOPPED })

	// Invalid sizes are refused and change nothing.
	for _, bad := range []struct {
		cpus *int32
		mem  *int64
	}{
		{cpus: vmtInt32(0)}, {cpus: vmtInt32(65)}, {mem: vmtInt64(100)}, {mem: vmtInt64((1 << 20) + 1)},
	} {
		_, err := ed.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id, Cpus: bad.cpus, MemoryMb: bad.mem}))
		vmtWantCode(t, err, connect.CodeInvalidArgument)
	}
	if got := e.getVM(ed, vm.Id); got.Cpus != 2 || got.MemoryMb != 2048 || got.Generation != 1 {
		t.Fatalf("refused resize changed the VM: %+v", got)
	}

	// A real resize bumps the generation once.
	res, err := ed.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id, Cpus: vmtInt32(4), MemoryMb: vmtInt64(4096)}))
	vmtMustOK(t, err)
	if res.Msg.Cpus != 4 || res.Msg.MemoryMb != 4096 || res.Msg.Generation != 2 {
		t.Fatalf("after resize: cpus=%d mem=%d gen=%d, want 4/4096/2", res.Msg.Cpus, res.Msg.MemoryMb, res.Msg.Generation)
	}

	// A label-only change does not bump the generation.
	res, err = ed.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id, Labels: map[string]string{"x": "y"}}))
	vmtMustOK(t, err)
	if res.Msg.Generation != 2 {
		t.Fatalf("label-only update changed generation to %d, want 2", res.Msg.Generation)
	}

	// Setting the sizes they already have is a no-op.
	before := res.Msg
	e.clk.Advance(time.Minute)
	res, err = ed.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id, Cpus: vmtInt32(4)}))
	vmtMustOK(t, err)
	if res.Msg.Generation != 2 || !res.Msg.UpdatedAt.AsTime().Equal(before.UpdatedAt.AsTime()) {
		t.Fatalf("same-size resize changed the VM: gen %d updated %v, want gen 2 updated %v",
			res.Msg.Generation, res.Msg.UpdatedAt.AsTime(), before.UpdatedAt.AsTime())
	}
}

func TestVMUpdateRefusedWhileRunningOrStarting(t *testing.T) {
	// BootPolls keeps a started VM BOOTING for several polls, so STARTING is observable.
	e := newEnv(t, func(_ *service.Config, f *fake.Config) { f.BootPolls = 5 })
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	vm := e.createVM(ed, "p1", "busy", img.Id, true)
	e.tick(1)
	if st := e.getVM(ed, vm.Id).State; st != vmdv1.VMState_VM_STATE_STARTING {
		t.Fatalf("after one tick state = %v, want STARTING", st)
	}
	_, err := ed.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id, Cpus: vmtInt32(4)}))
	vmtWantCode(t, err, connect.CodeFailedPrecondition)

	e.until(20, "running", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_RUNNING })
	_, err = ed.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id, MemoryMb: vmtInt64(4096)}))
	vmtWantCode(t, err, connect.CodeFailedPrecondition)
	if got := e.getVM(ed, vm.Id); got.Cpus != 2 || got.MemoryMb != 2048 {
		t.Fatalf("refused resize changed the VM: %+v", got)
	}
}

func TestVMUpdateExpectedGeneration(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()
	vm := e.createVM(ed, "p1", "gen", img.Id, false)

	_, err := ed.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id, ExpectedGeneration: 2, Labels: map[string]string{"k": "v"}}))
	vmtWantCode(t, err, connect.CodeAborted)
	if got := e.getVM(ed, vm.Id); got.Generation != 1 || len(got.Labels) != 0 {
		t.Fatalf("conflicting update was applied: %+v", got)
	}

	res, err := ed.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id, ExpectedGeneration: 1, Labels: map[string]string{"k": "v"}}))
	vmtMustOK(t, err)
	if res.Msg.Generation != 1 || res.Msg.Labels["k"] != "v" {
		t.Fatalf("matching expected_generation update = %+v", res.Msg)
	}
}

func TestVMUpdateNoOpLeavesVMUnchanged(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()
	req := vmtCreateReq("p1", "noop", img.Id)
	req.Labels = map[string]string{"a": "1"}
	vm := vmtCreate(t, ed, req)

	e.clk.Advance(time.Minute)
	res, err := ed.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id}))
	vmtMustOK(t, err)
	if res.Msg.Generation != vm.Generation || !res.Msg.UpdatedAt.AsTime().Equal(vm.UpdatedAt.AsTime()) {
		t.Fatalf("empty update changed the VM: gen %d updated %v, was gen %d updated %v",
			res.Msg.Generation, res.Msg.UpdatedAt.AsTime(), vm.Generation, vm.UpdatedAt.AsTime())
	}
}

// TestVMUpdateIdenticalLabelsIsNoOp is written against the spec: merging labels that already hold the same
// values is a no-op and should leave updated_at alone. The source treats any non-empty label map as a change.
func TestVMUpdateIdenticalLabelsIsNoOp(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()
	req := vmtCreateReq("p1", "same-labels", img.Id)
	req.Labels = map[string]string{"a": "1"}
	vm := vmtCreate(t, ed, req)

	e.clk.Advance(time.Minute)
	res, err := ed.VM.UpdateVM(ctx, connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id, Labels: map[string]string{"a": "1"}}))
	vmtMustOK(t, err)
	if res.Msg.Generation != vm.Generation {
		t.Fatalf("generation changed to %d", res.Msg.Generation)
	}
	if !res.Msg.UpdatedAt.AsTime().Equal(vm.UpdatedAt.AsTime()) {
		t.Fatalf("identical label merge bumped updated_at from %v to %v; a no-op should leave it stable",
			vm.UpdatedAt.AsTime(), res.Msg.UpdatedAt.AsTime())
	}
}

// ------------------------------------------------------------------------------------ start / stop

func TestVMStartStopIdempotent(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()
	vm := e.createVM(ed, "p1", "ss", img.Id, false)
	e.until(10, "stopped", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_STOPPED })

	call := func(start bool, force bool) *vmdv1.VM {
		t.Helper()
		if start {
			res, err := ed.VM.StartVM(ctx, connect.NewRequest(&vmdv1.StartVMRequest{Id: vm.Id}))
			vmtMustOK(t, err)
			return res.Msg
		}
		res, err := ed.VM.StopVM(ctx, connect.NewRequest(&vmdv1.StopVMRequest{Id: vm.Id, Force: force}))
		vmtMustOK(t, err)
		return res.Msg
	}

	if v := call(false, false); v.State != vmdv1.VMState_VM_STATE_STOPPED || v.DesiredState != vmdv1.PowerState_POWER_STATE_STOPPED {
		t.Fatalf("stop of a stopped VM: %+v", v)
	}
	for range 2 {
		if v := call(true, false); v.DesiredState != vmdv1.PowerState_POWER_STATE_RUNNING {
			t.Fatalf("start: desired = %v", v.DesiredState)
		}
	}
	e.until(10, "running", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_RUNNING })
	gen := e.getVM(ed, vm.Id).Generation
	if v := call(true, false); v.State != vmdv1.VMState_VM_STATE_RUNNING || v.Generation != gen {
		t.Fatalf("start of a running VM: state %v gen %d, want RUNNING gen %d", v.State, v.Generation, gen)
	}

	// Graceful stop, observed mid-flight, then escalated to a forced stop while STOPPING.
	call(false, false)
	e.tick(1)
	if st := e.getVM(ed, vm.Id).State; st != vmdv1.VMState_VM_STATE_STOPPING {
		t.Fatalf("after graceful stop and one tick state = %v, want STOPPING", st)
	}
	v := call(false, true)
	if v.DesiredState != vmdv1.PowerState_POWER_STATE_STOPPED || v.State != vmdv1.VMState_VM_STATE_STOPPING {
		t.Fatalf("forced stop while STOPPING: desired %v state %v", v.DesiredState, v.State)
	}
	e.until(10, "stopped after forced stop", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_STOPPED })
	call(false, true) // forced stop of a stopped VM is a no-op
}

func TestVMStartStopRefusedWhileDeleting(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()
	vm := e.createVM(ed, "p1", "dying", img.Id, false)

	_, err := ed.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: vm.Id}))
	vmtMustOK(t, err)
	_, err = ed.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: vm.Id}))
	vmtMustOK(t, err) // a repeated delete is accepted

	_, err = ed.VM.StartVM(ctx, connect.NewRequest(&vmdv1.StartVMRequest{Id: vm.Id}))
	vmtWantCode(t, err, connect.CodeFailedPrecondition)
	_, err = ed.VM.StopVM(ctx, connect.NewRequest(&vmdv1.StopVMRequest{Id: vm.Id}))
	vmtWantCode(t, err, connect.CodeFailedPrecondition)
}

func TestVMStartVMReconcilesToRunning(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	vm := e.createVM(ed, "p1", "cycle", img.Id, true)
	e.until(10, "running", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_RUNNING })
	for range 3 { // stop and start repeatedly; the reconciler must follow each change
		_, err := ed.VM.StopVM(context.Background(), connect.NewRequest(&vmdv1.StopVMRequest{Id: vm.Id}))
		vmtMustOK(t, err)
		e.until(10, "stopped", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_STOPPED })
		_, err = ed.VM.StartVM(context.Background(), connect.NewRequest(&vmdv1.StartVMRequest{Id: vm.Id}))
		vmtMustOK(t, err)
		e.until(10, "running", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_RUNNING })
	}
}

// ------------------------------------------------------------------------------------ holds

func TestVMHolds(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	adm := e.admin() // holds need the admin role (or the service principal)
	ctx := context.Background()
	vm := e.createVM(ed, "p1", "held", img.Id, false)

	add := func(id, holder string) (*vmdv1.VM, error) {
		res, err := adm.VM.AddHold(ctx, connect.NewRequest(&vmdv1.HoldRequest{Id: id, Holder: holder}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}

	v, err := add(vm.Id, "agentd/b")
	vmtMustOK(t, err)
	if !slices.Equal(v.Holds, []string{"agentd/b"}) {
		t.Fatalf("holds = %v", v.Holds)
	}
	v, err = add(vm.Id, "agentd/a")
	vmtMustOK(t, err)
	if !slices.Equal(v.Holds, []string{"agentd/a", "agentd/b"}) {
		t.Fatalf("holds after second add = %v, want sorted [agentd/a agentd/b]", v.Holds)
	}
	v, err = add(vm.Id, "agentd/b") // duplicate
	vmtMustOK(t, err)
	if !slices.Equal(v.Holds, []string{"agentd/a", "agentd/b"}) {
		t.Fatalf("duplicate add changed holds to %v", v.Holds)
	}

	_, err = add(vm.Id, "")
	vmtWantCode(t, err, connect.CodeInvalidArgument)

	// Removing a holder that is not there is a no-op.
	res, err := adm.VM.RemoveHold(ctx, connect.NewRequest(&vmdv1.HoldRequest{Id: vm.Id, Holder: "agentd/zzz"}))
	vmtMustOK(t, err)
	if !slices.Equal(res.Msg.Holds, []string{"agentd/a", "agentd/b"}) {
		t.Fatalf("unknown RemoveHold changed holds to %v", res.Msg.Holds)
	}

	// Holds block a plain delete and the error names the holders.
	_, err = ed.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: vm.Id}))
	vmtWantCode(t, err, connect.CodeFailedPrecondition)
	for _, h := range []string{"agentd/a", "agentd/b"} {
		if !strings.Contains(err.Error(), h) {
			t.Fatalf("delete refusal %q does not name holder %s", err, h)
		}
	}
	if got := e.getVM(ed, vm.Id); got.State == vmdv1.VMState_VM_STATE_DELETING {
		t.Fatal("refused delete still marked the VM as deleting")
	}

	// force=true overrides the holds.
	_, err = ed.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: vm.Id, Force: true}))
	vmtMustOK(t, err)
	e.until(10, "forced delete completes", func() bool {
		_, err := ed.VM.GetVM(ctx, connect.NewRequest(&vmdv1.GetVMRequest{Id: vm.Id}))
		return code(err) == connect.CodeNotFound
	})

	// Once every hold is released a plain delete goes through. A deleting VM refuses new holds.
	vm2 := e.createVM(ed, "p1", "released", img.Id, false)
	_, err = add(vm2.Id, "agentd/c")
	vmtMustOK(t, err)
	res, err = adm.VM.RemoveHold(ctx, connect.NewRequest(&vmdv1.HoldRequest{Id: vm2.Id, Holder: "agentd/c"}))
	vmtMustOK(t, err)
	if len(res.Msg.Holds) != 0 {
		t.Fatalf("holds after release = %v", res.Msg.Holds)
	}
	_, err = ed.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: vm2.Id}))
	vmtMustOK(t, err)
	_, err = add(vm2.Id, "agentd/d")
	vmtWantCode(t, err, connect.CodeFailedPrecondition)
}

func TestVMHoldsRequireAdminOrService(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	vm := e.createVM(e.admin(), "p1", "held-rbac", img.Id, false)
	req := func() *connect.Request[vmdv1.HoldRequest] {
		return connect.NewRequest(&vmdv1.HoldRequest{Id: vm.Id, Holder: "agentd/x"})
	}
	_, err := e.editor().VM.AddHold(context.Background(), req())
	vmtWantCode(t, err, connect.CodePermissionDenied)
	_, err = e.service().VM.AddHold(context.Background(), req())
	vmtMustOK(t, err)
	_, err = e.admin().VM.RemoveHold(context.Background(), req())
	vmtMustOK(t, err)
}

// ------------------------------------------------------------------------------------ list

func TestVMListPagination(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	var want []string
	for i := 1; i <= 5; i++ {
		want = append(want, e.createVM(ed, "p1", fmt.Sprintf("page-%d", i), img.Id, false).Id)
	}
	// Another tenant's VM must never appear in p1's pages.
	vmtCreate(t, e.otherTenant(), vmtCreateReq("p2", "page-other", img.Id))

	var got []string
	var sizes []int
	token := ""
	for range 10 {
		res, err := ed.VM.ListVMs(ctx, connect.NewRequest(&vmdv1.ListVMsRequest{ProjectId: "p1", PageSize: 2, PageToken: token}))
		vmtMustOK(t, err)
		sizes = append(sizes, len(res.Msg.Vms))
		for _, v := range res.Msg.Vms {
			if v.ProjectId != "p1" {
				t.Fatalf("page contains %s from project %s", v.Id, v.ProjectId)
			}
			got = append(got, v.Id)
		}
		token = res.Msg.NextPageToken
		if token == "" {
			break
		}
	}
	if token != "" {
		t.Fatal("pagination did not end")
	}
	if !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Fatalf("page sizes = %v, want [2 2 1]", sizes)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paged VMs = %v, want %v (each VM exactly once, in order)", got, want)
	}
}

func TestVMListLabelSelector(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()

	mk := func(c *clients, project, name string, labels map[string]string) string {
		req := vmtCreateReq(project, name, img.Id)
		req.Labels = labels
		return vmtCreate(t, c, req).Id
	}
	webDev := mk(ed, "p1", "web-dev", map[string]string{"env": "dev"})
	webProd := mk(ed, "p1", "web-prod", map[string]string{"env": "prod"})
	apiDev := mk(ed, "p1", "api-dev", map[string]string{"env": "dev", "tier": "api"})
	otherDev := mk(e.otherTenant(), "p2", "other-dev", map[string]string{"env": "dev"})

	ids := func(vms []*vmdv1.VM) []string {
		var out []string
		for _, v := range vms {
			out = append(out, v.Id)
		}
		slices.Sort(out)
		return out
	}
	check := func(name string, got []*vmdv1.VM, want ...string) {
		t.Helper()
		slices.Sort(want)
		if !slices.Equal(ids(got), want) {
			t.Fatalf("%s: got %v, want %v", name, ids(got), want)
		}
	}

	check("env=dev", vmtListAll(t, ed, "p1", map[string]string{"env": "dev"}), webDev, apiDev)
	check("env=dev,tier=api", vmtListAll(t, ed, "p1", map[string]string{"env": "dev", "tier": "api"}), apiDev)
	check("no selector", vmtListAll(t, ed, "p1", nil), webDev, webProd, apiDev)
	check("p2 env=dev", vmtListAll(t, e.otherTenant(), "p2", map[string]string{"env": "dev"}), otherDev)
}

// ------------------------------------------------------------------------------------ get / wait_for_state

func TestVMGetWaitForRunning(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	vm := e.createVM(ed, "p1", "waiter", img.Id, true)

	stop := make(chan struct{})
	stopOnce := sync.OnceFunc(func() { close(stop) })
	defer stopOnce()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(300 * time.Millisecond) // the long-poll is in flight with the VM still not running
		vmtDrive(e, stop)
	}()

	start := time.Now()
	res, err := ed.VM.GetVM(context.Background(), connect.NewRequest(&vmdv1.GetVMRequest{
		Id: vm.Id, WaitForState: vmdv1.VMState_VM_STATE_RUNNING, WaitSeconds: 10}))
	elapsed := time.Since(start)
	stopOnce()
	wg.Wait()

	vmtMustOK(t, err)
	if res.Msg.State != vmdv1.VMState_VM_STATE_RUNNING {
		t.Fatalf("state = %v, want RUNNING", res.Msg.State)
	}
	if elapsed < 200*time.Millisecond {
		t.Fatalf("returned after %v; the call should have waited for the reconciler", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("long-poll took %v", elapsed)
	}
}

func TestVMGetWaitReturnsImmediatelyIfInState(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("ready")
	start := time.Now()
	res, err := e.editor().VM.GetVM(context.Background(), connect.NewRequest(&vmdv1.GetVMRequest{
		Id: vm.Id, WaitForState: vmdv1.VMState_VM_STATE_RUNNING, WaitSeconds: 30}))
	vmtMustOK(t, err)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("already-running wait took %v", elapsed)
	}
	if res.Msg.State != vmdv1.VMState_VM_STATE_RUNNING {
		t.Fatalf("state = %v", res.Msg.State)
	}
}

func TestVMGetWaitTimesOutWithCurrentState(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	vm := e.createVM(ed, "p1", "idle", img.Id, false)
	e.until(10, "stopped", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_STOPPED })

	start := time.Now()
	res, err := ed.VM.GetVM(context.Background(), connect.NewRequest(&vmdv1.GetVMRequest{
		Id: vm.Id, WaitForState: vmdv1.VMState_VM_STATE_RUNNING, WaitSeconds: 1}))
	elapsed := time.Since(start)
	vmtMustOK(t, err)
	if res.Msg.State != vmdv1.VMState_VM_STATE_STOPPED {
		t.Fatalf("state after timeout = %v, want STOPPED", res.Msg.State)
	}
	if elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("wait_seconds=1 returned after %v", elapsed)
	}
}

func TestVMGetWaitReturnsEarlyOnError(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	// A non-transient failure sends the VM to ERROR on its first start attempt.
	e.node.FailNext("StartVM", errs.Invalid("bad disk layout"))
	vm := e.createVM(ed, "p1", "broken", img.Id, true)

	stop := make(chan struct{})
	stopOnce := sync.OnceFunc(func() { close(stop) })
	defer stopOnce()
	go vmtDrive(e, stop)

	start := time.Now()
	res, err := ed.VM.GetVM(context.Background(), connect.NewRequest(&vmdv1.GetVMRequest{
		Id: vm.Id, WaitForState: vmdv1.VMState_VM_STATE_RUNNING, WaitSeconds: 10}))
	elapsed := time.Since(start)
	stopOnce()

	vmtMustOK(t, err)
	if res.Msg.State != vmdv1.VMState_VM_STATE_ERROR {
		t.Fatalf("state = %v, want ERROR", res.Msg.State)
	}
	if !strings.Contains(res.Msg.StateReason, "bad disk layout") {
		t.Fatalf("state_reason = %q, want the failure", res.Msg.StateReason)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("returned after %v; ERROR should end the wait early", elapsed)
	}
}

// ------------------------------------------------------------------------------------ delete lifecycle

func TestVMDeleteKeepsDiskForReuse(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	first := e.createVM(ed, "p1", "keeper", img.Id, true)
	e.until(10, "running", func() bool { return e.getVM(ed, first.Id).State == vmdv1.VMState_VM_STATE_RUNNING })
	diskID := first.DiskIds[0]

	_, err := ed.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: first.Id, DeleteDisks: false}))
	vmtMustOK(t, err)
	e.until(10, "first VM gone", func() bool {
		_, err := ed.VM.GetVM(ctx, connect.NewRequest(&vmdv1.GetVMRequest{Id: first.Id}))
		return code(err) == connect.CodeNotFound
	})

	d := vmtDisk(t, ed, diskID)
	if d.AttachedVmId != "" {
		t.Fatalf("disk still attached to %q after its VM was deleted", d.AttachedVmId)
	}
	if d.State != vmdv1.DiskState_DISK_STATE_READY {
		t.Fatalf("kept disk state = %v, want READY", d.State)
	}

	second := vmtCreate(t, ed, &vmdv1.CreateVMRequest{ProjectId: "p1", Name: "reuser", Cpus: 2, MemoryMb: 2048, Start: true,
		Boot: &vmdv1.CreateVMRequest_DiskId{DiskId: diskID}})
	if len(second.DiskIds) != 1 || second.DiskIds[0] != diskID {
		t.Fatalf("reuse VM disks = %v, want [%s]", second.DiskIds, diskID)
	}
	e.until(10, "reuser running", func() bool { return e.getVM(ed, second.Id).State == vmdv1.VMState_VM_STATE_RUNNING })
}

func TestVMDeleteRemovesDisks(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	ctx := context.Background()

	vm := e.createVM(ed, "p1", "doomed", img.Id, false)
	e.until(10, "stopped", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_STOPPED })
	_, err := ed.VM.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{Id: vm.Id, DeleteDisks: true}))
	vmtMustOK(t, err)
	e.until(10, "disk removed", func() bool { return len(vmtListDisks(t, ed, "p1")) == 0 })
}

func TestVMDeleteRunningStopsFirst(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("runner")
	ed := e.editor()
	_, err := ed.VM.DeleteVM(context.Background(), connect.NewRequest(&vmdv1.DeleteVMRequest{Id: vm.Id, DeleteDisks: true}))
	vmtMustOK(t, err)
	e.until(20, "gone", func() bool {
		_, err := ed.VM.GetVM(context.Background(), connect.NewRequest(&vmdv1.GetVMRequest{Id: vm.Id}))
		return code(err) == connect.CodeNotFound
	})
	e.until(10, "disk removed", func() bool { return len(vmtListDisks(t, ed, "p1")) == 0 })

	calls := e.node.Calls()
	idx := func(name string) int { return slices.Index(calls, name) }
	stop, forget := idx("StopVM"), idx("ForgetVM")
	if stop < 0 || forget < 0 {
		t.Fatalf("node calls %v: want both StopVM and ForgetVM", calls)
	}
	if stop > forget {
		t.Fatalf("StopVM (at %d) must come before ForgetVM (at %d): %v", stop, forget, calls)
	}
	if del := idx("DeleteDisk"); del < forget {
		t.Fatalf("DeleteDisk (at %d) before ForgetVM (at %d): %v", del, forget, calls)
	}
}

// ------------------------------------------------------------------------------------ timestamps

func TestVMTimestamps(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base")
	ed := e.editor()
	vm := e.createVM(ed, "p1", "stamped", img.Id, false)
	if vm.CreatedAt == nil || vm.UpdatedAt == nil {
		t.Fatalf("created_at/updated_at not set: %+v", vm)
	}
	if !vm.CreatedAt.AsTime().Equal(e.clk.Now()) || !vm.UpdatedAt.AsTime().Equal(e.clk.Now()) {
		t.Fatalf("timestamps %v / %v, want the clock %v", vm.CreatedAt.AsTime(), vm.UpdatedAt.AsTime(), e.clk.Now())
	}

	e.clk.Advance(time.Minute)
	res, err := ed.VM.UpdateVM(context.Background(), connect.NewRequest(&vmdv1.UpdateVMRequest{Id: vm.Id, Labels: map[string]string{"k": "v"}}))
	vmtMustOK(t, err)
	if !res.Msg.CreatedAt.AsTime().Equal(vm.CreatedAt.AsTime()) {
		t.Fatal("created_at changed")
	}
	if !res.Msg.UpdatedAt.AsTime().After(vm.CreatedAt.AsTime()) {
		t.Fatalf("updated_at %v not after created_at %v", res.Msg.UpdatedAt.AsTime(), vm.CreatedAt.AsTime())
	}
}
