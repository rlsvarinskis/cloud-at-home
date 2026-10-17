// Package postgres implements store.Store on PostgreSQL. It passes the storetest conformance suite
// and matches the in-memory reference store (package store) with these details:
//
//   - Timestamps are timestamptz with microsecond precision. NextReconcile and FinishedAt store the
//     zero time as NULL; LeaseUntil stores it as the epoch, its column default. Other timestamps are
//     stored as given and read back in UTC.
//   - Labels are jsonb. A nil map is stored as JSON null and reads back as nil; an empty map reads
//     back as an empty, non-nil map.
//   - DiskIDs and Holds are text[]. A nil slice is stored as NULL and reads back as nil.
//   - IDs are ordered and compared as bytes (COLLATE "C"), like Go string comparison.
//   - Inside a Tx, a failed create or update is rolled back to a savepoint, so the transaction stays
//     usable after ErrAlreadyExists or ErrConflict, as it does with the memory store.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/pg"
	"example.com/homecloud/internal/vmd/domain"
	"example.com/homecloud/internal/vmd/store"
)

var _ store.Store = (*Store)(nil)

// querier is the part of *pgxpool.Pool and pgx.Tx that the store uses, so the same methods run
// inside or outside a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// scanner is satisfied by pgx.Row and pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// Store is a store.Store on PostgreSQL. A Store passed to a Tx callback is bound to that transaction.
type Store struct {
	db   querier
	pool *pgxpool.Pool
}

// New returns a Store that uses pool. The schema must already exist; see Migrate.
func New(pool *pgxpool.Pool) *Store {
	return &Store{db: pool, pool: pool}
}

// Tx runs fn in a transaction: it commits if fn returns nil and rolls back otherwise. The error from
// fn is returned unchanged. A Tx call inside fn reuses the same transaction.
func (s *Store) Tx(ctx context.Context, fn func(store.Store) error) error {
	if _, inTx := s.db.(pgx.Tx); inTx {
		return fn(s)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit
	if err := fn(&Store{db: tx, pool: s.pool}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// write runs fn, which may fail on a constraint. Inside a transaction it runs in a savepoint, so a
// failure undoes only this statement.
func (s *Store) write(ctx context.Context, fn func(db querier) error) error {
	tx, inTx := s.db.(pgx.Tx)
	if !inTx {
		return fn(s.db)
	}
	sp, err := tx.Begin(ctx) // a pgx.Tx begun inside a transaction is a savepoint
	if err != nil {
		return fmt.Errorf("savepoint: %w", err)
	}
	if err := fn(sp); err != nil {
		_ = sp.Rollback(ctx)
		return err
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("release savepoint: %w", err)
	}
	return nil
}

// insert runs a prepared INSERT (see insertSQL) and maps a unique violation to errs.ErrAlreadyExists.
func (s *Store) insert(ctx context.Context, kind, id, sql string, args pgx.NamedArgs) error {
	return s.write(ctx, func(db querier) error {
		_, err := db.Exec(ctx, sql, args)
		return writeErr(err, "create", kind, id)
	})
}

// update runs a compare-and-swap UPDATE (see updateSQL) and returns the new version. If no row
// matches, it reports errs.ErrNotFound when the row is missing and errs.ErrConflict when it is stale.
func (s *Store) update(ctx context.Context, kind, table, id, sql string, args pgx.NamedArgs) (int64, error) {
	var version int64
	err := s.write(ctx, func(db querier) error {
		err := db.QueryRow(ctx, sql, args).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingOrStale(ctx, db, kind, table, id)
		}
		return writeErr(err, "update", kind, id)
	})
	return version, err
}

// missingOrStale explains an UPDATE that matched no row.
func missingOrStale(ctx context.Context, db querier, kind, table, id string) error {
	var exists bool
	if err := db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+table+" WHERE id = $1)", id).Scan(&exists); err != nil {
		return fmt.Errorf("update %s %s: %w", kind, id, err)
	}
	if !exists {
		return errs.NotFound("%s %s", kind, id)
	}
	return fmt.Errorf("%s %s: %w", kind, id, errs.ErrConflict)
}

// remove deletes a row by ID and reports errs.ErrNotFound when there was none.
func (s *Store) remove(ctx context.Context, kind, table, id string) error {
	tag, err := s.db.Exec(ctx, "DELETE FROM "+table+" WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("delete %s %s: %w", kind, id, err)
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("%s %s", kind, id)
	}
	return nil
}

// writeErr maps a failed insert or update. A unique violation becomes errs.ErrAlreadyExists.
func writeErr(err error, op, kind, id string) error {
	switch {
	case err == nil:
		return nil
	case pg.IsUniqueViolation(err):
		return errs.Exists("%s %s", kind, id)
	default:
		return fmt.Errorf("%s %s %s: %w", op, kind, id, err)
	}
}

// getOne runs a single-row query and maps pgx.ErrNoRows to errs.ErrNotFound. desc names the
// requested row in error messages.
func getOne[T any](ctx context.Context, db querier, kind, desc, sql string, scan func(scanner) (T, error), args ...any) (T, error) {
	var zero T
	x, err := scan(db.QueryRow(ctx, sql, args...))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return zero, errs.NotFound("%s %s", kind, desc)
	case err != nil:
		return zero, fmt.Errorf("get %s %s: %w", kind, desc, err)
	}
	return x, nil
}

