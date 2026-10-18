# Contributing to Attic Cloud

This is the repository-wide developer guide: the tools, the layout, the layering rule, the make targets, the coding
conventions, the test strategy and CI. Each component's README covers its own design, flags and tests. When you change
one component, start with its README:

- Contract and libraries: [api](api/README.md), [libs/common](libs/common/README.md), [libs/authz](libs/authz/README.md),
  [libs/qemu](libs/qemu/README.md), [libs/server](libs/server/README.md)
- Services: [services/vmd](services/vmd/README.md), [services/agentd](services/agentd/README.md),
  [services/mcp](services/mcp/README.md)
- Tools and tests: [cmd/atticctl](cmd/atticctl/README.md), [e2e](e2e/README.md), [deploy](deploy/README.md)

Architecture decisions are in [docs/adr](docs/adr/). Design proposals are in [docs/design](docs/design/).

## Ground rules for a public repository

This repository is public. Before every commit:

- Never commit secrets: keys, tokens, passwords, `.env` files, private certificates or API keys. `.gitignore` covers
  some of these, but it does not replace checking `git status` and `git diff --cached`.
- Never commit deployment details: real domain names, host names, private network addresses or home directory paths.
  Use the documentation address ranges 192.0.2.0/24, 198.51.100.0/24 and 203.0.113.0/24, and reserved names such as
  `example.com`, in examples and tests.
- Never commit compiled binaries. Build into a directory that is not tracked, or delete the output.
- Never commit session or handoff notes. `HANDOFF*.md` and `handoff*.md` are gitignored; keep such notes outside the
  repository.

## Prerequisites

| Tool | Version | Needed for |
| --- | --- | --- |
| Go | 1.26 (`go.work` says `go 1.26.0`) | Everything |
| GNU make, bash | Any recent | The Makefile and `scripts/check-boundaries.sh` |
| C compiler (gcc) | Any | `-race` builds, which need cgo |
| protoc | 33.5 | `make proto`. The generated headers record it as v6.33.5 |
| protoc-gen-go | v1.36.11 | `make proto` |
| protoc-gen-connect-go | v1.21.1 | `make proto` |
| mockgen | v0.6.0 | `make mocks`. Run with `go run`, so the module proxy is needed on first use |
| PostgreSQL | Optional | Postgres tests. Without `ATTIC_TEST_POSTGRES`, embedded Postgres is started; its binaries are downloaded on first use |
| QEMU/KVM, ISO tools | Optional | Real-driver tests. They skip when the tools are missing |
| Docker | Optional | Image builds. CI builds them |

Install the two plugins with:

```
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.21.1
```

Install protoc from the protobuf 33.5 release. The plugins must be on `PATH`, usually in `$HOME/go/bin`. The Makefile
adds that directory to its own `PATH`, but your shell needs it too for direct commands.

## Repository layout

The repository is a Go workspace (`go.work`) of separate modules. Each module has its own `go.mod`. There is no root
module, so `go test ./...` at the root does not work. Run commands inside a module, or use make.

| Path | Contents |
| --- | --- |
| `api/` | Protocol Buffers definitions (`api/proto`) and their committed generated Go code (`api/gen`) |
| `libs/common/` | Error sentinels (`errs`), IDs (`ids`), Postgres helpers (`pg`, `pg/pgtest`) |
| `libs/authz/` | Tokens, roles, principals, Connect interceptors |
| `libs/qemu/` | The node driver interface, the QEMU driver, the fake driver and the node API |
| `libs/server/` | HTTP server, daemon flags, RPC metrics, health endpoints |
| `services/vmd/` | VM service: control plane and node roles |
| `services/agentd/` | Agent service: tasks, model client, runner |
| `services/mcp/` | MCP server that exposes tools over vmd and agentd |
| `cmd/atticctl/` | Admin CLI: keys, tokens, roles |
| `e2e/` | End-to-end tests across the services |
| `deploy/` | Dockerfiles and the compose stack |
| `docs/` | `adr/` (decisions), `design/` (proposals), `assets/` |
| `scripts/` | `check-boundaries.sh` |

Migrations live with their store, under `services/<svc>/store/postgres/migrations/`.

## Layering rules

A module may import only the sibling modules in its row. `scripts/check-boundaries.sh` enforces this by scanning the Go
imports of each module, tests included. CI runs the script. Third-party and standard-library imports are not checked.

| Module | May import (sibling modules) |
| --- | --- |
| `api` | (none) |
| `libs/common` | (none) |
| `libs/authz` | `api`, `libs/common` |
| `libs/qemu` | `api`, `libs/common`, `libs/authz` |
| `libs/server` | `api`, `libs/common`, `libs/authz` |
| `services/vmd` | `api`, `libs/common`, `libs/authz`, `libs/qemu`, `libs/server` |
| `services/agentd` | `api`, `libs/common`, `libs/authz`, `libs/server` |
| `services/mcp` | `api`, `libs/common`, `libs/authz`, `libs/server` |
| `services/auth` | `api`, `libs/common`, `libs/authz`, `libs/server` (reserved) |
| `cmd/atticctl` | `api`, `libs/common`, `libs/authz` |
| `e2e` | `api`, `libs/common`, `libs/authz`, `libs/qemu`, `libs/server`, `services/vmd`, `services/agentd`, `services/mcp` |

