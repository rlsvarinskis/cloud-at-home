package service_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/fake"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/service"
)

// ------------------------------------------------------------------------------------ helpers

const ditGiB = 1 << 30

func ditQuotaDiskGB(gb int) envOption {
	return func(c *service.Config, _ *fake.Config) { c.Quota.MaxDiskGB = gb }
}

func ditOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func ditWant(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %v, got success", want)
	}
	if got := code(err); got != want {
		t.Fatalf("want code %v, got %v (%v)", want, got, err)
	}
}

func ditReasonHas(t *testing.T, reason, want string) {
	t.Helper()
	if !strings.Contains(reason, want) {
		t.Fatalf("reason %q does not mention %q", reason, want)
	}
}

func ditBogus(e *env) *clients { return e.withToken("not-a-token") }

// ditReg registers an image on node n1 at the conventional path. An empty project means shared.
func ditReg(c *clients, project, name string) (*vmdv1.Image, error) {
	res, err := c.Image.RegisterImage(context.Background(), connect.NewRequest(&vmdv1.RegisterImageRequest{
		ProjectId: project, Name: name, NodeId: "n1", Path: "/images/" + name + ".qcow2"}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func ditGetImage(c *clients, id string) (*vmdv1.Image, error) {
	res, err := c.Image.GetImage(context.Background(), connect.NewRequest(&vmdv1.GetImageRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func ditDeleteImage(c *clients, id string) error {
	_, err := c.Image.DeleteImage(context.Background(), connect.NewRequest(&vmdv1.DeleteImageRequest{Id: id}))
	return err
}

// ditListImages returns the IDs of every page of ListImages, page by page.
func ditListImages(t *testing.T, c *clients, project string, pageSize int32) [][]string {
	t.Helper()
	var pages [][]string
	token := ""
	for range 50 {
		res, err := c.Image.ListImages(context.Background(), connect.NewRequest(&vmdv1.ListImagesRequest{
			ProjectId: project, PageSize: pageSize, PageToken: token}))
		ditOK(t, err)
		var page []string
		for _, i := range res.Msg.Images {
			page = append(page, i.Id)
		}
		pages = append(pages, page)
		if token = res.Msg.NextPageToken; token == "" {
			return pages
		}
	}
	t.Fatal("image pagination did not terminate")
	return nil
}

func ditCreateDisk(c *clients, project, name string, size int32, image, node string) (*vmdv1.Disk, error) {
	res, err := c.Disk.CreateDisk(context.Background(), connect.NewRequest(&vmdv1.CreateDiskRequest{
		ProjectId: project, Name: name, SizeGb: size, ImageId: image, NodeId: node}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func ditGetDisk(c *clients, id string) (*vmdv1.Disk, error) {
	res, err := c.Disk.GetDisk(context.Background(), connect.NewRequest(&vmdv1.GetDiskRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// ditListDisks returns the IDs of every page of ListDisks, page by page.
func ditListDisks(t *testing.T, c *clients, project string, pageSize int32) [][]string {
	t.Helper()
	var pages [][]string
	token := ""
	for range 50 {
		res, err := c.Disk.ListDisks(context.Background(), connect.NewRequest(&vmdv1.ListDisksRequest{
			ProjectId: project, PageSize: pageSize, PageToken: token}))
		ditOK(t, err)
		var page []string
		for _, d := range res.Msg.Disks {
			page = append(page, d.Id)
		}
		pages = append(pages, page)
		if token = res.Msg.NextPageToken; token == "" {
			return pages
		}
	}
	t.Fatal("disk pagination did not terminate")
	return nil
}

func ditFlat(pages [][]string) []string {
	var out []string
	for _, p := range pages {
		out = append(out, p...)
	}
	return out
}

func ditWaitImage(e *env, id string, want vmdv1.ImageState) {
	e.t.Helper()
	root := e.root()
	e.until(10, "image "+id+" "+want.String(), func() bool {
		img, err := ditGetImage(root, id)
		return err == nil && img.State == want
	})
}

func ditWaitImageGone(e *env, id string) {
	e.t.Helper()
	root := e.root()
	e.until(10, "image "+id+" gone", func() bool {
		_, err := ditGetImage(root, id)
		return code(err) == connect.CodeNotFound
	})
}

func ditWaitDisk(e *env, id string, want vmdv1.DiskState) {
	e.t.Helper()
	root := e.root()
	e.until(10, "disk "+id+" "+want.String(), func() bool {
		d, err := ditGetDisk(root, id)
		return err == nil && d.State == want
	})
}

func ditWaitDiskGone(e *env, id string) {
	e.t.Helper()
	root := e.root()
	e.until(10, "disk "+id+" gone", func() bool {
		_, err := ditGetDisk(root, id)
		return code(err) == connect.CodeNotFound
	})
}

func ditWaitVM(e *env, c *clients, id string, want vmdv1.VMState) {
	e.t.Helper()
	e.until(10, "vm "+id+" "+want.String(), func() bool { return e.getVM(c, id).State == want })
}

// ditImageReason reads the store directly: the API does not expose state_reason for images.
func ditImageReason(t *testing.T, e *env, id string) string {
	t.Helper()
	img, err := e.st.GetImage(context.Background(), id)
	ditOK(t, err)
	return img.StateReason
}

// ditDiskReason reads the reason from the store (the API exposes the same text: see TestFailureReasonsAreVisibleToClients).
func ditDiskReason(t *testing.T, e *env, id string) string {
	t.Helper()
	d, err := e.st.GetDisk(context.Background(), id)
	ditOK(t, err)
	return d.StateReason
}

// ditFakeHasImage asks the fake node whether it holds the image. It is only meant for the end of a
// test: when the image is absent the probe registers it.
func ditFakeHasImage(e *env, id string) bool {
	_, err := e.node.RegisterImage(context.Background(), &nodev1.RegisterImageRequest{ImageId: id, Path: "/probe.qcow2"})
	return errors.Is(err, errs.ErrAlreadyExists)
}

// ditFakeHasDisk asks the fake node whether it holds the disk. Same caveat as ditFakeHasImage.
func ditFakeHasDisk(e *env, id string) bool {
	_, err := e.node.CreateDisk(context.Background(), &nodev1.CreateDiskRequest{DiskId: id, SizeGb: 1})
	return errors.Is(err, errs.ErrAlreadyExists)
}

// ditFakeDiskSize reports whether the fake holds the disk at exactly n GB, without changing it when
// it does: a shrink to n-1 must be refused, and a resize to n must be a no-op.
func ditFakeDiskSize(e *env, id string, n int32) bool {
	ctx := context.Background()
	_, below := e.node.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: id, SizeGb: n - 1})
	_, at := e.node.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: id, SizeGb: n})
	return errors.Is(below, errs.ErrFailedPrecondition) && at == nil
}

func ditCount(calls []string, method string) int {
	n := 0
	for _, c := range calls {
		if c == method {
			n++
		}
	}
	return n
}

// ditProjectImage registers a project image as the global admin and waits until it is ready.
func ditProjectImage(t *testing.T, e *env, project, name string) *vmdv1.Image {
	t.Helper()
	img, err := ditReg(e.root(), project, name)
	ditOK(t, err)
	ditWaitImage(e, img.Id, vmdv1.ImageState_IMAGE_STATE_READY)
	return img
}

// ------------------------------------------------------------------------------------ images

func TestImageRegisterSharedByRootBecomesReady(t *testing.T) {
	e := newEnv(t)
	root := e.root()

	img, err := ditReg(root, "", "debian")
	ditOK(t, err)
	if img.ProjectId != "" || img.State != vmdv1.ImageState_IMAGE_STATE_CREATING || img.SizeBytes != 0 || img.NodeId != "n1" {
		t.Fatalf("fresh shared image: %+v", img)
	}
	got, err := ditGetImage(root, img.Id)
	ditOK(t, err)
	if got.State != vmdv1.ImageState_IMAGE_STATE_CREATING {
		t.Fatalf("before reconcile: state %v, want CREATING", got.State)
	}

	e.tick(1)
	got, err = ditGetImage(root, img.Id)
	ditOK(t, err)
	if got.State != vmdv1.ImageState_IMAGE_STATE_READY || got.SizeBytes != ditGiB {
		t.Fatalf("after reconcile: %+v, want READY with %d bytes", got, ditGiB)
	}
	if !ditFakeHasImage(e, img.Id) {
		t.Fatal("fake node does not hold the registered image")
	}
}

func TestImageRegisterProjectImageByProjectAdmin(t *testing.T) {
	e := newEnv(t)
	img, err := ditReg(e.admin(), "p1", "p1-data")
	ditOK(t, err)
	if img.ProjectId != "p1" || img.State != vmdv1.ImageState_IMAGE_STATE_CREATING {
		t.Fatalf("fresh project image: %+v", img)
	}
	ditWaitImage(e, img.Id, vmdv1.ImageState_IMAGE_STATE_READY)
	got, err := ditGetImage(e.admin(), img.Id)
	ditOK(t, err)
	if got.SizeBytes != ditGiB {
		t.Fatalf("size %d, want %d", got.SizeBytes, ditGiB)
	}
}

func TestImageRegisterAuthorization(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name    string
		c       *clients
		project string
	}{
		{"editor cannot register a project image", e.editor(), "p1"},
		{"viewer cannot register a project image", e.viewer(), "p1"},
		{"project admin cannot register a shared image", e.admin(), ""},
		{"editor cannot register a shared image", e.editor(), ""},
		{"other tenant admin cannot register into p1", e.as("user:admin-p2", bind("p2", authz.RoleAdmin)), "p1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ditReg(tc.c, tc.project, "img-authz")
			ditWant(t, err, connect.CodePermissionDenied)
		})
	}
}

func TestImageRegisterValidation(t *testing.T) {
	e := newEnv(t)
	root := e.root()
	ctx := context.Background()

	reg := func(project, name, p string) error {
		_, err := root.Image.RegisterImage(ctx, connect.NewRequest(&vmdv1.RegisterImageRequest{
			ProjectId: project, Name: name, NodeId: "n1", Path: p}))
		return err
	}
	ditWant(t, reg("*", "img", "/images/img.qcow2"), connect.CodeInvalidArgument)
	ditWant(t, reg("", "../x", "/images/x.qcow2"), connect.CodeInvalidArgument)
	ditWant(t, reg("", "a/b", "/images/x.qcow2"), connect.CodeInvalidArgument)
	ditWant(t, reg("", "Bad_Name", "/images/x.qcow2"), connect.CodeInvalidArgument)
	ditWant(t, reg("", "", "/images/x.qcow2"), connect.CodeInvalidArgument)
	ditWant(t, reg("", "ok-name", "images/x.qcow2"), connect.CodeInvalidArgument)   // relative
	ditWant(t, reg("", "ok-name", "../x"), connect.CodeInvalidArgument)             // relative, climbing
	ditWant(t, reg("", "ok-name", "/a/../b"), connect.CodeInvalidArgument)          // unclean
	ditWant(t, reg("", "ok-name", "/images/x.qcow2/"), connect.CodeInvalidArgument) // unclean (trailing slash)
	ditWant(t, reg("", "ok-name", ""), connect.CodeInvalidArgument)
	// The same checks apply to a project image.
	ditWant(t, reg("p1", "a/b", "/images/x.qcow2"), connect.CodeInvalidArgument)
	ditOK(t, reg("", "ok-name", "/images/ok.qcow2"))
}

func TestImageRegisterUnknownNode(t *testing.T) {
	e := newEnv(t)
	_, err := e.root().Image.RegisterImage(context.Background(), connect.NewRequest(&vmdv1.RegisterImageRequest{
		Name: "x", NodeId: "ghost", Path: "/images/x.qcow2"}))
	ditWant(t, err, connect.CodeFailedPrecondition)

	_, err = e.root().Image.RegisterImage(context.Background(), connect.NewRequest(&vmdv1.RegisterImageRequest{
		Name: "x", NodeId: "", Path: "/images/x.qcow2"}))
	ditWant(t, err, connect.CodeFailedPrecondition)
}

func TestImageRegisterDuplicateName(t *testing.T) {
	e := newEnv(t)
	_, err := ditReg(e.admin(), "p1", "dup")
	ditOK(t, err)
	_, err = ditReg(e.admin(), "p1", "dup")
	ditWant(t, err, connect.CodeAlreadyExists)

	_, err = ditReg(e.root(), "", "shared-dup")
	ditOK(t, err)
	_, err = ditReg(e.root(), "", "shared-dup")
	ditWant(t, err, connect.CodeAlreadyExists)

	// The same name in another project is fine.
	_, err = ditReg(e.as("user:admin-p2", bind("p2", authz.RoleAdmin)), "p2", "dup")
	ditOK(t, err)
}

func TestImageVisibilityAcrossTenants(t *testing.T) {
	e := newEnv(t)
	admin2 := e.as("user:admin-p2", bind("p2", authz.RoleAdmin))

	shared := e.sharedImage("shared-os")
	p1img, err := ditReg(e.admin(), "p1", "p1-data")
	ditOK(t, err)
	p2img, err := ditReg(admin2, "p2", "p2-data")
	ditOK(t, err)
	ditWaitImage(e, p1img.Id, vmdv1.ImageState_IMAGE_STATE_READY)
	ditWaitImage(e, p2img.Id, vmdv1.ImageState_IMAGE_STATE_READY)

	// Shared images are readable by every tenant, including viewers.
	for name, c := range map[string]*clients{"p1 admin": e.admin(), "p1 viewer": e.viewer(), "p2 admin": admin2} {
		if _, err := ditGetImage(c, shared.Id); err != nil {
			t.Errorf("%s: get shared image: %v", name, err)
		}
	}
	// Project images are not visible across tenants.
	_, err = ditGetImage(admin2, p1img.Id)
	ditWant(t, err, connect.CodeNotFound)
	_, err = ditGetImage(e.admin(), p2img.Id)
	ditWant(t, err, connect.CodeNotFound)
	_, err = ditGetImage(e.viewer(), p2img.Id)
	ditWant(t, err, connect.CodeNotFound)

	has := func(pages [][]string, id string) bool {
		for _, x := range ditFlat(pages) {
			if x == id {
				return true
			}
		}
		return false
	}
	p1 := ditListImages(t, e.admin(), "p1", 0)
	if !has(p1, shared.Id) || !has(p1, p1img.Id) || has(p1, p2img.Id) {
		t.Fatalf("p1 listing %v: want shared + p1 image, no p2 image", ditFlat(p1))
	}
	p2 := ditListImages(t, admin2, "p2", 0)
	if !has(p2, shared.Id) || !has(p2, p2img.Id) || has(p2, p1img.Id) {
		t.Fatalf("p2 listing %v: want shared + p2 image, no p1 image", ditFlat(p2))
	}

	// Listing another tenant's project is refused outright.
	_, err = e.otherTenant().Image.ListImages(context.Background(), connect.NewRequest(&vmdv1.ListImagesRequest{ProjectId: "p1"}))
	ditWant(t, err, connect.CodePermissionDenied)
	_, err = e.admin().Image.ListImages(context.Background(), connect.NewRequest(&vmdv1.ListImagesRequest{ProjectId: "*"}))
	ditWant(t, err, connect.CodeInvalidArgument)

	_, err = ditGetImage(ditBogus(e), shared.Id)
	ditWant(t, err, connect.CodeUnauthenticated)
}

func TestImageListPagination(t *testing.T) {
	e := newEnv(t)
	admin := e.admin()
	for i := range 5 {
		_, err := ditReg(e.root(), "", "shared-"+string(rune('a'+i)))
		ditOK(t, err)
	}
	own, err := ditReg(admin, "p1", "own")
	ditOK(t, err)
	e.tick(1) // all six become READY in one pass
	ditWaitImage(e, own.Id, vmdv1.ImageState_IMAGE_STATE_READY)

	pages := ditListImages(t, admin, "p1", 2)
	if len(pages) != 3 || len(pages[0]) != 2 || len(pages[1]) != 2 || len(pages[2]) != 2 {
		t.Fatalf("page sizes: %v, want [2 2 2]", pages)
	}
	seen := map[string]bool{}
	for _, id := range ditFlat(pages) {
		if seen[id] {
			t.Fatalf("image %s listed twice", id)
		}
		seen[id] = true
	}
	if len(seen) != 6 || !seen[own.Id] {
		t.Fatalf("listed %d images, want 6 including the project image", len(seen))
	}
}

func TestImageRegisterPermanentFailureMarksError(t *testing.T) {
	e := newEnv(t)
	e.node.FailNext("RegisterImage", errs.Invalid("corrupt qcow2 header"))
	img, err := ditReg(e.root(), "", "broken")
	ditOK(t, err)

	e.tick(1)
	got, err := ditGetImage(e.root(), img.Id)
	ditOK(t, err)
	if got.State != vmdv1.ImageState_IMAGE_STATE_ERROR {
		t.Fatalf("state %v, want ERROR", got.State)
	}
	ditReasonHas(t, ditImageReason(t, e, img.Id), "corrupt qcow2 header")

	e.tick(3) // a permanent failure is not retried
	got, err = ditGetImage(e.root(), img.Id)
	ditOK(t, err)
	if got.State != vmdv1.ImageState_IMAGE_STATE_ERROR {
		t.Fatalf("after more ticks: state %v, want ERROR", got.State)
	}
}

func TestImageRegisterTransientFailureStaysCreating(t *testing.T) {
	e := newEnv(t)
	root := e.root()

	e.node.FailNext("RegisterImage", errs.Unavailable("connection reset"))
	img, err := ditReg(root, "", "flaky")
	ditOK(t, err)
	e.tick(1)
	got, err := ditGetImage(root, img.Id)
	ditOK(t, err)
	if got.State != vmdv1.ImageState_IMAGE_STATE_CREATING {
		t.Fatalf("after transient failure: state %v, want CREATING", got.State)
	}
	e.tick(1)
	ditWaitImage(e, img.Id, vmdv1.ImageState_IMAGE_STATE_READY)

	// A node that is down for a while: the image waits, then becomes ready.
	img2, err := ditReg(root, "", "down-for-a-while")
	ditOK(t, err)
	e.node.SetDown(true)
	e.tick(3)
	got, err = ditGetImage(root, img2.Id)
	ditOK(t, err)
	if got.State != vmdv1.ImageState_IMAGE_STATE_CREATING {
		t.Fatalf("while node down: state %v, want CREATING", got.State)
	}
	e.node.SetDown(false)
	ditWaitImage(e, img2.Id, vmdv1.ImageState_IMAGE_STATE_READY)
}

func TestImageDeleteSharedNeedsGlobalAdmin(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("shared-del")

	for name, c := range map[string]*clients{
		"project admin": e.admin(),
		"editor":        e.editor(),
		"viewer":        e.viewer(),
		"other tenant":  e.as("user:admin-p2", bind("p2", authz.RoleAdmin)),
	} {
		t.Run(name, func(t *testing.T) {
			ditWant(t, ditDeleteImage(c, img.Id), connect.CodePermissionDenied)
		})
	}

	ditOK(t, ditDeleteImage(e.root(), img.Id))
	got, err := ditGetImage(e.root(), img.Id)
	ditOK(t, err)
	if got.State != vmdv1.ImageState_IMAGE_STATE_DELETING {
		t.Fatalf("after delete: state %v, want DELETING", got.State)
	}
	e.tick(1)
	ditWaitImageGone(e, img.Id)
	if ditFakeHasImage(e, img.Id) {
		t.Fatal("fake node still holds the deleted image")
	}
}

func TestImageDeleteProjectImageAuthorization(t *testing.T) {
	e := newEnv(t)
	admin2 := e.as("user:admin-p2", bind("p2", authz.RoleAdmin))
	img, err := ditReg(e.admin(), "p1", "proj")
	ditOK(t, err)
	ditWaitImage(e, img.Id, vmdv1.ImageState_IMAGE_STATE_READY)

	ditWant(t, ditDeleteImage(e.editor(), img.Id), connect.CodePermissionDenied)
	ditWant(t, ditDeleteImage(e.viewer(), img.Id), connect.CodePermissionDenied)
	ditWant(t, ditDeleteImage(admin2, img.Id), connect.CodeNotFound)

	ditOK(t, ditDeleteImage(e.admin(), img.Id))
	ditWaitImageGone(e, img.Id)
}

func TestImageDeleteTwiceWhileDeleting(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("twice")
	ditOK(t, ditDeleteImage(e.root(), img.Id))
	ditOK(t, ditDeleteImage(e.root(), img.Id)) // second call while DELETING is a no-op
	e.tick(1)
	ditWaitImageGone(e, img.Id)
	// Ambiguous: once the row is gone the code reports NotFound rather than succeeding again.
	ditWant(t, ditDeleteImage(e.root(), img.Id), connect.CodeNotFound)
}

func TestImageDeleteRefusedWhileDiskUsesIt(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base-in-use")
	d, err := ditCreateDisk(e.admin(), "p1", "user-disk", 10, img.Id, "")
	ditOK(t, err)
	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_READY)

	ditWant(t, ditDeleteImage(e.root(), img.Id), connect.CodeFailedPrecondition)
	got, err := ditGetImage(e.root(), img.Id)
	ditOK(t, err)
	if got.State != vmdv1.ImageState_IMAGE_STATE_READY {
		t.Fatalf("refused delete changed state to %v", got.State)
	}

	_, err = e.admin().Disk.DeleteDisk(context.Background(), connect.NewRequest(&vmdv1.DeleteDiskRequest{Id: d.Id}))
	ditOK(t, err)
	ditWaitDiskGone(e, d.Id)
	ditOK(t, ditDeleteImage(e.root(), img.Id))
	ditWaitImageGone(e, img.Id)
}

// ------------------------------------------------------------------------------------ disks

func TestDiskCreateFromSharedImage(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("debian")
	ed := e.editor()

	d, err := ditCreateDisk(ed, "p1", "data", 10, img.Id, "")
	ditOK(t, err)
	if d.State != vmdv1.DiskState_DISK_STATE_CREATING || d.ImageId != img.Id || d.NodeId != "n1" || d.SizeGb != 10 || d.ProjectId != "p1" {
		t.Fatalf("fresh disk: %+v", d)
	}
	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_READY)
	got, err := ditGetDisk(ed, d.Id)
	ditOK(t, err)
	if got.ImageId != img.Id || got.AttachedVmId != "" {
		t.Fatalf("ready disk: %+v", got)
	}
	if !ditFakeHasDisk(e, d.Id) {
		t.Fatal("fake node does not hold the disk")
	}
}

func TestDiskCreateWithoutImagePlacesOnMostFreeNode(t *testing.T) {
	e := newEnv(t) // a single node, n1: it has the most free space by default
	d, err := ditCreateDisk(e.editor(), "p1", "scratch", 5, "", "")
	ditOK(t, err)
	if d.NodeId != "n1" || d.ImageId != "" || d.State != vmdv1.DiskState_DISK_STATE_CREATING {
		t.Fatalf("fresh disk: %+v", d)
	}
	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_READY)
	if !ditFakeHasDisk(e, d.Id) {
		t.Fatal("fake node does not hold the disk")
	}
}

func TestDiskCreateSizeBounds(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	for _, size := range []int32{0, -5, 4097} {
		_, err := ditCreateDisk(ed, "p1", "d-size", size, "", "")
		ditWant(t, err, connect.CodeInvalidArgument)
	}
	_, err := ditCreateDisk(ed, "p1", "d-one", 1, "", "")
	ditOK(t, err) // the minimum itself is allowed
}

func TestDiskCreateExceedsFreeSpace(t *testing.T) {
	e := newEnv(t) // the fake reports 1 TiB free
	_, err := ditCreateDisk(e.editor(), "p1", "huge", 2000, "", "")
	ditWant(t, err, connect.CodeResourceExhausted)
	_, err = ditCreateDisk(e.editor(), "p1", "exact", 1024, "", "")
	ditOK(t, err)
}

func TestDiskCreateValidation(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	_, err := ditCreateDisk(ed, "p1", "Bad Name", 1, "", "")
	ditWant(t, err, connect.CodeInvalidArgument)
	_, err = ditCreateDisk(ed, "p1", "../x", 1, "", "")
	ditWant(t, err, connect.CodeInvalidArgument)
	_, err = ditCreateDisk(e.admin(), "", "no-project", 1, "", "")
	ditWant(t, err, connect.CodeInvalidArgument)
	_, err = ditCreateDisk(e.admin(), "*", "wildcard", 1, "", "")
	ditWant(t, err, connect.CodeInvalidArgument)
}

func TestDiskCreateDuplicateName(t *testing.T) {
	e := newEnv(t)
	_, err := ditCreateDisk(e.editor(), "p1", "d1", 1, "", "")
	ditOK(t, err)
	_, err = ditCreateDisk(e.editor(), "p1", "d1", 1, "", "")
	ditWant(t, err, connect.CodeAlreadyExists)
	// Another project may reuse the name.
	_, err = ditCreateDisk(e.otherTenant(), "p2", "d1", 1, "", "")
	ditOK(t, err)
}

func TestDiskCreateImageNotReady(t *testing.T) {
	e := newEnv(t)
	img, err := ditReg(e.root(), "", "not-yet")
	ditOK(t, err) // still CREATING: nothing has run the reconciler
	_, err = ditCreateDisk(e.editor(), "p1", "early", 5, img.Id, "")
	ditWant(t, err, connect.CodeFailedPrecondition)
}

func TestDiskCreateImagePlacementAndVisibility(t *testing.T) {
	e := newEnv(t)
	admin2 := e.as("user:admin-p2", bind("p2", authz.RoleAdmin))
	shared := e.sharedImage("shared-base")
	p2img := ditProjectImage(t, e, "p2", "p2-base")

	// Shared images may be used by any tenant.
	d, err := ditCreateDisk(admin2, "p2", "from-shared", 5, shared.Id, "")
	ditOK(t, err)
	if d.ProjectId != "p2" {
		t.Fatalf("disk project %q, want p2", d.ProjectId)
	}

	// Another tenant's project image is invisible.
	_, err = ditCreateDisk(e.editor(), "p1", "steal", 5, p2img.Id, "")
	ditWant(t, err, connect.CodeNotFound)

	// The image lives on n1: placing the disk elsewhere is refused, n1 is fine.
	_, err = ditCreateDisk(e.editor(), "p1", "elsewhere", 5, shared.Id, "n2")
	ditWant(t, err, connect.CodeInvalidArgument)
	_, err = ditCreateDisk(e.editor(), "p1", "same-node", 5, shared.Id, "n1")
	ditOK(t, err)
}

func TestDiskCreateQuota(t *testing.T) {
	e := newEnv(t, ditQuotaDiskGB(25))
	ed := e.editor()
	_, err := ditCreateDisk(ed, "p1", "q1", 10, "", "")
	ditOK(t, err)
	_, err = ditCreateDisk(ed, "p1", "q2", 15, "", "")
	ditOK(t, err) // exactly at the quota
	_, err = ditCreateDisk(ed, "p1", "q3", 1, "", "")
	ditWant(t, err, connect.CodeResourceExhausted)

	// Quotas are per project.
	_, err = ditCreateDisk(e.as("user:editor-p2", bind("p2", authz.RoleEditor)), "p2", "q1", 25, "", "")
	ditOK(t, err)
}

func TestDiskCreateAuthorization(t *testing.T) {
	e := newEnv(t)
	_, err := ditCreateDisk(e.viewer(), "p1", "v", 1, "", "")
	ditWant(t, err, connect.CodePermissionDenied)
	_, err = ditCreateDisk(e.editor(), "p2", "cross", 1, "", "")
	ditWant(t, err, connect.CodePermissionDenied)
	_, err = ditCreateDisk(ditBogus(e), "p1", "anon", 1, "", "")
	ditWant(t, err, connect.CodeUnauthenticated)
}

// Ambiguous: CreateDisk does not check that an explicit node_id (a placement hint without an image)
// exists, unlike RegisterImage and CreateVM which reject unknown nodes up front. The code accepts the
// request and the disk then fails in the reconciler. This test asserts that current behaviour.
// A node hint naming a node that is not registered is refused up front (like RegisterImage and CreateVM do),
// instead of producing a disk that can only fail later in the reconciler.
func TestDiskCreateUnknownPlacementNodeIsRejected(t *testing.T) {
	e := newEnv(t)
	_, err := ditCreateDisk(e.editor(), "p1", "ghost-placed", 5, "", "ghost")
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	l, lerr := e.editor().Disk.ListDisks(context.Background(), connect.NewRequest(&vmdv1.ListDisksRequest{ProjectId: "p1"}))
	if lerr != nil || len(l.Msg.Disks) != 0 {
		t.Fatalf("a rejected request must not leave a disk behind: %v %v", l, lerr)
	}
}

func TestDiskCreateTransientFailureRetried(t *testing.T) {
	e := newEnv(t)
	e.node.FailNext("CreateDisk", errs.Unavailable("blip"))
	d, err := ditCreateDisk(e.editor(), "p1", "retry", 5, "", "")
	ditOK(t, err)

	e.tick(1)
	got, err := ditGetDisk(e.editor(), d.Id)
	ditOK(t, err)
	if got.State != vmdv1.DiskState_DISK_STATE_CREATING {
		t.Fatalf("after transient failure: state %v, want CREATING", got.State)
	}
	e.tick(1)
	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_READY)
	if n := ditCount(e.node.Calls(), "CreateDisk"); n != 2 {
		t.Fatalf("CreateDisk called %d times, want 2 (one failure, one success)", n)
	}
}

func TestDiskCreatePermanentFailureMarksErrorAndFailsVMs(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()

	// (a) A disk that fails permanently is DISK_STATE_ERROR and cannot be booted from.
	d, err := ditCreateDisk(ed, "p1", "bad", 5, "", "")
	ditOK(t, err)
	e.node.FailNext("CreateDisk", errs.Invalid("bad geometry"))
	e.tick(1)
	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_ERROR)
	ditReasonHas(t, ditDiskReason(t, e, d.Id), "bad geometry")
	_, err = ed.VM.CreateVM(ctx, connect.NewRequest(&vmdv1.CreateVMRequest{
		ProjectId: "p1", Name: "boot-bad", Cpus: 1, MemoryMb: 512, Start: true,
		Boot: &vmdv1.CreateVMRequest_DiskId{DiskId: d.Id}}))
	ditWant(t, err, connect.CodeFailedPrecondition)

	// (b) A VM booting from a disk that is still CREATING goes ERROR when that disk fails.
	d2, err := ditCreateDisk(ed, "p1", "bad-later", 5, "", "")
	ditOK(t, err)
	vm, err := ed.VM.CreateVM(ctx, connect.NewRequest(&vmdv1.CreateVMRequest{
		ProjectId: "p1", Name: "boot-later", Cpus: 1, MemoryMb: 512, Start: true,
		Boot: &vmdv1.CreateVMRequest_DiskId{DiskId: d2.Id}}))
	ditOK(t, err)
	e.node.FailNext("CreateDisk", errs.Invalid("bad geometry two"))
	e.tick(1)
	ditWaitVM(e, ed, vm.Msg.Id, vmdv1.VMState_VM_STATE_ERROR)
	ditReasonHas(t, e.getVM(ed, vm.Msg.Id).StateReason, "bad geometry two")

	// (c) A VM created from an image whose boot disk fails permanently goes ERROR with the disk's reason.
	img := e.sharedImage("base-c")
	e.node.FailNext("CreateDisk", errs.Invalid("bad geometry three"))
	vm2 := e.createVM(ed, "p1", "from-image", img.Id, true)
	e.tick(1)
	ditWaitVM(e, ed, vm2.Id, vmdv1.VMState_VM_STATE_ERROR)
	ditReasonHas(t, e.getVM(ed, vm2.Id).StateReason, "bad geometry three")
}

// ------------------------------------------------------------------------------------ disk reads

func TestDiskListPagination(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	var want []string
	for i := range 5 {
		d, err := ditCreateDisk(ed, "p1", "d"+string(rune('a'+i)), 1, "", "")
		ditOK(t, err)
		want = append(want, d.Id)
	}
	pages := ditListDisks(t, e.viewer(), "p1", 2)
	if len(pages) != 3 || len(pages[0]) != 2 || len(pages[1]) != 2 || len(pages[2]) != 1 {
		t.Fatalf("page sizes: %v, want [2 2 1]", pages)
	}
	got := ditFlat(pages)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	if other := ditFlat(ditListDisks(t, e.otherTenant(), "p2", 0)); len(other) != 0 {
		t.Fatalf("p2 sees p1's disks: %v", other)
	}
}

func TestDiskGetAndListAccess(t *testing.T) {
	e := newEnv(t)
	d, err := ditCreateDisk(e.editor(), "p1", "mine", 1, "", "")
	ditOK(t, err)

	_, err = ditGetDisk(e.viewer(), d.Id)
	ditOK(t, err) // viewers read
	_, err = ditGetDisk(e.otherTenant(), d.Id)
	ditWant(t, err, connect.CodeNotFound)
	_, err = ditGetDisk(e.root(), "disk-does-not-exist")
	ditWant(t, err, connect.CodeNotFound)
	_, err = ditGetDisk(ditBogus(e), d.Id)
	ditWant(t, err, connect.CodeUnauthenticated)

	_, err = e.otherTenant().Disk.ListDisks(context.Background(), connect.NewRequest(&vmdv1.ListDisksRequest{ProjectId: "p1"}))
	ditWant(t, err, connect.CodePermissionDenied)
	_, err = e.viewer().Disk.ListDisks(context.Background(), connect.NewRequest(&vmdv1.ListDisksRequest{ProjectId: "*"}))
	ditWant(t, err, connect.CodeInvalidArgument)
	_, err = ditBogus(e).Disk.ListDisks(context.Background(), connect.NewRequest(&vmdv1.ListDisksRequest{ProjectId: "p1"}))
	ditWant(t, err, connect.CodeUnauthenticated)
}

// ------------------------------------------------------------------------------------ resize

func TestDiskResizeGrows(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	d, err := ditCreateDisk(ed, "p1", "grow", 10, "", "")
	ditOK(t, err)
	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_READY)

	res, err := ed.Disk.ResizeDisk(context.Background(), connect.NewRequest(&vmdv1.ResizeDiskRequest{Id: d.Id, SizeGb: 20}))
	ditOK(t, err)
	if res.Msg.SizeGb != 20 {
		t.Fatalf("resize response size %d, want 20", res.Msg.SizeGb)
	}
	got, err := ditGetDisk(ed, d.Id)
	ditOK(t, err)
	if got.SizeGb != 20 {
		t.Fatalf("stored size %d, want 20", got.SizeGb)
	}
	if !ditFakeDiskSize(e, d.Id, 20) {
		t.Fatal("fake node does not hold the grown disk")
	}
}

