# Attic Cloud architecture

Attic Cloud is a small private cloud: **vmd** manages virtual machines, disks and images on one or more hosts;
**agentd** runs LLM agents on top of vmd's VMs. Everything is multi-tenant (projects), authenticated, authorized
per project and stateful in a database, so any number of replicas can serve it.

```
                  clients (atticctl, SDKs, Claude, curl)
                        │  gRPC / gRPC-Web / HTTP+JSON  (Connect protocol, same handlers, TLS optional)
          ┌─────────────┴──────────────┐
          ▼                            ▼
   ┌────────────┐  Connect + JWT   ┌────────────┐
   │  agentd    │ ───────────────▶ │   vmd      │  control plane (stateless replicas)
   │  replicas  │  role "service"  │  replicas  │
   └─────┬──────┘                  └──┬──────┬──┘
         │                            │      │ NodeService (Connect, role "node")
   Postgres (agentd)            Postgres(es) (vmd)   ▼
   + Ollama (LLM)                          ┌───────────┐ ┌───────────┐
                                           │ node host │ │ node host │   one daemon per physical host
                                           │ QEMU/KVM  │ │ QEMU/KVM  │   (`vmd --role=node`)
                                           └───────────┘ └───────────┘
```
`vmd --role=all` runs the control plane and an in-process node on one machine; `--role=control` talks to remote
nodes (`--node id=URL`); `--role=node` is the host daemon.

## Services

### vmd
* **VMService / DiskService / ImageService** are declarative. An API call records a *desired state*; the
  **reconciler** moves reality towards it and records why it could not (`state_reason`).
* **ExecService** runs commands in a VM as *jobs that live inside the guest* (`/var/tmp/mcp-jobs/<id>`). No call
  blocks longer than 30 s, jobs survive control-plane and node-daemon restarts, and background children never block a
  call (ADR 3). It also moves small files in and out.
* **MetricsService** has per-VM guest metrics (CPU like `top`: 100 per busy vCPU, RAM, swap, load, per-filesystem usage
  attributed to the VM disk, host-side disk allocation) and cluster capacity for operators (cores, RAM, disk: total /
  allocated / available per node and overall).
* **ClusterService** is for operators: `SetNodeDraining` stops new VM starts on a node (running VMs are untouched).
* **NodeService** is the internal host API behind the `node.Driver` interface. Implementations: QEMU (real hosts), Remote
  (a node over the network), Fake (tests).
* `/metrics` (Prometheus, operator token), `/healthz`, `/readyz`.

### agentd
* An **Agent** is *always* tied to one VM (`vm_id` is mandatory) and many agents can share a VM, each with its own
  working directory. agentd places a **hold** on the VM through vmd's API so it cannot be deleted from under its agents
  (a forced delete turns the agents `ORPHANED`).
* A **Task** is queued in the database and run by any agentd replica that claims it with a *lease*. The conversation is
  persisted message by message; if a worker dies, another resumes from the stored messages and re-attaches to the
  command already running in the VM (idempotency keys), instead of running it twice.
* The caller chooses the model per agent (there is no default). A task ends `SUCCEEDED / FAILED / GAVE_UP / CANCELLED /
  ERROR`, decided by an optional `check_command` or by the model's explicit `VERDICT:` line.
* agentd uses only vmd's public API, as a service principal. It shares no database with vmd.

## Placement and the reconciler
* A VM runs on the node that holds its boot image/disk (images are registered per node; there is no cross-node image
  replication yet). `scheduler.Fits` gates every start against that node's capacity with the configured overcommit and
  host reserve, and refuses starts on a draining or down node. A VM that does not fit waits in `PENDING` with the reason
  and starts when room appears. (`scheduler.Pick`, a load-spreading chooser, exists and is tested but is not used for VM
  placement until images can live on several nodes.)
* The reconciler claims due VMs with a lease, so any number of control-plane replicas share the work. Failures to reach a
  node back off exponentially (capped at 60 s). Unexpected VM exits are counted separately (`Crashes`): the count is
  forgiven only while the VM stays healthy, and on the fourth crash the VM goes to `ERROR` and is retried ever more slowly.
  An explicit `StartVM` on an errored VM retries immediately.
* **Drain** is the operator's decision, stored apart from observed health, so an outage cannot erase it.

