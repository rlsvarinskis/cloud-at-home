// Package pg is the thin layer shared by the Postgres-backed stores: connecting and applying SQL
// migrations safely when several replicas start at the same time.
package pg

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open connects a pool and verifies the connection.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return pool, nil
}

var migrationName = regexp.MustCompile(`^(\d+)_.*\.up\.sql$`)

// Migrate applies, in numeric order, every dir/NNN_name.up.sql in fsys that has not been applied yet,
// each in its own transaction, recording versions in `table`. A Postgres advisory lock serialises
// concurrent callers, so any number of replicas may call it at start-up.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, dir, table string) error {
	if !regexp.MustCompile(`^[a-z_][a-z0-9_]*$`).MatchString(table) {
		return fmt.Errorf("migrate: bad table name %q", table)
	}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	type mig struct {
		version int
		file    string
	}
	var migs []mig
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, _ := strconv.Atoi(m[1])
		migs = append(migs, mig{v, e.Name()})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer conn.Release()
	const lockKey = 7243001 // arbitrary, same for every service; the table name scopes the actual work
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("migrate lock: %w", err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey) //nolint:errcheck

	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`, table)); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	applied := map[int]bool{}
	rows, err := conn.Query(ctx, fmt.Sprintf(`SELECT version FROM %s`, table))
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range migs {
		if applied[m.version] {
			continue
		}
		sql, err := fs.ReadFile(fsys, path.Join(dir, m.file))
		if err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", m.file, err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (version) VALUES ($1)`, table), m.version); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("migration %s: %w", m.file, err)
		}
	}
	return nil
}

// IsUniqueViolation reports whether err is a Postgres unique-constraint violation (SQLSTATE 23505).
func IsUniqueViolation(err error) bool {
	type coder interface{ SQLState() string }
	for err != nil {
		if c, ok := err.(coder); ok {
			return c.SQLState() == "23505"
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Placeholders returns "$1,$2,...,$n" starting at from (1-based), for building INSERTs.
func Placeholders(from, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "$" + strconv.Itoa(from+i)
	}
	return strings.Join(parts, ",")
}