func TestDiskResizeRejectsInvalidSizes(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	d, err := ditCreateDisk(ed, "p1", "sizes", 10, "", "")
	ditOK(t, err)
	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_READY)

	for _, size := range []int32{0, 5, 10, 4097} {
		_, err := ed.Disk.ResizeDisk(context.Background(), connect.NewRequest(&vmdv1.ResizeDiskRequest{Id: d.Id, SizeGb: size}))
		ditWant(t, err, connect.CodeInvalidArgument)
	}
	got, err := ditGetDisk(ed, d.Id)
	ditOK(t, err)
	if got.SizeGb != 10 || !ditFakeDiskSize(e, d.Id, 10) {
		t.Fatalf("rejected resizes changed the disk: stored %d", got.SizeGb)
	}
}

func TestDiskResizeNotReady(t *testing.T) {
	e := newEnv(t)
	d, err := ditCreateDisk(e.editor(), "p1", "creating", 10, "", "")
	ditOK(t, err)
	_, err = e.editor().Disk.ResizeDisk(context.Background(), connect.NewRequest(&vmdv1.ResizeDiskRequest{Id: d.Id, SizeGb: 20}))
	ditWant(t, err, connect.CodeFailedPrecondition)
}

func TestDiskResizeAttachedToRunningVM(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("web")
	bootDisk := vm.DiskIds[0]
	_, err := e.editor().Disk.ResizeDisk(context.Background(), connect.NewRequest(&vmdv1.ResizeDiskRequest{Id: bootDisk, SizeGb: 20}))
	ditWant(t, err, connect.CodeFailedPrecondition)

	if got := e.getVM(e.editor(), vm.Id); got.State != vmdv1.VMState_VM_STATE_RUNNING {
		t.Fatalf("VM changed to %v", got.State)
	}
	d, err := ditGetDisk(e.editor(), bootDisk)
	ditOK(t, err)
	if d.SizeGb != 10 || !ditFakeDiskSize(e, bootDisk, 10) {
		t.Fatalf("disk resized while in use: stored %d", d.SizeGb)
	}
}

