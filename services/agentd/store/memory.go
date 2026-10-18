package store

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
)

// Memory is an in-process Store and the reference implementation of the contract, used by unit tests and
// local development. A single mutex serialises every call. Tx runs fn against a deep copy of the data and
// swaps the copy in only if fn returns nil, so a transaction is all-or-nothing. Nothing is persisted.
type Memory struct {
	mu sync.Mutex
	st *state
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{st: newState()} }

// state is the unlocked data behind Memory. It implements Store without locking: Memory's methods take the
// mutex and delegate here, and Tx hands fn a private clone that no other caller can reach.
type state struct {
	agents   map[string]*domain.Agent
	tasks    map[string]*domain.Task
	steps    map[string][]*domain.Step    // task ID -> steps in Seq order (Seq 1..n)
	messages map[string][]*domain.Message // task ID -> messages in Seq order (Seq 1..n)
}

var _ Store = (*state)(nil)

func newState() *state {
	return &state{
		agents:   map[string]*domain.Agent{},
		tasks:    map[string]*domain.Task{},
		steps:    map[string][]*domain.Step{},
		messages: map[string][]*domain.Message{},
	}
}

// clone returns a deep copy of s; no pointer, map or slice is shared with the original.
func (s *state) clone() *state {
	c := newState()
	for id, a := range s.agents {
		c.agents[id] = copyAgent(a)
	}
	for id, t := range s.tasks {
		c.tasks[id] = copyTask(t)
	}
	for id, ss := range s.steps {
		out := make([]*domain.Step, len(ss))
		for i, st := range ss {
			out[i] = copyStep(st)
		}
		c.steps[id] = out
	}
	for id, ms := range s.messages {
		out := make([]*domain.Message, len(ms))
		for i, m := range ms {
			out[i] = copyMessage(m)
		}
		c.messages[id] = out
	}
	return c
}

// ---- locked public surface ----

// Tx runs fn against a private deep copy of the store. If fn returns nil the copy replaces the store
// atomically; otherwise the store is left untouched and fn's error is returned. The Store passed to fn must
// not be used after fn returns, and fn must use only that Store (calling m directly would deadlock).
func (m *Memory) Tx(_ context.Context, fn func(Store) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	work := m.st.clone()
	if err := fn(work); err != nil {
		return err
	}
	m.st = work
	return nil
}

func (m *Memory) CreateAgent(ctx context.Context, a *domain.Agent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.CreateAgent(ctx, a)
}

func (m *Memory) GetAgent(ctx context.Context, id string) (*domain.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.GetAgent(ctx, id)
}

func (m *Memory) GetAgentByIdempotencyKey(ctx context.Context, project, key string) (*domain.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.GetAgentByIdempotencyKey(ctx, project, key)
}

func (m *Memory) ListAgents(ctx context.Context, project, vmID, after string, limit int) ([]*domain.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.ListAgents(ctx, project, vmID, after, limit)
}

func (m *Memory) CountAgentsOnVM(ctx context.Context, vmID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.CountAgentsOnVM(ctx, vmID)
}

func (m *Memory) ListAgentsByVM(ctx context.Context, vmID string) ([]*domain.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.ListAgentsByVM(ctx, vmID)
}

func (m *Memory) UpdateAgent(ctx context.Context, a *domain.Agent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.UpdateAgent(ctx, a)
}

func (m *Memory) DeleteAgent(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.DeleteAgent(ctx, id)
}

func (m *Memory) CreateTask(ctx context.Context, t *domain.Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.CreateTask(ctx, t)
}

func (m *Memory) GetTask(ctx context.Context, id string) (*domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.GetTask(ctx, id)
}

func (m *Memory) GetTaskByIdempotencyKey(ctx context.Context, project, key string) (*domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.GetTaskByIdempotencyKey(ctx, project, key)
}

func (m *Memory) ListTasks(ctx context.Context, agentID, project string, state domain.TaskState, after string, limit int) ([]*domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.ListTasks(ctx, agentID, project, state, after, limit)
}

func (m *Memory) UpdateTask(ctx context.Context, t *domain.Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.UpdateTask(ctx, t)
}

