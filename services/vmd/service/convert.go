package service

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
)

func tsp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t.UTC())
}

func vmToProto(v *domain.VM) *vmdv1.VM {
	return &vmdv1.VM{
		Id: v.ID, ProjectId: v.ProjectID, Name: v.Name, NodeId: v.NodeID,
		Cpus: int32(v.CPUs), MemoryMb: v.MemoryMB, DiskIds: append([]string(nil), v.DiskIDs...),
		DesiredState: desiredToProto(v.Desired), State: vmStateToProto(v.State), StateReason: v.StateReason,
		Holds: append([]string(nil), v.Holds...), Labels: v.Labels, Generation: v.Generation,
		CreatedAt: tsp(v.CreatedAt), UpdatedAt: tsp(v.UpdatedAt),
	}
}

func desiredToProto(p domain.PowerState) vmdv1.PowerState {
	switch p {
	case domain.PowerRunning:
		return vmdv1.PowerState_POWER_STATE_RUNNING
	case domain.PowerStopped, domain.PowerDeleted:
		return vmdv1.PowerState_POWER_STATE_STOPPED
	}
	return vmdv1.PowerState_POWER_STATE_UNSPECIFIED
}

func vmStateToProto(s domain.VMState) vmdv1.VMState {
	switch s {
	case domain.VMPending:
		return vmdv1.VMState_VM_STATE_PENDING
	case domain.VMStopped:
		return vmdv1.VMState_VM_STATE_STOPPED
	case domain.VMStarting:
		return vmdv1.VMState_VM_STATE_STARTING
	case domain.VMRunning:
		return vmdv1.VMState_VM_STATE_RUNNING
	case domain.VMStopping:
		return vmdv1.VMState_VM_STATE_STOPPING
	case domain.VMError:
		return vmdv1.VMState_VM_STATE_ERROR
	case domain.VMDeleting:
		return vmdv1.VMState_VM_STATE_DELETING
	}
	return vmdv1.VMState_VM_STATE_UNSPECIFIED
}

func vmStateFromProto(s vmdv1.VMState) domain.VMState {
	for _, d := range []domain.VMState{domain.VMPending, domain.VMStopped, domain.VMStarting, domain.VMRunning,
		domain.VMStopping, domain.VMError, domain.VMDeleting} {
		if vmStateToProto(d) == s {
			return d
		}
	}
	return ""
}

func diskToProto(d *domain.Disk) *vmdv1.Disk {
	return &vmdv1.Disk{
		Id: d.ID, ProjectId: d.ProjectID, Name: d.Name, NodeId: d.NodeID, SizeGb: int32(d.SizeGB), ImageId: d.ImageID,
		State: diskStateToProto(d.State), AttachedVmId: d.AttachedVMID, CreatedAt: tsp(d.CreatedAt), StateReason: d.StateReason,
	}
}

func diskStateToProto(s domain.DiskState) vmdv1.DiskState {
	switch s {
	case domain.DiskCreating:
		return vmdv1.DiskState_DISK_STATE_CREATING
	case domain.DiskReady:
		return vmdv1.DiskState_DISK_STATE_READY
	case domain.DiskError:
		return vmdv1.DiskState_DISK_STATE_ERROR
	case domain.DiskDeleting:
		return vmdv1.DiskState_DISK_STATE_DELETING
	}
	return vmdv1.DiskState_DISK_STATE_UNSPECIFIED
}

func imageToProto(i *domain.Image) *vmdv1.Image {
	return &vmdv1.Image{
		Id: i.ID, ProjectId: i.ProjectID, Name: i.Name, NodeId: i.NodeID, State: imageStateToProto(i.State),
		SizeBytes: i.SizeBytes, SourceDiskId: i.SourceDiskID, CreatedAt: tsp(i.CreatedAt), StateReason: i.StateReason,
	}
}

func imageStateToProto(s domain.ImageState) vmdv1.ImageState {
	switch s {
	case domain.ImageCreating:
		return vmdv1.ImageState_IMAGE_STATE_CREATING
	case domain.ImageReady:
		return vmdv1.ImageState_IMAGE_STATE_READY
	case domain.ImageError:
		return vmdv1.ImageState_IMAGE_STATE_ERROR
	case domain.ImageDeleting:
		return vmdv1.ImageState_IMAGE_STATE_DELETING
	}
	return vmdv1.ImageState_IMAGE_STATE_UNSPECIFIED
}

func jobToProto(j *domain.Job) *vmdv1.Job {
	out := &vmdv1.Job{
		Id: j.ID, VmId: j.VMID, ProjectId: j.ProjectID, Command: j.Command, Workdir: j.Workdir,
		State: jobStateToProto(j.State), TimeoutSeconds: int32(j.TimeoutSeconds), KilledReason: j.KilledReason,
		StartedAt: tsp(j.StartedAt), FinishedAt: tsp(j.FinishedAt),
	}
	if j.ExitCode != nil {
		c := int32(*j.ExitCode)
		out.ExitCode = &c
	}
	return out
}

func jobStateToProto(s domain.JobState) vmdv1.JobState {
	switch s {
	case domain.JobRunning:
		return vmdv1.JobState_JOB_STATE_RUNNING
	case domain.JobSucceeded:
		return vmdv1.JobState_JOB_STATE_SUCCEEDED
	case domain.JobFailed:
		return vmdv1.JobState_JOB_STATE_FAILED
	case domain.JobCancelled:
		return vmdv1.JobState_JOB_STATE_CANCELLED
	case domain.JobTimedOut:
		return vmdv1.JobState_JOB_STATE_TIMED_OUT
	case domain.JobLost:
		return vmdv1.JobState_JOB_STATE_LOST
	}
	return vmdv1.JobState_JOB_STATE_UNSPECIFIED
}
