package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/pg"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
)

const taskCols = "id, agent_id, project_id, prompt, check_command, max_steps, state, verdict, result, " +
	"steps, attempts, cancel_requested, idempotency_key, lease_owner, lease_until, version, " +
	"created_at, started_at, finished_at"

// CreateTask inserts t. A duplicate ID, or an idempotency key already used in t's project, yields
// errs.ErrAlreadyExists. On success t.Version is set to 1. The agent is not checked for existence.
func (s *Store) CreateTask(ctx context.Context, t *domain.Task) error {
	_, err := s.q.Exec(ctx,
		"INSERT INTO tasks ("+taskCols+") VALUES ("+pg.Placeholders(1, 19)+")",
		t.ID, t.AgentID, t.ProjectID, t.Prompt, t.CheckCommand, t.MaxSteps, string(t.State), t.Verdict,
		t.Result, t.Steps, t.Attempts, t.CancelRequested, t.IdempotencyKey, t.LeaseOwner, t.LeaseUntil,
		int64(1), t.CreatedAt, nullTime(t.StartedAt), nullTime(t.FinishedAt))
	if err != nil {
		return uniqueErr(err, "task %s", t.ID)
	}
	t.Version = 1
	return nil
}

// GetTask returns the task with the given ID, or errs.ErrNotFound.
func (s *Store) GetTask(ctx context.Context, id string) (*domain.Task, error) {
	t, err := scanTask(s.q.QueryRow(ctx, "SELECT "+taskCols+" FROM tasks WHERE id = $1", id))
	if err != nil {
		return nil, notFound(err, "task %s", id)
	}
	return t, nil
}

// GetTaskByIdempotencyKey returns the task in project with the given key. An empty key never matches.
func (s *Store) GetTaskByIdempotencyKey(ctx context.Context, project, key string) (*domain.Task, error) {
	if key == "" {
		return nil, errs.NotFound("task with idempotency key %q", key)
	}
	t, err := scanTask(s.q.QueryRow(ctx,
		"SELECT "+taskCols+" FROM tasks WHERE project_id = $1 AND idempotency_key = $2", project, key))
	if err != nil {
		return nil, notFound(err, "task with idempotency key %q", key)
	}
	return t, nil
}

// ListTasks returns tasks filtered by agentID and/or project (at least one must be set) and by state (empty
// = any), ordered by ID after the exclusive cursor after. limit <= 0 means no limit.
func (s *Store) ListTasks(ctx context.Context, agentID, project string, state domain.TaskState, after string, limit int) ([]*domain.Task, error) {
	if agentID == "" && project == "" {
		return nil, errs.Invalid("list tasks: agent id or project is required")
	}
	var f filter
	if agentID != "" {
		f.add("agent_id = $%d", agentID)
	}
	if project != "" {
		f.add("project_id = $%d", project)
	}
	if state != "" {
		f.add("state = $%d", string(state))
	}
	if after != "" {
		f.add("id > $%d", after)
	}
	sql, args := f.selectSQL(taskCols, "tasks", "id", limit)
	return s.queryTasks(ctx, sql, args...)
}

// UpdateTask replaces the stored task with t if t.Version matches the stored version (errs.ErrConflict
// otherwise). On success t.Version is incremented.
func (s *Store) UpdateTask(ctx context.Context, t *domain.Task) error {
	var version int64
	err := s.q.QueryRow(ctx, `
		UPDATE tasks SET agent_id = $2, project_id = $3, prompt = $4, check_command = $5, max_steps = $6,
			state = $7, verdict = $8, result = $9, steps = $10, attempts = $11, cancel_requested = $12,
			idempotency_key = $13, lease_owner = $14, lease_until = $15, created_at = $16,
			started_at = $17, finished_at = $18, version = version + 1
		WHERE id = $1 AND version = $19
		RETURNING version`,
		t.ID, t.AgentID, t.ProjectID, t.Prompt, t.CheckCommand, t.MaxSteps, string(t.State), t.Verdict,
		t.Result, t.Steps, t.Attempts, t.CancelRequested, t.IdempotencyKey, t.LeaseOwner, t.LeaseUntil,
		t.CreatedAt, nullTime(t.StartedAt), nullTime(t.FinishedAt), t.Version).Scan(&version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return s.casMiss(ctx, "tasks", "task", t.ID)
		}
		return uniqueErr(err, "task %s", t.ID)
	}
	t.Version = version
	return nil
}

