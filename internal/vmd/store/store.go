// Package store defines vmd's persistence contract. Two implementations exist: memory (tests, single-node
// dev) and postgres (production). Both must pass storetest.Run.
package store

import (
	"context"
	"time"

	"example.com/homecloud/internal/vmd/domain"
)

// Store rules shared by every method:
//   - Get* of a missing row returns errs.ErrNotFound.
//   - Create* of a duplicate (ID, or project+name, or project+idempotency key) returns errs.ErrAlreadyExists.
//   - Update* is a compare-and-swap on Version: if the stored Version differs from the passed object's
//     it returns errs.ErrConflict; on success the passed object's Version is incremented in place.
//   - All list methods are keyset-paginated by ID (IDs are time-sortable ULIDs).
type Store interface {
	// Tx runs fn atomically: either every write inside it happens or none does.
	Tx(ctx context.Context, fn func(Store) error) error

	UpsertNode(ctx context.Context, n *domain.Node) error
	GetNode(ctx context.Context, id string) (*domain.Node, error)
	ListNodes(ctx context.Context) ([]*domain.Node, error)

	CreateImage(ctx context.Context, i *domain.Image) error
	GetImage(ctx context.Context, id string) (*domain.Image, error)
	// ListImages returns the project's images and, if includeShared, images with an empty project.
	ListImages(ctx context.Context, project string, includeShared bool, after string, limit int) ([]*domain.Image, error)
	UpdateImage(ctx context.Context, i *domain.Image) error
	DeleteImage(ctx context.Context, id string) error

	CreateDisk(ctx context.Context, d *domain.Disk) error
	GetDisk(ctx context.Context, id string) (*domain.Disk, error)
	ListDisks(ctx context.Context, project, after string, limit int) ([]*domain.Disk, error)
	ListDisksByState(ctx context.Context, state domain.DiskState, limit int) ([]*domain.Disk, error)
	UpdateDisk(ctx context.Context, d *domain.Disk) error
	DeleteDisk(ctx context.Context, id string) error

	CreateVM(ctx context.Context, v *domain.VM) error
	GetVM(ctx context.Context, id string) (*domain.VM, error)
	GetVMByIdempotencyKey(ctx context.Context, project, key string) (*domain.VM, error)
	ListVMs(ctx context.Context, f domain.VMFilter) ([]*domain.VM, error)
	ListVMsByNode(ctx context.Context, nodeID string) ([]*domain.VM, error)
	UpdateVM(ctx context.Context, v *domain.VM) error
	DeleteVM(ctx context.Context, id string) error
	// ClaimVMs leases up to limit VMs whose NextReconcile is due and whose lease has expired, so several
	// reconciler instances (replicas, shards) never work on the same VM. The returned copies carry the lease.
	ClaimVMs(ctx context.Context, owner string, now time.Time, ttl time.Duration, limit int) ([]*domain.VM, error)

	CreateJob(ctx context.Context, j *domain.Job) error
	GetJob(ctx context.Context, id string) (*domain.Job, error)
	GetJobByIdempotencyKey(ctx context.Context, project, key string) (*domain.Job, error)
	UpdateJob(ctx context.Context, j *domain.Job) error
	ListJobs(ctx context.Context, vmID, after string, limit int) ([]*domain.Job, error)
	ListRunningJobs(ctx context.Context, vmID string) ([]*domain.Job, error)

	Usage(ctx context.Context, project string) (domain.Usage, error)
}
