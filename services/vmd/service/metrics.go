package service

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/scheduler"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/store"
)

// MetricsServer implements vmdv1connect.MetricsServiceHandler.
type MetricsServer struct{ base }

// GetVMMetrics samples CPU/RAM/filesystem usage inside the guest and disk allocation on the host.
func (s *MetricsServer) GetVMMetrics(ctx context.Context, req *connect.Request[vmdv1.GetVMMetricsRequest]) (*connect.Response[vmdv1.VMMetrics], error) {
	vm, err := s.st.GetVM(ctx, req.Msg.VmId)
	if err != nil {
		return nil, err
	}
	if err := authz.RequireResource(ctx, authz.VMMetrics, vm.ProjectID, "vm", vm.ID); err != nil {
		return nil, err
	}
	m, err := SampleVM(ctx, s.nodes, vm, s.now())
	if err != nil {
		return nil, s.nodeErr(vm.NodeID, err)
	}
	return connect.NewResponse(m), nil
}

// SampleVM asks the VM's node for current metrics. Shared by the API and the Prometheus sampler.
func SampleVM(ctx context.Context, nodes Nodes, vm *domain.VM, at time.Time) (*vmdv1.VMMetrics, error) {
	d, err := runningDriver(nodes, vm)
	if err != nil {
		return nil, err
	}
	r, err := d.GetVMMetrics(ctx, &nodev1.GetVMMetricsRequest{VmId: vm.ID, DiskIds: vm.DiskIDs})
	if err != nil {
		return nil, err
	}
	out := &vmdv1.VMMetrics{
		VmId: vm.ID, SampledAt: timestamppb.New(at),
		CpuPercent: r.CpuPercent, CpuCores: r.CpuCores, Load1: r.Load1, Load5: r.Load5, Load15: r.Load15,
		MemTotalBytes: r.MemTotalBytes, MemUsedBytes: r.MemUsedBytes, MemAvailableBytes: r.MemAvailableBytes,
		SwapTotalBytes: r.SwapTotalBytes, SwapUsedBytes: r.SwapUsedBytes, UptimeSeconds: r.UptimeSeconds,
	}
	for _, f := range r.Filesystems {
		out.Filesystems = append(out.Filesystems, &vmdv1.GuestFilesystem{
			DiskId: f.DiskId, Device: f.Device, MountPoint: f.MountPoint, SizeBytes: f.SizeBytes, UsedBytes: f.UsedBytes})
	}
	for _, d := range r.Disks {
		out.Disks = append(out.Disks, &vmdv1.DiskAllocation{
			DiskId: d.DiskId, VirtualSizeBytes: d.VirtualSizeBytes, AllocatedBytes: d.AllocatedBytes})
	}
	return out, nil
}

// GetCapacity reports CPU, memory and disk capacity per node and in total, for operators.
func (s *MetricsServer) GetCapacity(ctx context.Context, _ *connect.Request[vmdv1.GetCapacityRequest]) (*connect.Response[vmdv1.GetCapacityResponse], error) {
	if err := authz.Require(ctx, authz.CapacityGet, ""); err != nil {
		return nil, err
	}
	nodes, total, err := ClusterCapacity(ctx, s.st, s.cfg.Scheduler)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&vmdv1.GetCapacityResponse{Nodes: nodes, Total: total}), nil
}

// ClusterCapacity computes per-node and overall capacity. "Available" is what the scheduler would still
// hand out (after the overcommit factors and the host's memory reserve), not raw free hardware.
// The total covers ready nodes only: a down or draining node's capacity is not available to anyone.
func ClusterCapacity(ctx context.Context, st store.Store, sc scheduler.Config) (nodes []*vmdv1.NodeCapacity, total *vmdv1.NodeCapacity, err error) {
	rows, err := st.ListNodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	total = &vmdv1.NodeCapacity{NodeId: "*", State: "ready"}
	for _, n := range rows {
		vms, err := st.ListVMsByNode(ctx, n.ID)
		if err != nil {
			return nil, nil, err
		}
		info := scheduler.NodeInfo{ID: n.ID, State: n.Effective(), CPUs: n.CPUs, MemoryMB: n.MemoryMB, DiskBytesFree: n.DiskBytesFree}
		c := &vmdv1.NodeCapacity{NodeId: n.ID, State: string(n.Effective()), CpusTotal: int32(n.CPUs), MemoryMbTotal: n.MemoryMB,
			DiskBytesTotal: n.DiskBytesTotal, DiskBytesFree: n.DiskBytesFree, VmsTotal: int32(len(vms))}
		if !n.LastHeartbeat.IsZero() {
			c.LastHeartbeat = timestamppb.New(n.LastHeartbeat)
		}
		for _, v := range vms {
			if v.State == domain.VMRunning {
				c.VmsRunning++
			}
			if v.Desired == domain.PowerRunning {
				info.AllocatedCPUs += v.CPUs
				info.AllocatedMemoryMB += v.MemoryMB
			}
		}
		c.CpusAllocated, c.MemoryMbAllocated = int32(info.AllocatedCPUs), info.AllocatedMemoryMB
		if n.Effective() == domain.NodeReady { // a down or draining node hands out nothing
			av := scheduler.Avail(info, sc)
			c.CpusAvailable, c.MemoryMbAvailable = int32(av.CPUs), av.MemoryMB
		}
		nodes = append(nodes, c)
		if n.Effective() == domain.NodeReady {
			total.CpusTotal += c.CpusTotal
			total.CpusAllocated += c.CpusAllocated
			total.CpusAvailable += c.CpusAvailable
			total.MemoryMbTotal += c.MemoryMbTotal
			total.MemoryMbAllocated += c.MemoryMbAllocated
			total.MemoryMbAvailable += c.MemoryMbAvailable
			total.DiskBytesTotal += c.DiskBytesTotal
			total.DiskBytesFree += c.DiskBytesFree
			total.VmsTotal += c.VmsTotal
			total.VmsRunning += c.VmsRunning
		}
	}
	return nodes, total, nil
}
