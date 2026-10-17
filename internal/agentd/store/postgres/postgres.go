// Package postgres implements agentd's store.Store on PostgreSQL.
//
// Every method runs on the Store's connection: the pool for a top-level Store, or the active transaction
// inside a Tx callback. Compare-and-swap updates, claims and sequence assignment are single statements or
// lock the owning task row, so they hold up under concurrent replicas.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"example.com/homecloud/internal/agentd/domain"
	"example.com/homecloud/internal/agentd/store"
	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/pg"
)

// Store is the PostgreSQL implementation of store.Store.
type Store struct {
	pool *pgxpool.Pool // non-nil only on a top-level Store; nil inside a transaction
	q    querier       // the pool, or the transaction the Store is bound to
}

var _ store.Store = (*Store)(nil)

// querier is the subset of *pgxpool.Pool and pgx.Tx used by the store.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// New returns a Store that uses pool. The schema must already exist; see Migrate.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: pool}
}

// Tx runs fn in a transaction. It commits if fn returns nil and rolls back otherwise; fn's error is returned
// unwrapped. Inside another Tx it runs fn on the enclosing transaction.
func (s *Store) Tx(ctx context.Context, fn func(store.Store) error) error {
	return s.withTx(ctx, func(t *Store) error { return fn(t) })
}

// withTx is Tx for internal use, passing the concrete type so methods can share the transaction.
func (s *Store) withTx(ctx context.Context, fn func(*Store) error) error {
	if s.pool == nil {
		return fn(s)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	// Rollback after a successful commit returns pgx.ErrTxClosed, which is ignored.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(&Store{q: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

// ---- agents ----

const agentCols = "id, project_id, name, vm_id, model, system_prompt, workdir, state, labels, " +
	"idempotency_key, version, created_at, updated_at"

// CreateAgent inserts a. It rejects an empty VMID with errs.ErrInvalidArgument and a duplicate ID, name or
// idempotency key with errs.ErrAlreadyExists. On success a.Version is set to 1.
func (s *Store) CreateAgent(ctx context.Context, a *domain.Agent) error {
	if a.VMID == "" {
		return errs.Invalid("agent %s: vm id is required", a.ID)
	}
	labels, err := json.Marshal(a.Labels)
	if err != nil {
		return fmt.Errorf("postgres: encode agent labels: %w", err)
	}
	_, err = s.q.Exec(ctx,
		"INSERT INTO agents ("+agentCols+") VALUES ("+pg.Placeholders(1, 13)+")",
		a.ID, a.ProjectID, a.Name, a.VMID, a.Model, a.SystemPrompt, a.Workdir, string(a.State),
		string(labels), a.IdempotencyKey, int64(1), a.CreatedAt, a.UpdatedAt)
	if err != nil {
		return uniqueErr(err, "agent %s", a.ID)
	}
	a.Version = 1
	return nil
}

// GetAgent returns the agent with the given ID, or errs.ErrNotFound.
func (s *Store) GetAgent(ctx context.Context, id string) (*domain.Agent, error) {
	a, err := scanAgent(s.q.QueryRow(ctx, "SELECT "+agentCols+" FROM agents WHERE id = $1", id))
	if err != nil {
		return nil, notFound(err, "agent %s", id)
	}
	return a, nil
}

// GetAgentByIdempotencyKey returns the agent in project with the given key. An empty key never matches.
func (s *Store) GetAgentByIdempotencyKey(ctx context.Context, project, key string) (*domain.Agent, error) {
	if key == "" {
		return nil, errs.NotFound("agent with idempotency key %q", key)
	}
	a, err := scanAgent(s.q.QueryRow(ctx,
		"SELECT "+agentCols+" FROM agents WHERE project_id = $1 AND idempotency_key = $2", project, key))
	if err != nil {
		return nil, notFound(err, "agent with idempotency key %q", key)
	}
	return a, nil
}

// ListAgents returns agents of project, optionally restricted to vmID (empty = any), ordered by ID after the
// exclusive cursor after. limit <= 0 means no limit.
func (s *Store) ListAgents(ctx context.Context, project, vmID, after string, limit int) ([]*domain.Agent, error) {
	var q filter
	q.add("project_id = $%d", project)
	if vmID != "" {
		q.add("vm_id = $%d", vmID)
	}
	if after != "" {
		q.add("id > $%d", after)
	}
	sql, args := q.selectSQL(agentCols, "agents", "id", limit)
	return s.queryAgents(ctx, sql, args...)
}

// CountAgentsOnVM returns the number of agents, across projects, that are bound to vmID.
func (s *Store) CountAgentsOnVM(ctx context.Context, vmID string) (int, error) {
	var n int
	if err := s.q.QueryRow(ctx, "SELECT count(*) FROM agents WHERE vm_id = $1", vmID).Scan(&n); err != nil {
		return 0, fmt.Errorf("postgres: count agents on vm %s: %w", vmID, err)
	}
	return n, nil
}

// ListAgentsByVM returns every agent bound to vmID, ordered by ID.
func (s *Store) ListAgentsByVM(ctx context.Context, vmID string) ([]*domain.Agent, error) {
	return s.queryAgents(ctx, "SELECT "+agentCols+" FROM agents WHERE vm_id = $1 ORDER BY id", vmID)
}

// UpdateAgent replaces the stored agent with a if a.Version matches the stored version
// (errs.ErrConflict otherwise). On success a.Version is incremented.
func (s *Store) UpdateAgent(ctx context.Context, a *domain.Agent) error {
	labels, err := json.Marshal(a.Labels)
	if err != nil {
		return fmt.Errorf("postgres: encode agent labels: %w", err)
	}
	var version int64
	err = s.q.QueryRow(ctx, `
		UPDATE agents SET project_id = $2, name = $3, vm_id = $4, model = $5, system_prompt = $6,
			workdir = $7, state = $8, labels = $9, idempotency_key = $10, created_at = $11,
			updated_at = $12, version = version + 1
		WHERE id = $1 AND version = $13
		RETURNING version`,
		a.ID, a.ProjectID, a.Name, a.VMID, a.Model, a.SystemPrompt, a.Workdir, string(a.State),
		string(labels), a.IdempotencyKey, a.CreatedAt, a.UpdatedAt, a.Version).Scan(&version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return s.casMiss(ctx, "agents", "agent", a.ID)
		}
		return uniqueErr(err, "agent %s", a.ID)
	}
	a.Version = version
	return nil
}

// DeleteAgent removes the agent and, in the same transaction, its tasks. Steps and messages go with the
// tasks through their foreign keys. A missing agent yields errs.ErrNotFound and changes nothing.
func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	return s.withTx(ctx, func(t *Store) error {
		tag, err := t.q.Exec(ctx, "DELETE FROM agents WHERE id = $1", id)
		if err != nil {
			return fmt.Errorf("postgres: delete agent %s: %w", id, err)
		}
		if tag.RowsAffected() == 0 {
			return errs.NotFound("agent %s", id)
		}
		if _, err := t.q.Exec(ctx, "DELETE FROM tasks WHERE agent_id = $1", id); err != nil {
			return fmt.Errorf("postgres: delete tasks of agent %s: %w", id, err)
		}
		return nil
	})
}