`services/auth` is reserved for the planned identity service (see [docs/design/auth-graph.md](docs/design/auth-graph.md)).
It has no module yet, so the script skips it. Its row is kept so that the rule is already written when the module
appears. Do not add a sibling import that this table does not list. Change the table in the script and in this section
together, with review.

## Make targets

The module list is `MODULES` in the Makefile. Add a new module there.

| Target | What it does |
| --- | --- |
| `make proto` | Deletes `api/gen` and regenerates it from `api/proto` with protoc and the two plugins |
| `make mocks` | Regenerates the gomock mocks for the interfaces that tests script (listed in the Makefile) |
| `make build` | `go build ./...` in every module. Writes no output directory. A module with one main package can leave an executable in its directory, so delete it before committing |
| `make vet` | `go vet ./...` in every module |
| `make test` | `go test -race -count=1 ./...` in every module |
| `make cover` | `go test -race -cover ./...` in every module. Its output goes through `tail`, so its exit status does not report failures |
| `make tidy` | `go mod tidy` in every module |
| `make boundaries` | Runs `scripts/check-boundaries.sh` |

`build`, `vet`, `test` and `tidy` stop at the first failing module. To build the binaries, run from the repository root:
`go build -o bin/ ./services/vmd/cmd/vmd ./services/agentd/cmd/agentd ./services/mcp/cmd/mcp ./cmd/atticctl`. `bin/` is
gitignored.

## Coding conventions

- **Documentation.** Every package has a package comment. Every exported identifier has a doc comment that starts with
  its name.
- **Formatting.** `gofmt -l .` must print nothing. CI fails otherwise.
- **Errors.** Return the sentinels of `libs/common/errs` (`NotFound`, `Invalid`, `Precondition`, `Exists`, `Exhausted`,
  `Unavailable`, `ErrConflict` and the `Err*` values), wrapped with context. `errs.ToConnect` maps them to Connect codes
  at the edge, in `obs.ErrorInterceptor` (`libs/server/obs`). Any other error becomes `internal`, and its text is hidden
  from clients. Errors from downstream systems go through `errs.Sanitize`, so that clients never see an address. Client
  code uses `errs.FromConnect` to get the sentinels back.
- **Transport stays at the edge.** Handlers convert between wire types and domain types. Business logic does not import
  transport types and returns `errs` values instead.
- **Generated code.** Never edit `api/gen` or a mock by hand. Regenerate it.
- **Mocks and fakes.** Use gomock for call-level assertions ("this method was called with these arguments"). Use the
  hand-written fakes for state-based tests, such as `libs/qemu/fake` and `services/agentd/llm/fake.go`.
- **Concurrency on rows.** Rows carry a version that updates check. A lost race returns `errs.ErrConflict`. The caller
  reads again and retries, with a bounded number of attempts.
- **Background work.** Work is claimed with a lease. Whatever a lease holder does must be safe if another worker repeats
  it. Each reconcile step must be idempotent.
- **Time.** Services and drivers take their clock as an injected function, so that tests control time.
- **Logging.** Use `log/slog` with the component and the resource IDs as fields. Never log tokens or key material.
- **IDs.** Create IDs with `ids.New(prefix)`. They are time-ordered and opaque to clients.
- **Limits.** Put each size or count limit in a named constant next to the code that enforces it.

## Testing strategy

The tests are layered by what they need. None of them sleeps for real time, and the race detector is on by default.

1. **Unit tests.** Pure logic: the scheduler, argument builders, the RBAC matrix and the token rules.
2. **Store conformance.** `storetest.Run` runs one suite against each implementation. vmd runs it on the memory store,
   the Postgres store and the sharded store. agentd runs it on the memory and Postgres stores.
3. **Postgres tests.** `libs/common/pg/pgtest` gives each test a fresh database. `ATTIC_TEST_POSTGRES` selects the
   server: an admin DSN for an existing server, `skip` to skip the Postgres tests, or unset to start embedded Postgres.
   Embedded Postgres caches its binaries in `attic-pg-cache` under the system temporary directory.
4. **Service tests.** The real handler stack runs under `httptest`, with the fake node driver and a manual clock. Tests
   advance the clock instead of sleeping.
5. **Daemon tests.** The `cmd` packages start the real daemon `run` function and check its configuration handling.
6. **End to end.** `e2e/` runs vmd and agentd in one process and calls them over HTTP. Only the node and the model are
   fakes.

Commands:

```
make test                                              # everything, with embedded Postgres
ATTIC_TEST_POSTGRES=skip make test                     # everything, without Postgres
cd libs/authz && go test -race -count=1 -run TestRolePermissionMatrix .
cd services/vmd && ATTIC_TEST_SHARDS=3 go test -count=1 ./service   # service suite on three memory shards
cd e2e && go test -race -count=1 -run TestGoldenPath ./...
```

