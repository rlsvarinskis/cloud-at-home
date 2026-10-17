package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"

	nodev1 "example.com/homecloud/gen/homecloud/node/v1"
	vmdv1 "example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/internal/auth"
	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/vmd/domain"
)

const (
	maxCommandBytes = 256 << 10
	maxFileBytes    = 8 << 20
	maxJobWait      = 30 // seconds a single call may block
	defaultTimeout  = 3600
	maxTimeout      = 7 * 24 * 3600
	defaultOutput   = 8 << 10
	maxOutput       = 64 << 10
)

// ExecServer implements vmdv1connect.ExecServiceHandler. Job metadata lives in the store; the job itself
// (its files and processes) lives in the guest, so it survives restarts of vmd and of the node daemon.
type ExecServer struct{ base }

// target resolves a VM for an operation, checking permission against its project.
func (s *ExecServer) target(ctx context.Context, vmID string, perm auth.Permission) (*domain.VM, error) {
	vm, err := s.st.GetVM(ctx, vmID)
	if err != nil {
		return nil, err
	}
	if err := auth.RequireResource(ctx, perm, vm.ProjectID, "vm", vmID); err != nil {
		return nil, err
	}
	return vm, nil
}

func (s *ExecServer) runningDriver(vm *domain.VM) (nodeDriver, error) {
	return runningDriver(s.nodes, vm)
}

// runningDriver returns the node driver of a RUNNING VM (Precondition otherwise).
func runningDriver(nodes Nodes, vm *domain.VM) (nodeDriver, error) {
	if vm.State != domain.VMRunning {
		return nil, errs.Precondition("vm %s is %s, not running", vm.ID, vm.State)
	}
	d, err := nodes.Driver(vm.NodeID)
	if err != nil {
		return nil, errs.Unavailable("node %s is not registered", vm.NodeID)
	}
	return d, nil
}

func clampOutput(n int32) int32 {
	switch {
	case n <= 0:
		return defaultOutput
	case n > maxOutput:
		return maxOutput
	}
	return n
}

// applyPoll folds a guest poll into the job record. It reports whether the job just became final.
func (s *ExecServer) applyPoll(j *domain.Job, r *nodev1.PollJobResponse) bool {
	if j.State.Final() {
		return false
	}
	switch r.State {
	case nodev1.GuestJobState_GUEST_JOB_STATE_RUNNING:
		return false
	case nodev1.GuestJobState_GUEST_JOB_STATE_EXITED:
		code := int(r.GetExitCode())
		j.ExitCode = &code
		switch {
		case r.Cancelled:
			j.State, j.KilledReason = domain.JobCancelled, "cancelled"
		case code == 124:
			j.State, j.KilledReason = domain.JobTimedOut, "timed out and was killed"
		case code == 0:
			j.State = domain.JobSucceeded
		default:
			j.State = domain.JobFailed
		}
	case nodev1.GuestJobState_GUEST_JOB_STATE_LOST:
		j.State, j.KilledReason = domain.JobLost, "the process vanished without an exit code (the VM stopped or rebooted while it ran)"
	default: // MISSING or unspecified
		j.State, j.KilledReason = domain.JobLost, "the job's files are gone from the VM"
	}
	j.FinishedAt = s.now()
	return true
}

func (s *ExecServer) saveJob(ctx context.Context, j *domain.Job) {
	if err := s.st.UpdateJob(ctx, j); err != nil && !errors.Is(err, errs.ErrConflict) {
		s.cfg.Log.Warn("update job", "job", j.ID, "err", err)
	}
}

// pollOnce asks the node for the job's state/output and updates the record when it finished.
func (s *ExecServer) pollOnce(ctx context.Context, vm *domain.VM, j *domain.Job, out, errOff int64, maxOut int32, tail bool) (*vmdv1.JobOutput, error) {
	drv, err := s.nodes.Driver(vm.NodeID)
	if err != nil {
		return nil, errs.Unavailable("node %s is not registered", vm.NodeID)
	}
	r, err := drv.PollJob(ctx, &nodev1.PollJobRequest{VmId: vm.ID, JobId: j.ID, StdoutOffset: out, StderrOffset: errOff, MaxOutput: maxOut, Tail: tail})
	if err != nil {
		return nil, s.nodeErr(vm.NodeID, err)
	}
	if s.applyPoll(j, r) {
		s.saveJob(ctx, j)
	}
	return &vmdv1.JobOutput{Job: jobToProto(j), Stdout: r.Stdout, Stderr: r.Stderr,
		StdoutOffset: r.StdoutOffset, StderrOffset: r.StderrOffset, Truncated: r.Truncated, More: r.More}, nil
}

// waitOutput polls until the job is final or the wait runs out.
func (s *ExecServer) waitOutput(ctx context.Context, vm *domain.VM, j *domain.Job, wait int32, out, errOff int64, maxOut int32, tail bool) (*vmdv1.JobOutput, error) {
	var res *vmdv1.JobOutput
	delay := 250 * time.Millisecond
	deadline := time.Now().Add(time.Duration(min(max(wait, 0), maxJobWait)) * time.Second)
	for {
		var err error
		if res, err = s.pollOnce(ctx, vm, j, out, errOff, maxOut, tail); err != nil {
			return nil, err
		}
		if j.State.Final() || !time.Now().Before(deadline) {
			return res, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(delay, time.Until(deadline))):
		}
		delay = min(delay*2, time.Second)
	}
}