func (s *Store) queryAgents(ctx context.Context, sql string, args ...any) ([]*domain.Agent, error) {
	rows, err := s.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: list agents: %w", err)
	}
	defer rows.Close()
	var out []*domain.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scan agent: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list agents: %w", err)
	}
	return out, nil
}

func scanAgent(r pgx.Row) (*domain.Agent, error) {
	var (
		a               domain.Agent
		state           string
		labels          []byte
		createdAt, updA time.Time
	)
	err := r.Scan(&a.ID, &a.ProjectID, &a.Name, &a.VMID, &a.Model, &a.SystemPrompt, &a.Workdir, &state,
		&labels, &a.IdempotencyKey, &a.Version, &createdAt, &updA)
	if err != nil {
		return nil, err
	}
	a.State = domain.AgentState(state)
	if err := json.Unmarshal(labels, &a.Labels); err != nil {
		return nil, fmt.Errorf("decode agent labels: %w", err)
	}
	a.CreatedAt = createdAt.UTC()
	a.UpdatedAt = updA.UTC()
	return &a, nil
}

// ---- helpers ----

// casMiss explains a compare-and-swap that matched no row: the row is missing or its version is stale.
func (s *Store) casMiss(ctx context.Context, table, kind, id string) error {
	var exists bool
	if err := s.q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+table+" WHERE id = $1)", id).Scan(&exists); err != nil {
		return fmt.Errorf("postgres: check %s %s: %w", kind, id, err)
	}
	if !exists {
		return errs.NotFound("%s %s", kind, id)
	}
	return fmt.Errorf("%s %s: stale version: %w", kind, id, errs.ErrConflict)
}

// notFound maps pgx.ErrNoRows to errs.NotFound with the given description and wraps other errors.
func notFound(err error, format string, a ...any) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.NotFound(format, a...)
	}
	return fmt.Errorf("postgres: %w", err)
}

// uniqueErr maps a unique-constraint violation to errs.ErrAlreadyExists and wraps other errors.
func uniqueErr(err error, format string, a ...any) error {
	if pg.IsUniqueViolation(err) {
		return errs.Exists(format, a...)
	}
	return fmt.Errorf("postgres: %w", err)
}

// nullTime maps the zero time to SQL NULL.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// limitArg returns the LIMIT argument for limit: nil (no limit) when limit <= 0.
func limitArg(limit int) any {
	if limit <= 0 {
		return nil
	}
	return int64(limit)
}

// filter builds a WHERE clause whose parameters are numbered as they are added. Only the placeholder
// numbers are formatted into the SQL; values always travel as arguments.
type filter struct {
	conds []string
	args  []any
}

// add appends a condition. format must contain one %d, which receives the placeholder number.
func (f *filter) add(format string, arg any) {
	f.args = append(f.args, arg)
	f.conds = append(f.conds, fmt.Sprintf(format, len(f.args)))
}

// selectSQL returns SELECT cols FROM table WHERE conds ORDER BY order [LIMIT n].
func (f *filter) selectSQL(cols, table, order string, limit int) (string, []any) {
	where := "TRUE"
	if len(f.conds) > 0 {
		where = strings.Join(f.conds, " AND ")
	}
	sql := "SELECT " + cols + " FROM " + table + " WHERE " + where + " ORDER BY " + order
	if limit > 0 {
		f.args = append(f.args, int64(limit))
		sql += " LIMIT $" + strconv.Itoa(len(f.args))
	}
	return sql, f.args
}