## Identity and authorization
* **Authentication**: bearer JWTs signed with Ed25519. Servers hold only *public* keys (`kid → key`), so there is no shared
  secret and no IAM database. `atticctl token mint` issues user tokens; agentd mints short-lived service tokens with its own
  key. Each trusted key can be limited to certain roles (a compromised agentd key cannot mint admin tokens). Tokens have a
  maximum lifetime (`--max-token-ttl`), are decoded strictly (one canonical spelling), and can be revoked by id through
  a file the daemons watch (`--revoked-tokens`).
* **Planned, not implemented**: a `services/auth` identity service (users with Argon2 password hashes, API keys and
  service accounts, OIDC login for Google and GitHub, an audit log, JWKS publishing, with services verifying its tokens
  offline). It does not exist yet; today tokens are minted by `atticctl` and by the services themselves, as described above.
* **Authorization**: RBAC scoped to **projects**. A token carries bindings `(project, role)`; project `*` means all.
  Roles: `viewer`, `editor`, `admin`, `service` (agentd → vmd), `node` (control plane → node), `operator` (global: capacity
  and drain). Handlers check a named permission against the *resource's* project. A caller with no access to a project gets
  `NotFound` for IDs in it, never `PermissionDenied`, so other tenants' IDs cannot be probed.
* **Quotas** (VMs, vCPUs, RAM, disk GB) are per project, enforced at create/start/resize, set with `--quota-*` (0 =
  unlimited, the default).
* Errors from downstream systems (a node, vmd as seen from agentd, the model backend) are reported to API clients without
  hostnames or ports; the detail goes to the log.

## Distribution and sharding
* **Stateless replicas.** State lives in Postgres. IDs are ULIDs generated locally; writes are compare-and-swap on a row
  version; creates and job starts take idempotency keys. Any replica can serve any request.
* **Work distribution by leases**, not leader election: the VM reconciler and agent task workers claim rows with
  `FOR UPDATE SKIP LOCKED` plus `lease_until`, so adding replicas adds throughput and a dead replica's work is picked up when
  its lease expires.
* **Horizontal compute**: one node daemon per host, registered with the control plane (`--node id=URL`).
* **Data sharding by project**: pass `--postgres-dsn` several times and vmd spreads projects across those databases with
  rendezvous hashing (`services/vmd/store/shard`). Resource IDs embed their shard (`vm_<ulid>-s02`) so lookups need no
  directory; node rows and shared images live on shard 0; a transaction stays inside one project's shard. **The set of
  databases is part of the data layout**: changing it re-routes projects, and there is no rebalancing tool yet.

## Failure model
| Failure | Behaviour |
|---|---|
| Node unreachable | VMs keep their last state, reason `node unreachable`, reconciler retries with backoff; the node is marked down after 15 s of silence; its drain flag survives |
| Node daemon restarts | VMs keep running (QEMU is detached); the daemon re-adopts them |
| VM crash | state/reason recorded, restarted; crash loop → `ERROR` with growing retry interval |
| Control-plane replica dies | its leases expire; another replica continues |
| Postgres down | API returns errors, `/readyz` fails; nothing is lost that was committed |
| agentd worker dies mid-task | lease expires, another worker resumes from the stored conversation |
| User cancels a task | the model call is aborted mid-reply and the running VM command is killed; shutdown or lost lease does *not* kill it (the successor re-attaches) |

Everything that can take long is asynchronous (jobs, tasks, VM transitions) with long-poll `wait` parameters capped at 30 s,
because clients and proxies give up around 60 s.

## Repository layout

The repository is a Go workspace (`go.work`). Each module has its own `go.mod`; modules refer to each other
through `go.work` and relative `replace` directives. Module paths are `github.com/rlsvarinskis/cloud-at-home/<module dir>`.
There is no root module, so `go test ./...` must run inside a module. The `make` targets loop over all modules:
`api libs/common libs/authz libs/qemu libs/server services/vmd services/agentd services/mcp cmd/atticctl e2e`.