func (s *ExecServer) StartJob(ctx context.Context, req *connect.Request[vmdv1.StartJobRequest]) (*connect.Response[vmdv1.JobOutput], error) {
	m := req.Msg
	vm, err := s.target(ctx, m.VmId, auth.ExecRun)
	if err != nil {
		return nil, err
	}
	switch {
	case strings.TrimSpace(m.Command) == "":
		return nil, errs.Invalid("command is required")
	case len(m.Command) > maxCommandBytes:
		return nil, errs.Invalid("command exceeds %d bytes", maxCommandBytes)
	case strings.ContainsRune(m.Command, 0) || strings.ContainsRune(m.Workdir, 0):
		return nil, errs.Invalid("command and workdir must not contain NUL bytes")
	}
	timeout := int(m.TimeoutSeconds)
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		return nil, errs.Invalid("timeout_seconds may not exceed %d", maxTimeout)
	}
	if m.IdempotencyKey != "" { // a retry of the same request must not run the command twice
		if j, err := s.st.GetJobByIdempotencyKey(ctx, vm.ProjectID, m.IdempotencyKey); err == nil {
			return s.replay(ctx, vm, j, m)
		}
	}
	drv, err := s.runningDriver(vm)
	if err != nil {
		return nil, err
	}
	j := &domain.Job{ID: s.newID("job", vm.ProjectID), VMID: vm.ID, ProjectID: vm.ProjectID, Command: m.Command, Workdir: m.Workdir,
		State: domain.JobRunning, TimeoutSeconds: timeout, IdempotencyKey: m.IdempotencyKey, StartedAt: s.now()}
	if err := s.st.CreateJob(ctx, j); err != nil {
		if m.IdempotencyKey != "" && errors.Is(err, errs.ErrAlreadyExists) {
			if prev, gerr := s.st.GetJobByIdempotencyKey(ctx, vm.ProjectID, m.IdempotencyKey); gerr == nil {
				return s.replay(ctx, vm, prev, m)
			}
		}
		return nil, err
	}
	if _, err := drv.StartJob(ctx, &nodev1.StartJobRequest{VmId: vm.ID, Spec: &nodev1.JobSpec{
		JobId: j.ID, Command: m.Command, Workdir: m.Workdir, TimeoutSeconds: int32(timeout)}}); err != nil {
		clientErr := s.nodeErr(vm.NodeID, err)
		j.State, j.KilledReason, j.FinishedAt = domain.JobFailed, "could not start: "+clientErr.Error(), s.now()
		s.saveJob(ctx, j)
		return nil, clientErr
	}
	res, err := s.waitOutput(ctx, vm, j, m.WaitSeconds, 0, 0, clampOutput(m.MaxOutput), true)
	return respond(res, err)
}

// replay answers a retried StartJob with the job the first attempt created. The key identifies one
// request: reusing it for a different VM or command is a client bug, reported rather than ignored.
func (s *ExecServer) replay(ctx context.Context, vm *domain.VM, prev *domain.Job, m *vmdv1.StartJobRequest) (*connect.Response[vmdv1.JobOutput], error) {
	if prev.VMID != vm.ID || prev.Command != m.Command || prev.Workdir != m.Workdir {
		return nil, errs.Invalid("idempotency_key %q was already used for a different request", m.IdempotencyKey)
	}
	res, err := s.waitOutput(ctx, vm, prev, m.WaitSeconds, 0, 0, clampOutput(m.MaxOutput), true)
	return respond(res, err)
}

func respond(res *vmdv1.JobOutput, err error) (*connect.Response[vmdv1.JobOutput], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

// loadJob returns a job and its VM, authorizing against the job's project. A VM that is gone or not
// running finalises a still-running job as lost, so callers never see a job "running" in a dead VM.
func (s *ExecServer) loadJob(ctx context.Context, jobID string, perm auth.Permission) (*domain.Job, *domain.VM, error) {
	j, err := s.st.GetJob(ctx, jobID)
	if err != nil {
		return nil, nil, err
	}
	if err := auth.RequireResource(ctx, perm, j.ProjectID, "job", jobID); err != nil {
		return nil, nil, err
	}
	vm, err := s.st.GetVM(ctx, j.VMID)
	if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return nil, nil, err
	}
	if (vm == nil || vm.State != domain.VMRunning) && !j.State.Final() {
		reason := "the VM was deleted"
		if vm != nil {
			reason = "vm stopped"
			if vm.StateReason != "" {
				reason += ": " + vm.StateReason
			} else {
				reason += " (state " + string(vm.State) + ")"
			}
		}
		j.State, j.KilledReason, j.FinishedAt = domain.JobLost, reason, s.now()
		s.saveJob(ctx, j)
	}
	return j, vm, nil
}

