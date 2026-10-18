package qemudriver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/qemu"
)

// imgInfo returns the size and format of the image at path, from qemu-img info.
func (d *Driver) imgInfo(ctx context.Context, path string) (qemu.ImageInfo, error) {
	out, err := d.cfg.Cmd.Run(ctx, "qemu-img", "info", "--output=json", path)
	if err != nil {
		return qemu.ImageInfo{}, fmt.Errorf("qemu-img info %s: %w", path, err)
	}
	info, err := qemu.ParseImageInfo(out)
	if err != nil {
		return qemu.ImageInfo{}, fmt.Errorf("qemu-img info %s: %w", path, err)
	}
	return info, nil
}

// qemuImgTo runs qemu-img with args, which must write to tmp, and then renames tmp to dst. On failure
// the temporary file is removed, so a half-written image never appears under its final name.
func (d *Driver) qemuImgTo(ctx context.Context, args []string, tmp, dst string) error {
	if _, err := d.cfg.Cmd.Run(ctx, "qemu-img", args...); err != nil {
		os.Remove(tmp)
		return errs.Unavailable("qemu-img %s: %v", args[0], err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("publish %s: %w", dst, err)
	}
	return nil
}

// RegisterImage adopts the existing qcow2 file at r.Path as image r.ImageId. The image is a symlink
// to that file, so the file is not copied.
func (d *Driver) RegisterImage(ctx context.Context, r *nodev1.RegisterImageRequest) (*nodev1.RegisterImageResponse, error) {
	id := r.GetImageId()
	if err := checkID("image id", id); err != nil {
		return nil, err
	}
	src := r.GetPath()
	if !filepath.IsAbs(src) {
		return nil, errs.Invalid("image path %q must be absolute", src)
	}
	unlock := d.lockKey(imageKey(id))
	defer unlock()

	dst := d.imagePath(id)
	if ok, err := fileExists(dst); err != nil {
		return nil, err
	} else if ok {
		return nil, errs.Exists("image %q", id)
	}
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errs.NotFound("image file %q", src)
		}
		return nil, fmt.Errorf("stat image file: %w", err)
	}
	info, err := d.imgInfo(ctx, src)
	if err != nil {
		return nil, errs.Invalid("%q is not a disk image: %v", src, err)
	}
	if info.Format != "qcow2" {
		return nil, errs.Invalid("image %q is %s, want qcow2", src, info.Format)
	}
	if err := os.Symlink(src, dst); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, errs.Exists("image %q", id)
		}
		return nil, fmt.Errorf("link image %q: %w", id, err)
	}
	return &nodev1.RegisterImageResponse{SizeBytes: info.VirtualSize}, nil
}

// DeleteImage removes image r.ImageId. A registered image's file is left in place: only the link
// goes. Disks overlaid on the image are not checked.
func (d *Driver) DeleteImage(_ context.Context, r *nodev1.DeleteImageRequest) (*nodev1.DeleteImageResponse, error) {
	id := r.GetImageId()
	if err := checkID("image id", id); err != nil {
		return nil, err
	}
	unlock := d.lockKey(imageKey(id))
	defer unlock()

	dst := d.imagePath(id)
	ok, err := fileExists(dst)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errs.NotFound("image %q", id)
	}
	if err := os.Remove(dst); err != nil {
		return nil, fmt.Errorf("remove image %q: %w", id, err)
	}
	return &nodev1.DeleteImageResponse{}, nil
}

// CreateDisk creates disk r.DiskId of r.SizeGb gibibytes. With r.BackingImageId the disk is a
// copy-on-write overlay on that image, and it must be at least as large as the image.
func (d *Driver) CreateDisk(ctx context.Context, r *nodev1.CreateDiskRequest) (*nodev1.CreateDiskResponse, error) {
	id := r.GetDiskId()
	if err := checkID("disk id", id); err != nil {
		return nil, err
	}
	size := int(r.GetSizeGb())
	if size < 1 {
		return nil, errs.Invalid("size_gb must be at least 1, got %d", size)
	}
	backing := r.GetBackingImageId()
	if backing != "" {
		if err := checkID("backing image id", backing); err != nil {
			return nil, err
		}
	}

	unlock := d.lockKey(diskKey(id))
	defer unlock()
	dst := d.diskPath(id)
	if ok, err := fileExists(dst); err != nil {
		return nil, err
	} else if ok {
		return nil, errs.Exists("disk %q", id)
	}

	var backingPath string
	if backing != "" {
		unlockImg := d.lockKey(imageKey(backing))
		defer unlockImg()
		backingPath = d.imagePath(backing)
		if _, err := os.Stat(backingPath); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, errs.NotFound("backing image %q", backing)
			}
			return nil, fmt.Errorf("stat backing image %q: %w", backing, err)
		}
		info, err := d.imgInfo(ctx, backingPath)
		if err != nil {
			return nil, errs.Unavailable("%v", err)
		}
		if int64(size)*gib < info.VirtualSize {
			return nil, errs.Invalid("disk of %d GB is smaller than image %q (%d bytes)", size, backing, info.VirtualSize)
		}
	}

	tmp := filepath.Join(d.disksDir(), "."+id+".tmp.qcow2")
	args, err := qemu.ImgCreateArgs(tmp, backingPath, size)
	if err != nil {
		return nil, errs.Invalid("%v", err)
	}
	if err := d.qemuImgTo(ctx, args, tmp, dst); err != nil {
		return nil, err
	}
	return &nodev1.CreateDiskResponse{}, nil
}

