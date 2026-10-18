package qemudriver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

// srcImage writes an image file src/<name> under a temp dir and returns its absolute path.
func srcImage(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRegisterImageLinksAndReportsSize(t *testing.T) {
	h := newHarness(t)
	src := srcImage(t, "golden.qcow2")
	h.cmd.sizes["src/golden.qcow2"] = 8 << 30
	resp, err := h.d.RegisterImage(context.Background(), &nodev1.RegisterImageRequest{ImageId: "img1", Path: src})
	if err != nil {
		t.Fatalf("RegisterImage: %v", err)
	}
	if resp.GetSizeBytes() != 8<<30 {
		t.Errorf("size = %d, want 8 GiB", resp.GetSizeBytes())
	}
	target, err := os.Readlink(filepath.Join(h.dir, "images", "img1.qcow2"))
	if err != nil || target != src {
		t.Errorf("image link = %q (%v), want %q", target, err, src)
	}
}

func TestRegisterImageRejections(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	src := srcImage(t, "golden.qcow2")
	raw := srcImage(t, "raw.img")
	h.cmd.formats["src/raw.img"] = "raw"
	bad := srcImage(t, "bad.bin")
	h.cmd.badImages["src/bad.bin"] = true

	if _, err := h.d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "img1", Path: "relative.qcow2"}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("relative path: err = %v, want invalid", err)
	}
	if _, err := h.d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "img1", Path: filepath.Join(h.dir, "absent.qcow2")}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing file: err = %v, want not found", err)
	}
	if _, err := h.d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "img1", Path: bad}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("unreadable image: err = %v, want invalid", err)
	}
	if _, err := h.d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "img1", Path: raw}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("raw image: err = %v, want invalid", err)
	}
	if _, err := h.d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "img1", Path: src}); err != nil {
		t.Fatalf("first RegisterImage: %v", err)
	}
	if _, err := h.d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "img1", Path: src}); !errors.Is(err, errs.ErrAlreadyExists) {
		t.Errorf("duplicate: err = %v, want already exists", err)
	}
}

func TestDeleteImageRemovesLinkOnly(t *testing.T) {
	h := newHarness(t)
	src := srcImage(t, "golden.qcow2")
	if _, err := h.d.DeleteImage(context.Background(), &nodev1.DeleteImageRequest{ImageId: "img1"}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown image: err = %v, want not found", err)
	}
	if _, err := h.d.RegisterImage(context.Background(), &nodev1.RegisterImageRequest{ImageId: "img1", Path: src}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.DeleteImage(context.Background(), &nodev1.DeleteImageRequest{ImageId: "img1"}); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("deleting the image removed the registered file: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(h.dir, "images", "img1.qcow2")); !os.IsNotExist(err) {
		t.Errorf("image link still present: %v", err)
	}
}

func TestCreateDisk(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "d1", SizeGb: 10}); err != nil {
		t.Fatalf("CreateDisk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "disks", "d1.qcow2")); err != nil {
		t.Errorf("disk file missing: %v", err)
	}
	if got := h.cmd.commandsNamed("qemu-img create -q -f qcow2 "); len(got) != 1 || !strings.HasSuffix(got[0], " 10G") {
		t.Errorf("qemu-img create calls = %v", got)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "disks", ".d1.tmp.qcow2")); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}
	if _, err := h.d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "d1", SizeGb: 10}); !errors.Is(err, errs.ErrAlreadyExists) {
		t.Errorf("duplicate: err = %v, want already exists", err)
	}
	if _, err := h.d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "d2", SizeGb: 0}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("size 0: err = %v, want invalid", err)
	}
	if _, err := h.d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "d2", SizeGb: 1, BackingImageId: "nope"}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing backing: err = %v, want not found", err)
	}
}

func TestCreateOverlayOnImage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	src := srcImage(t, "golden.qcow2")
	h.cmd.sizes["src/golden.qcow2"] = 8 << 30
	h.cmd.sizes["images/img1.qcow2"] = 8 << 30
	if _, err := h.d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "img1", Path: src}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "d1", BackingImageId: "img1", SizeGb: 4}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("disk smaller than image: err = %v, want invalid", err)
	}
	if _, err := h.d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "d1", BackingImageId: "img1", SizeGb: 10}); err != nil {
		t.Fatalf("overlay: %v", err)
	}
	want := "-b " + filepath.Join(h.dir, "images", "img1.qcow2") + " -F qcow2"
	if got := h.cmd.commandsNamed("qemu-img create"); len(got) != 1 || !strings.Contains(got[0], want) {
		t.Errorf("create call = %v, want backing %q", got, want)
	}
}

func TestCreateDiskFailureLeavesNothing(t *testing.T) {
	h := newHarness(t)
	h.cmd.fail["qemu-img create"] = errors.New("disk I/O error")
	if _, err := h.d.CreateDisk(context.Background(), &nodev1.CreateDiskRequest{DiskId: "d1", SizeGb: 10}); !errors.Is(err, errs.ErrUnavailable) {
		t.Fatalf("err = %v, want unavailable", err)
	}
	entries, _ := os.ReadDir(filepath.Join(h.dir, "disks"))
	if len(entries) != 0 {
		t.Errorf("disks dir after failed create: %v", entries)
	}
}

