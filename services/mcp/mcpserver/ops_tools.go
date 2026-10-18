package mcpserver

import (
	"context"
	"fmt"
	"math"
	"strings"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
)

// registerOpsTools registers the usage and fleet tools: agent_metrics (one machine), cluster_capacity (operators)
// and node_drain (operators). None of them waits on the VM, so each call is a single downstream request.
func (s *Server) registerOpsTools() {
	addTool(s, "agent_metrics",
		"Current usage of one machine, as plain text: cpu (top-style: 100% per busy vCPU, so the maximum is 100% per vCPU), "+
			"load averages, memory and swap, uptime, one line per guest filesystem (disk <disk_id or ?> <mount> <device>) and "+
			"one line per host-side disk allocation (the space its image really occupies vs. its virtual size). "+
			"The machine must be running; otherwise the error says so. Needs vm.metrics on the project. "+
			"Args: name (required, the machine's name), project (optional, defaults to the configured project).",
		s.opsAgentMetrics)
	addTool(s, "cluster_capacity",
		"Cluster capacity for operators, one line per node: state (ready, draining or down), CPUs and memory allocated to VMs "+
			"that should run, what the scheduler would still hand out, free disk, and running VMs. The final 'total:' line sums "+
			"ready nodes only. Operators only (needs capacity.get); no arguments.",
		s.opsClusterCapacity)
	addTool(s, "node_drain",
		"Operators only (needs node.drain). Stop (draining=true) or resume (draining=false) placing new VM starts on a node. "+
			"Running VMs on the node keep running; stopped VMs on it wait until the node is resumed. The decision persists across "+
			"outages and restarts. Returns the node's resulting state line. Args: node_id (required), draining (required, boolean).",
		s.opsNodeDrain)
}

type opsMetricsArgs struct {
	Name    string `json:"name" jsonschema:"the machine (VM) name to inspect"`
	Project string `json:"project,omitempty" jsonschema:"project of the machine; defaults to the configured project"`
}

type opsNoArgs struct{}

type opsDrainArgs struct {
	NodeID   string `json:"node_id" jsonschema:"ID of the node to drain or resume"`
	Draining bool   `json:"draining" jsonschema:"true stops placing new VM starts on the node; false resumes it"`
}

func (s *Server) opsAgentMetrics(ctx context.Context, in opsMetricsArgs) (string, error) {
	if in.Name == "" {
		return "", fmt.Errorf("name is required: give the machine's name (see agent_list)")
	}
	vm, err := s.vmByName(ctx, in.Project, in.Name)
	if err != nil {
		return "", err
	}
	res, err := s.cfg.Clients.Metrics.GetVMMetrics(ctx, connect.NewRequest(&vmdv1.GetVMMetricsRequest{VmId: vm.GetId()}))
	if err != nil {
		return "", err
	}
	return opsFormatVMMetrics(res.Msg), nil
}

func (s *Server) opsClusterCapacity(ctx context.Context, _ opsNoArgs) (string, error) {
	res, err := s.cfg.Clients.Metrics.GetCapacity(ctx, connect.NewRequest(&vmdv1.GetCapacityRequest{}))
	if err != nil {
		return "", err
	}
	lines := make([]string, 0, len(res.Msg.GetNodes())+1)
	for _, n := range res.Msg.GetNodes() {
		lines = append(lines, opsNodeLine(n))
	}
	lines = append(lines, "total: "+opsCapacityFigures(res.Msg.GetTotal()))
	return strings.Join(lines, "\n"), nil
}

func (s *Server) opsNodeDrain(ctx context.Context, in opsDrainArgs) (string, error) {
	if in.NodeID == "" {
		return "", fmt.Errorf("node_id is required: give the node's ID (see cluster_capacity)")
	}
	res, err := s.cfg.Clients.Cluster.SetNodeDraining(ctx, connect.NewRequest(&vmdv1.SetNodeDrainingRequest{
		NodeId: in.NodeID, Draining: in.Draining}))
	if err != nil {
		return "", err
	}
	return opsNodeLine(res.Msg), nil
}