// claimSQL leases the oldest due tasks in one statement. Due tasks are queued ones, or running ones whose
// lease has expired, that are not cancel-requested. The row locks taken by FOR UPDATE SKIP LOCKED keep
// concurrent claimers from picking the same task, and a row that another claimer changed in the meantime
// is re-checked against the WHERE clause before it is updated. In the SET list, t.state and t.attempts
// read the row as it was before the update.
const claimSQL = `
WITH due AS (
	SELECT id FROM tasks
	WHERE NOT cancel_requested AND state IN ('queued', 'running')
		AND (state = 'queued' OR lease_until <= $2)
	ORDER BY id
	LIMIT $4
	FOR UPDATE SKIP LOCKED
), claimed AS (
	UPDATE tasks AS t
	SET state = 'running',
		lease_owner = $1,
		lease_until = $3,
		started_at = COALESCE(t.started_at, $2),
		attempts = t.attempts + (t.state = 'running')::int,
		version = t.version + 1
	FROM due
	WHERE t.id = due.id
	RETURNING t.*
)
SELECT ` + taskCols + ` FROM claimed ORDER BY id`

// ClaimTasks leases up to limit tasks to owner until now+ttl, oldest first. It takes queued tasks and running
// tasks whose lease expired at or before now, skips cancel-requested tasks, and sets State to Running and
// StartedAt when unset. A task taken from an expired lease gets Attempts incremented. limit <= 0 means no
// limit.
func (s *Store) ClaimTasks(ctx context.Context, owner string, now time.Time, ttl time.Duration, limit int) ([]*domain.Task, error) {
	return s.queryTasks(ctx, claimSQL, owner, now, now.Add(ttl), limitArg(limit))
}

// ExtendLease moves the lease of taskID to now+ttl if owner still holds it. It changes nothing else and
// does not touch Version. An empty owner yields errs.ErrInvalidArgument, a missing task errs.ErrNotFound,
// and a lease held by someone else errs.ErrConflict.
func (s *Store) ExtendLease(ctx context.Context, taskID, owner string, now time.Time, ttl time.Duration) error {
	if owner == "" {
		return errs.Invalid("extend lease: owner is required")
	}
	tag, err := s.q.Exec(ctx,
		"UPDATE tasks SET lease_until = $3 WHERE id = $1 AND lease_owner = $2",
		taskID, owner, now.Add(ttl))
	if err != nil {
		return fmt.Errorf("postgres: extend lease of task %s: %w", taskID, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := s.q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1)", taskID).Scan(&exists); err != nil {
		return fmt.Errorf("postgres: check task %s: %w", taskID, err)
	}
	if !exists {
		return errs.NotFound("task %s", taskID)
	}
	return fmt.Errorf("task %s: lease is held by another owner: %w", taskID, errs.ErrConflict)
}

func (s *Store) queryTasks(ctx context.Context, sql string, args ...any) ([]*domain.Task, error) {
	rows, err := s.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: list tasks: %w", err)
	}
	defer rows.Close()
	var out []*domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scan task: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list tasks: %w", err)
	}
	return out, nil
}

func scanTask(r pgx.Row) (*domain.Task, error) {
	var (
		t                   domain.Task
		state               string
		leaseUntil          time.Time
		startedAt, finished *time.Time
		createdAt           time.Time
	)
	err := r.Scan(&t.ID, &t.AgentID, &t.ProjectID, &t.Prompt, &t.CheckCommand, &t.MaxSteps, &state,
		&t.Verdict, &t.Result, &t.Steps, &t.Attempts, &t.CancelRequested, &t.IdempotencyKey,
		&t.LeaseOwner, &leaseUntil, &t.Version, &createdAt, &startedAt, &finished)
	if err != nil {
		return nil, err
	}
	t.State = domain.TaskState(state)
	t.LeaseUntil = leaseUntil.UTC()
	t.CreatedAt = createdAt.UTC()
	if startedAt != nil {
		t.StartedAt = startedAt.UTC()
	}
	if finished != nil {
		t.FinishedAt = finished.UTC()
	}
	return &t, nil
}
