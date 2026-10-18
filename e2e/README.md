# e2e

`e2e` runs vmd, agentd and the MCP server in one test process and drives them only through their HTTP APIs. The
services are wired as in production: agentd calls vmd with a service token through the real client, and the MCP server
forwards its caller's token. Only the parts that need real hardware or a real model are replaced by fakes.

Module: `github.com/rlsvarinskis/cloud-at-home/e2e`

## Role

- Tests the seams between the services: authorization across vmd and agentd, holds, cancel, failures, idempotency,
  metrics, and the MCP tool path.
- Contains no production code. `doc.go` holds the package comment; everything else is `_test.go`.
- Does not replace unit tests. Each service's own tests cover its logic in detail. These tests cover what happens
  between the services.
- Does not start any daemon binary. The servers run as `httptest` servers in the test process.

## Position

```
            test ──HTTP (Connect)──► vmd (httptest) ◄──service token── agentd (httptest) ◄──HTTP── test
                                       │                              │
                              fake node (libs/qemu/fake)     scripted model (services/agentd/llm, Fake)
                                       ▲
  MCP tests: test ──MCP over HTTP──► mcpserver (httptest) ──caller's token──► vmd, agentd
```

- Imports `api`, `libs/common` (`pg`, `pg/pgtest`), `libs/authz`, `libs/qemu/fake`, `libs/server/obs`,
  `services/vmd` (`service`, `store`, `store/postgres`, `store/shard`, `scheduler`, `promcollect`),
  `services/agentd` (`llm`, `runner`, `service`, `store`, `store/postgres`, `vmclient`), and `services/mcp/mcpserver`.
- Nothing else may import `e2e`. Its `go.mod` has `replace` lines for the sibling modules, as the other modules do.
- External: `connectrpc.com/connect`, `github.com/jackc/pgx/v5`, `github.com/prometheus/client_golang`, the MCP Go SDK,
  and `embedded-postgres` through `pgtest`.

## Contract

There is no exported API. The tests are the interface. The harness is in `world_test.go`; a scenario is a
`Test...` function that builds a world, calls the services and asserts on the responses.

### Harness (`world_test.go`)

| Part | What the tests get |
| --- | --- |
| Clock | One manual clock, starting at 2026-10-18 12:00 UTC, shared by vmd, agentd, the fake node, the verifiers and the runner. Tests call `Advance` and never sleep for it. |
| vmd | `vmdsvc.NewHandler` behind `httptest`, with the real verifier and reconciler. One fake node `n1` with 16 vCPU, 64 GiB RAM and 2 TiB disk. Store: memory, Postgres, or three memory shards (`withShards(3)`). Reconciler resync 10 s, lease 30 s. |
| agentd | `agentdsvc.NewHandler` behind `httptest`, with the real `vmclient` that uses a service token signed by agentd's own key. Store: memory or Postgres. Runner with two workers. |
| Keys | An admin key, trusted by both servers. An agentd service key, trusted by vmd only with the `service` role; agentd trusts only the admin key. |
| Model | `llmHub`: one `llm.Fake` per model name, so agents run concurrently without sharing a reply queue. Tests script each reply and inspect the requests the fake recorded. |
| Node | `fake.Driver` records every command started (`execs`). `onExec` scripts its behaviour. |
| Callers | `root` (`user:root`, `*:admin`), and `as(subject, bindings...)` for any other principal. |

`eachBackend(t, pgToo, fn)` runs `fn` on the memory store as subtest `memory`, and on Postgres as subtest
`postgres` when `pgToo` is set. Postgres tests need `TestMain` (`main_test.go`), which calls `pgtest.Run`.

### Scenarios

| File | Test | Subtests | What it checks |
| --- | --- | --- | --- |
| `golden_test.go` | `TestGoldenPath` | `check passes`, `check fails` | Scenario 1. Register a shared image, create and start a VM, create an agent (default workdir, hold `agentd/<id>` on the VM), run a task whose model writes a file. The check command decides: `SUCCEEDED` when it passes, `FAILED` when it fails. The model's own verdict is kept in `verdict`. Both backends. |
| `golden_test.go` | `TestShardedVMD` | `golden path`, `golden path check fails`, `many agents one VM` | Scenario 9 (part). The same paths on vmd with three memory shards. |
| `golden_test.go` | `TestManyAgentsOneVM` | (per backend) | Scenario 2. Three agents on one VM run their tasks concurrently. |
| `authz_test.go` | `TestAuthorizationAcrossServices` | `viewer reads but cannot write`, `another project sees nothing of p1`, `another project cannot see or attach to p1's VM`, `agentd service key is limited to the service role on vmd`, `no token is unauthenticated` | Scenario 4. Permission and project checks on both services, including the not-found answer for another tenant's IDs. |
| `holds_test.go` | `TestHoldProtection` | `delete refused while held, allowed after the agent is deleted`, `force delete orphans the agent` | Scenario 3. A VM with agents cannot be deleted until they are gone; a forced delete leaves the agents orphaned. |
| `cancel_test.go` | `TestCancel` | `while the model call is blocked`, `while a long command runs` | Scenario 6. Cancelling a task aborts the model call or kills the running command. |
| `failure_test.go` | `TestFailureHandling` | `node goes down mid-task`, `VM crashes while a command runs`, `runner stopped mid-task is resumed by another runner` | Scenario 5. A node outage requeues the task, which then finishes without running its command twice. A VM crash marks the running job `LOST` and the model is told. A task whose runner stopped is resumed by another runner. |
| `idem_test.go` | `TestIdempotency` | (per backend) | Scenario 7. Retried create calls return one resource; a retried `StartJob` does not run the command twice. |
| `metrics_test.go` | `TestMetrics` | | Scenario 8. Both servers expose `/metrics` only to a caller with the operator role. |
| `mcp_test.go` | `TestMCPFullPath` | (per backend) | The MCP tools against real vmd and agentd: create a machine, run a command, write and read a file, run an agent task to completion, read its status and metrics, stop and destroy the machine. A background goroutine runs the reconciler so VMs boot. |
| `mcp_test.go` | `TestMCPAuth` | | MCP callers get exactly their own token's powers: no or bad token is refused, a viewer cannot create machines, and an editor of another project cannot see the machine. |