// opsFormatVMMetrics renders a VM sample in the fixed line order: cpu, load, memory, uptime, guest filesystems,
// host disk allocations. Each line is deterministic, so tests can compare whole strings.
func opsFormatVMMetrics(m *vmdv1.VMMetrics) string {
	cores := m.GetCpuCores()
	lines := []string{
		fmt.Sprintf("cpu: %.0f%% of %d%% (%d vCPUs, top-style: 100 per busy vCPU)", m.GetCpuPercent(), int64(cores)*100, cores),
		fmt.Sprintf("load: %.1f %.1f %.1f", m.GetLoad1(), m.GetLoad5(), m.GetLoad15()),
		fmt.Sprintf("memory: %s used / %s (%s available); swap %s / %s",
			opsBytes(m.GetMemUsedBytes()), opsBytes(m.GetMemTotalBytes()), opsBytes(m.GetMemAvailableBytes()),
			opsBytes(m.GetSwapUsedBytes()), opsBytes(m.GetSwapTotalBytes())),
		"uptime: " + opsUptime(m.GetUptimeSeconds()),
	}
	for _, f := range m.GetFilesystems() {
		id := f.GetDiskId()
		if id == "" {
			id = "?"
		}
		lines = append(lines, fmt.Sprintf("disk %s %s %s: %s used / %s (%d%%)", id, f.GetMountPoint(), f.GetDevice(),
			opsBytes(f.GetUsedBytes()), opsBytes(f.GetSizeBytes()), opsPercent(f.GetUsedBytes(), f.GetSizeBytes())))
	}
	for _, d := range m.GetDisks() {
		lines = append(lines, fmt.Sprintf("host disk %s: allocated %s of %s virtual",
			d.GetDiskId(), opsBytes(d.GetAllocatedBytes()), opsBytes(d.GetVirtualSizeBytes())))
	}
	return strings.Join(lines, "\n")
}

// opsNodeLine is "node <id> [<state>]: <capacity figures>".
func opsNodeLine(n *vmdv1.NodeCapacity) string {
	return fmt.Sprintf("node %s [%s]: %s", n.GetNodeId(), n.GetState(), opsCapacityFigures(n))
}

// opsCapacityFigures is the part of a capacity line after the node prefix (also used for the total line).
func opsCapacityFigures(n *vmdv1.NodeCapacity) string {
	return fmt.Sprintf("cpus %d/%d allocated (%d available), memory %d/%d MiB (%d available), disk %s free of %s, vms %d/%d running",
		n.GetCpusAllocated(), n.GetCpusTotal(), n.GetCpusAvailable(),
		n.GetMemoryMbAllocated(), n.GetMemoryMbTotal(), n.GetMemoryMbAvailable(),
		opsBytes(n.GetDiskBytesFree()), opsBytes(n.GetDiskBytesTotal()),
		n.GetVmsRunning(), n.GetVmsTotal())
}

// opsBytes formats a byte count with a binary unit and one decimal: "0 B", "1023 B", "1.0 KiB", "4.2 GiB", "1.0 TiB".
// Plain bytes have no decimal. Negative counts (never valid, but a bad sample must not print nonsense) read as 0 B.
// A value that would round up to 1024 of a unit is shown in the next unit, so "1023.99 KiB" reads "1.0 MiB".
func opsBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	i := -1
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if math.Round(v*10) >= 10240 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// opsPercent is used/size as a whole percent, rounded; 0 when size is unknown.
func opsPercent(used, size int64) int64 {
	if size <= 0 || used <= 0 {
		return 0
	}
	return int64(math.Round(float64(used) * 100 / float64(size)))
}

// opsUptime renders seconds as "42s", "5m", "1h 12m" or "2d 3h 4m". Negative values read as 0s.
func opsUptime(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	d, h, m, s := sec/86400, sec%86400/3600, sec%3600/60, sec%60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh %dm", d, h, m)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