func TestResizeDisk(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.mkDisk("d1")
	h.cmd.sizes["disks/d1.qcow2"] = 10 << 30
	path := filepath.Join(h.dir, "disks", "d1.qcow2")

	if _, err := h.d.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: "d1", SizeGb: 20}); err != nil {
		t.Fatalf("grow: %v", err)
	}
	if got := h.cmd.commandsNamed("qemu-img resize " + path + " 20G"); len(got) != 1 {
		t.Errorf("resize calls = %v", got)
	}
	h.cmd.sizes["disks/d1.qcow2"] = 20 << 30
	if _, err := h.d.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: "d1", SizeGb: 20}); err != nil {
		t.Errorf("same size: %v", err)
	}
	if got := h.cmd.commandsNamed("qemu-img resize"); len(got) != 1 {
		t.Errorf("same size ran qemu-img resize again: %v", got)
	}
	if _, err := h.d.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: "d1", SizeGb: 5}); !errors.Is(err, errs.ErrFailedPrecondition) {
		t.Errorf("shrink: err = %v, want precondition", err)
	}
	if _, err := h.d.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: "nope", SizeGb: 5}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown disk: err = %v, want not found", err)
	}
	if _, err := h.d.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: "d1", SizeGb: 0}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("size 0: err = %v, want invalid", err)
	}
}

func TestResizeDiskRefusedWhileInUse(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	if _, err := h.d.ResizeDisk(context.Background(), &nodev1.ResizeDiskRequest{DiskId: "d1", SizeGb: 20}); !errors.Is(err, errs.ErrFailedPrecondition) {
		t.Errorf("err = %v, want precondition", err)
	}
	if got := h.cmd.commandsNamed("qemu-img resize"); len(got) != 0 {
		t.Errorf("resize ran while in use: %v", got)
	}
}

func TestDeleteDiskRefusedWhileVMRuns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.mkDisk("d1")
	h.start("vm1", "d1")
	if _, err := h.d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: "d1"}); !errors.Is(err, errs.ErrFailedPrecondition) {
		t.Fatalf("delete while running: err = %v, want precondition", err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "disks", "d1.qcow2")); err != nil {
		t.Fatalf("disk removed despite the refusal: %v", err)
	}
	if _, err := h.d.StopVM(ctx, &nodev1.StopVMRequest{VmId: "vm1", Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: "d1"}); err != nil {
		t.Fatalf("delete after stop: %v", err)
	}
	if _, err := h.d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: "d1"}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("second delete: err = %v, want not found", err)
	}
}

func TestSnapshotDisk(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.mkDisk("d1")
	h.cmd.sizes["disks/d1.qcow2"] = 6 << 30

	resp, err := h.d.SnapshotDisk(ctx, &nodev1.SnapshotDiskRequest{DiskId: "d1", ImageId: "snap1"})
	if err != nil {
		t.Fatalf("SnapshotDisk: %v", err)
	}
	if resp.GetSizeBytes() != 6<<30 {
		t.Errorf("size = %d, want 6 GiB", resp.GetSizeBytes())
	}
	fi, err := os.Lstat(filepath.Join(h.dir, "images", "snap1.qcow2"))
	if err != nil || !fi.Mode().IsRegular() {
		t.Errorf("snapshot image is not a regular file: %v, %v", fi, err)
	}
	if got := h.cmd.commandsNamed("qemu-img convert -q -O qcow2 " + filepath.Join(h.dir, "disks", "d1.qcow2")); len(got) != 1 {
		t.Errorf("convert calls = %v", got)
	}
	if _, err := h.d.SnapshotDisk(ctx, &nodev1.SnapshotDiskRequest{DiskId: "d1", ImageId: "snap1"}); !errors.Is(err, errs.ErrAlreadyExists) {
		t.Errorf("existing image: err = %v, want already exists", err)
	}
	if _, err := h.d.SnapshotDisk(ctx, &nodev1.SnapshotDiskRequest{DiskId: "nope", ImageId: "snap2"}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown disk: err = %v, want not found", err)
	}
}

func TestSnapshotDiskRefusedWhileInUse(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	if _, err := h.d.SnapshotDisk(context.Background(), &nodev1.SnapshotDiskRequest{DiskId: "d1", ImageId: "snap1"}); !errors.Is(err, errs.ErrFailedPrecondition) {
		t.Errorf("err = %v, want precondition", err)
	}
	if got := h.cmd.commandsNamed("qemu-img convert"); len(got) != 0 {
		t.Errorf("convert ran while in use: %v", got)
	}
}

func TestDiskCommandFailureIsUnavailable(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.cmd.fail["qemu-img resize"] = errors.New("qemu-img: lock held")
	h.cmd.sizes["disks/d1.qcow2"] = 1 << 30
	if _, err := h.d.ResizeDisk(context.Background(), &nodev1.ResizeDiskRequest{DiskId: "d1", SizeGb: 2}); !errors.Is(err, errs.ErrUnavailable) {
		t.Errorf("err = %v, want unavailable", err)
	}
}
