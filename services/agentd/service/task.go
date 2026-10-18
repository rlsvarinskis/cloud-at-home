package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/ids"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
)

// TaskServer implements agentdv1connect.TaskServiceHandler.
type TaskServer struct{ base }

// CreateTask queues a task for an active agent. Retrying with the same idempotency key returns the
// task created by the first call.
func (s *TaskServer) CreateTask(ctx context.Context, req *connect.Request[agentdv1.CreateTaskRequest]) (*connect.Response[agentdv1.Task], error) {
	m := req.Msg
	if m.AgentId == "" {
		return nil, errs.Invalid("agent_id is required")
	}
	if strings.TrimSpace(m.Prompt) == "" {
		return nil, errs.Invalid("prompt is required")
	}
	if len(m.Prompt) > maxPromptBytes {
		return nil, errs.Invalid("prompt is %d bytes; the limit is %d", len(m.Prompt), maxPromptBytes)
	}
	if len(m.CheckCommand) > maxCheckCommandBytes {
		return nil, errs.Invalid("check_command is %d bytes; the limit is %d", len(m.CheckCommand), maxCheckCommandBytes)
	}
	if err := checkNoNUL("prompt", m.Prompt); err != nil {
		return nil, err
	}
	if err := checkNoNUL("check_command", m.CheckCommand); err != nil {
		return nil, err
	}
	maxSteps := int(m.MaxSteps)
	switch {
	case m.MaxSteps < 0:
		return nil, errs.Invalid("max_steps must not be negative")
	case m.MaxSteps == 0:
		maxSteps = defaultMaxSteps
	case m.MaxSteps > maxMaxSteps:
		maxSteps = maxMaxSteps
	}

	a, err := s.st.GetAgent(ctx, m.AgentId)
	if err != nil {
		return nil, err
	}
	if err := authz.RequireResource(ctx, authz.TaskCreate, a.ProjectID, "agent", a.ID); err != nil {
		return nil, err
	}
	if m.IdempotencyKey != "" {
		if t, err := s.st.GetTaskByIdempotencyKey(ctx, a.ProjectID, m.IdempotencyKey); err == nil {
			return s.sameAgentTask(t, a.ID)
		}
	}
	if a.State != domain.AgentActive {
		return nil, errs.Precondition("agent %s is %s", a.ID, a.State)
	}

	t := &domain.Task{
		ID: ids.New("task"), AgentID: a.ID, ProjectID: a.ProjectID, Prompt: m.Prompt, CheckCommand: m.CheckCommand,
		MaxSteps: maxSteps, State: domain.TaskQueued, IdempotencyKey: m.IdempotencyKey, CreatedAt: s.now(),
	}
	if err := s.st.CreateTask(ctx, t); err != nil {
		if m.IdempotencyKey != "" && errors.Is(err, errs.ErrAlreadyExists) {
			if existing, gerr := s.st.GetTaskByIdempotencyKey(ctx, a.ProjectID, m.IdempotencyKey); gerr == nil {
				return s.sameAgentTask(existing, a.ID)
			}
		}
		return nil, err
	}
	return connect.NewResponse(taskToProto(t)), nil
}

// sameAgentTask returns t if it belongs to agentID. An idempotency key reused for another agent is a
// conflicting request, not a replay, so it is reported as AlreadyExists.
func (s *TaskServer) sameAgentTask(t *domain.Task, agentID string) (*connect.Response[agentdv1.Task], error) {
	if t.AgentID != agentID {
		return nil, errs.Exists("idempotency key %q was already used for another agent's task", t.IdempotencyKey)
	}
	return connect.NewResponse(taskToProto(t)), nil
}

