# homecloud architecture

homecloud is a small private cloud: **vmd** manages virtual machines, disks and images on one or more
hosts; **agentd** runs LLM agents on top of vmd's VMs. Everything is multi-tenant (projects), authenticated,
authorized per project, and stateful in a database, so that any number of replicas can serve it.

```
                    clients (hcctl, SDKs, Claude, curl)
                          │  gRPC / gRPC-Web / HTTP+JSON  (Connect protocol, same handlers)
            ┌─────────────┴──────────────┐
            ▼                            ▼
     ┌────────────┐   gRPC + service   ┌────────────┐
     │  agentd    │ ─────────────────▶ │   vmd      │  control plane (stateless replicas)
     │ replicas   │    JWT, role       │  replicas  │
     └─────┬──────┘    "service"       └──┬──────┬──┘
           │                              │      │ NodeService (gRPC, role "node")
      Postgres (agentd)             Postgres(vmd) ▼
      + Ollama (LLM)                       ┌───────────┐ ┌───────────┐
                                           │ node host │ │ node host │  one daemon per physical host
                                           │ QEMU/KVM  │ │ QEMU/KVM  │
                                           └───────────┘ └───────────┘
```

## Services

### vmd (VMs + disks + images + exec + metrics)
* **VMService / DiskService / ImageService** – declarative resources. An API call records a *desired state*;
  a **reconciler** moves reality towards it and records why it could not (`state_reason`).
* **ExecService** – run commands in a VM as *jobs* that live inside the guest (`/var/tmp/mcp-jobs/<id>`):
  no call blocks longer than 30 s, jobs survive control-plane and node-daemon restarts, background children
  never block a call. Also small file put/get.
* **MetricsService** – per-VM CPU/RAM/filesystem usage measured inside the guest, per-disk allocation
  measured on the host, and cluster capacity (CPU cores, RAM, disk: total / allocated / available per node
  and overall) for operators.
* **NodeService** (internal) – the per-host hypervisor API. The control plane talks to nodes only through the
  `node.Driver` interface; implementations: QEMU (real), Remote (another host over gRPC), Fake (tests).

### agentd (agents + tasks)
* An **Agent** is *always* tied to one VM (`vm_id` is mandatory); many agents can share a VM, each with its
  own working directory. agentd places a **hold** on the VM (`vmd.AddHold`) so a VM cannot be deleted
  from under its agents (unless forced, in which case agents become `ORPHANED`).
* A **Task** is queued in the database and run by any agentd replica that claims it with a *lease*. The
  conversation is persisted after every message, so when a worker dies the lease expires and another
  worker resumes the task from the stored messages.
* The model is chosen by the caller per agent (no default). Final status is `SUCCEEDED / FAILED / GAVE_UP /
  CANCELLED / ERROR`, decided by an optional `check_command` or by the model's explicit verdict.
* agentd uses *only* vmd's public API (as a service principal). It has no access to vmd's database.

## Identity and authorization
* **Authentication**: bearer JWTs signed with Ed25519. Servers hold only *public* keys (`kid → key`), so
  there is no shared secret and no central IAM database. `hcctl token mint` issues user tokens; agentd mints
  short-lived service tokens with its own key. Each trusted key can be limited to certain roles (a
  compromised agentd key cannot mint admin tokens). Tokens have a maximum lifetime and can be revoked by `jti`.
* **Authorization**: RBAC scoped to **projects**. A token carries bindings `(project, role)`; project `*`
  means all. Roles: `viewer`, `editor`, `admin`, `service` (agentd → vmd), `node` (control plane → node),
  `operator` (global capacity view). Handlers check a named permission against the *resource's* project.
  Callers with no access to a project get `NotFound` for its resources, never `PermissionDenied`
  (no probing of other tenants' IDs).
* Quotas per project (VMs, vCPUs, RAM, disk GB) are enforced at create/start/resize.

## Distribution and sharding
* **Stateless replicas.** All state is in Postgres. IDs are ULIDs generated locally (no sequences), writes are
  compare-and-swap on a row version, creates take an idempotency key. Any replica can serve any request.
* **Work distribution by leases**, not leader election: the VM reconciler and the agent task workers *claim*
  rows (`FOR UPDATE SKIP LOCKED` + `lease_until`), so adding replicas adds throughput and a dead replica's work
  is picked up when its lease expires.
* **Horizontal compute**: one node daemon per host. The scheduler places VMs/disks by free CPU/RAM/disk with
  overcommit factors; nodes can be drained. A VM belongs to the node holding its disks.
* **Data sharding**: every resource is project-scoped and no API lists across projects, so the store can be
  partitioned by `project_id` (`store/shard` maps a project to one of N databases by rendezvous hashing).
  Cross-project reads exist only for operators and use fan-out.

## Failure model
* Node unreachable → its VMs keep their last known state, reason `node unreachable`, reconciler retries with
  exponential backoff. Node daemon restart → VMs keep running (QEMU is detached); runtime is re-read.
* VM crash/unexpected exit → state `ERROR`/reason, restarted if `desired=running`.
* Control-plane crash mid-operation → operations are idempotent steps driven by desired state; the lease expires
  and another replica continues.
* Everything that can take long is asynchronous (jobs, tasks, VM transitions) with long-poll `wait` parameters
  capped at 30 s, because clients and proxies give up around 60 s.

## Repository layout
```
proto/            API definitions (source of truth)       gen/        generated code (committed)
cmd/vmd           control plane (+ optional embedded node)  cmd/agentd   agent server
cmd/hcctl         admin CLI (keys, tokens)
internal/auth     tokens, RBAC, interceptors              internal/errs   shared error model
internal/vmd      domain, store (memory/postgres/shard), scheduler, reconciler, node drivers, services
internal/agentd   domain, store, llm (Ollama/fake), runner, services
migrations/       SQL schema per service                  deploy/     Dockerfiles, compose, examples
```

## Testing strategy
* Unit tests for pure logic (scheduler, parsers, argument builders, RBAC matrix, tokens).
* **Conformance suites** (`storetest`) run against every store implementation: memory always, Postgres when
  `HOMECLOUD_TEST_POSTGRES` is set.
* Service tests run the real Connect handlers over `httptest` with the fake node driver and fake LLM, using
  real tokens, so authentication, authorization, validation and the reconciler are exercised end to end.
* Mocks (`go.uber.org/mock`) for interfaces that need call-level assertions; hand-written fakes where state matters.
