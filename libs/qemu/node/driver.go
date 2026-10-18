// Package node defines what a physical host can do for vmd. The control plane only ever talks to a
// Driver; implementations are the QEMU driver (real host), a Remote driver (another host over the
// node API), and a fake (tests).
package node

import (
	"context"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/node/v1"
)

// Driver is the node API as plain Go calls. Errors use the sentinels in libs/common/errs
// (ErrNotFound, ErrUnavailable, ErrFailedPrecondition, ...), whichever implementation is behind it.
type Driver interface {
	GetCapacity(ctx context.Context, r *nodev1.GetCapacityRequest) (*nodev1.GetCapacityResponse, error)

	RegisterImage(ctx context.Context, r *nodev1.RegisterImageRequest) (*nodev1.RegisterImageResponse, error)
	DeleteImage(ctx context.Context, r *nodev1.DeleteImageRequest) (*nodev1.DeleteImageResponse, error)

	CreateDisk(ctx context.Context, r *nodev1.CreateDiskRequest) (*nodev1.CreateDiskResponse, error)
	ResizeDisk(ctx context.Context, r *nodev1.ResizeDiskRequest) (*nodev1.ResizeDiskResponse, error)
	DeleteDisk(ctx context.Context, r *nodev1.DeleteDiskRequest) (*nodev1.DeleteDiskResponse, error)
	SnapshotDisk(ctx context.Context, r *nodev1.SnapshotDiskRequest) (*nodev1.SnapshotDiskResponse, error)

	StartVM(ctx context.Context, r *nodev1.StartVMRequest) (*nodev1.StartVMResponse, error)
	StopVM(ctx context.Context, r *nodev1.StopVMRequest) (*nodev1.StopVMResponse, error)
	GetVMRuntime(ctx context.Context, r *nodev1.GetVMRuntimeRequest) (*nodev1.GetVMRuntimeResponse, error)
	ForgetVM(ctx context.Context, r *nodev1.ForgetVMRequest) (*nodev1.ForgetVMResponse, error)

	StartJob(ctx context.Context, r *nodev1.StartJobRequest) (*nodev1.StartJobResponse, error)
	PollJob(ctx context.Context, r *nodev1.PollJobRequest) (*nodev1.PollJobResponse, error)
	CancelJob(ctx context.Context, r *nodev1.CancelJobRequest) (*nodev1.CancelJobResponse, error)
	PutFile(ctx context.Context, r *nodev1.PutFileRequest) (*nodev1.PutFileResponse, error)
	GetFile(ctx context.Context, r *nodev1.GetFileRequest) (*nodev1.GetFileResponse, error)

	GetVMMetrics(ctx context.Context, r *nodev1.GetVMMetricsRequest) (*nodev1.GetVMMetricsResponse, error)
}
