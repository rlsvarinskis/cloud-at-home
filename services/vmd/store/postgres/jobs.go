package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
)

var jobCols = []string{
	"id", "vm_id", "project_id", "command", "workdir", "state", "exit_code", "timeout_seconds",
	"killed_reason", "idempotency_key", "version", "started_at", "finished_at",
}

var (
	jobSelect = selectSQL("jobs", jobCols)
	jobInsert = insertSQL("jobs", jobCols)
	jobUpdate = updateSQL("jobs", jobCols) + " WHERE id = @id AND version = @expected RETURNING version"
)

func jobArgs(j *domain.Job) pgx.NamedArgs {
	return pgx.NamedArgs{
		"id": j.ID, "vm_id": j.VMID, "project_id": j.ProjectID, "command": j.Command, "workdir": j.Workdir,
		"state": string(j.State), "exit_code": j.ExitCode, "timeout_seconds": j.TimeoutSeconds,
		"killed_reason": j.KilledReason, "idempotency_key": j.IdempotencyKey, "version": j.Version,
		"started_at": j.StartedAt.UTC(), "finished_at": nullableTime(j.FinishedAt),
	}
}

// CreateJob inserts j with Version 1 and sets j.Version.
func (s *Store) CreateJob(ctx context.Context, j *domain.Job) error {
	args := jobArgs(j)
	args["version"] = int64(1)
	if err := s.insert(ctx, "job", j.ID, jobInsert, args); err != nil {
		return err
	}
	j.Version = 1
	return nil
}

// GetJob returns the job with the given ID.
func (s *Store) GetJob(ctx context.Context, id string) (*domain.Job, error) {
	return getOne(ctx, s.db, "job", id, jobSelect+" WHERE id = $1", scanJob, id)
}

// GetJobByIdempotencyKey returns the project's job created with key. An empty key matches nothing.
func (s *Store) GetJobByIdempotencyKey(ctx context.Context, project, key string) (*domain.Job, error) {
	if key == "" {
		return nil, errs.NotFound("job idempotency key %q", key)
	}
	return getOne(ctx, s.db, "job", "idempotency key "+key, jobSelect+" WHERE project_id = $1 AND idempotency_key = $2", scanJob, project, key)
}

// UpdateJob is a compare-and-swap on Version; see package store.
func (s *Store) UpdateJob(ctx context.Context, j *domain.Job) error {
	args := jobArgs(j)
	args["expected"] = j.Version
	v, err := s.update(ctx, "job", "jobs", j.ID, jobUpdate, args)
	if err != nil {
		return err
	}
	j.Version = v
	return nil
}

// ListJobs returns the jobs of vmID after the cursor, ordered by ID.
func (s *Store) ListJobs(ctx context.Context, vmID, after string, limit int) ([]*domain.Job, error) {
	var f filter
	f.add("vm_id = " + f.arg(vmID))
	f.keyset(after)
	sql, args := f.selectWhere(jobCols, "jobs", limit)
	return listRows(ctx, s.db, sql, scanJob, args...)
}

// ListRunningJobs returns the running jobs of vmID, ordered by ID.
func (s *Store) ListRunningJobs(ctx context.Context, vmID string) ([]*domain.Job, error) {
	var f filter
	f.add("vm_id = " + f.arg(vmID))
	f.add("state = 'running'") // literal, so the partial index jobs_running_idx applies
	sql, args := f.selectWhere(jobCols, "jobs", 0)
	return listRows(ctx, s.db, sql, scanJob, args...)
}

func scanJob(r scanner) (*domain.Job, error) {
	var (
		j     domain.Job
		state string
		exit  *int
		fin   *time.Time
	)
	if err := r.Scan(&j.ID, &j.VMID, &j.ProjectID, &j.Command, &j.Workdir, &state, &exit, &j.TimeoutSeconds,
		&j.KilledReason, &j.IdempotencyKey, &j.Version, &j.StartedAt, &fin); err != nil {
		return nil, err
	}
	j.State = domain.JobState(state)
	j.ExitCode = exit
	if fin != nil {
		j.FinishedAt = fin.UTC()
	}
	j.StartedAt = j.StartedAt.UTC()
	return &j, nil
}
