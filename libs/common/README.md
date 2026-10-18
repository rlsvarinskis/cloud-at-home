# libs/common

`libs/common` holds the Go packages every service shares: the error vocabulary (`errs`), identifier generation (`ids`),
the Postgres helpers (`pg`) and a per-test Postgres database (`pg/pgtest`). It has no transport, no handlers and no
business logic.

Module: `github.com/rlsvarinskis/cloud-at-home/libs/common`

## Role

- `errs`: sentinel errors shared by all layers; mapping to and from Connect codes; hiding internals from clients.
- `ids`: prefixed, time-ordered, collision-free identifiers without coordination, and sharded identifiers.
- `pg`: opening a pool, and a forward-only migration runner that is safe to run from several replicas at once.
- `pg/pgtest`: a fresh, empty Postgres database for each test, from an existing server or an embedded one.
- Does not know the domain (VMs, tasks) or the wire format of any service. Services return `errs` values and
  the edge converts them with `errs.ToConnect`.

## Position

```
            +---------- libs/common ------------+
services/* -| errs  ids  pg  pg/pgtest (tests) |- libs/authz, libs/server, libs/qemu
  e2e       +---------------------------------+
```

- Imported by every other module. It imports no sibling module (`scripts/check-boundaries.sh` allows none).
- External: `connectrpc.com/connect` (errs), `github.com/jackc/pgx/v5` (pg), `github.com/oklog/ulid/v2` (ids),
  `github.com/fergusstrange/embedded-postgres` (pgtest only).
- Only test code imports `pg/pgtest`. Importing it from production code would link the embedded server.

## Contract

### `errs`

```go
var (
    ErrNotFound, ErrAlreadyExists, ErrInvalidArgument, ErrFailedPrecondition, ErrPermissionDenied,
    ErrUnauthenticated, ErrResourceExhausted, ErrUnavailable, ErrConflict error
)
```

`ErrConflict` means optimistic concurrency failed.

Constructors. Each returns an error that wraps its sentinel, with the formatted text after a colon, so that
`errors.Is(err, errs.ErrNotFound)` holds:

| Function | Sentinel |
| --- | --- |
| `NotFound(format string, a ...any) error` | `ErrNotFound` |
| `Invalid(format string, a ...any) error` | `ErrInvalidArgument` |
| `Precondition(format string, a ...any) error` | `ErrFailedPrecondition` |
| `Exists(format string, a ...any) error` | `ErrAlreadyExists` |
| `Exhausted(format string, a ...any) error` | `ErrResourceExhausted` |
| `Unavailable(format string, a ...any) error` | `ErrUnavailable` |

Conversion functions:

| Function | Behaviour |
| --- | --- |
| `ToConnect(err error) error` | `nil` stays `nil`. An error that already holds a `*connect.Error` is returned as is. Otherwise the sentinel decides the Connect code (table below). An unknown error becomes `internal` with the message `internal error`, and its text is not sent. |
| `FromConnect(err error) error` | The inverse for clients. A Connect error with a code in the table gets the matching sentinel, as `"<sentinel>: <server message>"`. Other codes, and non-Connect errors, are returned unchanged. |
| `Sanitize(what string, err error) (clientErr error, hidden bool)` | For errors from downstream systems (a node, another service, the model backend). `nil` gives `nil, false`. Errors that mean something to the caller pass through with `hidden = false`: canceled, deadline exceeded, not found, invalid, precondition, already exists, permission denied, exhausted, conflict and unauthenticated. Any other error, including `ErrUnavailable`, becomes `Unavailable("%s is unavailable", what)` with `hidden = true`, so the caller can log the original. |

Mapping used by `ToConnect` (checked in this order):

