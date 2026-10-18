package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/node"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/scheduler"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/store"
)

// Nodes is what the services need to know about hosts.
type Nodes interface {
	// Driver returns the driver for a node (errs.ErrNotFound if unknown).
	Driver(id string) (node.Driver, error)
	// Info reports every known node with its capacity and what is already allocated on it.
	Info(ctx context.Context) ([]scheduler.NodeInfo, error)
}

// NodeManager keeps the registry of node drivers and their last known capacity in the store, so that
// every control-plane replica sees the same view of the fleet.
type NodeManager struct {
	st        store.Store
	log       *slog.Logger
	now       func() time.Time
	downAfter time.Duration

	mu      sync.RWMutex
	drivers map[string]node.Driver
	addrs   map[string]string
}

func NewNodeManager(st store.Store, log *slog.Logger, now func() time.Time) *NodeManager {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &NodeManager{st: st, log: log, now: now, downAfter: 15 * time.Second,
		drivers: map[string]node.Driver{}, addrs: map[string]string{}}
}

// Add registers (or replaces) a node's driver. address is informational (shown to operators).
func (m *NodeManager) Add(id, address string, d node.Driver) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drivers[id], m.addrs[id] = d, address
}

func (m *NodeManager) Driver(id string) (node.Driver, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.drivers[id]
	if !ok {
		return nil, errs.NotFound("node %s", id)
	}
	return d, nil
}

// Poll refreshes every registered node's capacity and health in the store.
func (m *NodeManager) Poll(ctx context.Context) {
	m.mu.RLock()
	ids := make([]string, 0, len(m.drivers))
	for id := range m.drivers {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			m.pollOne(ctx, id)
		}(id)
	}
	wg.Wait()
}

func (m *NodeManager) pollOne(ctx context.Context, id string) {
	d, err := m.Driver(id)
	if err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := d.GetCapacity(cctx, &nodev1.GetCapacityRequest{})
	prev, perr := m.st.GetNode(ctx, id)
	n := &domain.Node{ID: id, Address: m.addrs[id], State: domain.NodeReady}
	legacyDrain := false
	if perr == nil {
		*n = *prev
		n.Address = m.addrs[id]
		if n.State == domain.NodeDraining { // older rows stored the drain decision as the state
			n.State, legacyDrain = domain.NodeReady, true
		}
	}
	if err != nil {
		// Only declare the node down after it has been silent for a while. Drain is the operator's decision and
		// lives apart from health (the store never lets a health update change it), so an outage cannot erase it.
		if perr == nil && m.now().Sub(prev.LastHeartbeat) > m.downAfter {
			if prev.State != domain.NodeDown {
				m.log.Warn("node down", "node", id, "err", err)
			}
			n.State = domain.NodeDown
		}
		if perr != nil {
			n.State, n.LastHeartbeat = domain.NodeDown, time.Time{}
		}
	} else {
		c := res.GetCapacity()
		n.CPUs, n.MemoryMB, n.DiskBytesFree, n.DiskBytesTotal = int(c.GetCpusTotal()), c.GetMemoryMbTotal(), c.GetDiskBytesFree(), c.GetDiskBytesTotal()
		n.LastHeartbeat, n.State = m.now(), domain.NodeReady
	}
	if err := m.st.UpsertNode(ctx, n); err != nil {
		m.log.Error("upsert node", "node", id, "err", err)
		return
	}
	if legacyDrain {
		_ = m.st.SetNodeDrain(ctx, id, true)
	}
}

// Run polls until ctx is cancelled.
func (m *NodeManager) Run(ctx context.Context, every time.Duration) {
	m.Poll(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Poll(ctx)
		}
	}
}

// Info joins the node rows with the allocation implied by the VMs placed on each node.
func (m *NodeManager) Info(ctx context.Context) ([]scheduler.NodeInfo, error) {
	rows, err := m.st.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]scheduler.NodeInfo, 0, len(rows))
	for _, n := range rows {
		vms, err := m.st.ListVMsByNode(ctx, n.ID)
		if err != nil {
			return nil, fmt.Errorf("list vms of %s: %w", n.ID, err)
		}
		ni := scheduler.NodeInfo{ID: n.ID, State: n.Effective(), CPUs: n.CPUs, MemoryMB: n.MemoryMB, DiskBytesFree: n.DiskBytesFree}
		for _, v := range vms {
			if v.Desired == domain.PowerRunning {
				ni.AllocatedCPUs += v.CPUs
				ni.AllocatedMemoryMB += v.MemoryMB
			}
		}
		out = append(out, ni)
	}
	return out, nil
}
