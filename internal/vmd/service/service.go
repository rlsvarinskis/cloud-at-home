package service

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"time"

	"example.com/homecloud/internal/auth"
	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/vmd/domain"
	"example.com/homecloud/internal/vmd/scheduler"
	"example.com/homecloud/internal/vmd/store"
)

// base carries what every service handler needs.
type base struct {
	st    store.Store
	nodes Nodes
	cfg   Config
}

func (b *base) now() time.Time { return b.cfg.Now().UTC() }

var nameRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validName(kind, n string) error {
	if !nameRE.MatchString(n) {
		return errs.Invalid("%s name %q must be 1-63 chars of a-z, 0-9 and '-', starting with a letter", kind, n)
	}
	return nil
}

func validProject(p string) error {
	if p == "" || p == auth.AnyProject {
		return errs.Invalid("project_id is required")
	}
	return nil
}

func (b *base) pageSize(n int32) int {
	switch {
	case n <= 0:
		return 50
	case n > 200:
		return 200
	}
	return int(n)
}

// nextToken: lists fetch limit+1 rows; the extra row proves there is another page.
func nextToken[T any](items []T, limit int, id func(T) string) ([]T, string) {
	if len(items) > limit {
		items = items[:limit]
		return items, id(items[len(items)-1])
	}
	return items, ""
}

// poll re-evaluates check every 200 ms until it reports done, the wait expires or ctx ends.
// waitSeconds is clamped to 30 (clients and proxies give up around 60 s).
func poll(ctx context.Context, waitSeconds int32, check func(context.Context) (bool, error)) error {
	deadline := time.Now().Add(time.Duration(min(max(waitSeconds, 0), 30)) * time.Second)
	for {
		done, err := check(ctx)
		if err != nil || done {
			return err
		}
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

var errUnchanged = errors.New("unchanged")

// mutateVM is the read-modify-write loop used by every VM update: load, authorize against the VM's
// own project, apply mut, and write with compare-and-swap, retrying when another writer got in between.
func (b *base) mutateVM(ctx context.Context, id string, perm auth.Permission, mut func(*domain.VM) error) (*domain.VM, error) {
	for range 8 {
		v, err := b.st.GetVM(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := auth.RequireResource(ctx, perm, v.ProjectID, "vm", id); err != nil {
			return nil, err
		}
		if err := mut(v); err != nil {
			if errors.Is(err, errUnchanged) {
				return v, nil
			}
			return nil, err
		}
		v.UpdatedAt = b.now()
		if err := b.st.UpdateVM(ctx, v); err != nil {
			if errors.Is(err, errs.ErrConflict) {
				continue
			}
			return nil, err
		}
		return v, nil
	}
	return nil, errs.ErrConflict
}

// checkQuota verifies that adding the given amounts keeps the project within its quota.
func (b *base) checkQuota(ctx context.Context, project string, addVMs, addCPUs int, addMemMB int64, addDiskGB int) error {
	q := b.cfg.Quota
	u, err := b.st.Usage(ctx, project)
	if err != nil {
		return err
	}
	switch {
	case q.MaxVMs > 0 && u.VMs+addVMs > q.MaxVMs:
		return errs.Exhausted("project %s would exceed its quota of %d VMs", project, q.MaxVMs)
	case q.MaxCPUs > 0 && u.CPUs+addCPUs > q.MaxCPUs:
		return errs.Exhausted("project %s would exceed its quota of %d running vCPUs (using %d)", project, q.MaxCPUs, u.CPUs)
	case q.MaxMemoryMB > 0 && u.MemoryMB+addMemMB > q.MaxMemoryMB:
		return errs.Exhausted("project %s would exceed its quota of %d MB running memory (using %d)", project, q.MaxMemoryMB, u.MemoryMB)
	case q.MaxDiskGB > 0 && u.DiskGB+addDiskGB > q.MaxDiskGB:
		return errs.Exhausted("project %s would exceed its quota of %d GB disk (using %d)", project, q.MaxDiskGB, u.DiskGB)
	}
	return nil
}

func (b *base) checkVMSpec(cpus int, memMB int64) error {
	switch {
	case cpus < 1 || cpus > b.cfg.MaxVMCPUs:
		return errs.Invalid("cpus must be between 1 and %d", b.cfg.MaxVMCPUs)
	case memMB < b.cfg.MinMemoryMB || memMB > b.cfg.MaxMemoryMB:
		return errs.Invalid("memory_mb must be between %d and %d", b.cfg.MinMemoryMB, b.cfg.MaxMemoryMB)
	}
	return nil
}

// visibleImage loads an image and checks the caller may use it: its own project's, or a shared one.
func (b *base) visibleImage(ctx context.Context, project, id string) (*domain.Image, error) {
	img, err := b.st.GetImage(ctx, id)
	if err != nil {
		return nil, err
	}
	if img.ProjectID != "" && img.ProjectID != project {
		return nil, errs.NotFound("image %s", id)
	}
	return img, nil
}

func addHold(holds []string, h string) []string {
	if slices.Contains(holds, h) {
		return holds
	}
	out := append(slices.Clone(holds), h)
	slices.Sort(out)
	return out
}

func removeHold(holds []string, h string) []string {
	return slices.DeleteFunc(slices.Clone(holds), func(x string) bool { return x == h })
}

// Short aliases used across the package.
type (
	storeTx  = store.Store
	nodeInfo = scheduler.NodeInfo
)