func (s *ExecServer) PollJob(ctx context.Context, req *connect.Request[vmdv1.PollJobRequest]) (*connect.Response[vmdv1.JobOutput], error) {
	m := req.Msg
	j, vm, err := s.loadJob(ctx, m.JobId, auth.ExecGet)
	if err != nil {
		return nil, err
	}
	if m.StdoutOffset < 0 || m.StderrOffset < 0 {
		return nil, errs.Invalid("offsets must not be negative")
	}
	if vm == nil || vm.State != domain.VMRunning {
		// Nothing to read from: report the record as it is (final, with the reason it ended).
		return connect.NewResponse(&vmdv1.JobOutput{Job: jobToProto(j), StdoutOffset: m.StdoutOffset, StderrOffset: m.StderrOffset}), nil
	}
	res, err := s.waitOutput(ctx, vm, j, m.WaitSeconds, m.StdoutOffset, m.StderrOffset, clampOutput(m.MaxOutput), m.Tail)
	return respond(res, err)
}

func (s *ExecServer) CancelJob(ctx context.Context, req *connect.Request[vmdv1.CancelJobRequest]) (*connect.Response[vmdv1.JobOutput], error) {
	j, vm, err := s.loadJob(ctx, req.Msg.JobId, auth.ExecCancel)
	if err != nil {
		return nil, err
	}
	if j.State.Final() || vm == nil || vm.State != domain.VMRunning {
		return connect.NewResponse(&vmdv1.JobOutput{Job: jobToProto(j)}), nil
	}
	drv, err := s.runningDriver(vm)
	if err != nil {
		return nil, err
	}
	if _, err := drv.CancelJob(ctx, &nodev1.CancelJobRequest{VmId: vm.ID, JobId: j.ID}); err != nil {
		return nil, s.nodeErr(vm.NodeID, err)
	}
	res, err := s.waitOutput(ctx, vm, j, 3, 0, 0, defaultOutput, true)
	return respond(res, err)
}

func (s *ExecServer) ListJobs(ctx context.Context, req *connect.Request[vmdv1.ListJobsRequest]) (*connect.Response[vmdv1.ListJobsResponse], error) {
	m := req.Msg
	if _, err := s.target(ctx, m.VmId, auth.ExecList); err != nil {
		return nil, err
	}
	limit := s.pageSize(m.PageSize)
	jobs, err := s.st.ListJobs(ctx, m.VmId, m.PageToken, limit+1)
	if err != nil {
		return nil, err
	}
	jobs, next := nextToken(jobs, limit, func(j *domain.Job) string { return j.ID })
	res := &vmdv1.ListJobsResponse{NextPageToken: next}
	for _, j := range jobs {
		res.Jobs = append(res.Jobs, jobToProto(j))
	}
	return connect.NewResponse(res), nil
}

func (s *ExecServer) PutFile(ctx context.Context, req *connect.Request[vmdv1.PutFileRequest]) (*connect.Response[vmdv1.PutFileResponse], error) {
	m := req.Msg
	vm, err := s.target(ctx, m.VmId, auth.FileWrite)
	if err != nil {
		return nil, err
	}
	switch {
	case m.Path == "" || strings.ContainsRune(m.Path, 0):
		return nil, errs.Invalid("path is required and must not contain NUL")
	case len(m.Content) > maxFileBytes:
		return nil, errs.Invalid("file exceeds %d bytes", maxFileBytes)
	case m.Mode > 0o7777:
		return nil, errs.Invalid("mode %o is not a valid permission mask", m.Mode)
	}
	drv, err := s.runningDriver(vm)
	if err != nil {
		return nil, err
	}
	mode := m.Mode
	if mode == 0 {
		mode = 0o644 // the documented default
	}
	if _, err := drv.PutFile(ctx, &nodev1.PutFileRequest{VmId: vm.ID, Path: m.Path, Content: m.Content, Mode: mode}); err != nil {
		return nil, s.nodeErr(vm.NodeID, err)
	}
	return connect.NewResponse(&vmdv1.PutFileResponse{}), nil
}

func (s *ExecServer) GetFile(ctx context.Context, req *connect.Request[vmdv1.GetFileRequest]) (*connect.Response[vmdv1.GetFileResponse], error) {
	m := req.Msg
	vm, err := s.target(ctx, m.VmId, auth.FileRead)
	if err != nil {
		return nil, err
	}
	if m.Path == "" || strings.ContainsRune(m.Path, 0) {
		return nil, errs.Invalid("path is required and must not contain NUL")
	}
	drv, err := s.runningDriver(vm)
	if err != nil {
		return nil, err
	}
	max := m.MaxBytes
	if max <= 0 || max > maxFileBytes {
		max = maxFileBytes
	}
	res, err := drv.GetFile(ctx, &nodev1.GetFileRequest{VmId: vm.ID, Path: m.Path, MaxBytes: max})
	if err != nil {
		return nil, s.nodeErr(vm.NodeID, err)
	}
	return connect.NewResponse(&vmdv1.GetFileResponse{Content: res.Content, Truncated: res.Truncated}), nil
}
