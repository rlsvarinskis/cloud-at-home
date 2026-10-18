// Package service implements agentd's public API (Agent, Task and Model services) and the HTTP surface
// that serves them. Business rules live here; persistence goes through store.Store, VM interaction
// through vmclient.VM and model access through llm.Client.
package service

import (
	"log/slog"
	"time"
)

// Limits on user-supplied content. They are constants rather than configuration because the store and
// the runner are sized around them.
const (
	maxSystemPromptBytes = 8 << 10  // 8 KiB
	maxPromptBytes       = 64 << 10 // 64 KiB
	maxCheckCommandBytes = 16 << 10 // 16 KiB
	defaultMaxSteps      = 40
	maxMaxSteps          = 200
	maxWaitSeconds       = 30
	defaultMaxCallTokens = 32768 // per-reply token cap when a task does not set one
)

// VRAM budget defaults. The fits_vram check compares a model's weights (its size on disk) with the budget less a
// headroom: a rough heuristic that ignores the KV cache and the runtime's own memory.
const (
	defaultVRAMGiB  = 16
	vramHeadroomGiB = 0.5
)

// Config tunes the service. The zero value is usable: withDefaults fills every unset field.
type Config struct {
	// HoldPrefix names the holder under which an agent places its hold on its VM: the holder is
	// HoldPrefix + "/" + agent ID. Default "agentd".
	HoldPrefix string
	// VRAMGiB is the GPU memory budget that ListModels compares model sizes against. Default 16.
	VRAMGiB float64

	Log *slog.Logger
	Now func() time.Time // injectable clock for tests
}

func (c Config) withDefaults() Config {
	if c.HoldPrefix == "" {
		c.HoldPrefix = "agentd"
	}
	if c.VRAMGiB <= 0 {
		c.VRAMGiB = defaultVRAMGiB
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}
