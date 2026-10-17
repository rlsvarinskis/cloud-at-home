// Package store defines agentd's persistence contract (memory implementation for tests/dev, Postgres for production).
package store

import (
	"context"
	"time"

	"example.com/homecloud/internal/agentd/domain"
)

// Rules shared by every method:
//   - Get* of a missing row returns errs.ErrNotFound; Create* of a duplicate returns errs.ErrAlreadyExists
//     (duplicates: same ID, same project+name for agents, same project+idempotency key).
//   - Update* is a compare-and-swap on Version: mismatch -> errs.ErrConflict; on success the passed
//     object's Version is incremented in place.
//   - Lists are keyset-paginated by ID (IDs are time-sortable): `after` is exclusive; limit<=0 means no limit.
type Store interface {
	Tx(ctx context.Context, fn func(Store) error) error

	CreateAgent(ctx context.Context, a *domain.Agent) error
	GetAgent(ctx context.Context, id string) (*domain.Agent, error)
	GetAgentByIdempotencyKey(ctx context.Context, project, key string) (*domain.Agent, error)
	ListAgents(ctx context.Context, project, vmID, after string, limit int) ([]*domain.Agent, error) // vmID "" = any
	CountAgentsOnVM(ctx context.Context, vmID string) (int, error)
	ListAgentsByVM(ctx context.Context, vmID string) ([]*domain.Agent, error)
	UpdateAgent(ctx context.Context, a *domain.Agent) error
	// DeleteAgent removes the agent together with its tasks, steps and messages.
	DeleteAgent(ctx context.Context, id string) error

	CreateTask(ctx context.Context, t *domain.Task) error
	GetTask(ctx context.Context, id string) (*domain.Task, error)
	GetTaskByIdempotencyKey(ctx context.Context, project, key string) (*domain.Task, error)
	// ListTasks filters by agentID and/or project (at least one non-empty) and optionally by state ("" = any).
	ListTasks(ctx context.Context, agentID, project string, state domain.TaskState, after string, limit int) ([]*domain.Task, error)
	UpdateTask(ctx context.Context, t *domain.Task) error

	// ClaimTasks leases up to limit tasks that are Queued, or Running with an expired lease. For each it sets
	// LeaseOwner=owner, LeaseUntil=now+ttl, State=Running, StartedAt (if unset); a task taken over from an
	// expired lease also gets Attempts incremented. Tasks with CancelRequested are not claimed. Oldest first.
	ClaimTasks(ctx context.Context, owner string, now time.Time, ttl time.Duration, limit int) ([]*domain.Task, error)
	// ExtendLease pushes LeaseUntil to now+ttl only if owner still holds the lease (else errs.ErrConflict).
	ExtendLease(ctx context.Context, taskID, owner string, now time.Time, ttl time.Duration) error

	// AppendStep and AppendMessage assign the next dense Seq (1,2,3,...) for the task and return it.
	AppendStep(ctx context.Context, s *domain.Step) (int, error)
	ListSteps(ctx context.Context, taskID string, afterSeq, limit int) ([]*domain.Step, error)
	AppendMessage(ctx context.Context, m *domain.Message) (int, error)
	ListMessages(ctx context.Context, taskID string) ([]*domain.Message, error)
}