func TestDiskResizeAttachedToStoppedVM(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	img := e.sharedImage("base-stopped")
	vm := e.createVM(ed, "p1", "stopped", img.Id, false)
	ditWaitVM(e, ed, vm.Id, vmdv1.VMState_VM_STATE_STOPPED)

	bootDisk := e.getVM(ed, vm.Id).DiskIds[0]
	_, err := ed.Disk.ResizeDisk(context.Background(), connect.NewRequest(&vmdv1.ResizeDiskRequest{Id: bootDisk, SizeGb: 20}))
	ditOK(t, err)
	d, err := ditGetDisk(ed, bootDisk)
	ditOK(t, err)
	if d.SizeGb != 20 || !ditFakeDiskSize(e, bootDisk, 20) {
		t.Fatalf("stopped VM's disk not grown: stored %d", d.SizeGb)
	}
}

func TestDiskResizeQuota(t *testing.T) {
	e := newEnv(t, ditQuotaDiskGB(25))
	ed := e.editor()
	d, err := ditCreateDisk(ed, "p1", "quota-grow", 10, "", "")
	ditOK(t, err)
	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_READY)

	_, err = ed.Disk.ResizeDisk(context.Background(), connect.NewRequest(&vmdv1.ResizeDiskRequest{Id: d.Id, SizeGb: 20}))
	ditOK(t, err) // 20 GB in use of 25
	_, err = ed.Disk.ResizeDisk(context.Background(), connect.NewRequest(&vmdv1.ResizeDiskRequest{Id: d.Id, SizeGb: 30}))
	ditWant(t, err, connect.CodeResourceExhausted)
}