```
go.work                              the workspace: the nine modules below
Makefile                             proto, mocks, build, vet, test, cover, tidy, boundaries
scripts/check-boundaries.sh          fails when a module imports a sibling it may not (rules below)
api/                                 module: API definitions and generated code
  proto/attic/{vmd,agentd,node}/v1/   .proto files, the source of truth for the wire format
  gen/                               generated Go code for proto/ (committed; make proto)
libs/
  common/                            module: shared building blocks
    errs/                            error vocabulary, mapped to Connect codes at the edge
    ids/                             ULID-based IDs with a resource prefix (and shard suffixes)
    pg/                              Postgres pool and migration runner (advisory lock); pgtest/ for tests
  authz/                             module, package authz: tokens, keys, principals, RBAC, revocation,
                                     Connect interceptors, service tokens
  qemu/                              module: everything that runs VMs on a host
    node/                            the node Driver interface (driver.go); mock/
    qemudriver/                      QEMU/KVM driver (disks, firewall, jobs, metrics, export, system)
    qemu/                            QEMU argument builder
    guestexec/, guestmetrics/        guest job scripts and guest metrics
    sshrunner/                       SSH access to guests
    remote/                          Driver client that talks to a node over the network
    nodeserver/                      handler that serves the node API
    fake/                            in-memory fake Driver for tests
  server/                            module: shared server plumbing
    obs/                             RPC metrics, /metrics, health checks, error interceptor
    daemon/                          trusted-key and revocation flags, logger, HTTP server with graceful shutdown
services/
  vmd/                               module: the VM service
    domain/                          vmd's model (not the wire types)
    store/                           store interface; memory, postgres/ (migrations/), shard/, storetest/, mock/
    service/                         Connect handlers, reconciler, node registry; servicemock/
    scheduler/                       capacity arithmetic and placement helpers
    promcollect/                     Prometheus collector for vmd state
    cmd/vmd/                         the vmd binary (--role=all|control|node)
  agentd/                            module: the agent service
    domain/, store/                  agentd's model; store with postgres/ (migrations/), storetest/, mock/
    llm/                             model client interface, Ollama client, fake, mock/
    vmclient/                        the only way agentd calls vmd; mock/
    runner/                          task workers: leases, conversation loop, commands, sanitising
    service/                         agentd's Connect handlers
    metrics/                         task and step counters, task duration histogram
    cmd/agentd/                      the agentd binary
  mcp/                               module: MCP server for Claude and other MCP clients (see its README)
    mcpserver/                       tool implementations; forwards the caller's token to vmd and agentd
    cmd/mcp/                         the mcp binary
cmd/
  atticctl/                             module: admin CLI (keys, tokens, roles)
e2e/                                 module: vmd and agentd together in one process, over their HTTP APIs
deploy/                              Dockerfiles, compose stack, deployment notes (not a module)
docs/                                this documentation
```

Migrations live with their store: `services/*/store/postgres/migrations/`.

### Layering rules

`scripts/check-boundaries.sh` (run by `make boundaries`) fails when a module imports a sibling module that its
row below does not allow. Imports are checked in tests as well as in the code.

| Module | May import |
| --- | --- |
| `api` | nothing |
| `libs/common` | nothing |
| `libs/authz` | `api`, `libs/common` |
| `libs/qemu` | `api`, `libs/common`, `libs/authz` |
| `libs/server` | `api`, `libs/common`, `libs/authz` |
| `services/vmd` | `api`, `libs/common`, `libs/authz`, `libs/qemu`, `libs/server` |
| `services/agentd` | `api`, `libs/common`, `libs/authz`, `libs/server` |
| `services/mcp` | `api`, `libs/common`, `libs/authz`, `libs/server` |
| `cmd/atticctl` | `api`, `libs/common`, `libs/authz` |
| `e2e` | anything |

No service imports another service. agentd talks to vmd only through the generated gRPC client in `api/gen`
(`services/agentd/vmclient`).

## Testing strategy
* Unit tests for pure logic (scheduler, parsers, argument builders, RBAC matrix, tokens).
* **Conformance suites** (`storetest`) run against every store: memory, Postgres (a real PostgreSQL started by the tests)
  and the shard router.
* Service tests run the real handlers over HTTP with the fake node and fake LLM, real tokens and a manual clock; the whole
  vmd suite can also run on a sharded store (`ATTIC_TEST_SHARDS=3`).
* `e2e/` runs vmd and agentd together through their real APIs, on memory and Postgres.
* The daemons' real `run` functions are started against real Postgres in the tests of `services/*/cmd/*`.
* gomock mocks (`make mocks`) script failures the fakes cannot (a broken database, exact call order and arguments).

## Known limitations
* No cross-node image replication, live migration or in-place VM resize (resize needs a stop).
* Changing the set of shard databases needs a manual migration.
* `agent` task queue depth is not exported as a metric (the store has no cluster-wide count).
* Deleting an image races with a concurrent disk creation from it in a narrow window.
* The QEMU driver and the guest scripts are unit-tested with fakes and local shells; they have not been run against a real
  KVM host in this repository's CI.
