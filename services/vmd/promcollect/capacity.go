package promcollect

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/service"
)

// bytesPerMB converts the node API's memory figures (MiB) to the bytes the gauges export.
const bytesPerMB = 1 << 20

// allVMStates is every VM state, so the vms gauge always carries all of them (zeros included).
var allVMStates = []domain.VMState{
	domain.VMPending, domain.VMStopped, domain.VMStarting, domain.VMRunning,
	domain.VMStopping, domain.VMError, domain.VMDeleting,
}

// collectCapacity emits node and cluster capacity, and VM counts, read fresh from the store. If the
// store cannot be read the whole group is skipped and counted as a scrape error.
func (c *Collector) collectCapacity(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	nodes, total, err := service.ClusterCapacity(ctx, c.st, c.sched)
	if err != nil {
		c.scrapeFailed(sourceCapacity, err)
		return
	}
	states, desired, err := c.countVMs(ctx)
	if err != nil {
		c.scrapeFailed(sourceCapacity, err)
		return
	}

	ready := 0
	for _, n := range nodes {
		id := n.GetNodeId()
		state := n.GetState()
		if state == string(domain.NodeReady) {
			ready++
		}
		c.gauge(ch, nodeUp, boolFloat(state == string(domain.NodeReady)), id)
		c.gauge(ch, nodeDraining, boolFloat(state == string(domain.NodeDraining)), id)
		c.gauge(ch, nodeCPUsTotal, float64(n.GetCpusTotal()), id)
		c.gauge(ch, nodeCPUsAllocated, float64(n.GetCpusAllocated()), id)
		c.gauge(ch, nodeCPUsAvailable, float64(n.GetCpusAvailable()), id)
		c.gauge(ch, nodeMemTotal, float64(n.GetMemoryMbTotal())*bytesPerMB, id)
		c.gauge(ch, nodeMemAllocated, float64(n.GetMemoryMbAllocated())*bytesPerMB, id)
		c.gauge(ch, nodeMemAvailable, float64(n.GetMemoryMbAvailable())*bytesPerMB, id)
		c.gauge(ch, nodeDiskTotal, float64(n.GetDiskBytesTotal()), id)
		c.gauge(ch, nodeDiskFree, float64(n.GetDiskBytesFree()), id)
		c.gauge(ch, nodeVMsTotal, float64(n.GetVmsTotal()), id)
		c.gauge(ch, nodeVMsRunning, float64(n.GetVmsRunning()), id)
		hb := 0.0
		if t := n.GetLastHeartbeat(); t != nil {
			hb = float64(t.AsTime().UnixNano()) / 1e9
		}
		c.gauge(ch, nodeLastHeartbeatTS, hb, id)
	}

	c.gauge(ch, clusterCPUsTotal, float64(total.GetCpusTotal()))
	c.gauge(ch, clusterCPUsAllocated, float64(total.GetCpusAllocated()))
	c.gauge(ch, clusterCPUsAvailable, float64(total.GetCpusAvailable()))
	c.gauge(ch, clusterMemTotal, float64(total.GetMemoryMbTotal())*bytesPerMB)
	c.gauge(ch, clusterMemAllocated, float64(total.GetMemoryMbAllocated())*bytesPerMB)
	c.gauge(ch, clusterMemAvailable, float64(total.GetMemoryMbAvailable())*bytesPerMB)
	c.gauge(ch, clusterDiskTotal, float64(total.GetDiskBytesTotal()))
	c.gauge(ch, clusterDiskFree, float64(total.GetDiskBytesFree()))
	c.gauge(ch, clusterVMsTotal, float64(total.GetVmsTotal()))
	c.gauge(ch, clusterVMsRunning, float64(total.GetVmsRunning()))
	c.gauge(ch, clusterNodesReady, float64(ready))

	for _, s := range allVMStates {
		c.gauge(ch, vmsByState, float64(states[s]), string(s))
	}
	c.gauge(ch, vmsDesiredRunning, float64(desired))
}

// countVMs counts the VMs on every node by state, and how many are desired running. VMs that are not
// placed on a listed node (no node yet) are not counted.
func (c *Collector) countVMs(ctx context.Context) (map[domain.VMState]int, int, error) {
	rows, err := c.st.ListNodes(ctx)
	if err != nil {
		return nil, 0, err
	}
	states := make(map[domain.VMState]int, len(allVMStates))
	desired := 0
	for _, n := range rows {
		vms, err := c.st.ListVMsByNode(ctx, n.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("list vms of %s: %w", n.ID, err)
		}
		for _, v := range vms {
			states[v.State]++
			if v.Desired == domain.PowerRunning {
				desired++
			}
		}
	}
	return states, desired, nil
}

// scrapeFailed counts and logs a source that could not be read.
func (c *Collector) scrapeFailed(source string, err error) {
	c.scrapeErrors.WithLabelValues(source).Inc()
	c.log.Warn("scrape source failed", "source", source, "err", err)
}
