package qemudriver

import (
	"context"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/guestexec"
)

// StartJob runs a detached command in VM r.VmId. The VM must be RUNNING.
func (d *Driver) StartJob(ctx context.Context, r *nodev1.StartJobRequest) (*nodev1.StartJobResponse, error) {
	spec := r.GetSpec()
	if err := checkID("job id", spec.GetJobId()); err != nil {
		return nil, err
	}
	port, err := d.requireRunning(ctx, r.GetVmId())
	if err != nil {
		return nil, err
	}
	err = d.exec(port).Start(ctx, guestexec.Spec{
		JobID:          spec.GetJobId(),
		Command:        spec.GetCommand(),
		Workdir:        spec.GetWorkdir(),
		TimeoutSeconds: int(spec.GetTimeoutSeconds()),
	})
	if err != nil {
		return nil, err
	}
	return &nodev1.StartJobResponse{}, nil
}

// PollJob reports a job's state and a page of its output. A VM that is not RUNNING has no readable
// jobs, so the result is MISSING, as it is for an unknown job.
func (d *Driver) PollJob(ctx context.Context, r *nodev1.PollJobRequest) (*nodev1.PollJobResponse, error) {
	if err := checkID("job id", r.GetJobId()); err != nil {
		return nil, err
	}
	if r.GetStdoutOffset() < 0 || r.GetStderrOffset() < 0 {
		return nil, errs.Invalid("offsets must not be negative")
	}
	if r.GetMaxOutput() < 0 {
		return nil, errs.Invalid("max_output must not be negative")
	}
	obs, err := d.observe(ctx, r.GetVmId())
	if err != nil {
		return nil, err
	}
	if obs.state != nodev1.RuntimeState_RUNTIME_STATE_RUNNING {
		return &nodev1.PollJobResponse{State: nodev1.GuestJobState_GUEST_JOB_STATE_MISSING}, nil
	}
	return d.exec(obs.rec.SSHPort).Poll(ctx, r.GetJobId(), r.GetStdoutOffset(), r.GetStderrOffset(),
		int(r.GetMaxOutput()), r.GetTail())
}

// CancelJob kills a job in VM r.VmId. Cancelling a finished job does nothing, and so does cancelling
// a job in a VM that is not RUNNING, because its processes are already gone.
func (d *Driver) CancelJob(ctx context.Context, r *nodev1.CancelJobRequest) (*nodev1.CancelJobResponse, error) {
	if err := checkID("job id", r.GetJobId()); err != nil {
		return nil, err
	}
	obs, err := d.observe(ctx, r.GetVmId())
	if err != nil {
		return nil, err
	}
	if obs.state != nodev1.RuntimeState_RUNTIME_STATE_RUNNING {
		return &nodev1.CancelJobResponse{}, nil
	}
	if err := d.exec(obs.rec.SSHPort).Cancel(ctx, r.GetJobId()); err != nil {
		return nil, err
	}
	return &nodev1.CancelJobResponse{}, nil
}

// PutFile writes r.Content to r.Path in VM r.VmId, atomically. The VM must be RUNNING.
func (d *Driver) PutFile(ctx context.Context, r *nodev1.PutFileRequest) (*nodev1.PutFileResponse, error) {
	if r.GetPath() == "" {
		return nil, errs.Invalid("path is required")
	}
	port, err := d.requireRunning(ctx, r.GetVmId())
	if err != nil {
		return nil, err
	}
	if err := d.exec(port).PutFile(ctx, r.GetPath(), r.GetContent(), r.GetMode()); err != nil {
		return nil, err
	}
	return &nodev1.PutFileResponse{}, nil
}

// GetFile reads a regular file from VM r.VmId, up to r.MaxBytes. The VM must be RUNNING.
func (d *Driver) GetFile(ctx context.Context, r *nodev1.GetFileRequest) (*nodev1.GetFileResponse, error) {
	if r.GetPath() == "" {
		return nil, errs.Invalid("path is required")
	}
	port, err := d.requireRunning(ctx, r.GetVmId())
	if err != nil {
		return nil, err
	}
	content, truncated, err := d.exec(port).GetFile(ctx, r.GetPath(), r.GetMaxBytes())
	if err != nil {
		return nil, err
	}
	return &nodev1.GetFileResponse{Content: content, Truncated: truncated}, nil
}