// listRows runs a multi-row query and scans every row.
func listRows[T any](ctx context.Context, db querier, sql string, scan func(scanner) (T, error), args ...any) ([]T, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		x, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("list: scan: %w", err)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	return out, nil
}

// filter accumulates WHERE conditions with numbered placeholders. Values go only into args; the
// SQL text is built from constants and placeholders.
type filter struct {
	conds []string
	args  []any
}

// arg registers v as a query argument and returns its placeholder.
func (f *filter) arg(v any) string {
	f.args = append(f.args, v)
	return "$" + strconv.Itoa(len(f.args))
}

// add adds a condition whose values were registered with arg.
func (f *filter) add(cond string) { f.conds = append(f.conds, cond) }

// keyset restricts the result to IDs after the cursor. An empty cursor means the first page.
func (f *filter) keyset(after string) {
	if after != "" {
		f.add("id > " + f.arg(after))
	}
}

// selectWhere returns SELECT cols FROM table with the accumulated conditions, ordered by id and
// limited to limit rows when limit is positive, along with the arguments.
func (f *filter) selectWhere(cols []string, table string, limit int) (string, []any) {
	sql := selectSQL(table, cols)
	if len(f.conds) > 0 {
		sql += " WHERE " + strings.Join(f.conds, " AND ")
	}
	sql += " ORDER BY id"
	if limit > 0 {
		sql += " LIMIT " + f.arg(int64(limit))
	}
	return sql, f.args
}

// selectSQL returns "SELECT cols FROM table".
func selectSQL(table string, cols []string) string {
	return "SELECT " + strings.Join(cols, ", ") + " FROM " + table
}

// qualify prefixes every column with table, for use where names would be ambiguous.
func qualify(table string, cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = table + "." + c
	}
	return strings.Join(q, ", ")
}

// insertSQL returns an INSERT over cols with named parameters (@col), to be run with a pgx.NamedArgs.
func insertSQL(table string, cols []string) string {
	params := make([]string, len(cols))
	for i, c := range cols {
		params[i] = "@" + c
	}
	return "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(params, ", ") + ")"
}

// updateSQL returns "UPDATE table SET col = @col, ..., version = version + 1" for every column except
// id and version. The caller appends the WHERE clause, which must compare id and the expected version.
func updateSQL(table string, cols []string) string {
	var sets []string
	for _, c := range cols {
		if c != "id" && c != "version" {
			sets = append(sets, c+" = @"+c)
		}
	}
	sets = append(sets, "version = version + 1")
	return "UPDATE " + table + " SET " + strings.Join(sets, ", ")
}

// nullableTime maps the zero time to NULL.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// epoch is the lease_until value that stands for the zero time.
var epoch = time.Unix(0, 0).UTC()

// leaseValue maps the zero time to the epoch, the column's default.
func leaseValue(t time.Time) time.Time {
	if t.IsZero() {
		return epoch
	}
	return t.UTC()
}

// fromLease is the inverse of leaseValue.
func fromLease(t time.Time) time.Time {
	if t.Equal(epoch) {
		return time.Time{}
	}
	return t.UTC()
}

// Usage sums what a project consumes. CPUs and memory count only VMs whose desired state is running.
func (s *Store) Usage(ctx context.Context, project string) (domain.Usage, error) {
	const sql = `SELECT
		(SELECT COUNT(*) FROM vms WHERE project_id = $1),
		(SELECT COALESCE(SUM(cpus), 0) FROM vms WHERE project_id = $1 AND desired = $2),
		(SELECT COALESCE(SUM(memory_mb), 0)::bigint FROM vms WHERE project_id = $1 AND desired = $2),
		(SELECT COALESCE(SUM(size_gb), 0) FROM disks WHERE project_id = $1)`
	var vms, cpus, mem, disk int64
	if err := s.db.QueryRow(ctx, sql, project, string(domain.PowerRunning)).Scan(&vms, &cpus, &mem, &disk); err != nil {
		return domain.Usage{}, fmt.Errorf("usage %q: %w", project, err)
	}
	return domain.Usage{VMs: int(vms), CPUs: int(cpus), MemoryMB: mem, DiskGB: int(disk)}, nil
}
