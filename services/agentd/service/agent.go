package service

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/agentd/v1"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/ids"
)

// AgentServer implements agentdv1connect.AgentServiceHandler.
type AgentServer struct{ base }

// holder is the name under which an agent holds its VM in vmd.
func (b *base) holder(agentID string) string { return b.cfg.HoldPrefix + "/" + agentID }

// CreateAgent validates the request, checks that the VM exists in the same project and is not being
// deleted, validates the model, stores the agent and then places a hold on the VM. If the hold cannot be
// placed the stored agent is removed again, so an agent never exists without its hold being attempted.
func (s *AgentServer) CreateAgent(ctx context.Context, req *connect.Request[agentdv1.CreateAgentRequest]) (*connect.Response[agentdv1.Agent], error) {
	m := req.Msg
	if err := validProject(m.ProjectId); err != nil {
		return nil, err
	}
	if err := authz.Require(ctx, authz.AgentCreate, m.ProjectId); err != nil {
		return nil, err
	}
	if err := validName("agent", m.Name); err != nil {
		return nil, err
	}
	if m.Model == "" {
		return nil, errs.Invalid("model is required")
	}
	if m.VmId == "" {
		return nil, errs.Invalid("vm_id is required")
	}
	if err := validSystemPrompt(m.SystemPrompt); err != nil {
		return nil, err
	}
	if m.Workdir != "" {
		if err := validWorkdir(m.Workdir); err != nil {
			return nil, err
		}
	}
	if m.IdempotencyKey != "" {
		if a, err := s.st.GetAgentByIdempotencyKey(ctx, m.ProjectId, m.IdempotencyKey); err == nil {
			return connect.NewResponse(agentToProto(a)), nil
		}
	}

	vm, err := s.vm.GetVM(ctx, m.VmId)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return nil, errs.NotFound("vm %s", m.VmId)
		}
		return nil, s.downstream("vmd", err)
	}
	// A VM of another project is reported as missing so that tenants cannot probe each other's VMs.
	if vm.GetProjectId() != m.ProjectId {
		return nil, errs.NotFound("vm %s", m.VmId)
	}
	if vm.GetState() == vmdv1.VMState_VM_STATE_DELETING {
		return nil, errs.Precondition("vm %s is being deleted", m.VmId)
	}
	if err := s.checkModel(ctx, m.Model); err != nil {
		return nil, err
	}

	id := ids.New("agent")
	workdir := m.Workdir
	if workdir == "" {
		workdir = "/home/agent/agents/" + id
	}
	now := s.now()
	a := &domain.Agent{
		ID: id, ProjectID: m.ProjectId, Name: m.Name, VMID: m.VmId, Model: m.Model,
		SystemPrompt: m.SystemPrompt, Workdir: workdir, State: domain.AgentActive, Labels: m.Labels,
		IdempotencyKey: m.IdempotencyKey, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.st.CreateAgent(ctx, a); err != nil {
		// A concurrent request with the same idempotency key won the race: return its agent.
		if m.IdempotencyKey != "" && errors.Is(err, errs.ErrAlreadyExists) {
			if existing, gerr := s.st.GetAgentByIdempotencyKey(ctx, m.ProjectId, m.IdempotencyKey); gerr == nil {
				return connect.NewResponse(agentToProto(existing)), nil
			}
		}
		return nil, err
	}
	if _, err := s.vm.AddHold(ctx, m.VmId, s.holder(id)); err != nil {
		if derr := s.st.DeleteAgent(ctx, id); derr != nil {
			s.cfg.Log.Error("rollback of agent after failed hold failed", "agent", id, "err", derr)
		}
		return nil, s.downstream("vmd", err)
	}
	return connect.NewResponse(agentToProto(a)), nil
}