func TestDiskResizeAuthorization(t *testing.T) {
	e := newEnv(t)
	d, err := ditCreateDisk(e.editor(), "p1", "authz", 10, "", "")
	ditOK(t, err)
	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_READY)

	_, err = e.viewer().Disk.ResizeDisk(context.Background(), connect.NewRequest(&vmdv1.ResizeDiskRequest{Id: d.Id, SizeGb: 20}))
	ditWant(t, err, connect.CodePermissionDenied)
	_, err = e.otherTenant().Disk.ResizeDisk(context.Background(), connect.NewRequest(&vmdv1.ResizeDiskRequest{Id: d.Id, SizeGb: 20}))
	ditWant(t, err, connect.CodeNotFound)
}

// ------------------------------------------------------------------------------------ delete

func TestDiskDeleteAttachedRefused(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	vm := e.runningVM("busy")
	_, err := ed.Disk.DeleteDisk(context.Background(), connect.NewRequest(&vmdv1.DeleteDiskRequest{Id: vm.DiskIds[0]}))
	ditWant(t, err, connect.CodeFailedPrecondition)

	img := e.sharedImage("base-stopped-del")
	stopped := e.createVM(ed, "p1", "stopped-del", img.Id, false)
	ditWaitVM(e, ed, stopped.Id, vmdv1.VMState_VM_STATE_STOPPED)
	_, err = ed.Disk.DeleteDisk(context.Background(), connect.NewRequest(&vmdv1.DeleteDiskRequest{Id: e.getVM(ed, stopped.Id).DiskIds[0]}))
	ditWant(t, err, connect.CodeFailedPrecondition)

	d, err := ditGetDisk(ed, vm.DiskIds[0])
	ditOK(t, err)
	if d.State != vmdv1.DiskState_DISK_STATE_READY || d.AttachedVmId != vm.Id {
		t.Fatalf("refused delete changed the disk: %+v", d)
	}
}

