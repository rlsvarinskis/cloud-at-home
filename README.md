# homecloud

homecloud is a small private cloud written in Go. It has two servers:

- **vmd** manages virtual machines, disks, images, in-guest command execution and metrics. It runs as a
  control plane, as a per-host node daemon, or both in one process.
- **agentd** runs LLM agents on top of vmd's VMs. An agent is always tied to one VM. A task given to an
  agent is executed by a model that drives the VM through a single `bash` tool.

Both servers speak the Connect protocol, which is gRPC, gRPC-Web and plain HTTP+JSON on the same paths.
All resources belong to projects. Every call is authenticated with an Ed25519-signed bearer token and
authorized per project.

Status: vmd and agentd are implemented, and `make test` passes. The `e2e/` tests run both servers in one
process over HTTP, and `services/vmd/cmd/vmd` and `services/agentd/cmd/agentd` have tests that start the real daemons against Postgres. No guest
has been booted in this repository's CI, and the development host used for these docs has no `genisoimage`, so the
QEMU driver and the guest scripts are covered by unit tests with fakes. See
[ARCHITECTURE.md](docs/ARCHITECTURE.md#known-limitations).

## Architecture

```
                    clients (hcctl, SDKs, Claude, curl)
                          │  Connect / gRPC / gRPC-Web, JSON over HTTP
            ┌─────────────┴──────────────┐
            ▼                            ▼
     ┌────────────┐   gRPC + service   ┌────────────┐
     │  agentd    │ ─────────────────▶ │   vmd      │  control plane (stateless replicas)
     │ replicas   │    JWT, role      │  replicas  │
     └─────┬──────┘    "service"       └──┬──────┬──┘
           │                              │      │ node API (role "node")
      Postgres (agentd)             Postgres (vmd) ▼
      + Ollama (LLM)                       ┌───────────┐ ┌───────────┐
                                           │ node host │ │ node host │
                                           │ QEMU/KVM  │ │ QEMU/KVM  │
                                           └───────────┘ └───────────┘
```

The design, including placement, the reconciler, sharding and the failure model, is in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). The decisions themselves are recorded in
[docs/adr/](docs/adr/).

## Features

vmd:

- Declarative VMs, disks and images. An API call records a desired state; a reconciler moves the VM
  towards it and records why it could not (`state_reason`).
- Copy-on-write disks from images, snapshots of a detached disk into a new image, grow-only disk resize.
- Commands run in the guest as detached jobs (`StartJob`, `PollJob` with output offsets, `CancelJob`).
  Calls wait at most 30 seconds. Jobs survive restarts of the control plane and of the node daemon.
- Small file transfer into and out of a running guest (up to 8 MiB per file).
- Per-VM guest metrics (CPU, memory, swap, filesystems), per-disk host allocation, and cluster capacity.
- Draining a node (`ClusterService.SetNodeDraining`): running VMs keep running, nothing new starts there.
- Per-project quotas (`--quota-*`, unlimited by default).
- Prometheus metrics on `/metrics` for operators.
- Holds: another service can hold a VM so that it cannot be deleted from under it.
- Ed25519 bearer tokens with per-key role restrictions, project-scoped RBAC, revocation of single tokens
  (`--revoked-tokens`), and not-found answers for resources in projects the caller cannot see.
- Postgres as the system of record, with leases and compare-and-swap so that several replicas can serve
  and reconcile at the same time. Projects can be spread over several databases (`--postgres-dsn` repeated).
  An in-memory store is available for development.

agentd:

- Agents with a model chosen by the caller, a system prompt, a working directory inside the VM and labels.
- Tasks queued in the database and run by worker replicas under leases. The conversation is stored after
  every message, so another worker can resume a task whose owner died.
- Optional `check_command` that decides the final state, independent of the model's own verdict.
- Long-poll task reads, cancellation (which also stops the running command), and a list of the commands a task ran.

## Quick start (single machine, development)