// GetAgent returns one agent. Callers without access to its project get NotFound.
func (s *AgentServer) GetAgent(ctx context.Context, req *connect.Request[agentdv1.GetAgentRequest]) (*connect.Response[agentdv1.Agent], error) {
	a, err := s.st.GetAgent(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	if err := authz.RequireResource(ctx, authz.AgentGet, a.ProjectID, "agent", a.ID); err != nil {
		return nil, err
	}
	return connect.NewResponse(agentToProto(a)), nil
}

// ListAgents pages through the agents of one project, optionally only those on one VM.
func (s *AgentServer) ListAgents(ctx context.Context, req *connect.Request[agentdv1.ListAgentsRequest]) (*connect.Response[agentdv1.ListAgentsResponse], error) {
	m := req.Msg
	if err := validProject(m.ProjectId); err != nil {
		return nil, err
	}
	if err := authz.Require(ctx, authz.AgentList, m.ProjectId); err != nil {
		return nil, err
	}
	limit := pageSize(m.PageSize)
	rows, err := s.st.ListAgents(ctx, m.ProjectId, m.VmId, m.PageToken, limit+1)
	if err != nil {
		return nil, err
	}
	rows, next := nextToken(rows, limit, func(a *domain.Agent) string { return a.ID })
	res := &agentdv1.ListAgentsResponse{NextPageToken: next}
	for _, a := range rows {
		res.Agents = append(res.Agents, agentToProto(a))
	}
	return connect.NewResponse(res), nil
}

// UpdateAgent changes the model and/or the system prompt. Unset fields are left alone.
func (s *AgentServer) UpdateAgent(ctx context.Context, req *connect.Request[agentdv1.UpdateAgentRequest]) (*connect.Response[agentdv1.Agent], error) {
	m := req.Msg
	if m.Model != nil {
		if *m.Model == "" {
			return nil, errs.Invalid("model cannot be empty")
		}
		if err := s.checkModel(ctx, *m.Model); err != nil {
			return nil, err
		}
	}
	if m.SystemPrompt != nil {
		if err := validSystemPrompt(*m.SystemPrompt); err != nil {
			return nil, err
		}
	}
	a, err := s.mutateAgent(ctx, m.Id, authz.AgentUpdate, func(a *domain.Agent) error {
		changed := false
		if m.Model != nil && *m.Model != a.Model {
			a.Model, changed = *m.Model, true
		}
		if m.SystemPrompt != nil && *m.SystemPrompt != a.SystemPrompt {
			a.SystemPrompt, changed = *m.SystemPrompt, true
		}
		if !changed {
			return errUnchanged
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(agentToProto(a)), nil
}

// mutateAgent is the read-modify-write loop for agents: load, authorize against the agent's project,
// apply mut, and write with compare-and-swap, retrying when another writer got in between.
func (b *base) mutateAgent(ctx context.Context, id string, perm authz.Permission, mut func(*domain.Agent) error) (*domain.Agent, error) {
	for range maxCASAttempts {
		a, err := b.st.GetAgent(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := authz.RequireResource(ctx, perm, a.ProjectID, "agent", id); err != nil {
			return nil, err
		}
		if err := mut(a); err != nil {
			if errors.Is(err, errUnchanged) {
				return a, nil
			}
			return nil, err
		}
		a.UpdatedAt = b.now()
		if err := b.st.UpdateAgent(ctx, a); err != nil {
			if errors.Is(err, errs.ErrConflict) {
				continue
			}
			return nil, err
		}
		return a, nil
	}
	return nil, fmt.Errorf("agent %s: too many concurrent updates: %w", id, errs.ErrConflict)
}

// DeleteAgent cancels the agent's tasks, removes the agent (its tasks, steps and messages go with it) and
// releases its hold on the VM. A failed release is logged, not returned: the hold is harmless and can be
// cleared later, and the agent is already gone.
func (s *AgentServer) DeleteAgent(ctx context.Context, req *connect.Request[agentdv1.DeleteAgentRequest]) (*connect.Response[agentdv1.DeleteAgentResponse], error) {
	id := req.Msg.Id
	a, err := s.st.GetAgent(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := authz.RequireResource(ctx, authz.AgentDelete, a.ProjectID, "agent", id); err != nil {
		return nil, err
	}
	tasks, err := s.st.ListTasks(ctx, id, "", "", "", 0)
	if err != nil {
		return nil, err
	}
	// The caller is authorized for the agent, and its tasks belong to the same project, so no further check.
	for _, t := range tasks {
		if t.State.Final() {
			continue
		}
		if _, err := s.mutateTask(ctx, t.ID, nil, s.requestCancel); err != nil && !errors.Is(err, errs.ErrNotFound) {
			return nil, err
		}
	}
	if err := s.st.DeleteAgent(ctx, id); err != nil {
		return nil, err
	}
	if _, err := s.vm.RemoveHold(ctx, a.VMID, s.holder(id)); err != nil && !errors.Is(err, errs.ErrNotFound) {
		s.cfg.Log.Warn("release of VM hold failed; it can be cleared later", "agent", id, "vm", a.VMID, "err", err)
	}
	return connect.NewResponse(&agentdv1.DeleteAgentResponse{}), nil
}
