package store

import (
	"context"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/vmd/domain"
)

// Memory is an in-process Store. It is the reference implementation of the contract and what unit tests use.
type Memory struct {
	mu *sync.Mutex // shared between a Memory and its Tx views
	d  *data
}

type data struct {
	nodes  map[string]*domain.Node
	images map[string]*domain.Image
	disks  map[string]*domain.Disk
	vms    map[string]*domain.VM
	jobs   map[string]*domain.Job
}

func newData() *data {
	return &data{nodes: map[string]*domain.Node{}, images: map[string]*domain.Image{}, disks: map[string]*domain.Disk{},
		vms: map[string]*domain.VM{}, jobs: map[string]*domain.Job{}}
}

func (d *data) clone() *data {
	c := newData()
	for k, v := range d.nodes {
		c.nodes[k] = cp(v)
	}
	for k, v := range d.images {
		c.images[k] = cp(v)
	}
	for k, v := range d.disks {
		c.disks[k] = cp(v)
	}
	for k, v := range d.vms {
		c.vms[k] = cpVM(v)
	}
	for k, v := range d.jobs {
		c.jobs[k] = cpJob(v)
	}
	return c
}

func NewMemory() *Memory { return &Memory{mu: &sync.Mutex{}, d: newData()} }

func cp[T any](v *T) *T { c := *v; return &c }

func cpVM(v *domain.VM) *domain.VM {
	c := *v
	c.DiskIDs = slices.Clone(v.DiskIDs)
	c.Holds = slices.Clone(v.Holds)
	c.Labels = maps.Clone(v.Labels)
	return &c
}

func cpJob(j *domain.Job) *domain.Job {
	c := *j
	if j.ExitCode != nil {
		x := *j.ExitCode
		c.ExitCode = &x
	}
	return &c
}

// Tx works on a private copy of the data and swaps it in only if fn succeeds, giving all-or-nothing
// semantics. The copy is a Memory with its own lock, so calls inside fn do not deadlock on the outer one.
func (m *Memory) Tx(ctx context.Context, fn func(Store) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	work := &Memory{mu: &sync.Mutex{}, d: m.d.clone()}
	if err := fn(work); err != nil {
		return err
	}
	m.d = work.d
	return nil
}

// Every method below takes the lock for its whole duration.
func (m *Memory) with(fn func()) { m.mu.Lock(); defer m.mu.Unlock(); fn() }

