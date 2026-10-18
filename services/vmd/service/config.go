// Package service implements vmd's public API (VM, Disk, Image, Exec, Metrics services) and the
// reconciler that drives resources towards their desired state.
package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/ids"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/scheduler"
)

// Quota limits what one project may consume. Zero means "unlimited" for that dimension.
type Quota struct {
	MaxVMs      int
	MaxCPUs     int   // vCPUs of VMs that should be running
	MaxMemoryMB int64 // RAM of VMs that should be running
	MaxDiskGB   int
}

type Config struct {
	Quota     Quota
	Scheduler scheduler.Config

	// Resource limits per VM / disk.
	MaxVMCPUs     int   // default 64
	MinMemoryMB   int64 // default 256
	MaxMemoryMB   int64 // default 1 MiB (= 1 TiB)
	MaxDiskSizeGB int   // default 4096
	DefaultDiskGB int   // default 20

	// Reconciler.
	ReconcileInterval time.Duration // how often to look for due work (default 1s)
	ResyncInterval    time.Duration // re-check running VMs this often (default 10s)
	LeaseTTL          time.Duration // claim lease (default 30s)
	BatchSize         int           // VMs claimed per tick (default 32)

	// Sharding hooks (leave nil for a single database). NewID makes resource IDs; for a sharded store it
	// should embed the project's shard (shard.Store.NewID). ProjectContext tags a context so a transaction is
	// routed to the project's shard (shard.WithProject).
	NewID          func(prefix, project string) string
	ProjectContext func(ctx context.Context, project string) context.Context

	Log *slog.Logger
	Now func() time.Time // injectable clock for tests
}

func (c Config) withDefaults() Config {
	if c.MaxVMCPUs <= 0 {
		c.MaxVMCPUs = 64
	}
	if c.MinMemoryMB <= 0 {
		c.MinMemoryMB = 256
	}
	if c.MaxMemoryMB <= 0 {
		c.MaxMemoryMB = 1 << 20
	}
	if c.MaxDiskSizeGB <= 0 {
		c.MaxDiskSizeGB = 4096
	}
	if c.DefaultDiskGB <= 0 {
		c.DefaultDiskGB = 20
	}
	if c.ReconcileInterval <= 0 {
		c.ReconcileInterval = time.Second
	}
	if c.ResyncInterval <= 0 {
		c.ResyncInterval = 10 * time.Second
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 30 * time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 32
	}
	if c.NewID == nil {
		c.NewID = func(prefix, _ string) string { return ids.New(prefix) }
	}
	if c.ProjectContext == nil {
		c.ProjectContext = func(ctx context.Context, _ string) context.Context { return ctx }
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}