func TestDiskDeleteDetachedRemovesFromFake(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	d, err := ditCreateDisk(ed, "p1", "gone-soon", 5, "", "")
	ditOK(t, err)
	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_READY)

	_, err = ed.Disk.DeleteDisk(context.Background(), connect.NewRequest(&vmdv1.DeleteDiskRequest{Id: d.Id}))
	ditOK(t, err)
	got, err := ditGetDisk(ed, d.Id)
	ditOK(t, err)
	if got.State != vmdv1.DiskState_DISK_STATE_DELETING {
		t.Fatalf("after delete: state %v, want DELETING", got.State)
	}
	ditWaitDiskGone(e, d.Id)
	if ditFakeHasDisk(e, d.Id) {
		t.Fatal("fake node still holds the deleted disk")
	}
}

func TestDiskDeleteTwiceWhileDeleting(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	d, err := ditCreateDisk(ed, "p1", "twice", 5, "", "")
	ditOK(t, err)
	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_READY)

	ditOK(t, func() error {
		_, err := ed.Disk.DeleteDisk(context.Background(), connect.NewRequest(&vmdv1.DeleteDiskRequest{Id: d.Id}))
		return err
	}())
	ditOK(t, func() error {
		_, err := ed.Disk.DeleteDisk(context.Background(), connect.NewRequest(&vmdv1.DeleteDiskRequest{Id: d.Id}))
		return err
	}()) // no-op while DELETING
	e.tick(1)
	ditWaitDiskGone(e, d.Id)
	// Ambiguous: after the row is gone a further delete reports NotFound rather than succeeding.
	_, err = ed.Disk.DeleteDisk(context.Background(), connect.NewRequest(&vmdv1.DeleteDiskRequest{Id: d.Id}))
	ditWant(t, err, connect.CodeNotFound)
}

