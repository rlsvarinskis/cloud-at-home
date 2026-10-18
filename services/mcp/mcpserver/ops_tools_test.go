package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1/vmdv1connect"
)

const opsGiB = int64(1) << 30

// opsFakeVMs answers ListVMs from a fixed list and records the project asked for.
type opsFakeVMs struct {
	vmdv1connect.VMServiceClient
	vms         []*vmdv1.VM
	err         error
	gotProjects []string
}

func (f *opsFakeVMs) ListVMs(_ context.Context, req *connect.Request[vmdv1.ListVMsRequest]) (*connect.Response[vmdv1.ListVMsResponse], error) {
	f.gotProjects = append(f.gotProjects, req.Msg.GetProjectId())
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(&vmdv1.ListVMsResponse{Vms: f.vms}), nil
}

// opsFakeMetrics answers GetVMMetrics and GetCapacity.
type opsFakeMetrics struct {
	vmdv1connect.MetricsServiceClient
	metrics    *vmdv1.VMMetrics
	metricsErr error
	gotVMID    string
	capacity   *vmdv1.GetCapacityResponse
	capErr     error
}

func (f *opsFakeMetrics) GetVMMetrics(_ context.Context, req *connect.Request[vmdv1.GetVMMetricsRequest]) (*connect.Response[vmdv1.VMMetrics], error) {
	f.gotVMID = req.Msg.GetVmId()
	if f.metricsErr != nil {
		return nil, f.metricsErr
	}
	return connect.NewResponse(f.metrics), nil
}

func (f *opsFakeMetrics) GetCapacity(context.Context, *connect.Request[vmdv1.GetCapacityRequest]) (*connect.Response[vmdv1.GetCapacityResponse], error) {
	if f.capErr != nil {
		return nil, f.capErr
	}
	return connect.NewResponse(f.capacity), nil
}

// opsFakeCluster answers SetNodeDraining and records the request.
type opsFakeCluster struct {
	vmdv1connect.ClusterServiceClient
	node   *vmdv1.NodeCapacity
	err    error
	gotReq *vmdv1.SetNodeDrainingRequest
}

func (f *opsFakeCluster) SetNodeDraining(_ context.Context, req *connect.Request[vmdv1.SetNodeDrainingRequest]) (*connect.Response[vmdv1.NodeCapacity], error) {
	f.gotReq = req.Msg
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(f.node), nil
}

func opsSampleMetrics() *vmdv1.VMMetrics {
	return &vmdv1.VMMetrics{
		VmId:              "vm-1",
		CpuPercent:        312.4,
		CpuCores:          8,
		Load1:             3.1,
		Load5:             2.8,
		Load15:            2.0,
		MemTotalBytes:     16 * opsGiB,
		MemUsedBytes:      opsGiBOf(4.2),
		MemAvailableBytes: opsGiBOf(11.8),
		UptimeSeconds:     4320,
		Filesystems: []*vmdv1.GuestFilesystem{
			{DiskId: "d1", Device: "/dev/vda1", MountPoint: "/", SizeBytes: 40 * opsGiB, UsedBytes: opsGiBOf(12.3)},
			{DiskId: "", Device: "/dev/vdb1", MountPoint: "/data", SizeBytes: 10 * opsGiB, UsedBytes: 0},
		},
		Disks: []*vmdv1.DiskAllocation{
			{DiskId: "d1", VirtualSizeBytes: 40 * opsGiB, AllocatedBytes: opsGiBOf(3.2)},
		},
	}
}

func opsWantMetrics() string {
	return strings.Join([]string{
		"cpu: 312% of 800% (8 vCPUs, top-style: 100 per busy vCPU)",
		"load: 3.1 2.8 2.0",
		"memory: 4.2 GiB used / 16.0 GiB (11.8 GiB available); swap 0 B / 0 B",
		"uptime: 1h 12m",
		"disk d1 / /dev/vda1: 12.3 GiB used / 40.0 GiB (31%)",
		"disk ? /data /dev/vdb1: 0 B used / 10.0 GiB (0%)",
		"host disk d1: allocated 3.2 GiB of 40.0 GiB virtual",
	}, "\n")
}