func (m *Memory) ClaimTasks(ctx context.Context, owner string, now time.Time, ttl time.Duration, limit int) ([]*domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.ClaimTasks(ctx, owner, now, ttl, limit)
}

func (m *Memory) ExtendLease(ctx context.Context, taskID, owner string, now time.Time, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.ExtendLease(ctx, taskID, owner, now, ttl)
}

func (m *Memory) AppendStep(ctx context.Context, s *domain.Step) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.AppendStep(ctx, s)
}

func (m *Memory) ListSteps(ctx context.Context, taskID string, afterSeq, limit int) ([]*domain.Step, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.ListSteps(ctx, taskID, afterSeq, limit)
}

func (m *Memory) AppendMessage(ctx context.Context, msg *domain.Message) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.AppendMessage(ctx, msg)
}

func (m *Memory) ListMessages(ctx context.Context, taskID string) ([]*domain.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.ListMessages(ctx, taskID)
}

// ---- unlocked implementation (also the Store handed to Tx callbacks) ----

// Tx on a state is nested inside the enclosing transaction: fn runs directly on s. It is not a savepoint,
// so an error from fn does not roll back earlier writes; the enclosing Tx decides the outcome.
func (s *state) Tx(_ context.Context, fn func(Store) error) error { return fn(s) }

func (s *state) CreateAgent(_ context.Context, a *domain.Agent) error {
	if a.VMID == "" {
		return errs.Invalid("agent %s: vm id is required", a.ID)
	}
	if _, ok := s.agents[a.ID]; ok {
		return errs.Exists("agent %s", a.ID)
	}
	if err := s.checkAgentUnique(a); err != nil {
		return err
	}
	a.Version = 1
	s.agents[a.ID] = copyAgent(a)
	return nil
}

func (s *state) GetAgent(_ context.Context, id string) (*domain.Agent, error) {
	a, ok := s.agents[id]
	if !ok {
		return nil, errs.NotFound("agent %s", id)
	}
	return copyAgent(a), nil
}

func (s *state) GetAgentByIdempotencyKey(_ context.Context, project, key string) (*domain.Agent, error) {
	if key != "" {
		for _, a := range s.agents {
			if a.ProjectID == project && a.IdempotencyKey == key {
				return copyAgent(a), nil
			}
		}
	}
	return nil, errs.NotFound("agent with idempotency key %q", key)
}

func (s *state) ListAgents(_ context.Context, project, vmID, after string, limit int) ([]*domain.Agent, error) {
	var all []*domain.Agent
	for _, a := range s.agents {
		if a.ProjectID == project && (vmID == "" || a.VMID == vmID) {
			all = append(all, copyAgent(a))
		}
	}
	return byID(all, agentKey, after, limit), nil
}

func (s *state) CountAgentsOnVM(_ context.Context, vmID string) (int, error) {
	n := 0
	for _, a := range s.agents {
		if a.VMID == vmID {
			n++
		}
	}
	return n, nil
}

func (s *state) ListAgentsByVM(_ context.Context, vmID string) ([]*domain.Agent, error) {
	var all []*domain.Agent
	for _, a := range s.agents {
		if a.VMID == vmID {
			all = append(all, copyAgent(a))
		}
	}
	return byID(all, agentKey, "", 0), nil
}

func (s *state) UpdateAgent(_ context.Context, a *domain.Agent) error {
	cur, ok := s.agents[a.ID]
	if !ok {
		return errs.NotFound("agent %s", a.ID)
	}
	if cur.Version != a.Version {
		return versionConflict("agent", a.ID)
	}
	if err := s.checkAgentUnique(a); err != nil {
		return err
	}
	a.Version++
	s.agents[a.ID] = copyAgent(a)
	return nil
}

// DeleteAgent removes the agent and, with it, every task of the agent and their steps and messages.
func (s *state) DeleteAgent(_ context.Context, id string) error {
	if _, ok := s.agents[id]; !ok {
		return errs.NotFound("agent %s", id)
	}
	for tid, t := range s.tasks {
		if t.AgentID == id {
			delete(s.tasks, tid)
			delete(s.steps, tid)
			delete(s.messages, tid)
		}
	}
	delete(s.agents, id)
	return nil
}

