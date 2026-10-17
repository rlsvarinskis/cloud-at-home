# Development

This document is for people changing homecloud's code: the layout, the generated code, the test strategy, how
to add an RPC, a permission or a store method, the coding conventions, and the known limitations. The design is in
[ARCHITECTURE.md](ARCHITECTURE.md); the decisions are in [adr/](adr/).

## Contents

- [Repository layout](#repository-layout)
- [Generated code](#generated-code)
- [Build, vet and test](#build-vet-and-test)
- [Testing strategy](#testing-strategy)
- [Adding an RPC](#adding-an-rpc)
- [Adding a permission or a role](#adding-a-permission-or-a-role)
- [Adding a store method](#adding-a-store-method)
- [Coding conventions](#coding-conventions)
- [Known limitations](#known-limitations)

## Repository layout

| Path | Contents |
| --- | --- |
| `proto/homecloud/{vmd,agentd,node}/v1/*.proto` | The API definitions. The source of truth for the wire format. |
| `gen/` | Generated Go code for `proto/`. Committed. Never edit by hand. |
| `cmd/vmd` | The vmd binary: flags, wiring of stores (one or several Postgres databases), drivers, the reconciler and the HTTP server. |
| `cmd/agentd` | The agentd binary: flags, Postgres or memory store, service tokens, runner and HTTP server. |
| `cmd/hcctl` | The admin CLI: keys, tokens, roles. |
| `internal/auth` | Tokens (`token.go`), keys, principals, RBAC (`rbac.go`), revocation list (`revocations.go`), Connect interceptors (`connect.go`). |
| `internal/errs` | Error sentinels and the mapping to Connect codes. `Sanitize` turns downstream failures into `unavailable` without addresses. |
| `internal/daemon` | Trusted-key and revocation flags, logger, HTTP server with graceful shutdown. |
| `internal/obs` | RPC metrics, `/metrics`, health checks, error interceptor. |
| `internal/ids` | Prefixed ULID IDs, and shard-suffixed IDs (`sharded.go`). |
| `internal/pg` | Postgres pool, migration runner (advisory lock), `pgtest` for tests. |
| `internal/vmd/domain` | vmd's model, separate from the wire types. |
| `internal/vmd/store` | `store.go` (interface), `memory.go`, `postgres/` (with `migrations/`), `shard/`, `storetest/` (conformance suite), `mock/`. |
| `internal/vmd/service` | Connect handlers (`vm.go`, `disk.go`, `image.go`, `exec.go`, `metrics.go`, `cluster.go`, `server.go`), the reconciler, the node registry. |
| `internal/vmd/scheduler` | Pure capacity arithmetic (`Fits`, `Avail`, `Pick`). |
| `internal/vmd/node` | `driver.go` (the node interface), `qemudriver/`, `qemu/`, `guestexec/`, `guestmetrics/`, `sshrunner/`, `remote/`, `nodeserver/`, `fake/`, `mock/`. |
| `internal/vmd/promcollect` | Prometheus collector for vmd state: capacity, VM counts, guest samples, reconciler counters. |
| `internal/agentd/metrics` | agentd's task and step counters and task-duration histogram. |
| `internal/e2e` | End-to-end tests: vmd and agentd in one process, talking over real HTTP. |
| `internal/agentd/domain` | agentd's model. |
| `internal/agentd/store` | Store interface, `memory.go`, `postgres/` (with `migrations/`), `storetest/`, `mock/`. |
| `internal/agentd/llm` | The model client interface, the Ollama client, and `fake.go` for tests. |
| `internal/agentd/vmclient` | The only way agentd calls vmd. |
| `internal/agentd/runner` | Task workers: leases, the conversation loop, commands, sanitising. |
| `internal/agentd/service` | agentd's Connect handlers. |
| `deploy/` | Dockerfiles for vmd, agentd and node; docker-compose stack. |

Migrations live with their store: `internal/vmd/store/postgres/migrations/` and
`internal/agentd/store/postgres/migrations/`. Never edit a migration that has shipped.

## Generated code

### Protobuf and Connect (`make proto`)

```
make proto
```

The target deletes `gen/` and regenerates it with `protoc -I proto -I /usr/include` and the plugins
`protoc-gen-go` and `protoc-gen-connect-go`, writing next to the sources (`paths=source_relative`).

Versions. CI installs `protoc` 33.5 (the generated headers record `protoc v6.33.5`), `protoc-gen-go` v1.36.11 and
`protoc-gen-connect-go` v1.21.1. The runtime library versions in `go.mod` are `google.golang.org/protobuf` v1.36.12
and `connectrpc.com/connect` v1.21.1. Install the same tool versions locally, or the regenerated code will differ
from what CI expects.

The CI job `protobuf generated code is up to date` runs `make proto` and fails if the working tree changes.
Commit the regenerated `gen/` with every `.proto` change.

### Mocks (`make mocks`)

```
make mocks
```

Runs `mockgen` v0.6.0 (pinned in the Makefile) in source mode for these interfaces:

| Interface | Mock |
| --- | --- |
| `internal/vmd/store/store.go` | `internal/vmd/store/mock/mock_store.go` |
| `internal/agentd/store/store.go` | `internal/agentd/store/mock/mock_store.go` |
| `internal/vmd/node/driver.go` | `internal/vmd/node/mock/mock_driver.go` |
| `internal/vmd/service/nodes.go` | `internal/vmd/service/servicemock/mock_nodes.go` |
| `internal/agentd/llm/llm.go` | `internal/agentd/llm/mock/mock_llm.go` |
| `internal/agentd/vmclient/vmclient.go` | `internal/agentd/vmclient/mock/mock_vmclient.go` |

Regenerate after changing one of these interfaces. Mocks are for call-level assertions ("this method was called
with these arguments"). State-based tests use the hand-written fakes instead.

## Build, vet and test

| Command | What it does |
| --- | --- |
| `make build` | `go build ./...` (compiles everything; writes no binaries). |
| `make vet` | `go vet ./...`. |
| `make test` | `go test -race -count=1 ./...`. |
| `make cover` | Coverage for all packages, with a summary line. |
| `make tidy` | `go mod tidy`. |

To build binaries, pass an output directory: `go build -o bin/ ./cmd/...`.

CI runs `go build`, `go vet`, `gofmt -l` (fails on unformatted files), `go test -race -count=1 ./...`, the
protobuf check, and a Docker build of the `vmd`, `agentd` and `node` images. Run `gofmt -l .` before committing.

## Testing strategy

Tests are layered by what they need. Each layer runs fast, and none sleeps for real time.

**Pure logic.** Unit tests for the scheduler (`scheduler_test.go`), the qemu argument builder (`qemu/args_test.go`),
the guest job scripts (`guestexec_test.go`), the firewall rules (`firewall_test.go`), the RBAC matrix and tokens
(`auth_test.go`, including `TestRolePermissionMatrix` and `TestRolePermissionsAreListed`), the LLM clients
(`llm/*_test.go`).

**Store conformance.** One suite, `storetest.Run(t, newStore)`, runs against every implementation:

- vmd: `internal/vmd/store/storetest/storetest.go`, run by `TestMemory` (`store/memory_test.go`),
  `TestPostgres` (`store/postgres/postgres_test.go`) and the shard router: `TestConformanceOneShard` and
  `TestConformanceThreeShards` (`store/shard/shard_test.go`). The suite also checks that the store never shares
  memory with its callers, that concurrent writers on one version lead to exactly one winner, and that the
  operator's drain decision survives health updates.
- agentd: `internal/agentd/store/storetest/storetest.go`, run by the memory and Postgres tests.

Postgres tests use `internal/pg/pgtest`, which gives each test a fresh database. Without configuration a real
Postgres is started once per test process with `embedded-postgres`. Its binaries are downloaded on first use and
cached. Set `HOMECLOUD_TEST_POSTGRES` to an admin DSN to use an existing server, or to `skip` to skip these tests.

**Service tests.** `internal/vmd/service/testenv_test.go` builds the real vmd HTTP surface with `httptest`, using:

- the real token verifier and real signed tokens, one per role (`e.admin()`, `e.editor()`, `e.viewer()`,
  `e.otherTenant()`, `e.root()`, `e.operator()`, `e.service()`);
- the in-memory store, or the sharded store;
- the fake node driver (`internal/vmd/node/fake`), which holds images, disks, VMs and guest files in memory and
  scripts failures with `Crash`, `SetDown`, `FailNext` and `SetMetrics`, and records calls (`Calls`);
- a manual clock shared by the service, the reconciler and the fake node (`clock.Advance`). Tests advance time
  instead of sleeping.

The reconciler is driven one tick at a time with `e.tick(n)`. `e.until(max, what, cond)` ticks until a condition
holds or fails the test. Tests cover the authorization matrix (not-found versus permission-denied), validation,
the reconciler's backoff and crash loops, drains (`drain_test.go`), idempotency, and the job lifecycle.

To run the whole vmd service suite on a sharded store, set `HOMECLOUD_TEST_SHARDS`:

```
HOMECLOUD_TEST_SHARDS=3 go test ./internal/vmd/service
```

**End to end.** `internal/e2e` runs vmd and agentd in one process and lets them talk over their real HTTP APIs. It
uses the fake node driver and the scripted LLM, on both the memory and Postgres stores. `TestShardedVMD` runs the
golden path on a project-sharded vmd store. The scenarios cover authorization across the two services, holds,
cancel, idempotency, failure handling, metrics, and several agents sharing one VM.

**Daemon tests.** `cmd/vmd` and `cmd/agentd` have tests that start the real daemon (its `run` function) against
Postgres. They cover the all-in-one role, refusal of bad configuration, several databases with project sharding,
the quota flags, and `-revoked-tokens` (a token stops working once its id is added to the file).

**Driver tests.** `qemudriver/driver_test.go` and `fakes_test.go` run the QEMU driver against fake commanders,
process tables and filesystems (`FS`, `Procs`, `Cmd` in the driver config), without starting QEMU.

**agentd runner and service tests.** `internal/agentd/service/testenv_test.go` runs agentd's handlers with a fake
vmd client and the scripted fake LLM (`llm.Fake`: `Say` and `CallTool` build replies, and `Script` holds the
replies `Chat` hands out in order). The runner tests use the same fakes.

### Running one package

```
go test -race -count=1 -run TestRolePermissionMatrix ./internal/auth/
HOMECLOUD_TEST_POSTGRES=skip go test -count=1 ./internal/vmd/service/
```

### Checking that tests catch a broken rule

A passing test shows that the code does what the test says, not that the test would notice a change. When you change
a rule (a permission, a state transition, a limit), break the rule on purpose once and check that a test fails. This
is a habit to keep, not a CI step.

## Adding an RPC

1. **Define it.** Add the message types and the `rpc` line to the service in `proto/homecloud/<svc>/v1/*.proto`.
   Put the limits and waiting rules in the comments, as the existing messages do. Each request names its project
   (`project_id`) or resource (`id`), or it is global (as `ClusterService` is).
2. **Regenerate.** `make proto`. Commit `gen/`.
3. **Permission.** Use an existing permission if one fits. Otherwise add a constant to
   `internal/auth/rbac.go` and grant it in the role sets (see
   [Adding a permission or a role](#adding-a-permission-or-a-role)).
4. **Handler.** Add a method on the service's server type (`VMServer`, `DiskServer`, `ClusterServer`, ...). Follow the
   order used by the existing handlers:
   1. validate the request shape (`validProject`, `validName`, size and range checks);
   2. authorize: `auth.Require(ctx, perm, project)` when the request names a project, or
      `auth.RequireResource(ctx, perm, resource.ProjectID, kind, id)` after loading the resource by ID;
   3. check preconditions on state (`errs.Precondition`);
   4. write, with compare-and-swap for updates (see [Coding conventions](#coding-conventions)).
   Long waits go through `poll(ctx, waitSeconds, check)`, which clamps to 30 s.
5. **Tests.** Add a `*_test.go` in the same package using `newEnv` from `testenv_test.go`. Cover at least: no token
   (`unauthenticated`), a caller with no binding on the project (`not_found` for IDs, `permission_denied` for lists),
   a caller with a lower role (`permission_denied`), validation, the happy path, and the state preconditions.
6. **Documentation.** Add the RPC to [API.md](API.md), with its permission, errors and limits. If the permission
   or role table changed, regenerate it with `go run ./cmd/hcctl roles -v` and update [AUTH.md](AUTH.md).
7. **Node work, if the RPC needs a host.** Add the call to `node.proto`, to the `node.Driver` interface in
   `internal/vmd/node/driver.go`, and implement it in every driver: `qemudriver` (real), `remote` (client to a node),
   `fake` (tests), and the `nodeserver` handler that serves the node API. Then run `make mocks` and `make proto`.

## Adding a permission or a role

Permissions and roles live in `internal/auth/rbac.go`.

- **Permission.** Add a constant, such as `VMSnapshot Permission = "vm.snapshot"`, and add it to the role
  sets that should have it. `viewer` is the base of `editor`, which is the base of `admin`. Put read-only
  permissions in `viewer`.
- **Role.** Add a constant and an entry in `rolePerms`. `Roles()` and `KnownRole` follow the map, so
  `hcctl token mint -bind` and `-trusted-key` accept the new role without other changes.
- **Global permissions.** Check them with `auth.Require(ctx, perm, "")`. Only a binding on project `*` satisfies
  them. Shared resources (for example shared images) use `auth.RequireAnyProject` for read permissions.
- **Tests.** `TestRolePermissionMatrix` and `TestRolePermissionsAreListed` in `internal/auth/auth_test.go` pin the
  matrix. Update them with the change.
- **Docs.** Regenerate the table in [AUTH.md](AUTH.md) with `go run ./cmd/hcctl roles -v`.

## Adding a store method

The stores are vmd's `store.Store` interface (`internal/vmd/store/store.go`) and agentd's (`internal/agentd/store/store.go`).
The steps below are for vmd. agentd is the same shape.

1. **Interface.** Add the method to `store.Store`.
2. **Memory.** Implement it in `internal/vmd/store/memory.go`, under the store's lock. Copy values in and out, so
   the caller cannot change stored data through a pointer it kept.
3. **Postgres.** Implement it in the file for its entity under `internal/vmd/store/postgres/` (`vms.go`,
   `disks.go`, `images.go`, `jobs.go`, `nodes.go`). Updates use the row's `version` column in the `WHERE` clause and
   return `errs.ErrConflict` when no row matched. For a new column, add a new migration file
   `internal/vmd/store/postgres/migrations/NNN_name.up.sql`. Never edit a migration that has shipped. The runner
   applies only `*.up.sql` files, in numeric order.
4. **Shard.** Implement it in `internal/vmd/store/shard/shard.go`. The shard `Store` must implement every
   method (`var _ store.Store = (*Store)(nil)` enforces this at compile time). Choose the routing: by project
   (`ForProject`), by ID (`find`, `mutate`), or fan out and merge across shards (`fanOut`, `mergeByID`) for lists that
   span projects.
5. **Conformance.** Add cases to `internal/vmd/store/storetest/storetest.go`. They run against the memory, Postgres and
   shard stores. Add shard-specific cases in `shard_test.go` where routing matters.
6. **Mocks.** `make mocks`, since the mock of `store.Store` changes.
7. **Run.** `go test -race -count=1 ./internal/vmd/store/... ./internal/vmd/service/`.

## Coding conventions

These are visible in the code and are relied on by the tests.

- **Errors.** Business code returns the sentinels in `internal/errs` (`errs.NotFound`, `errs.Invalid`,
  `errs.Precondition`, `errs.Exists`, `errs.Exhausted`, `errs.Unavailable`, `errs.ErrConflict`), wrapped with context.
  `errs.ToConnect` maps them to Connect codes at the edge (`obs.ErrorInterceptor`). Any other error becomes `internal`,
  with its text hidden from the client and logged with the caller and procedure. Callers of other services use
  `errs.FromConnect` to get the sentinels back. Errors from nodes and other services go through `errs.Sanitize`, so
  that clients never see an address.
- **Transport types stay in handlers.** The Connect handler methods (the `*Server` types, in `vm.go`, `disk.go`,
  `image.go`, `exec.go`, `metrics.go`, `cluster.go`, `server.go`) are the only vmd files that import `connectrpc.com/connect`.
  `reconciler.go`, `nodes.go`, the stores, the scheduler and the domain do not. The same holds in agentd's
  `service` package for its handlers. The runner does not use transport types either.
- **Compare-and-swap everywhere.** Every row has a `Version` that the store checks on update. Read-modify-write
  loops retry on `ErrConflict`: `mutateVM` tries 8 times, `mutateAgent` and `mutateTask` try `maxCASAttempts` (8)
  times, and the task runner's `fenced` writes check the lease in the same transaction.
- **Idempotent reconcile steps.** Each step can run again after a crash. Reconciler steps treat "already exists"
  as success (`createImage`, `CreateDisk`), and "not found" as done when deleting (`DeleteImage`, `DeleteDisk`, `ForgetVM`).
  The reconciler classifies errors with `transient(err)`: transient errors are retried with backoff, others move the
  object to its error state.
- **Leases, not leaders.** Background work is claimed with a lease (`ClaimVMs`, `ClaimTasks`). Anything a lease owner
  does must be safe if another worker repeats it.
- **IDs.** `ids.New(prefix)` gives a lowercase ULID with a resource prefix (`vm_`, `disk_`, `img_`, `job_`, `agent_`,
  `task_`, `tok_`). IDs sort by creation time. Shard-aware IDs (`ids.NewSharded`) carry their shard as a `-sNN` suffix.
- **Names and paths.** Resource names match `^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`. Paths sent to nodes are absolute and
  clean. IDs used as file names on nodes match `^[a-zA-Z0-9_-]{1,64}$` (`qemudriver.checkID`).
- **NUL bytes.** Rejected in commands, paths and prompts. Output from guests is cleaned before it is stored
  (agentd's `runner.Clean` removes NUL bytes and replaces invalid UTF-8), because Postgres `TEXT` cannot hold NUL.
- **Injectable time.** Services, reconcilers, drivers and tokens take a `Now` function, so tests control time.
- **Logging.** `log/slog` with `component` and resource IDs as fields. Tokens and key material are never logged.
- **Limits in one place.** Size and count limits are constants next to the code that enforces them (`exec.go`,
  `service/config.go`, `agentd/service/config.go`, `agentd/runner/sanitize.go`).

## Known limitations

These follow from the code as it is written. Each is a candidate for future work. The design-level limitations are
listed in [ARCHITECTURE.md](ARCHITECTURE.md#known-limitations).

- **No live resize.** `UpdateVM` changes CPUs and memory only while the VM is stopped and its desired state is
  stopped. A disk can grow only, and only while detached or its VM is stopped.
- **Shared-image deletion race.** `DeleteImage` counts the disks that use the image, and then marks the image
  `DELETING` in a separate write. `CreateDisk` checks that the image is `READY` in a separate step as well. A disk
  created between those steps can refer to an image that is then deleted. The reconciler removes the image file
  without checking again.
- **Quotas are not checked on VM resize.** `UpdateVM` does not call the quota check; `StartVM` does.
- **Placement is by image or disk, not by the scheduler.** `scheduler.Pick` exists but is not called. Capacity is
  checked when a VM starts (`fits`), not when it is created.
- **Shard layout depends on order.** The rendezvous weight uses each database's position in the `--postgres-dsn`
  list. Reordering the flags re-routes projects. There is no migration tool.
- **Crash loops are not logged at `ERROR`.** A VM in a crash loop becomes `ERROR` with a reason and is retried with
  backoff. The reconciler writes no `ERROR` log line for it. Alert on the state.
- **Database errors are `internal`.** Postgres failures reach clients as `internal`, not `unavailable`, so retry
  logic cannot tell them apart from bugs.
- **Node-unreachable prefix is broad.** Any error classed as unavailable, including a missing host tool such as
  `genisoimage`, is reported in `state_reason` as `node unreachable: <node id>`. The tool's error is only in the log.
- **Running jobs are marked `LOST` lazily.** Nothing sweeps them when a VM stops. The mark happens when `PollJob` or
  `CancelJob` reads them. `Store.ListRunningJobs` exists but no service calls it.
- **Job output is guest-side.** vmd keeps no output. Output is readable only while the guest keeps the job directory
  (the 50 most recent finished jobs). Agents store excerpts of output (the first and last 2000 bytes of each step, and
  results up to 32 KiB).
- **Single node per VM, no migration.** A VM and its disks live on one node. Nothing moves them.
- **Guest-side scripts assume a Linux guest with the listed tools.** See the guest requirements in
  [OPERATIONS.md](OPERATIONS.md#guest-image).
- **Agent `ORPHANED` is set lazily.** It is set when a task finds that its VM is gone, not when the VM is force-deleted.
  Deleting a held VM with `force` does not change the agent until its next task runs.