`ATTIC_TEST_SHARDS` is read by `services/vmd/service/testenv_test.go`. A value above 1 runs the whole service suite on
that many memory shards.

A passing test shows that the code does what the test says. It does not show that the test would notice a change. When
you change a rule (a permission, a state transition, a limit), break the rule on purpose once and check that a test
fails. This is a habit, not a CI step.

## CI

`.github/workflows/ci.yml` runs on every push and pull request. This section describes the file. It does not record a CI
run.

- **`build, vet, test`** (ubuntu-latest, Go 1.26, module cache enabled). Runs `make build`, `make vet`, a `gofmt -l .`
  check that fails if any file is listed, `make test`, and `./scripts/check-boundaries.sh`. It restores a cache of the
  embedded Postgres binaries, keyed on `go.work` and the `go.mod` files.
- **`protobuf generated code is up to date`.** Installs protoc 33.5 and the two plugins at the versions above, runs
  `make proto`, and fails if `git diff` shows a change in `api/gen` or if `api/gen` has new untracked files.
- **`docker build (vmd, agentd, mcp, node)`.** Builds `deploy/Dockerfile.<image>` with the repository as context and does not
  push. `deploy/Dockerfile.mcp` exists, but this matrix does not build it.

## Adding an RPC

1. **Define it.** Add the messages and the `rpc` line to the service in `api/proto/attic/<svc>/v1/*.proto`. Put the
   limits and waiting rules in the comments. A request names its project (`project_id`) or its resource (`id`), unless
   it is global.
2. **Regenerate.** Run `make proto`. Commit `api/gen` with the `.proto` change. Details are in
   [api/README.md](api/README.md#generating-the-code).
3. **Permission.** Reuse an existing permission if one fits. Otherwise see [Adding a permission or a role](#adding-a-permission-or-a-role).
4. **Handler.** Add the method to the service that owns the RPC. Validate the request, authorize before reading
   resources, check state preconditions, and write with a version check. The component README has the handler layout.
5. **Tests.** Cover at least: no token (`unauthenticated`), a caller with no binding on the project (`not_found` for IDs,
   `permission_denied` for lists), a lower role (`permission_denied`), invalid input, the happy path, and the state
   preconditions.
6. **Documentation.** Add the RPC to the service index in [api/README.md](api/README.md) and to the component README.
7. **Node work.** If the RPC needs a host, add it to `node.proto` and to the `node.Driver` interface in
   `libs/qemu/node/driver.go`. Implement it in every driver: `qemudriver` (real), `remote` (client), `fake` (tests) and
   the `nodeserver` handler. Then run `make mocks` and `make proto`.

## Adding a permission or a role

Permissions and roles are defined in `libs/authz/rbac.go`.

- **Permission.** Add a constant and add it to the role sets that should have it. Read-only permissions belong in
  `viewer`, which is the base of `editor`, which is the base of `admin`.
- **Role.** Add a constant and an entry in `rolePerms`. `atticctl` and the servers pick the new role up from the map.
- **Tests.** `TestRolePermissionMatrix` and `TestRolePermissionsAreListed` in `libs/authz/auth_test.go` pin the matrix.
  Update them with the change.
- **Docs.** Regenerate the role table with `cd cmd/atticctl && go run . roles -v`, and update
  [libs/authz/README.md](libs/authz/README.md#roles-and-permissions).

## Adding a store method and a migration

1. **Interface.** Add the method to the store interface: `services/vmd/store/store.go` or `services/agentd/store/store.go`.
2. **Implementations.** Implement it in the memory store and the Postgres store. vmd's shard router also implements the
   interface, and must route the method.
3. **Conformance.** Add cases to the store's `storetest` suite so that every implementation is checked.
4. **Mocks.** Run `make mocks`, since the store mock changes.
5. **Migration.** For a schema change, add `NNN_name.up.sql` to the store's `migrations/` directory, with the next
   number. The files are embedded in the binary. `pg.Migrate` applies them in numeric order, each in its own transaction,
   under a Postgres advisory lock, so several replicas may start at once. Never edit a migration that has shipped. Add a
   new one.
6. **Run.** For example, `cd services/vmd && go test -race -count=1 ./store/... ./service/`.

## Commit hygiene

- Before committing, check `git status` and `git diff --cached`. Look for secrets, real host names, home directory paths,
  binaries and notes.
- Run `make boundaries`, `make vet`, `gofmt -l .` and the tests of the modules you touched.
- When a `.proto` file changes, regenerate and commit `api/gen` in the same commit.
- Make one logical change per commit, and say why in the message.
- Never rewrite a migration that has shipped.
- `.gitignore` covers `bin/`, `*.test`, `.env`, `*.pem` (except under `testdata/`), the binaries of `cmd/atticctl`,
  `services/*/cmd/*` and `HANDOFF*.md`. Do not add exceptions to it for convenience.
