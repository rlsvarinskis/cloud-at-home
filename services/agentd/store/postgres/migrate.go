package postgres

import (
	"context"
	"embed"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/pg"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate applies the embedded schema migrations to the database behind pool. It is safe to call from
// several replicas at once and is a no-op once every migration has been applied.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return pg.Migrate(ctx, pool, migrationFS, "migrations", "schema_migrations_agentd")
}