// ResizeDisk grows disk r.DiskId to r.SizeGb gibibytes. Growing to the current size does nothing.
// Shrinking is a precondition failure, and so is resizing a disk that a live VM uses.
func (d *Driver) ResizeDisk(ctx context.Context, r *nodev1.ResizeDiskRequest) (*nodev1.ResizeDiskResponse, error) {
	id := r.GetDiskId()
	if err := checkID("disk id", id); err != nil {
		return nil, err
	}
	size := int(r.GetSizeGb())
	if size < 1 {
		return nil, errs.Invalid("size_gb must be at least 1, got %d", size)
	}
	unlock := d.lockKey(diskKey(id))
	defer unlock()

	path := d.diskPath(id)
	if ok, err := fileExists(path); err != nil {
		return nil, err
	} else if !ok {
		return nil, errs.NotFound("disk %q", id)
	}
	info, err := d.imgInfo(ctx, path)
	if err != nil {
		return nil, errs.Unavailable("%v", err)
	}
	want := int64(size) * gib
	switch {
	case want < info.VirtualSize:
		return nil, errs.Precondition("disk %q cannot shrink from %d to %d GB", id, info.VirtualSize/gib, size)
	case want == info.VirtualSize:
		return &nodev1.ResizeDiskResponse{}, nil
	}
	inUse, err := d.diskInUse(id)
	if err != nil {
		return nil, err
	}
	if inUse {
		return nil, errs.Precondition("disk %q is in use by a VM", id)
	}
	args, err := qemu.ImgResizeArgs(path, size)
	if err != nil {
		return nil, errs.Invalid("%v", err)
	}
	if _, err := d.cfg.Cmd.Run(ctx, "qemu-img", args...); err != nil {
		return nil, errs.Unavailable("qemu-img resize: %v", err)
	}
	return &nodev1.ResizeDiskResponse{}, nil
}

// DeleteDisk removes disk r.DiskId. It is refused while a VM with a live process has the disk attached.
func (d *Driver) DeleteDisk(ctx context.Context, r *nodev1.DeleteDiskRequest) (*nodev1.DeleteDiskResponse, error) {
	id := r.GetDiskId()
	if err := checkID("disk id", id); err != nil {
		return nil, err
	}
	unlock := d.lockKey(diskKey(id))
	defer unlock()

	path := d.diskPath(id)
	if ok, err := fileExists(path); err != nil {
		return nil, err
	} else if !ok {
		return nil, errs.NotFound("disk %q", id)
	}
	inUse, err := d.diskInUse(id)
	if err != nil {
		return nil, err
	}
	if inUse {
		return nil, errs.Precondition("disk %q is in use by a VM", id)
	}
	if err := os.Remove(path); err != nil {
		return nil, fmt.Errorf("remove disk %q: %w", id, err)
	}
	return &nodev1.DeleteDiskResponse{}, nil
}

// SnapshotDisk flattens disk r.DiskId into the new image r.ImageId. The disk must not be in use by
// a live VM, and the image must not exist yet.
func (d *Driver) SnapshotDisk(ctx context.Context, r *nodev1.SnapshotDiskRequest) (*nodev1.SnapshotDiskResponse, error) {
	diskID, imageID := r.GetDiskId(), r.GetImageId()
	if err := checkID("disk id", diskID); err != nil {
		return nil, err
	}
	if err := checkID("image id", imageID); err != nil {
		return nil, err
	}
	unlockDisk := d.lockKey(diskKey(diskID))
	defer unlockDisk()
	unlockImage := d.lockKey(imageKey(imageID))
	defer unlockImage()

	src := d.diskPath(diskID)
	if ok, err := fileExists(src); err != nil {
		return nil, err
	} else if !ok {
		return nil, errs.NotFound("disk %q", diskID)
	}
	dst := d.imagePath(imageID)
	if ok, err := fileExists(dst); err != nil {
		return nil, err
	} else if ok {
		return nil, errs.Exists("image %q", imageID)
	}
	inUse, err := d.diskInUse(diskID)
	if err != nil {
		return nil, err
	}
	if inUse {
		return nil, errs.Precondition("disk %q is in use by a VM", diskID)
	}
	info, err := d.imgInfo(ctx, src)
	if err != nil {
		return nil, errs.Unavailable("%v", err)
	}
	tmp := filepath.Join(d.imagesDir(), "."+imageID+".tmp.qcow2")
	args, err := qemu.ImgConvertArgs(src, tmp)
	if err != nil {
		return nil, errs.Invalid("%v", err)
	}
	if err := d.qemuImgTo(ctx, args, tmp, dst); err != nil {
		return nil, err
	}
	return &nodev1.SnapshotDiskResponse{SizeBytes: info.VirtualSize}, nil
}
