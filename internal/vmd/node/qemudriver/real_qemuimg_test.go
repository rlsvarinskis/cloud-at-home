package qemudriver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	nodev1 "example.com/homecloud/gen/homecloud/node/v1"
	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/vmd/node/guestexec"
)

type realInfo struct {
	VirtualSize int64  `json:"virtual-size"`
	Format      string `json:"format"`
	Backing     string `json:"backing-filename"`
}

func qemuImgInfo(t *testing.T, path string) realInfo {
	t.Helper()
	out, err := exec.Command("qemu-img", "info", "--output=json", path).Output()
	if err != nil {
		t.Fatalf("qemu-img info %s: %v", path, err)
	}
	var i realInfo
	if err := json.Unmarshal(out, &i); err != nil {
		t.Fatal(err)
	}
	return i
}

// The disk and image layer against the REAL qemu-img (skipped where it is not installed): argument order,
// backing chains, growing, flattening a snapshot, and refusing to shrink.
func TestDiskAndImageLayerAgainstRealQemuImg(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img is not installed")
	}
	ctx := context.Background()
	data := t.TempDir()
	d, err := New(Config{
		DataDir: data, Keys: fixedKeys("ssh-ed25519 AAAATEST test"),
		NewRunner: func(int) guestexec.Runner { return noGuest{} },
		// Cmd, Procs and FS stay nil: the real host.
	})
	if err != nil {
		t.Fatal(err)
	}

	// A real 1 GiB qcow2 to act as the golden image, registered like an operator would.
	golden := filepath.Join(t.TempDir(), "golden.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", golden, "1G").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "golden", Path: golden}); err != nil {
		t.Fatalf("register image: %v", err)
	}
	if _, err := d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "golden", Path: golden}); !errors.Is(err, errs.ErrAlreadyExists) {
		t.Fatalf("registering twice: %v", err)
	}
	notQcow := filepath.Join(t.TempDir(), "raw.img")
	if err := os.WriteFile(notQcow, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "raw", Path: notQcow}); err == nil {
		t.Fatal("a file that is not a qcow2 must be refused")
	}

	// A copy-on-write overlay on that image, larger than it.
	if _, err := d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "d1", BackingImageId: "golden", SizeGb: 2}); err != nil {
		t.Fatalf("create overlay: %v", err)
	}
	disk := filepath.Join(data, "disks", "d1.qcow2")
	if i := qemuImgInfo(t, disk); i.Format != "qcow2" || i.Backing == "" || i.VirtualSize != 2<<30 {
		t.Fatalf("overlay: %+v", i)
	}
	if _, err := d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "d1", BackingImageId: "golden", SizeGb: 2}); !errors.Is(err, errs.ErrAlreadyExists) {
		t.Fatalf("creating a disk twice: %v", err)
	}
	if _, err := d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "tiny", BackingImageId: "golden", SizeGb: 0}); err == nil {
		t.Fatal("size 0 must be refused")
	}

	// Grow, but never shrink.
	if _, err := d.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: "d1", SizeGb: 3}); err != nil {
		t.Fatalf("grow: %v", err)
	}
	if i := qemuImgInfo(t, disk); i.VirtualSize != 3<<30 {
		t.Fatalf("after growing: %+v", i)
	}
	if _, err := d.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: "d1", SizeGb: 2}); !errors.Is(err, errs.ErrFailedPrecondition) {
		t.Fatalf("shrinking must be refused: %v", err)
	}

	// Snapshot: a flattened, independent image (no backing file).
	snap, err := d.SnapshotDisk(ctx, &nodev1.SnapshotDiskRequest{DiskId: "d1", ImageId: "snap1"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.SizeBytes <= 0 {
		t.Fatalf("snapshot size %d", snap.SizeBytes)
	}
	if i := qemuImgInfo(t, filepath.Join(data, "images", "snap1.qcow2")); i.Backing != "" || i.VirtualSize != 3<<30 || i.Format != "qcow2" {
		t.Fatalf("snapshot must be flattened and keep the size: %+v", i)
	}
	if _, err := d.SnapshotDisk(ctx, &nodev1.SnapshotDiskRequest{DiskId: "d1", ImageId: "snap1"}); !errors.Is(err, errs.ErrAlreadyExists) && !errors.Is(err, errs.ErrFailedPrecondition) {
		t.Fatalf("snapshotting onto an existing image: %v", err)
	}

	// A disk made from the snapshot works too; then clean up.
	if _, err := d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "d2", BackingImageId: "snap1", SizeGb: 3}); err != nil {
		t.Fatalf("disk from snapshot: %v", err)
	}
	for _, id := range []string{"d1", "d2"} {
		if _, err := d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: id}); err != nil {
			t.Fatalf("delete %s: %v", id, err)
		}
	}
	if _, err := os.Stat(disk); !os.IsNotExist(err) {
		t.Fatal("the disk file should be gone")
	}
	if _, err := d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: "d1"}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("deleting twice: %v", err)
	}
	for _, id := range []string{"snap1", "golden"} {
		if _, err := d.DeleteImage(ctx, &nodev1.DeleteImageRequest{ImageId: id}); err != nil {
			t.Fatalf("delete image %s: %v", id, err)
		}
	}
	if _, err := os.Stat(golden); err != nil {
		t.Fatal("deleting a registered image must not delete the operator's original file")
	}
}
