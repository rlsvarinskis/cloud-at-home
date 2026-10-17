package postgres

import (
	"context"
	"embed"

	"github.com/jackc/pgx/v5/pgxpool"

	"example.com/homecloud/internal/pg"
)

//go:embed migrations
var migrations embed.FS

// migrationsTable records which migrations have been applied. It is separate from other services'
// tables so several services can share one database.
const migrationsTable = "schema_migrations_vmd"

// Migrate brings the schema up to date. It is safe to call from several replicas at once and does
// nothing when the schema is current.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return pg.Migrate(ctx, pool, migrations, "migrations", migrationsTable)
}