Prerequisites: Go 1.26 or later. To start real VMs you also need the host tools listed in
[docs/OPERATIONS.md](docs/OPERATIONS.md#node-host-requirements). Without them the API works, but starting a VM fails
with a `state_reason` of `node unreachable: <node id>` and the VM stays `STOPPED`.

1. Build the binaries into `bin/`:

   ```
   mkdir -p bin && go build -o bin/ ./services/vmd/cmd/vmd ./services/agentd/cmd/agentd ./cmd/hcctl
   ```

   The repository is a Go workspace (`go.work`) with one module per directory and no root module. `make build`
   compiles every module and writes no binaries.

2. Generate a signing key and mint an admin token. The key pair is written to `dev/keys/`; `hcctl` refuses
   to overwrite existing files.

   ```
   ./bin/hcctl key generate -out dev/keys -id admin
   TOKEN=$(./bin/hcctl token mint -key dev/keys/admin.key -kid admin \
     -sub user:me -type user -bind '*:admin' -ttl 24h)
   ```

   `-bind project:role` may be repeated. `*` as the project means every project. The default token
   lifetime is 24 hours; `vmd` rejects tokens longer than its `-max-token-ttl`, which defaults to 24 hours.

3. Start vmd with the control plane and an in-process node:

   ```
   ./bin/vmd --role=all --listen=127.0.0.1:8080 \
     --trusted-key=admin=dev/keys/admin.pub \
     --ssh-key=dev/ssh_ed25519 \
     --no-firewall --run-as= \
     --data-dir="$PWD/dev/data" --log-level=info
   ```

   - `--trusted-key kid=path[:roles]` trusts the public key `path` for tokens whose key id is `kid`.
     Without at least one trusted key vmd refuses to start.
   - `--ssh-key` is required for a node. The private key is created with mode 0600 if it does not exist, and
     its public half is written to `dev/ssh_ed25519.pub`.
   - `--no-firewall` skips the guest network lockdown. Without it vmd installs `iptables` rules that stop
     guests from reaching private networks, which needs root or `CAP_NET_ADMIN`, and fails at start-up if
     the rules cannot be installed. Use `--no-firewall` only for development.
   - `--data-dir` must be an absolute path; vmd refuses a relative one. `$PWD` gives the absolute path used here.
   - `--run-as=` (empty) runs the guests as the current user. The default is the `qemu` user, which must
     exist.
   - Without `--postgres-dsn` (or `HOMECLOUD_POSTGRES_DSN`) vmd uses the in-memory store and logs a warning.
     All state is lost on restart.
   - `--revoked-tokens FILE` is optional. If given, the file must exist when vmd starts.

4. Call the API:

   ```
   curl -sS -X POST http://127.0.0.1:8080/homecloud.vmd.v1.VMService/ListVMs \
     -H "Authorization: Bearer $TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"projectId": "proj-a"}'
   ```

   The health endpoints are `/healthz` (liveness) and `/readyz` (readiness, checks the store).

See [docs/API.md](docs/API.md) for the full API and a worked workflow, and [docs/OPERATIONS.md](docs/OPERATIONS.md)
for a multi-host deployment.

## Repository layout

```
api/                   module: API definitions (api/proto, source of truth) and generated Go code (api/gen, committed)
libs/common/           module: errs (error vocabulary), ids (prefixed ULIDs), pg (Postgres pool, migrations, pgtest)
libs/authz/            module, package authz: tokens, RBAC, revocation list, Connect interceptors, service tokens
libs/qemu/             module: node driver interface (node/), qemudriver, qemu, guest scripts, sshrunner, remote, nodeserver, fake
libs/server/           module: obs (RPC metrics, /metrics, health checks), daemon (flags, logging, HTTP server)
services/vmd/         module: vmd (domain, store, scheduler, service, promcollect) and cmd/vmd (control plane and node daemon)
services/agentd/      module: agentd (domain, store, llm, vmclient, runner, service, metrics) and cmd/agentd
cmd/hcctl/             module: admin CLI: keys, tokens, roles
e2e/                   module: end-to-end tests, vmd and agentd in one process, over their HTTP APIs
deploy/                Dockerfiles, docker-compose development stack, deployment notes
docs/                  ARCHITECTURE.md, adr/, API.md, AUTH.md, OPERATIONS.md, DEVELOPMENT.md
```

The module layout and the rules on which module may import which are in
[ARCHITECTURE.md](docs/ARCHITECTURE.md#repository-layout).

## Tests

```
make build         # go build ./... in every module
make test          # go test -race -count=1 ./... in every module
make vet           # go vet ./... in every module
make cover         # coverage summary
make boundaries    # fail if a module imports a sibling it may not
```

`go test ./...` at the repository root does not work. Run it inside a module, for example
`cd services/vmd && go test ./...`, or use `make test`.

Store conformance suites (`storetest`) run against every store implementation. The Postgres suites need a
Postgres server. Choose one of:

- Default: a real Postgres is started for the test process with `embedded-postgres`. Its binaries are
  downloaded on first use and cached, so the first run needs network access.
- `HOMECLOUD_TEST_POSTGRES=postgres://admin-dsn...`: use an existing server. Each test gets a fresh
  database on it.
- `HOMECLOUD_TEST_POSTGRES=skip`: skip the Postgres tests.

Service tests run the real Connect handlers over `httptest` with a fake node driver and a fake LLM, using
real signed tokens. `cd services/vmd && HOMECLOUD_TEST_SHARDS=3 go test ./service` runs the same suite on a store
sharded over three databases. See [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md).

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): components, placement, identity, distribution, failure model, known limitations.
- [docs/adr/](docs/adr/): decisions on Connect RPC, Postgres with leases, and jobs living in the guest.
- [docs/API.md](docs/API.md): every service, its RPCs, permissions, errors and limits, with examples.
- [docs/AUTH.md](docs/AUTH.md): tokens, roles, projects, revocation, service-to-service auth, key rotation, hardening.
- [docs/OPERATIONS.md](docs/OPERATIONS.md): deployment shapes, node host setup, reconciler behaviour, draining,
  failure handling, metrics and alerts, quotas, capacity, backups and upgrades.
- [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md): regenerating code, testing, adding RPCs, permissions and
  store methods, conventions and known limitations.
- [deploy/README.md](deploy/README.md): container images and the docker-compose development stack.