func TestDiskDeleteAuthorization(t *testing.T) {
	e := newEnv(t)
	d, err := ditCreateDisk(e.editor(), "p1", "authz-del", 5, "", "")
	ditOK(t, err)
	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_READY)

	_, err = e.viewer().Disk.DeleteDisk(context.Background(), connect.NewRequest(&vmdv1.DeleteDiskRequest{Id: d.Id}))
	ditWant(t, err, connect.CodePermissionDenied)
	_, err = e.otherTenant().Disk.DeleteDisk(context.Background(), connect.NewRequest(&vmdv1.DeleteDiskRequest{Id: d.Id}))
	ditWant(t, err, connect.CodeNotFound)
	got, err := ditGetDisk(e.viewer(), d.Id)
	ditOK(t, err)
	if got.State != vmdv1.DiskState_DISK_STATE_READY {
		t.Fatalf("refused delete changed state to %v", got.State)
	}
}

// ------------------------------------------------------------------------------------ snapshots

func TestDiskSnapshotDetachedDiskBootsVM(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()
	img := e.sharedImage("debian-snap")
	d, err := ditCreateDisk(ed, "p1", "data", 10, img.Id, "")
	ditOK(t, err)
	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_READY)

	snap, err := ed.Disk.SnapshotDisk(ctx, connect.NewRequest(&vmdv1.SnapshotDiskRequest{Id: d.Id, ImageName: "data-snap"}))
	ditOK(t, err)
	if snap.Msg.State != vmdv1.ImageState_IMAGE_STATE_CREATING || snap.Msg.SourceDiskId != d.Id || snap.Msg.ProjectId != "p1" || snap.Msg.NodeId != "n1" {
		t.Fatalf("fresh snapshot image: %+v", snap.Msg)
	}
	ditWaitImage(e, snap.Msg.Id, vmdv1.ImageState_IMAGE_STATE_READY)
	got, err := ditGetImage(ed, snap.Msg.Id)
	ditOK(t, err)
	if got.SizeBytes != 10*ditGiB {
		t.Fatalf("snapshot size %d, want %d", got.SizeBytes, 10*ditGiB)
	}

	vm := e.createVM(ed, "p1", "from-snap", snap.Msg.Id, true)
	ditWaitVM(e, ed, vm.Id, vmdv1.VMState_VM_STATE_RUNNING)
}