func (s *state) CreateTask(_ context.Context, t *domain.Task) error {
	if _, ok := s.tasks[t.ID]; ok {
		return errs.Exists("task %s", t.ID)
	}
	if err := s.checkTaskUnique(t); err != nil {
		return err
	}
	t.Version = 1
	s.tasks[t.ID] = copyTask(t)
	return nil
}

func (s *state) GetTask(_ context.Context, id string) (*domain.Task, error) {
	t, ok := s.tasks[id]
	if !ok {
		return nil, errs.NotFound("task %s", id)
	}
	return copyTask(t), nil
}

func (s *state) GetTaskByIdempotencyKey(_ context.Context, project, key string) (*domain.Task, error) {
	if key != "" {
		for _, t := range s.tasks {
			if t.ProjectID == project && t.IdempotencyKey == key {
				return copyTask(t), nil
			}
		}
	}
	return nil, errs.NotFound("task with idempotency key %q", key)
}

func (s *state) ListTasks(_ context.Context, agentID, project string, state domain.TaskState, after string, limit int) ([]*domain.Task, error) {
	if agentID == "" && project == "" {
		return nil, errs.Invalid("list tasks: agent id or project is required")
	}
	var all []*domain.Task
	for _, t := range s.tasks {
		if agentID != "" && t.AgentID != agentID {
			continue
		}
		if project != "" && t.ProjectID != project {
			continue
		}
		if state != "" && t.State != state {
			continue
		}
		all = append(all, copyTask(t))
	}
	return byID(all, taskKey, after, limit), nil
}

func (s *state) UpdateTask(_ context.Context, t *domain.Task) error {
	cur, ok := s.tasks[t.ID]
	if !ok {
		return errs.NotFound("task %s", t.ID)
	}
	if cur.Version != t.Version {
		return versionConflict("task", t.ID)
	}
	if err := s.checkTaskUnique(t); err != nil {
		return err
	}
	t.Version++
	s.tasks[t.ID] = copyTask(t)
	return nil
}

// ClaimTasks takes queued tasks, and running tasks whose lease has expired (LeaseUntil not after now), in ID
// order. Cancel-requested tasks are skipped. A task taken over from an expired lease gets Attempts++.
func (s *state) ClaimTasks(_ context.Context, owner string, now time.Time, ttl time.Duration, limit int) ([]*domain.Task, error) {
	var due []*domain.Task
	for _, t := range s.tasks {
		if t.CancelRequested {
			continue
		}
		claimable := t.State == domain.TaskQueued ||
			(t.State == domain.TaskRunning && !t.LeaseUntil.After(now))
		if claimable {
			due = append(due, t)
		}
	}
	due = byID(due, taskKey, "", limit)
	out := make([]*domain.Task, 0, len(due))
	for _, t := range due {
		if t.State == domain.TaskRunning {
			t.Attempts++
		}
		t.State = domain.TaskRunning
		t.LeaseOwner = owner
		t.LeaseUntil = now.Add(ttl)
		if t.StartedAt.IsZero() {
			t.StartedAt = now
		}
		t.Version++
		out = append(out, copyTask(t))
	}
	return out, nil
}

// ExtendLease moves LeaseUntil to now+ttl. It touches nothing else, not even Version, so a worker's copy of
// the task stays usable for UpdateTask.
func (s *state) ExtendLease(_ context.Context, taskID, owner string, now time.Time, ttl time.Duration) error {
	if owner == "" {
		return errs.Invalid("extend lease: owner is required")
	}
	t, ok := s.tasks[taskID]
	if !ok {
		return errs.NotFound("task %s", taskID)
	}
	if t.LeaseOwner != owner {
		return fmt.Errorf("task %s: lease is held by another owner: %w", taskID, errs.ErrConflict)
	}
	t.LeaseUntil = now.Add(ttl)
	return nil
}