// GetTask returns one task. With wait_for_not and wait_seconds it long-polls: it returns as soon as the
// task's state differs from wait_for_not, or when the wait (at most 30 s) runs out.
func (s *TaskServer) GetTask(ctx context.Context, req *connect.Request[agentdv1.GetTaskRequest]) (*connect.Response[agentdv1.Task], error) {
	m := req.Msg
	want := taskStateFromProto(m.WaitForNot)
	var out *domain.Task
	err := poll(ctx, m.WaitSeconds, func(ctx context.Context) (bool, error) {
		t, err := s.st.GetTask(ctx, m.Id)
		if err != nil {
			return false, err
		}
		if err := authz.RequireResource(ctx, authz.TaskGet, t.ProjectID, "task", t.ID); err != nil {
			return false, err
		}
		out = t
		return want == "" || t.State != want, nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(taskToProto(out)), nil
}

// ListTasks lists the tasks of one agent or of one project (exactly one of the two must be given),
// optionally only those in one state.
func (s *TaskServer) ListTasks(ctx context.Context, req *connect.Request[agentdv1.ListTasksRequest]) (*connect.Response[agentdv1.ListTasksResponse], error) {
	m := req.Msg
	if (m.AgentId == "") == (m.ProjectId == "") {
		return nil, errs.Invalid("exactly one of agent_id or project_id is required")
	}
	state := taskStateFromProto(m.State)
	limit := pageSize(m.PageSize)

	var rows []*domain.Task
	var err error
	if m.AgentId != "" {
		a, gerr := s.st.GetAgent(ctx, m.AgentId)
		if gerr != nil {
			return nil, gerr
		}
		if err := authz.RequireResource(ctx, authz.TaskList, a.ProjectID, "agent", a.ID); err != nil {
			return nil, err
		}
		rows, err = s.st.ListTasks(ctx, a.ID, "", state, m.PageToken, limit+1)
	} else {
		if err := validProject(m.ProjectId); err != nil {
			return nil, err
		}
		if err := authz.Require(ctx, authz.TaskList, m.ProjectId); err != nil {
			return nil, err
		}
		rows, err = s.st.ListTasks(ctx, "", m.ProjectId, state, m.PageToken, limit+1)
	}
	if err != nil {
		return nil, err
	}
	rows, next := nextToken(rows, limit, func(t *domain.Task) string { return t.ID })
	res := &agentdv1.ListTasksResponse{NextPageToken: next}
	for _, t := range rows {
		res.Tasks = append(res.Tasks, taskToProto(t))
	}
	return connect.NewResponse(res), nil
}

// CancelTask stops a task. A queued task is cancelled at once; a running task is flagged and the worker
// stops it within about a second; a finished task is returned unchanged. Cancelling twice is harmless.
func (s *TaskServer) CancelTask(ctx context.Context, req *connect.Request[agentdv1.CancelTaskRequest]) (*connect.Response[agentdv1.Task], error) {
	authz := func(t *domain.Task) error {
		return authz.RequireResource(ctx, authz.TaskCancel, t.ProjectID, "task", t.ID)
	}
	t, err := s.mutateTask(ctx, req.Msg.Id, authz, s.requestCancel)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(taskToProto(t)), nil
}

// requestCancel applies the cancellation rules to t in place. It reports errUnchanged when there is
// nothing to write.
func (b *base) requestCancel(t *domain.Task) error {
	switch {
	case t.State.Final():
		return errUnchanged
	case t.State == domain.TaskQueued:
		t.State, t.FinishedAt, t.Result = domain.TaskCancelled, b.now(), "[cancelled]"
		return nil
	case t.CancelRequested:
		return errUnchanged
	}
	t.CancelRequested = true
	return nil
}

// mutateTask is the read-modify-write loop for tasks, with the same shape as mutateAgent. authz may be nil
// when the caller has already authorized the task's agent.
func (b *base) mutateTask(ctx context.Context, id string, authz func(*domain.Task) error, mut func(*domain.Task) error) (*domain.Task, error) {
	for range maxCASAttempts {
		t, err := b.st.GetTask(ctx, id)
		if err != nil {
			return nil, err
		}
		if authz != nil {
			if err := authz(t); err != nil {
				return nil, err
			}
		}
		if err := mut(t); err != nil {
			if errors.Is(err, errUnchanged) {
				return t, nil
			}
			return nil, err
		}
		if err := b.st.UpdateTask(ctx, t); err != nil {
			if errors.Is(err, errs.ErrConflict) {
				continue
			}
			return nil, err
		}
		return t, nil
	}
	return nil, fmt.Errorf("task %s: too many concurrent updates: %w", id, errs.ErrConflict)
}

// ListSteps returns the steps of a task after after_seq, at most limit of them (default 100, max 500).
func (s *TaskServer) ListSteps(ctx context.Context, req *connect.Request[agentdv1.ListStepsRequest]) (*connect.Response[agentdv1.ListStepsResponse], error) {
	m := req.Msg
	t, err := s.st.GetTask(ctx, m.TaskId)
	if err != nil {
		return nil, err
	}
	if err := authz.RequireResource(ctx, authz.TaskGet, t.ProjectID, "task", t.ID); err != nil {
		return nil, err
	}
	limit := int(m.Limit)
	switch {
	case limit <= 0:
		limit = 100
	case limit > 500:
		limit = 500
	}
	rows, err := s.st.ListSteps(ctx, t.ID, int(max(m.AfterSeq, 0)), limit)
	if err != nil {
		return nil, err
	}
	res := &agentdv1.ListStepsResponse{}
	for _, st := range rows {
		res.Steps = append(res.Steps, stepToProto(st))
	}
	return connect.NewResponse(res), nil
}
