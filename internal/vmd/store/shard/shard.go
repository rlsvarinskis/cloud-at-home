// Package shard horizontally partitions vmd's database by project. A Store wraps N underlying
// store.Stores (each may be a separate Postgres) and itself implements store.Store, so the rest of
// vmd is unaware of the split.
//
// Placement rules:
//   - Rows of a project live on the shard chosen by ForProject (rendezvous hashing, so adding a shard
//     moves only about 1/N of the projects). Shared images and every row with an empty project live on
//     shard 0, the "system shard".
//   - Node rows live on shard 0.
//   - IDs created by NewID carry their shard, so ID-keyed calls go straight to the right shard. IDs
//     without a shard suffix are probed across all shards.
//   - Operations that cannot name a single project (ClaimVMs, ListVMsByNode, ...) fan out and merge.
//   - A transaction can span only one shard, so Tx needs a project hint from WithProject.
package shard

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"crypto/sha256"
	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/ids"
	"example.com/homecloud/internal/vmd/domain"
	"example.com/homecloud/internal/vmd/store"
)

// maxShards is the largest number of shards a sharded ID can address (two hex digits).
const maxShards = 256

// systemShard holds node rows, shared images and every row with an empty project.
const systemShard = 0

// Store is a store.Store that routes each call to one of its shards or fans it out across them.
// It is safe for concurrent use if its shards are.
type Store struct {
	shards []store.Store
	rr     atomic.Uint64 // round-robin start offset for ClaimVMs
}

var _ store.Store = (*Store)(nil)

// New returns a Store over shards, which must hold 1 to 256 non-nil stores. Shard i is shards[i].
func New(shards []store.Store) (*Store, error) {
	if n := len(shards); n < 1 || n > maxShards {
		return nil, errs.Invalid("shard: need 1..%d shards, got %d", maxShards, n)
	}
	for i, sh := range shards {
		if sh == nil {
			return nil, errs.Invalid("shard: shard %d is nil", i)
		}
	}
	return &Store{shards: slices.Clone(shards)}, nil
}

// Shards returns the number of shards.
func (s *Store) Shards() int { return len(s.shards) }

// ForProject returns the index of the shard that holds project's rows. It is rendezvous hashing: each
// shard gets a pseudo-random score for the project and the highest score wins. The empty project maps
// to the system shard (0).
func (s *Store) ForProject(project string) int {
	if project == "" {
		return systemShard
	}
	best, bestScore := 0, weight(project, 0)
	for i := 1; i < len(s.shards); i++ {
		if w := weight(project, i); w > bestScore {
			best, bestScore = i, w
		}
	}
	return best
}

// weight is the rendezvous score of project on shard i.
func weight(project string, i int) uint64 {
	buf := make([]byte, 0, len(project)+1+8)
	buf = append(buf, project...)
	buf = append(buf, 0)
	buf = binary.BigEndian.AppendUint64(buf, uint64(i))
	sum := sha256.Sum256(buf)
	return binary.BigEndian.Uint64(sum[:8])
}

// NewID returns a new sharded ID for an object of project, placed on that project's shard, for example
// "vm_01hx...-s0a". Create the object with this ID and it routes without probing.
func (s *Store) NewID(prefix, project string) string {
	return ids.NewSharded(prefix, s.ForProject(project))
}

// projectKey is the context key for the Tx project hint.
type projectKey struct{}

// WithProject returns a context carrying project as the hint that Tx uses to pick the shard.
func WithProject(ctx context.Context, project string) context.Context {
	return context.WithValue(ctx, projectKey{}, project)
}

// Tx runs fn in one transaction on the shard that holds the project named by WithProject. fn gets that
// shard's transaction store, so only rows of that shard are covered by the transaction. Writes to node
// rows inside fn therefore land on the transaction's shard, not on the system shard; keep node writes
// outside Tx or in a transaction whose project is on shard 0. Without a hint, Tx works only with a
// single shard.
func (s *Store) Tx(ctx context.Context, fn func(store.Store) error) error {
	if project, ok := ctx.Value(projectKey{}).(string); ok {
		return s.shards[s.ForProject(project)].Tx(ctx, fn)
	}
	if len(s.shards) == 1 {
		return s.shards[0].Tx(ctx, fn)
	}
	return errs.Invalid("shard: Tx needs shard.WithProject on the context")
}