func TestDiskSnapshotOfStoppedVMDisk(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	img := e.sharedImage("base-snap-stopped")
	vm := e.createVM(ed, "p1", "stopped-snap", img.Id, false)
	ditWaitVM(e, ed, vm.Id, vmdv1.VMState_VM_STATE_STOPPED)
	boot := e.getVM(ed, vm.Id).DiskIds[0]

	snap, err := ed.Disk.SnapshotDisk(context.Background(), connect.NewRequest(&vmdv1.SnapshotDiskRequest{Id: boot, ImageName: "stopped-copy"}))
	ditOK(t, err)
	ditWaitImage(e, snap.Msg.Id, vmdv1.ImageState_IMAGE_STATE_READY)
	got, err := ditGetImage(ed, snap.Msg.Id)
	ditOK(t, err)
	if got.SourceDiskId != boot {
		t.Fatalf("source disk %q, want %q", got.SourceDiskId, boot)
	}
	if e.getVM(ed, vm.Id).State != vmdv1.VMState_VM_STATE_STOPPED {
		t.Fatal("snapshot disturbed the stopped VM")
	}
}

func TestDiskSnapshotOfRunningVMRefused(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("snap-busy")
	_, err := e.editor().Disk.SnapshotDisk(context.Background(), connect.NewRequest(&vmdv1.SnapshotDiskRequest{
		Id: vm.DiskIds[0], ImageName: "busy-copy"}))
	ditWant(t, err, connect.CodeFailedPrecondition)
}

