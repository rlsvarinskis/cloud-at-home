# libs/common

`libs/common` holds small helpers that every other module uses: a shared error vocabulary, sortable resource IDs,
and the Postgres plumbing shared by the stores (connecting, applying migrations, and a test database).

Module path: `github.com/rlsvarinskis/cloud-at-home/libs/common`.

## Packages

- `errs`: the error kinds services return (`NotFound`, `Invalid`, `Exists`, `Exhausted`, `Precondition`,
  `Unavailable`, ...), plus `ToConnect` and `FromConnect` to map them to and from Connect status codes at the edge.
  `Sanitize` hides details of downstream errors from API clients.
- `ids`: `New(prefix)` creates ULID-based IDs with a prefix, and `NewSharded(prefix, shard)` embeds a shard suffix
  (for example `vm_<ulid>-s02`). `ShardOf` reads that suffix back.
- `pg`: `Open` creates a pgx connection pool, `Migrate` applies SQL migrations from an `fs.FS` under a Postgres advisory
  lock (so several replicas can start at once), `Placeholders` builds `$n` lists and `IsUniqueViolation` detects
  duplicate keys.
- `pg/pgtest`: gives tests a fresh, empty database. It uses the server named by `ATTIC_TEST_POSTGRES`, or starts
  an embedded Postgres once per test process. `ATTIC_TEST_POSTGRES=skip` skips Postgres tests.

## Public entry points

- `errs.NotFound`, `errs.Invalid`, `errs.Exists`, `errs.Exhausted`, `errs.Precondition`, `errs.Unavailable` (wrap
  these with context), `errs.ToConnect`, `errs.FromConnect`, `errs.Sanitize`.
- `ids.New`, `ids.NewSharded`, `ids.ShardOf`.
- `pg.Open(ctx, dsn)`, `pg.Migrate(ctx, pool, fsys, dir, table)`, `pg.Placeholders`, `pg.IsUniqueViolation`.
- `pgtest.DSN(t)` and `pgtest.Run(m)` (call `Run` from `TestMain`).

## Dependencies

- Depends on no other module in this repository. External requirements: `connectrpc.com/connect` (for `errs`),
  `pgx/v5`, `oklog/ulid/v2`, and for tests `embedded-postgres`.
- Must NOT depend on `api`, `libs/authz`, `libs/qemu`, `libs/server` or any service. Everything else depends on it,
  so it must stay at the bottom of the layering.
- Note that `go.mod` has `replace` directives for `api`, `libs/authz`, `libs/qemu` and `libs/server`. They are not
  imports and do not create a dependency, but they are a trap if someone adds a real import.

## Build and test

```
cd libs/common && go build ./...
cd libs/common && go test -race ./...
```

The `pg` and `pgtest` tests need either `ATTIC_TEST_POSTGRES` (an admin DSN for a running server) or the
ability to download and run the embedded Postgres binaries on first use. Without either, set
`ATTIC_TEST_POSTGRES=skip`.

## Notes and limitations

- `errs` is the only package in this module that imports the Connect transport. Other packages should return `errs`
  kinds and let the edge convert them.
- `pg.Migrate` checks the table name with a regular expression (`^[a-z_][a-z0-9_]*$`) because it is put into SQL
  text.
- `pgtest.Run` is for test binaries only. It starts a database for the whole test process.
