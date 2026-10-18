package service

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
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
		FinishedAt: tsp(t.FinishedAt), Reason: t.Reason, VerdictSource: t.VerdictSource,
		PromptTokens: t.PromptTokens, CompletionTokens: t.CompletionTokens, ModelCalls: int32(t.ModelCalls),
		PartialCalls: int32(t.PartialCalls), FirstTokenSeconds: t.FirstTokenSeconds,
		FirstCommandSeconds: t.FirstCommandSeconds, LiveAnswer: t.LiveAnswer, LiveReasoning: t.LiveReasoning,
		Calls: callsToProto(t.Calls), CheckEachStep: t.CheckEachStep, MaxMinutes: int32(t.MaxMinutes),
		MaxCallTokens: int32(t.MaxCallTokens),
	}
}

func callsToProto(calls []domain.CallRecord) []*agentdv1.CallStat {
	out := make([]*agentdv1.CallStat, 0, len(calls))
	for _, c := range calls {
		out = append(out, &agentdv1.CallStat{Index: int32(c.Index), Seconds: c.Seconds,
			PromptTokens: c.PromptTokens, GenTokens: c.GenTokens, End: c.End, Partial: c.Partial, Note: c.Note})
	}
	return out
}

func taskStateToProto(s domain.TaskState) agentdv1.TaskState {
	switch s {
	case domain.TaskQueued:
		return agentdv1.TaskState_TASK_STATE_QUEUED
	case domain.TaskRunning:
		return agentdv1.TaskState_TASK_STATE_RUNNING
	case domain.TaskSucceeded:
		return agentdv1.TaskState_TASK_STATE_SUCCEEDED
	case domain.TaskSucceededUnverified:
		return agentdv1.TaskState_TASK_STATE_SUCCEEDED_UNVERIFIED
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
		domain.TaskSucceededUnverified, domain.TaskFailed, domain.TaskGaveUp, domain.TaskCancelled, domain.TaskError} {
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

// modelToProto converts an installed model. The size is also given in GiB, and fits_vram compares the weights
// with the budget less the headroom.
func modelToProto(m llm.ModelInfo, vramGiB float64) *agentdv1.Model {
	sizeGiB := float64(m.SizeBytes) / (1 << 30)
	return &agentdv1.Model{Name: m.Name, SizeBytes: m.SizeBytes, SizeGib: sizeGiB, Params: m.Params,
		Quant: m.Quant, ContextLength: m.ContextLength, FitsVram: sizeGiB <= vramGiB-vramHeadroomGiB}
}
