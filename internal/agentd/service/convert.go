package service

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentdv1 "example.com/homecloud/gen/homecloud/agentd/v1"
	"example.com/homecloud/internal/agentd/domain"
	"example.com/homecloud/internal/agentd/llm"
)

// tsp converts t to a protobuf timestamp; the zero time becomes nil (unset).
func tsp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t.UTC())
}

func agentToProto(a *domain.Agent) *agentdv1.Agent {
	return &agentdv1.Agent{
		Id: a.ID, ProjectId: a.ProjectID, Name: a.Name, VmId: a.VMID,
		Model: a.Model, SystemPrompt: a.SystemPrompt, Workdir: a.Workdir,
		State: agentStateToProto(a.State), Labels: copyLabels(a.Labels),
		CreatedAt: tsp(a.CreatedAt), UpdatedAt: tsp(a.UpdatedAt),
	}
}

func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func agentStateToProto(s domain.AgentState) agentdv1.AgentState {
	switch s {
	case domain.AgentActive:
		return agentdv1.AgentState_AGENT_STATE_ACTIVE
	case domain.AgentOrphaned:
		return agentdv1.AgentState_AGENT_STATE_ORPHANED
	case domain.AgentDeleting:
		return agentdv1.AgentState_AGENT_STATE_DELETING
	}
	return agentdv1.AgentState_AGENT_STATE_UNSPECIFIED
}

func taskToProto(t *domain.Task) *agentdv1.Task {
	return &agentdv1.Task{
		Id: t.ID, AgentId: t.AgentID, ProjectId: t.ProjectID, Prompt: t.Prompt,
		CheckCommand: t.CheckCommand, MaxSteps: int32(t.MaxSteps), State: taskStateToProto(t.State),
		Verdict: t.Verdict, Result: t.Result, Steps: int32(t.Steps), Attempts: int32(t.Attempts),
		CancelRequested: t.CancelRequested, CreatedAt: tsp(t.CreatedAt), StartedAt: tsp(t.StartedAt),
		FinishedAt: tsp(t.FinishedAt),
	}
}

func taskStateToProto(s domain.TaskState) agentdv1.TaskState {
	switch s {
	case domain.TaskQueued:
		return agentdv1.TaskState_TASK_STATE_QUEUED
	case domain.TaskRunning:
		return agentdv1.TaskState_TASK_STATE_RUNNING
	case domain.TaskSucceeded:
		return agentdv1.TaskState_TASK_STATE_SUCCEEDED
	case domain.TaskFailed:
		return agentdv1.TaskState_TASK_STATE_FAILED
	case domain.TaskGaveUp:
		return agentdv1.TaskState_TASK_STATE_GAVE_UP
	case domain.TaskCancelled:
		return agentdv1.TaskState_TASK_STATE_CANCELLED
	case domain.TaskError:
		return agentdv1.TaskState_TASK_STATE_ERROR
	}
	return agentdv1.TaskState_TASK_STATE_UNSPECIFIED
}

// taskStateFromProto maps a wire state to the domain state. The unspecified state maps to "", which
// callers read as "no state given".
func taskStateFromProto(s agentdv1.TaskState) domain.TaskState {
	for _, d := range []domain.TaskState{domain.TaskQueued, domain.TaskRunning, domain.TaskSucceeded,
		domain.TaskFailed, domain.TaskGaveUp, domain.TaskCancelled, domain.TaskError} {
		if taskStateToProto(d) == s {
			return d
		}
	}
	return ""
}

func stepToProto(s *domain.Step) *agentdv1.Step {
	out := &agentdv1.Step{Seq: int32(s.Seq), Command: s.Command, Output: s.Output, At: tsp(s.At)}
	if s.ExitCode != nil {
		c := int32(*s.ExitCode)
		out.ExitCode = &c
	}
	return out
}

func modelToProto(m llm.ModelInfo) *agentdv1.Model {
	return &agentdv1.Model{Name: m.Name, SizeBytes: m.SizeBytes}
}