The scenario numbers are the ones in the file comments.

### Helpers

Helpers used across the scenarios: `runningVM` (fresh shared image, create, start, wait until running), `sharedImage`,
`createTask` and `createTaskEach`, `runTasks`, `runWorkers`, `tickVMD` and `until` (advance the reconciler one tick
at a time), `jobs` and `jobFor`, `commandRuns` (what the fake node started), `sortedCopy`, and `runAsync` and
`awaitErr` (run a runner in a goroutine with a 10 s bound).

## Behaviour and guarantees

- The tests are deterministic: time comes from the manual clock, and the scripted model gives the same replies on every
  run. The real waits are short sleeps in helpers (5 ms, 20 ms) and the services' 200 ms polling loops.
- Each test builds its own world, and worlds share no state. No test calls `t.Parallel`.
- A Postgres world uses a fresh database (`pgtest.DSN`), dropped at the end of the test.

## Configuration

| Name | Effect |
| --- | --- |
| `ATTIC_TEST_POSTGRES` | Admin DSN of an existing server, or `skip` to skip the Postgres subtests. Unset means embedded Postgres (see [libs/common](../libs/common/README.md)). |

## Data

The tests create and remove their own databases. Nothing is written outside the test's temporary directories.

## Observability

Scenario 8 reads the Prometheus exposition of both servers. The other tests do not check metrics.

## Failure modes

When a scenario fails, the message names the step and the response. The fake node's command log and the scripted model's
recorded requests are the first places to look. A flaky test means a scenario depends on real time somewhere; report it.

## Running and testing

```
cd e2e && go vet ./...
cd e2e && go test -race -count=1 ./...
cd e2e && go test -race -count=1 -run 'TestGoldenPath/memory/check_passes' -v ./...
cd e2e && ATTIC_TEST_POSTGRES=skip go test -count=1 ./...
cd e2e && ATTIC_TEST_POSTGRES='postgres://USER:PASSWORD@127.0.0.1:5432/postgres?sslmode=disable' go test -count=1 ./...
```

Use `-count=1` to avoid cached results. Without `ATTIC_TEST_POSTGRES` the Postgres subtests start embedded Postgres,
which downloads its binaries on first use.

## Not covered

- Real QEMU/KVM. The node is `libs/qemu/fake`: no guest boots, no SSH, no disk images are written, and no firewall rules
  are installed. The real driver's tests in `libs/qemu/qemudriver` skip when QEMU, KVM or an ISO builder is missing.
- Real models and Ollama. Replies are scripted. Model quality, and the behaviour of a real backend's token limits and
  streaming, are not tested here.
- The daemon binaries: flags, signal handling, `daemon.Serve`, TLS, cleartext HTTP/2 and the `-trusted-key` and
  `-revoked-tokens` flags. These are covered by the services' `cmd` tests and `libs/server` tests. The worlds use no
  revocation list.
- More than one node. The world has a single node, `n1`, so placement across nodes and draining of a node in a fleet are
  not exercised.
- Sharded Postgres. Sharding runs on memory stores only.
- Load, latency and long waits in real time. The clock is manual.

## Layout

```
e2e/
  doc.go              package comment; no code
  main_test.go        TestMain: pgtest.Run
  world_test.go       the harness: clock, LLM hub, fake node log, world, clients, helpers
  golden_test.go      scenarios 1, 2 and 9 (part)
  authz_test.go       scenario 4
  holds_test.go       scenario 3
  cancel_test.go      scenario 6
  failure_test.go     scenario 5
  idem_test.go        scenario 7
  metrics_test.go     scenario 8
  mcp_test.go         MCP tool path and MCP authorization
  go.mod, go.sum
```

## Related

- [services/vmd/README.md](../services/vmd/README.md) and [services/agentd/README.md](../services/agentd/README.md): the
  services under test.
- [services/mcp/README.md](../services/mcp/README.md): the MCP server under test.
- [libs/qemu/README.md](../libs/qemu/README.md): the fake node and the real driver it stands in for.
- [libs/authz/README.md](../libs/authz/README.md): the tokens and roles the authorization scenarios use.
- [libs/common/README.md](../libs/common/README.md): `pgtest`, the test database.
- [api/README.md](../api/README.md): the APIs called over HTTP.