| Condition | Connect code | HTTP status (Connect protocol) |
| --- | --- | --- |
| `ErrNotFound` | `not_found` | 404 |
| `ErrAlreadyExists` | `already_exists` | 409 |
| `ErrInvalidArgument` | `invalid_argument` | 400 |
| `ErrFailedPrecondition` | `failed_precondition` | 400 |
| `ErrPermissionDenied` | `permission_denied` | 403 |
| `ErrUnauthenticated` | `unauthenticated` | 401 |
| `ErrResourceExhausted` | `resource_exhausted` | 429 |
| `ErrUnavailable` | `unavailable` | 503 |
| `ErrConflict` | `aborted` | 409 |
| `context.DeadlineExceeded` | `deadline_exceeded` | 504 |
| `context.Canceled` | `canceled` | 499 |
| anything else | `internal` (message `internal error`) | 500 |

`FromConnect` maps the same codes back, except `deadline_exceeded` and `canceled`, which stay as Connect errors.

### `ids`

```go
func New(prefix string) string                   // "vm_01hx...": prefix, "_", lowercase ULID (26 characters)
// NewSharded: New(prefix) + "-s" + 2 lowercase hex digits; panics if shard is outside 0..255
func NewSharded(prefix string, shard int) string
func ShardOf(id string) (shard int, ok bool)     // reads the "-sNN" suffix; ok is false if there is none
```

- `New` is safe for concurrent use. IDs from one process are strictly increasing (a monotonic ULID entropy source
  under a mutex). IDs sort by creation time.
- A sharded ID still matches `^[a-zA-Z0-9_-]{1,64}$`, and it still sorts by creation time because the suffix is last.
- `ShardOf` accepts only lowercase hex digits. Upper-case or malformed suffixes give `ok = false`.

