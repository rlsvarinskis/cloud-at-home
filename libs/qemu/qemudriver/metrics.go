package qemudriver

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/guestmetrics"
)

// memInfoPath is the file that reports host memory.
const memInfoPath = "/proc/meminfo"

// parseMeminfo returns MemTotal and MemAvailable from /proc/meminfo content, in bytes.
func parseMeminfo(data []byte) (total, avail int64, err error) {
	var haveTotal, haveAvail bool
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var dst *int64
		var seen *bool
		switch fields[0] {
		case "MemTotal:":
			dst, seen = &total, &haveTotal
		case "MemAvailable:":
			dst, seen = &avail, &haveAvail
		default:
			continue
		}
		v, perr := strconv.ParseInt(fields[1], 10, 64)
		if perr != nil || v < 0 {
			return 0, 0, fmt.Errorf("meminfo: bad value in %q", line)
		}
		*dst, *seen = v*1024, true
	}
	if !haveTotal || !haveAvail {
		return 0, 0, fmt.Errorf("meminfo: MemTotal or MemAvailable is missing")
	}
	return total, avail, nil
}

// GetCapacity reports the CPUs, memory and disk space of the host. Disk figures come from the
// file system holding DataDir, and free space is the space an unprivileged user can allocate.
func (d *Driver) GetCapacity(_ context.Context, _ *nodev1.GetCapacityRequest) (*nodev1.GetCapacityResponse, error) {
	raw, err := d.cfg.FS.ReadFile(memInfoPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", memInfoPath, err)
	}
	memTotal, memAvail, err := parseMeminfo(raw)
	if err != nil {
		return nil, err
	}
	diskTotal, diskFree, err := d.cfg.FS.Statfs(d.dataDir)
	if err != nil {
		return nil, fmt.Errorf("statfs %s: %w", d.dataDir, err)
	}
	return &nodev1.GetCapacityResponse{Capacity: &nodev1.Capacity{
		CpusTotal:         int32(d.cfg.FS.NumCPU()),
		MemoryMbTotal:     memTotal / mib,
		MemoryMbAvailable: memAvail / mib,
		DiskBytesTotal:    diskTotal,
		DiskBytesFree:     diskFree,
	}}, nil
}

// GetVMMetrics collects the guest's CPU, memory, load, uptime and file system figures over ssh, and
// the allocation of each disk in r.DiskIds. The VM must be RUNNING.
func (d *Driver) GetVMMetrics(ctx context.Context, r *nodev1.GetVMMetricsRequest) (*nodev1.GetVMMetricsResponse, error) {
	id := r.GetVmId()
	diskIDs := r.GetDiskIds()
	for _, dk := range diskIDs {
		if err := checkID("disk id", dk); err != nil {
			return nil, err
		}
	}
	port, err := d.requireRunning(ctx, id)
	if err != nil {
		return nil, err
	}

	mctx, cancel := context.WithTimeout(ctx, metricsTimeout)
	defer cancel()
	stdout, stderr, code, err := d.cfg.NewRunner(port).Run(mctx, bashCommand(guestmetrics.Script), nil)
	if err != nil {
		return nil, errs.Unavailable("collect metrics from vm %q: %v", id, err)
	}
	if code != 0 {
		return nil, errs.Unavailable("metrics script in vm %q exited %d: %s", id, code, strings.TrimSpace(string(stderr)))
	}
	s, err := guestmetrics.Parse(string(stdout), diskIDs)
	if err != nil {
		return nil, fmt.Errorf("parse metrics of vm %q: %w", id, err)
	}

	resp := &nodev1.GetVMMetricsResponse{
		CpuPercent:        s.CPUPercent,
		CpuCores:          int32(s.CPUCores),
		Load1:             s.Load1,
		Load5:             s.Load5,
		Load15:            s.Load15,
		MemTotalBytes:     s.MemTotal,
		MemUsedBytes:      s.MemUsed,
		MemAvailableBytes: s.MemAvailable,
		SwapTotalBytes:    s.SwapTotal,
		SwapUsedBytes:     s.SwapUsed,
		UptimeSeconds:     s.UptimeSeconds,
	}
	for _, f := range s.Filesystems {
		resp.Filesystems = append(resp.Filesystems, &nodev1.GuestFilesystem{
			DiskId:     f.DiskID,
			Device:     f.Device,
			MountPoint: f.MountPoint,
			SizeBytes:  f.SizeBytes,
			UsedBytes:  f.UsedBytes,
		})
	}
	for _, dk := range diskIDs {
		exists, err := fileExists(d.diskPath(dk))
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, errs.NotFound("disk %q", dk)
		}
		info, err := d.imgInfo(ctx, d.diskPath(dk))
		if err != nil {
			return nil, errs.Unavailable("%v", err)
		}
		resp.Disks = append(resp.Disks, &nodev1.DiskAllocation{
			DiskId:           dk,
			VirtualSizeBytes: info.VirtualSize,
			AllocatedBytes:   info.ActualSize,
		})
	}
	return resp, nil
}