// AppendStep stores a copy of st with the next dense Seq, which is also written back into st.
func (s *state) AppendStep(_ context.Context, st *domain.Step) (int, error) {
	if _, ok := s.tasks[st.TaskID]; !ok {
		return 0, errs.NotFound("task %s", st.TaskID)
	}
	st.Seq = len(s.steps[st.TaskID]) + 1
	s.steps[st.TaskID] = append(s.steps[st.TaskID], copyStep(st))
	return st.Seq, nil
}

func (s *state) ListSteps(_ context.Context, taskID string, afterSeq, limit int) ([]*domain.Step, error) {
	if _, ok := s.tasks[taskID]; !ok {
		return nil, errs.NotFound("task %s", taskID)
	}
	var out []*domain.Step
	for _, st := range s.steps[taskID] {
		if st.Seq <= afterSeq {
			continue
		}
		out = append(out, copyStep(st))
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// AppendMessage stores a copy of m with the next dense Seq, which is also written back into m.
func (s *state) AppendMessage(_ context.Context, m *domain.Message) (int, error) {
	if _, ok := s.tasks[m.TaskID]; !ok {
		return 0, errs.NotFound("task %s", m.TaskID)
	}
	m.Seq = len(s.messages[m.TaskID]) + 1
	s.messages[m.TaskID] = append(s.messages[m.TaskID], copyMessage(m))
	return m.Seq, nil
}

func (s *state) ListMessages(_ context.Context, taskID string) ([]*domain.Message, error) {
	if _, ok := s.tasks[taskID]; !ok {
		return nil, errs.NotFound("task %s", taskID)
	}
	out := make([]*domain.Message, 0, len(s.messages[taskID]))
	for _, m := range s.messages[taskID] {
		out = append(out, copyMessage(m))
	}
	return out, nil
}

// ---- helpers ----

// checkAgentUnique reports ErrAlreadyExists if another agent in a's project has the same name, or the same
// non-empty idempotency key.
func (s *state) checkAgentUnique(a *domain.Agent) error {
	for id, o := range s.agents {
		if id == a.ID || o.ProjectID != a.ProjectID {
			continue
		}
		if o.Name == a.Name {
			return errs.Exists("agent named %q", a.Name)
		}
		if a.IdempotencyKey != "" && o.IdempotencyKey == a.IdempotencyKey {
			return errs.Exists("idempotency key %q", a.IdempotencyKey)
		}
	}
	return nil
}

// checkTaskUnique reports ErrAlreadyExists if another task in t's project has the same non-empty
// idempotency key.
func (s *state) checkTaskUnique(t *domain.Task) error {
	if t.IdempotencyKey == "" {
		return nil
	}
	for id, o := range s.tasks {
		if id != t.ID && o.ProjectID == t.ProjectID && o.IdempotencyKey == t.IdempotencyKey {
			return errs.Exists("idempotency key %q", t.IdempotencyKey)
		}
	}
	return nil
}

func versionConflict(kind, id string) error {
	return fmt.Errorf("%s %s: stale version: %w", kind, id, errs.ErrConflict)
}

func agentKey(a *domain.Agent) string { return a.ID }
func taskKey(t *domain.Task) string   { return t.ID }

// byID sorts items by key, keeps those whose key is greater than after, and truncates to limit (<=0: no
// limit). It sorts in place and returns the resulting slice.
func byID[T any](items []T, key func(T) string, after string, limit int) []T {
	slices.SortFunc(items, func(a, b T) int { return strings.Compare(key(a), key(b)) })
	var out []T
	for _, it := range items {
		if after != "" && key(it) <= after {
			continue
		}
		out = append(out, it)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

func copyAgent(a *domain.Agent) *domain.Agent {
	c := *a
	c.Labels = maps.Clone(a.Labels)
	return &c
}

func copyTask(t *domain.Task) *domain.Task {
	c := *t
	if t.Calls != nil {
		c.Calls = append([]domain.CallRecord(nil), t.Calls...)
	}
	return &c
}

func copyStep(st *domain.Step) *domain.Step {
	c := *st
	if st.ExitCode != nil {
		code := *st.ExitCode
		c.ExitCode = &code
	}
	return &c
}

func copyMessage(m *domain.Message) *domain.Message {
	c := *m
	c.JSON = bytes.Clone(m.JSON)
	return &c
}