func (m *Memory) UpsertNode(ctx context.Context, n *domain.Node) error {
	m.with(func() {
		c := cp(n)
		if old, ok := m.d.nodes[n.ID]; ok {
			c.Drain = old.Drain // the drain flag is only ever changed by SetNodeDrain
		}
		m.d.nodes[n.ID] = c
	})
	return nil
}
func (m *Memory) SetNodeDrain(ctx context.Context, id string, drain bool) (err error) {
	m.with(func() {
		n, ok := m.d.nodes[id]
		if !ok {
			err = errs.NotFound("node %s", id)
			return
		}
		n.Drain = drain
	})
	return
}
func (m *Memory) GetNode(ctx context.Context, id string) (n *domain.Node, err error) {
	m.with(func() {
		if x, ok := m.d.nodes[id]; ok {
			n = cp(x)
		} else {
			err = errs.NotFound("node %s", id)
		}
	})
	return
}
func (m *Memory) ListNodes(ctx context.Context) (out []*domain.Node, err error) {
	m.with(func() {
		for _, n := range m.d.nodes {
			out = append(out, cp(n))
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return
}

// ---- generic helpers (no locking) ----

func after[T any](items []T, id func(T) string, after string, limit int) []T {
	sort.Slice(items, func(i, j int) bool { return id(items[i]) < id(items[j]) })
	var out []T
	for _, it := range items {
		if after != "" && id(it) <= after {
			continue
		}
		out = append(out, it)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func cas(stored, given int64, kind, id string) error {
	if stored != given {
		return errs.ErrConflict
	}
	return nil
}

// ---- images ----

func (m *Memory) CreateImage(ctx context.Context, i *domain.Image) (err error) {
	m.with(func() {
		if _, ok := m.d.images[i.ID]; ok {
			err = errs.Exists("image %s", i.ID)
			return
		}
		for _, o := range m.d.images {
			if o.ProjectID == i.ProjectID && o.Name == i.Name {
				err = errs.Exists("image named %q", i.Name)
				return
			}
		}
		i.Version = 1
		m.d.images[i.ID] = cp(i)
	})
	return
}
func (m *Memory) GetImage(ctx context.Context, id string) (i *domain.Image, err error) {
	m.with(func() {
		if x, ok := m.d.images[id]; ok {
			i = cp(x)
		} else {
			err = errs.NotFound("image %s", id)
		}
	})
	return
}
func (m *Memory) ListImages(ctx context.Context, project string, shared bool, aft string, limit int) (out []*domain.Image, err error) {
	var all []*domain.Image
	m.with(func() {
		for _, i := range m.d.images {
			if (project != "" && i.ProjectID == project) || (shared && i.ProjectID == "") {
				all = append(all, cp(i))
			}
		}
	})
	return after(all, func(i *domain.Image) string { return i.ID }, aft, limit), nil
}
func (m *Memory) ListImagesByState(ctx context.Context, st domain.ImageState, limit int) ([]*domain.Image, error) {
	var all []*domain.Image
	m.with(func() {
		for _, i := range m.d.images {
			if i.State == st {
				all = append(all, cp(i))
			}
		}
	})
	return after(all, func(i *domain.Image) string { return i.ID }, "", limit), nil
}
func (m *Memory) UpdateImage(ctx context.Context, i *domain.Image) (err error) {
	m.with(func() {
		s, ok := m.d.images[i.ID]
		if !ok {
			err = errs.NotFound("image %s", i.ID)
			return
		}
		if err = cas(s.Version, i.Version, "image", i.ID); err != nil {
			return
		}
		i.Version++
		m.d.images[i.ID] = cp(i)
	})
	return
}
func (m *Memory) DeleteImage(ctx context.Context, id string) (err error) {
	m.with(func() {
		if _, ok := m.d.images[id]; !ok {
			err = errs.NotFound("image %s", id)
			return
		}
		delete(m.d.images, id)
	})
	return
}

// ---- disks ----

func (m *Memory) CreateDisk(ctx context.Context, d *domain.Disk) (err error) {
	m.with(func() {
		if _, ok := m.d.disks[d.ID]; ok {
			err = errs.Exists("disk %s", d.ID)
			return
		}
		for _, o := range m.d.disks {
			if o.ProjectID == d.ProjectID && o.Name == d.Name {
				err = errs.Exists("disk named %q", d.Name)
				return
			}
		}
		d.Version = 1
		m.d.disks[d.ID] = cp(d)
	})
	return
}
func (m *Memory) GetDisk(ctx context.Context, id string) (d *domain.Disk, err error) {
	m.with(func() {
		if x, ok := m.d.disks[id]; ok {
			d = cp(x)
		} else {
			err = errs.NotFound("disk %s", id)
		}
	})
	return
}
func (m *Memory) ListDisks(ctx context.Context, project, aft string, limit int) ([]*domain.Disk, error) {
	var all []*domain.Disk
	m.with(func() {
		for _, d := range m.d.disks {
			if d.ProjectID == project {
				all = append(all, cp(d))
			}
		}
	})
	return after(all, func(d *domain.Disk) string { return d.ID }, aft, limit), nil
}
func (m *Memory) ListDisksByState(ctx context.Context, st domain.DiskState, limit int) ([]*domain.Disk, error) {
	var all []*domain.Disk
	m.with(func() {
		for _, d := range m.d.disks {
			if d.State == st {
				all = append(all, cp(d))
			}
		}
	})
	return after(all, func(d *domain.Disk) string { return d.ID }, "", limit), nil
}
func (m *Memory) CountDisksUsingImage(ctx context.Context, imageID string) (n int, err error) {
	m.with(func() {
		for _, d := range m.d.disks {
			if d.ImageID == imageID {
				n++
			}
		}
	})
	return
}
func (m *Memory) UpdateDisk(ctx context.Context, d *domain.Disk) (err error) {
	m.with(func() {
		s, ok := m.d.disks[d.ID]
		if !ok {
			err = errs.NotFound("disk %s", d.ID)
			return
		}
		if err = cas(s.Version, d.Version, "disk", d.ID); err != nil {
			return
		}
		d.Version++
		m.d.disks[d.ID] = cp(d)
	})
	return
}
func (m *Memory) DeleteDisk(ctx context.Context, id string) (err error) {
	m.with(func() {
		if _, ok := m.d.disks[id]; !ok {
			err = errs.NotFound("disk %s", id)
			return
		}
		delete(m.d.disks, id)
	})
	return
}

// ---- VMs ----

func (m *Memory) CreateVM(ctx context.Context, v *domain.VM) (err error) {
	m.with(func() {
		if _, ok := m.d.vms[v.ID]; ok {
			err = errs.Exists("vm %s", v.ID)
			return
		}
		for _, o := range m.d.vms {
			if o.ProjectID != v.ProjectID {
				continue
			}
			if o.Name == v.Name {
				err = errs.Exists("vm named %q", v.Name)
				return
			}
			if v.IdempotencyKey != "" && o.IdempotencyKey == v.IdempotencyKey {
				err = errs.Exists("idempotency key %q", v.IdempotencyKey)
				return
			}
		}
		v.Version = 1
		m.d.vms[v.ID] = cpVM(v)
	})
	return
}
func (m *Memory) GetVM(ctx context.Context, id string) (v *domain.VM, err error) {
	m.with(func() {
		if x, ok := m.d.vms[id]; ok {
			v = cpVM(x)
		} else {
			err = errs.NotFound("vm %s", id)
		}
	})
	return
}
func (m *Memory) GetVMByIdempotencyKey(ctx context.Context, project, key string) (v *domain.VM, err error) {
	m.with(func() {
		for _, x := range m.d.vms {
			if x.ProjectID == project && key != "" && x.IdempotencyKey == key {
				v = cpVM(x)
				return
			}
		}
		err = errs.NotFound("vm with idempotency key")
	})
	return
}
func (m *Memory) ListVMs(ctx context.Context, f domain.VMFilter) ([]*domain.VM, error) {
	var all []*domain.VM
	m.with(func() {
	next:
		for _, v := range m.d.vms {
			if v.ProjectID != f.ProjectID {
				continue
			}
			for k, want := range f.Labels {
				if v.Labels[k] != want {
					continue next
				}
			}
			all = append(all, cpVM(v))
		}
	})
	return after(all, func(v *domain.VM) string { return v.ID }, f.After, f.Limit), nil
}
func (m *Memory) ListVMsByNode(ctx context.Context, nodeID string) ([]*domain.VM, error) {
	var all []*domain.VM
	m.with(func() {
		for _, v := range m.d.vms {
			if v.NodeID == nodeID {
				all = append(all, cpVM(v))
			}
		}
	})
	return after(all, func(v *domain.VM) string { return v.ID }, "", 0), nil
}
func (m *Memory) UpdateVM(ctx context.Context, v *domain.VM) (err error) {
	m.with(func() {
		s, ok := m.d.vms[v.ID]
		if !ok {
			err = errs.NotFound("vm %s", v.ID)
			return
		}
		if err = cas(s.Version, v.Version, "vm", v.ID); err != nil {
			return
		}
		v.Version++
		m.d.vms[v.ID] = cpVM(v)
	})
	return
}
func (m *Memory) DeleteVM(ctx context.Context, id string) (err error) {
	m.with(func() {
		if _, ok := m.d.vms[id]; !ok {
			err = errs.NotFound("vm %s", id)
			return
		}
		delete(m.d.vms, id)
	})
	return
}
func (m *Memory) ClaimVMs(ctx context.Context, owner string, now time.Time, ttl time.Duration, limit int) ([]*domain.VM, error) {
	var due []*domain.VM
	m.with(func() {
		for _, v := range m.d.vms {
			if !v.NextReconcile.IsZero() && !v.NextReconcile.After(now) && !v.LeaseUntil.After(now) {
				due = append(due, v)
			}
		}
		sort.Slice(due, func(i, j int) bool {
			if !due[i].NextReconcile.Equal(due[j].NextReconcile) {
				return due[i].NextReconcile.Before(due[j].NextReconcile)
			}
			return due[i].ID < due[j].ID
		})
		if limit > 0 && len(due) > limit {
			due = due[:limit]
		}
		for _, v := range due {
			v.LeaseOwner, v.LeaseUntil = owner, now.Add(ttl)
			v.Version++
		}
	})
	out := make([]*domain.VM, len(due))
	for i, v := range due {
		out[i] = cpVM(v)
	}
	return out, nil
}

// ---- jobs ----

func (m *Memory) CreateJob(ctx context.Context, j *domain.Job) (err error) {
	m.with(func() {
		if _, ok := m.d.jobs[j.ID]; ok {
			err = errs.Exists("job %s", j.ID)
			return
		}
		for _, o := range m.d.jobs {
			if j.IdempotencyKey != "" && o.ProjectID == j.ProjectID && o.IdempotencyKey == j.IdempotencyKey {
				err = errs.Exists("idempotency key %q", j.IdempotencyKey)
				return
			}
		}
		j.Version = 1
		m.d.jobs[j.ID] = cpJob(j)
	})
	return
}
func (m *Memory) GetJob(ctx context.Context, id string) (j *domain.Job, err error) {
	m.with(func() {
		if x, ok := m.d.jobs[id]; ok {
			j = cpJob(x)
		} else {
			err = errs.NotFound("job %s", id)
		}
	})
	return
}
func (m *Memory) GetJobByIdempotencyKey(ctx context.Context, project, key string) (j *domain.Job, err error) {
	m.with(func() {
		for _, x := range m.d.jobs {
			if x.ProjectID == project && key != "" && x.IdempotencyKey == key {
				j = cpJob(x)
				return
			}
		}
		err = errs.NotFound("job with idempotency key")
	})
	return
}
func (m *Memory) UpdateJob(ctx context.Context, j *domain.Job) (err error) {
	m.with(func() {
		s, ok := m.d.jobs[j.ID]
		if !ok {
			err = errs.NotFound("job %s", j.ID)
			return
		}
		if err = cas(s.Version, j.Version, "job", j.ID); err != nil {
			return
		}
		j.Version++
		m.d.jobs[j.ID] = cpJob(j)
	})
	return
}
func (m *Memory) ListJobs(ctx context.Context, vmID, aft string, limit int) ([]*domain.Job, error) {
	var all []*domain.Job
	m.with(func() {
		for _, j := range m.d.jobs {
			if j.VMID == vmID {
				all = append(all, cpJob(j))
			}
		}
	})
	return after(all, func(j *domain.Job) string { return j.ID }, aft, limit), nil
}
func (m *Memory) ListRunningJobs(ctx context.Context, vmID string) ([]*domain.Job, error) {
	var all []*domain.Job
	m.with(func() {
		for _, j := range m.d.jobs {
			if j.VMID == vmID && j.State == domain.JobRunning {
				all = append(all, cpJob(j))
			}
		}
	})
	return after(all, func(j *domain.Job) string { return j.ID }, "", 0), nil
}

func (m *Memory) Usage(ctx context.Context, project string) (u domain.Usage, err error) {
	m.with(func() {
		for _, v := range m.d.vms {
			if v.ProjectID == project {
				u.VMs++
				if v.Desired == domain.PowerRunning {
					u.CPUs += v.CPUs
					u.MemoryMB += v.MemoryMB
				}
			}
		}
		for _, d := range m.d.disks {
			if d.ProjectID == project {
				u.DiskGB += d.SizeGB
			}
		}
	})
	return
}