// probeOrder returns the shards to try for an ID-keyed call. A sharded ID tries its own shard first;
// the rest follow, so a row found elsewhere (legacy data, or an unrelated ID that merely ends in a
// shard-like suffix) is still reached. Unsuffixed IDs try every shard in order.
func (s *Store) probeOrder(id string) []int {
	order := make([]int, 0, len(s.shards))
	first := -1
	if k, ok := ids.ShardOf(id); ok && k < len(s.shards) {
		order = append(order, k)
		first = k
	}
	for i := range s.shards {
		if i != first {
			order = append(order, i)
		}
	}
	return order
}

// find runs a read on the shards that may hold id and returns the first hit. NotFound from every shard
// is returned as NotFound.
func find[T any](s *Store, id string, op func(store.Store) (T, error)) (T, error) {
	var err error
	for _, i := range s.probeOrder(id) {
		var v T
		if v, err = op(s.shards[i]); !errors.Is(err, errs.ErrNotFound) {
			return v, err
		}
	}
	var zero T
	return zero, err
}

// mutate runs a write on the shard that holds id. Updates and deletes of a missing row return NotFound.
func (s *Store) mutate(id string, op func(store.Store) error) error {
	var err error
	for _, i := range s.probeOrder(id) {
		if err = op(s.shards[i]); !errors.Is(err, errs.ErrNotFound) {
			return err
		}
	}
	return err
}

// ensureAbsentElsewhere fails with AlreadyExists if an unsuffixed id already exists on a shard other
// than home. Sharded IDs are unique by construction, so they skip the check.
func (s *Store) ensureAbsentElsewhere(id string, home int, get func(store.Store) error) error {
	if _, ok := ids.ShardOf(id); ok {
		return nil
	}
	for i, sh := range s.shards {
		if i == home {
			continue
		}
		err := get(sh)
		if err == nil {
			return errs.Exists("%s already exists", id)
		}
		if !errors.Is(err, errs.ErrNotFound) {
			return err
		}
	}
	return nil
}

// fanOut reads from every shard and merges the results by ID, keeping at most limit (limit<=0: all).
func fanOut[T any](s *Store, limit int, id func(T) string, list func(store.Store) ([]T, error)) ([]T, error) {
	var all []T
	for _, sh := range s.shards {
		part, err := list(sh)
		if err != nil {
			return nil, err
		}
		all = append(all, part...)
	}
	return mergeByID(all, limit, id), nil
}