The prefixes used by the services are listed in [api/README.md](../../api/README.md#identifiers).

### `pg`

```go
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error)
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, dir, table string) error
func IsUniqueViolation(err error) bool
func Placeholders(from, n int) string
```

- `Open` creates a pool and pings it. If the ping fails the pool is closed and an error wrapping `postgres ping:` is
  returned.
- `Migrate` applies every file in `dir` named `NNN_<name>.up.sql` that has no row in `table`, in numeric order. Files
  that do not match the pattern (for example `*.down.sql`) are ignored.
  - `table` must match `^[a-z_][a-z0-9_]*$`. It is put into SQL text, so it is checked first.
  - The whole run holds `pg_advisory_lock(7243001)` on one connection. Replicas that start together queue behind it; the
    first one applies the migrations and the others find them applied. The lock key is the same for every service, but
    each service has its own `table`, so the records do not collide.
  - The `table` is created if it is missing (`version int primary key, applied_at timestamptz`).
  - Each migration runs in its own transaction together with its row insert. A failing migration is rolled back and the
    error names the file: `migration NNN_name.up.sql: ...`. Earlier migrations stay applied.
  - Migrations are forward only. Records are keyed by version number. Two files with the same number cannot both be
    applied: the second one fails on its version insert.
- `IsUniqueViolation` reports SQLSTATE `23505` anywhere in the error chain.
- `Placeholders(2, 3)` returns `"$2,$3,$4"`, for building multi-row `INSERT`s.

The services call it from their `Migrate` functions with their own table: `schema_migrations_vmd` for vmd and
`schema_migrations_agentd` for agentd, each with its embedded `migrations/` directory.

### `pg/pgtest`

```go
func Run(m *testing.M) int    // call from TestMain: func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }
func DSN(t testing.TB) string // a DSN of a new, empty database, dropped when the test ends
```

- `DSN` creates a database named `t_<pid>_<n>` on the server and returns a DSN to it. The database is removed at test
  cleanup with `DROP DATABASE ... WITH (FORCE)`.
- Where the server comes from:
  1. `ATTIC_TEST_POSTGRES` set to an admin DSN: that server is used. The user must be allowed to `CREATE DATABASE`.
  2. Otherwise a real Postgres is started once per test process by `embedded-postgres`. It listens on a free port on
     `127.0.0.1` with superuser `postgres` and password `postgres`, and it stores its data in a temporary directory
     that `Run` removes. Its binaries are downloaded on first use into `attic-pg-cache` under the system temp directory
     (`$TMPDIR`) and reused by later runs. Start-up times out after 90 s.
- `ATTIC_TEST_POSTGRES=skip` makes `DSN` skip the test. If the embedded server cannot start, the test is skipped with
  the reason.
- A package that calls `DSN` must have the `TestMain` shown above. Without `ATTIC_TEST_POSTGRES` set, `DSN` fails the
  test instead of starting a server that nothing would stop.

## Behaviour and guarantees

- `ids.New` gives unique IDs across goroutines. Across processes, IDs are ordered only to the millisecond.
- `errs` values are plain Go errors. They carry no transport state until `ToConnect` is applied at the edge.
- `pg.Migrate` does not run a migration again once its row is committed, and within one run it applies files in version
  order.

## Configuration

| Name | Where | Meaning |
| --- | --- | --- |
| `ATTIC_TEST_POSTGRES` | `pgtest` | Admin DSN of an existing server, or `skip`. Unset means embedded Postgres. |
| `TMPDIR` | `pgtest` | Parent of the binary cache (`attic-pg-cache`) and of the temporary data directory. |

Nothing else is configurable. Migration directories and table names are arguments.

## Data

| Object | Created by | Contents |
| --- | --- | --- |
| `schema_migrations_<service>` | `pg.Migrate` | One row per applied migration: `version`, `applied_at`. |

`pg` creates no other tables.

## Observability

None. Errors are returned to the caller; `pg.Migrate` wraps them with the file name.

## Failure modes and recovery

| Situation | Effect | Action |
| --- | --- | --- |
| Postgres unreachable at start-up | `Open` or `Migrate` returns an error; the service does not start. | Restore the database and restart. |
| A migration fails | Its transaction is rolled back, earlier migrations stay applied, the process stops. | Fix the SQL in a new migration file. Never edit a shipped one. |
| Another replica is migrating | The second caller blocks on the advisory lock until the first finishes. | None. The wait ends only with the context. |
| The embedded server cannot download its binaries | `DSN` skips the test with the error. | Set `ATTIC_TEST_POSTGRES` to a server you run, or allow the download. |

## Running and testing

```
cd libs/common && go test -race -count=1 ./...                     # pg tests use embedded Postgres
cd libs/common && ATTIC_TEST_POSTGRES=skip go test -count=1 ./...  # no Postgres needed
cd libs/common && ATTIC_TEST_POSTGRES='postgres://USER:PASSWORD@127.0.0.1:5432/postgres?sslmode=disable' \
  go test -count=1 ./pg/...
```

The tests cover: `errs` round trips, and hidden internal and infrastructure errors (`errs_test.go`); `ids` format,
uniqueness and sort order, and sharded suffixes (`ids_test.go`, `sharded_test.go`); and in `pg_test.go`, migrations
applied once and in order under concurrent callers, and rollback of a bad migration.

## Layout

```
libs/common/
  errs/errs.go          sentinels, constructors, ToConnect, FromConnect, Sanitize
  ids/ids.go            New (prefix + ULID)
  ids/sharded.go        NewSharded, ShardOf
  pg/pg.go              Open, Migrate, IsUniqueViolation, Placeholders
  pg/pgtest/pgtest.go   Run, DSN; embedded or external Postgres for tests
  */*_test.go           tests
```

## Related

- [api/README.md](../../api/README.md): the wire conventions, identifiers and error codes that `errs` maps to.
- [libs/authz/README.md](../authz/README.md): returns `errs.ErrUnauthenticated` and `errs.ErrPermissionDenied`.
- [libs/server/README.md](../server/README.md): the HTTP server and error interceptor that call `errs.ToConnect`.
- [services/vmd/README.md](../../services/vmd/README.md) and
  [services/agentd/README.md](../../services/agentd/README.md):
  the Postgres stores that use `pg`, and their migrations.
- [e2e/README.md](../../e2e/README.md): runs the Postgres scenarios through `pgtest`.