func TestDiskSnapshotValidation(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()
	d, err := ditCreateDisk(ed, "p1", "snap-src", 5, "", "")
	ditOK(t, err)

	// Disk still CREATING.
	_, err = ed.Disk.SnapshotDisk(ctx, connect.NewRequest(&vmdv1.SnapshotDiskRequest{Id: d.Id, ImageName: "early"}))
	ditWant(t, err, connect.CodeFailedPrecondition)

	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_READY)
	_, err = ed.Disk.SnapshotDisk(ctx, connect.NewRequest(&vmdv1.SnapshotDiskRequest{Id: d.Id, ImageName: "Bad_Name"}))
	ditWant(t, err, connect.CodeInvalidArgument)
	_, err = ed.Disk.SnapshotDisk(ctx, connect.NewRequest(&vmdv1.SnapshotDiskRequest{Id: d.Id, ImageName: "../up"}))
	ditWant(t, err, connect.CodeInvalidArgument)

	_, err = e.viewer().Disk.SnapshotDisk(ctx, connect.NewRequest(&vmdv1.SnapshotDiskRequest{Id: d.Id, ImageName: "viewer-try"}))
	ditWant(t, err, connect.CodePermissionDenied)
	_, err = e.otherTenant().Disk.SnapshotDisk(ctx, connect.NewRequest(&vmdv1.SnapshotDiskRequest{Id: d.Id, ImageName: "other-try"}))
	ditWant(t, err, connect.CodeNotFound)

	_, err = ed.Disk.SnapshotDisk(ctx, connect.NewRequest(&vmdv1.SnapshotDiskRequest{Id: d.Id, ImageName: "dup"}))
	ditOK(t, err)
	_, err = ed.Disk.SnapshotDisk(ctx, connect.NewRequest(&vmdv1.SnapshotDiskRequest{Id: d.Id, ImageName: "dup"}))
	ditWant(t, err, connect.CodeAlreadyExists)
}

func TestDiskSnapshotPermanentFailureMarksImageError(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	d, err := ditCreateDisk(ed, "p1", "snap-fail", 5, "", "")
	ditOK(t, err)
	ditWaitDisk(e, d.Id, vmdv1.DiskState_DISK_STATE_READY)

	e.node.FailNext("SnapshotDisk", errs.Precondition("disk busy on host"))
	snap, err := ed.Disk.SnapshotDisk(context.Background(), connect.NewRequest(&vmdv1.SnapshotDiskRequest{Id: d.Id, ImageName: "broken-snap"}))
	ditOK(t, err)
	e.tick(1)
	got, err := ditGetImage(ed, snap.Msg.Id)
	ditOK(t, err)
	if got.State != vmdv1.ImageState_IMAGE_STATE_ERROR {
		t.Fatalf("state %v, want ERROR", got.State)
	}
	ditReasonHas(t, ditImageReason(t, e, snap.Msg.Id), "disk busy on host")
}
