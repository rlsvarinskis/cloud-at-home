package promcollect

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	vmdv1 "example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/internal/vmd/domain"
	"example.com/homecloud/internal/vmd/service"
)

// SampleOnce runs one sampling pass: it samples every running VM, with at most Concurrency samples in
// flight and SampleTimeout per VM, and updates the cache. A VM that is no longer running is dropped
// from the cache. A VM whose sample fails keeps its previous one, which expires on its own schedule.
// Passes never overlap.
func (c *Collector) SampleOnce(ctx context.Context) {
	c.sampleMu.Lock()
	defer c.sampleMu.Unlock()

	at := c.opts.Now()
	running, err := c.runningVMs(ctx)
	if err != nil {
		// Leave the cache alone: without a list we cannot tell which VMs stopped.
		c.log.Warn("list running vms", "err", err)
		return
	}

	type result struct {
		vm *domain.VM
		m  *vmdv1.VMMetrics
		// err is the sample error, if any.
		err error
	}
	results := make([]result, len(running))
	sem := make(chan struct{}, c.opts.Concurrency)
	var wg sync.WaitGroup
	for i, vm := range running {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			vctx, cancel := context.WithTimeout(ctx, c.opts.SampleTimeout)
			defer cancel()
			m, err := service.SampleVM(vctx, c.nodes, vm, at)
			results[i] = result{vm: vm, m: m, err: err}
		}()
	}
	wg.Wait()

	keep := make(map[string]bool, len(running))
	for _, vm := range running {
		keep[vm.ID] = true
	}
	c.mu.Lock()
	for id := range c.cache {
		if !keep[id] {
			delete(c.cache, id)
		}
	}
	for _, r := range results {
		if r.err != nil {
			c.sampleErrors.Inc()
			c.log.Warn("sample vm", "vm", r.vm.ID, "err", r.err)
			continue
		}
		c.cache[r.vm.ID] = &cachedSample{
			vmID: r.vm.ID, project: r.vm.ProjectID, name: r.vm.Name, node: r.vm.NodeID,
			at: at, m: r.m,
		}
	}
	c.mu.Unlock()

	now := c.opts.Now()
	c.sampleDuration.Set(now.Sub(at).Seconds())
	c.sampleTimestamp.Set(float64(now.UnixNano()) / 1e9)
}

// runningVMs lists the VMs in the running state on every node the node registry knows.
func (c *Collector) runningVMs(ctx context.Context) ([]*domain.VM, error) {
	infos, err := c.nodes.Info(ctx)
	if err != nil {
		return nil, err
	}
	var out []*domain.VM
	for _, ni := range infos {
		vms, err := c.st.ListVMsByNode(ctx, ni.ID)
		if err != nil {
			return nil, fmt.Errorf("list vms of %s: %w", ni.ID, err)
		}
		for _, v := range vms {
			if v.State == domain.VMRunning {
				out = append(out, v)
			}
		}
	}
	return out, nil
}

// collectVMs emits the cached guest metrics. A sample older than three sampling intervals is not
// exported; it stays in the cache until the VM stops or a later sample succeeds.
func (c *Collector) collectVMs(ch chan<- prometheus.Metric) {
	now := c.opts.Now()
	stale := staleSampleIntervals * c.opts.SampleInterval

	c.mu.Lock()
	samples := make([]*cachedSample, 0, len(c.cache))
	for _, s := range c.cache {
		if now.Sub(s.at) <= stale {
			samples = append(samples, s)
		}
	}
	c.mu.Unlock()
	sort.Slice(samples, func(i, j int) bool { return samples[i].vmID < samples[j].vmID })

	for _, s := range samples {
		c.emitVM(ch, now, s)
	}
}

// emitVM emits every per-VM, per-filesystem and per-disk series of one sample.
func (c *Collector) emitVM(ch chan<- prometheus.Metric, now time.Time, s *cachedSample) {
	m := s.m
	lv := []string{s.vmID, s.project, s.name, s.node}
	c.gauge(ch, vmCPUPercent, m.GetCpuPercent(), lv...)
	c.gauge(ch, vmCPUCores, float64(m.GetCpuCores()), lv...)
	c.gauge(ch, vmLoad1, m.GetLoad1(), lv...)
	c.gauge(ch, vmLoad5, m.GetLoad5(), lv...)
	c.gauge(ch, vmLoad15, m.GetLoad15(), lv...)
	c.gauge(ch, vmMemTotal, float64(m.GetMemTotalBytes()), lv...)
	c.gauge(ch, vmMemUsed, float64(m.GetMemUsedBytes()), lv...)
	c.gauge(ch, vmMemAvailable, float64(m.GetMemAvailableBytes()), lv...)
	c.gauge(ch, vmSwapTotal, float64(m.GetSwapTotalBytes()), lv...)
	c.gauge(ch, vmSwapUsed, float64(m.GetSwapUsedBytes()), lv...)
	c.gauge(ch, vmUptime, float64(m.GetUptimeSeconds()), lv...)
	c.gauge(ch, vmMetricsAge, now.Sub(s.at).Seconds(), lv...)
	for _, f := range m.GetFilesystems() {
		fl := []string{s.vmID, s.project, s.name, s.node, f.GetDiskId(), f.GetDevice(), f.GetMountPoint()}
		c.gauge(ch, vmFSSize, float64(f.GetSizeBytes()), fl...)
		c.gauge(ch, vmFSUsed, float64(f.GetUsedBytes()), fl...)
	}
	for _, d := range m.GetDisks() {
		dl := []string{s.vmID, s.project, s.name, s.node, d.GetDiskId()}
		c.gauge(ch, vmDiskVirtualSize, float64(d.GetVirtualSizeBytes()), dl...)
		c.gauge(ch, vmDiskAllocated, float64(d.GetAllocatedBytes()), dl...)
	}
}