func opsWebVM() *vmdv1.VM {
	return &vmdv1.VM{Id: "vm-1", Name: "web", ProjectId: "p1"}
}

func TestOpsBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{-1, "0 B"},
		{-1 << 40, "0 B"},
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1047552, "1023.0 KiB"}, // 1023 KiB exactly: still KiB
		{1048536, "1.0 MiB"},    // 1023.96 KiB rounds to 1024.0, so it moves up a unit
		{1048575, "1.0 MiB"},    // one byte below 1 MiB
		{1048576, "1.0 MiB"},    // exactly 1 MiB
		{opsGiBOf(4.2), "4.2 GiB"},
		{16 * opsGiB, "16.0 GiB"},
		{1 << 40, "1.0 TiB"},
		{1024 << 40, "1024.0 TiB"}, // no unit above TiB
		{1<<63 - 1, "8388608.0 TiB"},
	}
	for _, c := range cases {
		if got := opsBytes(c.n); got != c.want {
			t.Errorf("opsBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestOpsUptime(t *testing.T) {
	cases := []struct {
		sec  int64
		want string
	}{
		{-5, "0s"},
		{0, "0s"},
		{59, "59s"},
		{60, "1m"},
		{4320, "1h 12m"},
		{86399, "23h 59m"},
		{86400, "1d 0h 0m"},
		{90061, "1d 1h 1m"},
	}
	for _, c := range cases {
		if got := opsUptime(c.sec); got != c.want {
			t.Errorf("opsUptime(%d) = %q, want %q", c.sec, got, c.want)
		}
	}
}

func TestOpsPercent(t *testing.T) {
	if got := opsPercent(5, 0); got != 0 {
		t.Errorf("unknown size: got %d, want 0", got)
	}
	if got := opsPercent(-1, 10); got != 0 {
		t.Errorf("negative used: got %d, want 0", got)
	}
	if got := opsPercent(10, 40); got != 25 {
		t.Errorf("10/40: got %d, want 25", got)
	}
	if got := opsPercent(1, 3); got != 33 {
		t.Errorf("1/3: got %d, want 33", got)
	}
	if got := opsPercent(2, 3); got != 67 {
		t.Errorf("2/3: got %d, want 67", got)
	}
}

func TestOpsAgentMetricsHappyPath(t *testing.T) {
	vms := &opsFakeVMs{vms: []*vmdv1.VM{{Id: "vm-0", Name: "other"}, opsWebVM()}}
	met := &opsFakeMetrics{metrics: opsSampleMetrics()}
	cs := newTestSession(t, Clients{VMs: vms, Metrics: met})

	out, isErr := callTool(t, cs, "agent_metrics", map[string]any{"name": "web"})
	if isErr {
		t.Fatalf("unexpected tool error: %s", out)
	}
	if out != opsWantMetrics() {
		t.Errorf("output mismatch\n got:\n%s\nwant:\n%s", out, opsWantMetrics())
	}
	if met.gotVMID != "vm-1" {
		t.Errorf("GetVMMetrics asked for %q, want vm-1", met.gotVMID)
	}
	if len(vms.gotProjects) != 1 || vms.gotProjects[0] != "p1" {
		t.Errorf("ListVMs projects = %v, want [p1] (the configured default)", vms.gotProjects)
	}
}

func TestOpsAgentMetricsExplicitProjectAndNoDisks(t *testing.T) {
	vms := &opsFakeVMs{vms: []*vmdv1.VM{{Id: "vm-9", Name: "api", ProjectId: "p2"}}}
	m := &vmdv1.VMMetrics{CpuPercent: 0, CpuCores: 1, UptimeSeconds: 5}
	met := &opsFakeMetrics{metrics: m}
	cs := newTestSession(t, Clients{VMs: vms, Metrics: met})

	out, isErr := callTool(t, cs, "agent_metrics", map[string]any{"name": "api", "project": "p2"})
	if isErr {
		t.Fatalf("unexpected tool error: %s", out)
	}
	want := strings.Join([]string{
		"cpu: 0% of 100% (1 vCPUs, top-style: 100 per busy vCPU)",
		"load: 0.0 0.0 0.0",
		"memory: 0 B used / 0 B (0 B available); swap 0 B / 0 B",
		"uptime: 5s",
	}, "\n")
	if out != want {
		t.Errorf("output mismatch\n got:\n%s\nwant:\n%s", out, want)
	}
	if vms.gotProjects[0] != "p2" {
		t.Errorf("ListVMs project = %q, want p2", vms.gotProjects[0])
	}
	if met.gotVMID != "vm-9" {
		t.Errorf("GetVMMetrics asked for %q, want vm-9", met.gotVMID)
	}
}

func TestOpsAgentMetricsValidationAndErrors(t *testing.T) {
	t.Run("missing name", func(t *testing.T) {
		cs := newTestSession(t, Clients{VMs: &opsFakeVMs{}, Metrics: &opsFakeMetrics{}})
		out, isErr := callTool(t, cs, "agent_metrics", map[string]any{"name": ""})
		if !isErr || !strings.Contains(out, "name is required") {
			t.Errorf("got (%q, %v), want a name-is-required tool error", out, isErr)
		}
	})
	t.Run("machine not found", func(t *testing.T) {
		cs := newTestSession(t, Clients{VMs: &opsFakeVMs{}, Metrics: &opsFakeMetrics{}})
		out, isErr := callTool(t, cs, "agent_metrics", map[string]any{"name": "ghost"})
		if !isErr || !strings.Contains(out, "not_found") || !strings.Contains(out, `no machine named "ghost" in project "p1"`) {
			t.Errorf("got (%q, %v), want a not_found tool error", out, isErr)
		}
	})
	t.Run("list error passes through", func(t *testing.T) {
		vms := &opsFakeVMs{err: connect.NewError(connect.CodeUnavailable, errors.New("vmd is down"))}
		cs := newTestSession(t, Clients{VMs: vms, Metrics: &opsFakeMetrics{}})
		out, isErr := callTool(t, cs, "agent_metrics", map[string]any{"name": "web"})
		if !isErr || out != "unavailable: vmd is down" {
			t.Errorf("got (%q, %v), want the unavailable error unchanged", out, isErr)
		}
	})
	t.Run("stopped vm error passes through", func(t *testing.T) {
		vms := &opsFakeVMs{vms: []*vmdv1.VM{opsWebVM()}}
		met := &opsFakeMetrics{metricsErr: connect.NewError(connect.CodeFailedPrecondition, errors.New("vm vm-1 is stopped, not running"))}
		cs := newTestSession(t, Clients{VMs: vms, Metrics: met})
		out, isErr := callTool(t, cs, "agent_metrics", map[string]any{"name": "web"})
		if !isErr || out != "failed_precondition: vm vm-1 is stopped, not running" {
			t.Errorf("got (%q, %v), want the precondition error unchanged", out, isErr)
		}
	})
	t.Run("permission error passes through", func(t *testing.T) {
		vms := &opsFakeVMs{vms: []*vmdv1.VM{opsWebVM()}}
		met := &opsFakeMetrics{metricsErr: connect.NewError(connect.CodePermissionDenied, errors.New("missing vm.metrics"))}
		cs := newTestSession(t, Clients{VMs: vms, Metrics: met})
		out, isErr := callTool(t, cs, "agent_metrics", map[string]any{"name": "web"})
		if !isErr || out != "permission_denied: missing vm.metrics" {
			t.Errorf("got (%q, %v), want the permission error unchanged", out, isErr)
		}
	})
	t.Run("plain error", func(t *testing.T) {
		vms := &opsFakeVMs{vms: []*vmdv1.VM{opsWebVM()}}
		met := &opsFakeMetrics{metricsErr: errors.New("boom")}
		cs := newTestSession(t, Clients{VMs: vms, Metrics: met})
		out, isErr := callTool(t, cs, "agent_metrics", map[string]any{"name": "web"})
		if !isErr || out != "boom" {
			t.Errorf("got (%q, %v), want boom", out, isErr)
		}
	})
}

func TestOpsClusterCapacity(t *testing.T) {
	met := &opsFakeMetrics{capacity: &vmdv1.GetCapacityResponse{
		Nodes: []*vmdv1.NodeCapacity{
			{NodeId: "n1", State: "ready", CpusTotal: 16, CpusAllocated: 6, CpusAvailable: 12,
				MemoryMbTotal: 32768, MemoryMbAllocated: 8192, MemoryMbAvailable: 24576,
				DiskBytesTotal: 500 * opsGiB, DiskBytesFree: 120 * opsGiB, VmsTotal: 3, VmsRunning: 2},
			{NodeId: "n2", State: "draining", CpusTotal: 8,
				MemoryMbTotal: 16384, DiskBytesTotal: 250 * opsGiB, DiskBytesFree: 50 * opsGiB, VmsTotal: 1},
		},
		Total: &vmdv1.NodeCapacity{NodeId: "*", State: "ready", CpusTotal: 24, CpusAllocated: 6, CpusAvailable: 12,
			MemoryMbTotal: 49152, MemoryMbAllocated: 8192, MemoryMbAvailable: 24576,
			DiskBytesTotal: 750 * opsGiB, DiskBytesFree: 170 * opsGiB, VmsTotal: 3, VmsRunning: 2},
	}}
	cs := newTestSession(t, Clients{Metrics: met})

	out, isErr := callTool(t, cs, "cluster_capacity", map[string]any{})
	if isErr {
		t.Fatalf("unexpected tool error: %s", out)
	}
	want := strings.Join([]string{
		"node n1 [ready]: cpus 6/16 allocated (12 available), memory 8192/32768 MiB (24576 available), disk 120.0 GiB free of 500.0 GiB, vms 2/3 running",
		"node n2 [draining]: cpus 0/8 allocated (0 available), memory 0/16384 MiB (0 available), disk 50.0 GiB free of 250.0 GiB, vms 0/1 running",
		"total: cpus 6/24 allocated (12 available), memory 8192/49152 MiB (24576 available), disk 170.0 GiB free of 750.0 GiB, vms 2/3 running",
	}, "\n")
	if out != want {
		t.Errorf("output mismatch\n got:\n%s\nwant:\n%s", out, want)
	}
}

func TestOpsClusterCapacityNoNodesAndNilTotal(t *testing.T) {
	cs := newTestSession(t, Clients{Metrics: &opsFakeMetrics{capacity: &vmdv1.GetCapacityResponse{}}})
	out, isErr := callTool(t, cs, "cluster_capacity", map[string]any{})
	if isErr {
		t.Fatalf("unexpected tool error: %s", out)
	}
	want := "total: cpus 0/0 allocated (0 available), memory 0/0 MiB (0 available), disk 0 B free of 0 B, vms 0/0 running"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestOpsClusterCapacityErrorPassesThrough(t *testing.T) {
	met := &opsFakeMetrics{capErr: connect.NewError(connect.CodePermissionDenied, errors.New("missing capacity.get"))}
	cs := newTestSession(t, Clients{Metrics: met})
	out, isErr := callTool(t, cs, "cluster_capacity", map[string]any{})
	if !isErr || out != "permission_denied: missing capacity.get" {
		t.Errorf("got (%q, %v), want the permission error unchanged", out, isErr)
	}
}

func TestOpsNodeDrain(t *testing.T) {
	cl := &opsFakeCluster{node: &vmdv1.NodeCapacity{NodeId: "n1", State: "draining", CpusTotal: 16,
		CpusAllocated: 6, MemoryMbTotal: 32768, DiskBytesTotal: 500 * opsGiB, DiskBytesFree: 120 * opsGiB,
		VmsTotal: 3, VmsRunning: 2}}
	cs := newTestSession(t, Clients{Cluster: cl})

	out, isErr := callTool(t, cs, "node_drain", map[string]any{"node_id": "n1", "draining": true})
	if isErr {
		t.Fatalf("unexpected tool error: %s", out)
	}
	want := "node n1 [draining]: cpus 6/16 allocated (0 available), memory 0/32768 MiB (0 available), disk 120.0 GiB free of 500.0 GiB, vms 2/3 running"
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	if cl.gotReq.GetNodeId() != "n1" || !cl.gotReq.GetDraining() {
		t.Errorf("SetNodeDraining got %v, want node n1 draining=true", cl.gotReq)
	}
}

func TestOpsNodeDrainResume(t *testing.T) {
	cl := &opsFakeCluster{node: &vmdv1.NodeCapacity{NodeId: "n2", State: "ready"}}
	cs := newTestSession(t, Clients{Cluster: cl})
	out, isErr := callTool(t, cs, "node_drain", map[string]any{"node_id": "n2", "draining": false})
	if isErr {
		t.Fatalf("unexpected tool error: %s", out)
	}
	if !strings.HasPrefix(out, "node n2 [ready]: ") {
		t.Errorf("got %q, want a node n2 [ready] line", out)
	}
	if cl.gotReq.GetDraining() {
		t.Errorf("draining = true, want false (resume)")
	}
}

func TestOpsNodeDrainValidationAndErrors(t *testing.T) {
	t.Run("missing node_id", func(t *testing.T) {
		cs := newTestSession(t, Clients{Cluster: &opsFakeCluster{}})
		out, isErr := callTool(t, cs, "node_drain", map[string]any{"node_id": "", "draining": true})
		if !isErr || !strings.Contains(out, "node_id is required") {
			t.Errorf("got (%q, %v), want a node_id-is-required tool error", out, isErr)
		}
	})
	t.Run("unknown node", func(t *testing.T) {
		cl := &opsFakeCluster{err: connect.NewError(connect.CodeNotFound, errors.New("node nx not found"))}
		cs := newTestSession(t, Clients{Cluster: cl})
		out, isErr := callTool(t, cs, "node_drain", map[string]any{"node_id": "nx", "draining": true})
		if !isErr || out != "not_found: node nx not found" {
			t.Errorf("got (%q, %v), want the not_found error unchanged", out, isErr)
		}
	})
	t.Run("permission error passes through", func(t *testing.T) {
		cl := &opsFakeCluster{err: connect.NewError(connect.CodePermissionDenied, errors.New("missing node.drain"))}
		cs := newTestSession(t, Clients{Cluster: cl})
		out, isErr := callTool(t, cs, "node_drain", map[string]any{"node_id": "n1", "draining": true})
		if !isErr || out != "permission_denied: missing node.drain" {
			t.Errorf("got (%q, %v), want the permission error unchanged", out, isErr)
		}
	})
}

func TestOpsToolSchemasRequiredFields(t *testing.T) {
	cs := newTestSession(t, Clients{})
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	required := map[string][]string{}
	for _, tl := range res.Tools {
		if !strings.HasPrefix(tl.Name, "agent_metrics") && tl.Name != "node_drain" && tl.Name != "cluster_capacity" {
			continue
		}
		schema, ok := tl.InputSchema.(map[string]any)
		if !ok {
			t.Fatalf("%s: input schema has type %T", tl.Name, tl.InputSchema)
		}
		req, _ := schema["required"].([]any)
		for _, r := range req {
			required[tl.Name] = append(required[tl.Name], r.(string))
		}
	}
	if got := strings.Join(required["agent_metrics"], ","); got != "name" {
		t.Errorf("agent_metrics required = %q, want name", got)
	}
	if got := strings.Join(required["node_drain"], ","); got != "node_id,draining" {
		t.Errorf("node_drain required = %q, want node_id,draining", got)
	}
	if got := required["cluster_capacity"]; len(got) != 0 {
		t.Errorf("cluster_capacity required = %v, want none", got)
	}
}

// opsGiBOf returns f gibibytes in bytes.
func opsGiBOf(f float64) int64 { return int64(f * float64(opsGiB)) }