// mergeByID sorts items by ID and keeps at most limit of them (limit<=0: all).
func mergeByID[T any](items []T, limit int, id func(T) string) []T {
	sort.SliceStable(items, func(i, j int) bool { return id(items[i]) < id(items[j]) })
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

func imageID(i *domain.Image) string { return i.ID }
func diskID(d *domain.Disk) string   { return d.ID }
func vmID(v *domain.VM) string       { return v.ID }
func jobID(j *domain.Job) string     { return j.ID }

// ---- nodes (system shard) ----

func (s *Store) UpsertNode(ctx context.Context, n *domain.Node) error {
	return s.shards[systemShard].UpsertNode(ctx, n)
}

func (s *Store) SetNodeDrain(ctx context.Context, id string, drain bool) error {
	return s.shards[systemShard].SetNodeDrain(ctx, id, drain)
}

func (s *Store) GetNode(ctx context.Context, id string) (*domain.Node, error) {
	return s.shards[systemShard].GetNode(ctx, id)
}

func (s *Store) ListNodes(ctx context.Context) ([]*domain.Node, error) {
	return s.shards[systemShard].ListNodes(ctx)
}

// ---- images ----

func (s *Store) CreateImage(ctx context.Context, i *domain.Image) error {
	home := s.ForProject(i.ProjectID)
	if err := s.ensureAbsentElsewhere(i.ID, home, func(sh store.Store) error {
		_, err := sh.GetImage(ctx, i.ID)
		return err
	}); err != nil {
		return err
	}
	return s.shards[home].CreateImage(ctx, i)
}

func (s *Store) GetImage(ctx context.Context, id string) (*domain.Image, error) {
	return find(s, id, func(sh store.Store) (*domain.Image, error) { return sh.GetImage(ctx, id) })
}

// ListImages returns the project's images and, if includeShared, the shared images, which live on the
// system shard. The two sources are merged by ID and paginated together.
func (s *Store) ListImages(ctx context.Context, project string, includeShared bool, after string, limit int) ([]*domain.Image, error) {
	home := s.ForProject(project)
	if project == "" || home == systemShard || !includeShared {
		return s.shards[home].ListImages(ctx, project, includeShared, after, limit)
	}
	mine, err := s.shards[home].ListImages(ctx, project, false, after, limit)
	if err != nil {
		return nil, err
	}
	shared, err := s.shards[systemShard].ListImages(ctx, "", true, after, limit)
	if err != nil {
		return nil, err
	}
	return mergeByID(append(mine, shared...), limit, imageID), nil
}

func (s *Store) ListImagesByState(ctx context.Context, state domain.ImageState, limit int) ([]*domain.Image, error) {
	return fanOut(s, limit, imageID, func(sh store.Store) ([]*domain.Image, error) {
		return sh.ListImagesByState(ctx, state, limit)
	})
}

func (s *Store) UpdateImage(ctx context.Context, i *domain.Image) error {
	return s.mutate(i.ID, func(sh store.Store) error { return sh.UpdateImage(ctx, i) })
}

func (s *Store) DeleteImage(ctx context.Context, id string) error {
	return s.mutate(id, func(sh store.Store) error { return sh.DeleteImage(ctx, id) })
}

// ---- disks ----

func (s *Store) CreateDisk(ctx context.Context, d *domain.Disk) error {
	home := s.ForProject(d.ProjectID)
	if err := s.ensureAbsentElsewhere(d.ID, home, func(sh store.Store) error {
		_, err := sh.GetDisk(ctx, d.ID)
		return err
	}); err != nil {
		return err
	}
	return s.shards[home].CreateDisk(ctx, d)
}

func (s *Store) GetDisk(ctx context.Context, id string) (*domain.Disk, error) {
	return find(s, id, func(sh store.Store) (*domain.Disk, error) { return sh.GetDisk(ctx, id) })
}

func (s *Store) ListDisks(ctx context.Context, project, after string, limit int) ([]*domain.Disk, error) {
	return s.shards[s.ForProject(project)].ListDisks(ctx, project, after, limit)
}

func (s *Store) ListDisksByState(ctx context.Context, state domain.DiskState, limit int) ([]*domain.Disk, error) {
	return fanOut(s, limit, diskID, func(sh store.Store) ([]*domain.Disk, error) {
		return sh.ListDisksByState(ctx, state, limit)
	})
}

// CountDisksUsingImage sums the count over all shards, since disks of any project may use an image.
func (s *Store) CountDisksUsingImage(ctx context.Context, imageID string) (int, error) {
	total := 0
	for _, sh := range s.shards {
		n, err := sh.CountDisksUsingImage(ctx, imageID)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func (s *Store) UpdateDisk(ctx context.Context, d *domain.Disk) error {
	return s.mutate(d.ID, func(sh store.Store) error { return sh.UpdateDisk(ctx, d) })
}

func (s *Store) DeleteDisk(ctx context.Context, id string) error {
	return s.mutate(id, func(sh store.Store) error { return sh.DeleteDisk(ctx, id) })
}

// ---- VMs ----

func (s *Store) CreateVM(ctx context.Context, v *domain.VM) error {
	home := s.ForProject(v.ProjectID)
	if err := s.ensureAbsentElsewhere(v.ID, home, func(sh store.Store) error {
		_, err := sh.GetVM(ctx, v.ID)
		return err
	}); err != nil {
		return err
	}
	return s.shards[home].CreateVM(ctx, v)
}

func (s *Store) GetVM(ctx context.Context, id string) (*domain.VM, error) {
	return find(s, id, func(sh store.Store) (*domain.VM, error) { return sh.GetVM(ctx, id) })
}

func (s *Store) GetVMByIdempotencyKey(ctx context.Context, project, key string) (*domain.VM, error) {
	return s.shards[s.ForProject(project)].GetVMByIdempotencyKey(ctx, project, key)
}

func (s *Store) ListVMs(ctx context.Context, f domain.VMFilter) ([]*domain.VM, error) {
	return s.shards[s.ForProject(f.ProjectID)].ListVMs(ctx, f)
}

// ListVMsByNode fans out: a node's VMs can belong to any project.
func (s *Store) ListVMsByNode(ctx context.Context, nodeID string) ([]*domain.VM, error) {
	return fanOut(s, 0, vmID, func(sh store.Store) ([]*domain.VM, error) {
		return sh.ListVMsByNode(ctx, nodeID)
	})
}

func (s *Store) UpdateVM(ctx context.Context, v *domain.VM) error {
	return s.mutate(v.ID, func(sh store.Store) error { return sh.UpdateVM(ctx, v) })
}

func (s *Store) DeleteVM(ctx context.Context, id string) error {
	return s.mutate(id, func(sh store.Store) error { return sh.DeleteVM(ctx, id) })
}

// ClaimVMs claims due VMs from every shard. It starts at a shard that rotates on each call, so under a
// small limit no shard starves, and it stops once limit VMs are claimed (limit<=0: no limit). The
// result is ordered by NextReconcile, then ID. If a shard fails, the error is returned and the VMs
// already claimed on other shards keep their leases until they expire.
func (s *Store) ClaimVMs(ctx context.Context, owner string, now time.Time, ttl time.Duration, limit int) ([]*domain.VM, error) {
	n := len(s.shards)
	start := int((s.rr.Add(1) - 1) % uint64(n))
	var got []*domain.VM
	var firstErr error
	for k := 0; k < n; k++ {
		want := 0 // 0 asks the shard for everything due
		if limit > 0 {
			if want = limit - len(got); want <= 0 {
				break
			}
		}
		claimed, err := s.shards[(start+k)%n].ClaimVMs(ctx, owner, now, ttl, want)
		if err != nil && firstErr == nil {
			firstErr = err // keep going: VMs claimed on healthy shards must not be stranded under a lease
		}
		got = append(got, claimed...)
	}
	sort.SliceStable(got, func(i, j int) bool {
		a, b := got[i], got[j]
		if !a.NextReconcile.Equal(b.NextReconcile) {
			return a.NextReconcile.Before(b.NextReconcile)
		}
		return a.ID < b.ID
	})
	return got, firstErr // possibly both: callers process the claimed VMs and report the error
}

// ---- jobs ----

func (s *Store) CreateJob(ctx context.Context, j *domain.Job) error {
	home := s.ForProject(j.ProjectID)
	if err := s.ensureAbsentElsewhere(j.ID, home, func(sh store.Store) error {
		_, err := sh.GetJob(ctx, j.ID)
		return err
	}); err != nil {
		return err
	}
	return s.shards[home].CreateJob(ctx, j)
}

func (s *Store) GetJob(ctx context.Context, id string) (*domain.Job, error) {
	return find(s, id, func(sh store.Store) (*domain.Job, error) { return sh.GetJob(ctx, id) })
}

func (s *Store) GetJobByIdempotencyKey(ctx context.Context, project, key string) (*domain.Job, error) {
	return s.shards[s.ForProject(project)].GetJobByIdempotencyKey(ctx, project, key)
}

func (s *Store) UpdateJob(ctx context.Context, j *domain.Job) error {
	return s.mutate(j.ID, func(sh store.Store) error { return sh.UpdateJob(ctx, j) })
}

// ListJobs fans out: a job's project is not part of the call, so any shard may hold the VM's jobs.
func (s *Store) ListJobs(ctx context.Context, vmID, after string, limit int) ([]*domain.Job, error) {
	return fanOut(s, limit, jobID, func(sh store.Store) ([]*domain.Job, error) {
		return sh.ListJobs(ctx, vmID, after, limit)
	})
}

// ListRunningJobs fans out for the same reason as ListJobs.
func (s *Store) ListRunningJobs(ctx context.Context, vmID string) ([]*domain.Job, error) {
	return fanOut(s, 0, jobID, func(sh store.Store) ([]*domain.Job, error) {
		return sh.ListRunningJobs(ctx, vmID)
	})
}

// ---- usage ----

func (s *Store) Usage(ctx context.Context, project string) (domain.Usage, error) {
	return s.shards[s.ForProject(project)].Usage(ctx, project)
}
