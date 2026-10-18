# deploy

Container images and the Docker Compose stack for Attic Cloud, plus notes for running it on real hosts. The stack runs
the whole system on one Docker host: the control plane, one VM node, the agent service, the MCP server and Postgres,
with an optional CPU Ollama. For what each service does, see the [root README](../README.md).

## Role

- Owns how the services are packaged (`Dockerfile.*`) and wired together (`docker-compose.yml`).
- Does not contain service code or configuration defaults; every flag used here is documented in the service's own
  README ([vmd](../services/vmd/README.md), [agentd](../services/agentd/README.md), [mcp](../services/mcp/README.md)).

## Files

| File | Purpose |
| --- | --- |
| `docker-compose.yml` | The stack: `postgres`, `vmd` (control plane), `node` (QEMU/KVM), `agentd`, `mcp`, optional `ollama`. |
| `Dockerfile.vmd` | Control plane on distroless (nonroot). No QEMU. |
| `Dockerfile.node` | The same `vmd` binary with `--role=node`, on Debian stable slim with `qemu-system-x86`, `qemu-utils`, `genisoimage`, `openssh-client`, `iptables` and a `qemu` user. |
| `Dockerfile.agentd`, `Dockerfile.mcp` | Those services on distroless (nonroot). |
| `postgres-init.sql` | Creates the `agentd` database on first start (vmd uses the default `attic` database). |
| `keys/` (gitignored) | Signing keys you generate; mounted read-only. |
| `images/` (gitignored) | Guest base images (qcow2); mounted read-only into the node at `/images`. |
| `.env` (gitignored) | Settings for compose (below). |

All images build from the repository root (`context: ..`) and need no BuildKit or buildx plugin. The root
`.dockerignore` keeps keys, `.env`, images and VCS data out of every build context.

## The stack

| Service | Image | Command highlights | Published (loopback) |
| --- | --- | --- | --- |
| `postgres` | `postgres:17` | databases `attic` and `agentd`; healthcheck gates the others | none |
| `vmd` | `Dockerfile.vmd` | `--role=control`, `--node=node1=http://node:8080`, trusts `admin` and `agentd` (role `service` only), signs node calls with `vmd.key` | `ATTIC_VMD_PORT` (8080) |
| `node` | `Dockerfile.node` | `--node-id=node1`, `--run-as=qemu`, trusts only `vmd.pub` with role `node`, `--allow-dns=127.0.0.11` | none |
| `agentd` | `Dockerfile.agentd` | `--vmd-url=http://vmd:8080`, `--ollama-url`, signs service tokens with `agentd.key` | `ATTIC_AGENTD_PORT` (8081) |
| `mcp` | `Dockerfile.mcp` | forwards the caller's bearer token to vmd and agentd | `ATTIC_MCP_PORT` (8082) |
| `ollama` (profile `llm`) | `ollama/ollama` | CPU only, no GPU reservation | `ATTIC_OLLAMA_PORT` (11434) |

Each service mounts only the key files it needs: vmd gets `admin.pub`, `agentd.pub` and `vmd.key`; the node gets
`vmd.pub`; agentd gets `admin.pub` and `agentd.key`; mcp gets `admin.pub`.

### Settings

Set these in `deploy/.env` or the environment. Only the first is required.

| Variable | Default | Meaning |
| --- | --- | --- |
| `ATTIC_POSTGRES_PASSWORD` | none (required) | Password of the `attic` Postgres user; compose refuses to start without it. |
| `ATTIC_UID`, `ATTIC_GID` | 65532 | User the `vmd` and `agentd` containers run as. It must be able to read `keys/*.key`, which `atticctl` writes with mode 0600, so set them to your own ids (`id -u`, `id -g`). |
| `COMPOSE_PROJECT_NAME` | `attic` | Prefix of every container, network and volume. |
| `ATTIC_BIND` | `127.0.0.1` | Host address the ports are published on. |
| `ATTIC_VMD_PORT`, `ATTIC_AGENTD_PORT`, `ATTIC_MCP_PORT`, `ATTIC_OLLAMA_PORT` | 8080, 8081, 8082, 11434 | Host ports. |
| `ATTIC_SUBNET` | `172.30.77.0/24` | Subnet of the stack's private network. |
| `ATTIC_NODE_CPUS`, `ATTIC_NODE_MEM` | 4, `6g` | Hard caps (`cpus`, `mem_limit`) of the node container, which hosts the guests. |
| `ATTIC_NODE_RESERVE_MB` | 1024 | RAM the node keeps back for itself (`--reserve-memory-mb`). |
| `ATTIC_IMAGES_DIR` | `./images` | Host folder of guest images, mounted read-only at `/images` in the node. |
| `ATTIC_DEFAULT_PROJECT`, `ATTIC_DEFAULT_IMAGE` | `default`, empty | Defaults of the MCP server's tools. |
| `ATTIC_OLLAMA_URL` | `http://ollama:11434` | Where agentd finds Ollama (set it to use an existing server). |

## Running the stack

The commands are in the [root README quick start](../README.md#quick-start-docker). In short, from the repository
root: generate `admin`, `agentd` and `vmd` keys into `deploy/keys` with `atticctl`, write `deploy/.env`, put a qcow2
guest image in `deploy/images`, then `cd deploy && docker compose up -d --build`. Register the image on node `node1`
(path `/images/<file>`), create a VM from it, and run commands in it through vmd or the MCP server.

To run next to other Docker projects without clashing, give it its own project name, ports and subnet, and cap the
node:

```bash
cd deploy
COMPOSE_PROJECT_NAME=attic-test ATTIC_VMD_PORT=18180 ATTIC_AGENTD_PORT=18181 ATTIC_MCP_PORT=18182 \
  ATTIC_SUBNET=172.30.88.0/24 ATTIC_NODE_CPUS=2 ATTIC_NODE_MEM=3g docker compose up -d --build
COMPOSE_PROJECT_NAME=attic-test ... docker compose --profile llm down -v --rmi local     # remove everything
```

Use the same variables for every compose command of that project.

For a GPU, add a `deploy.resources.reservations.devices` entry to the `ollama` service in a compose override file, or
point `ATTIC_OLLAMA_URL` at an Ollama you already run.

## Security notes

- Ports are published on loopback by default. Put TLS in front of anything reachable beyond the host: the services
  accept plain HTTP unless given `--tls-cert` and `--tls-key`.
- The node container is the only privileged piece: it has `/dev/kvm` and `NET_ADMIN`. The guest firewall rules
  (`-j REJECT` for private ranges, keyed on the `qemu` user) are installed in the container's own network namespace and
  never touch the host's. Guests can resolve names through Docker's embedded resolver only (`--allow-dns=127.0.0.11`).
- Private keys exist only on the host, mounted read-only into the one service that signs with them. vmd and agentd
  hold only public keys of other signers. See [libs/authz](../libs/authz/README.md#keys) for rotation.
- Never commit `deploy/keys`, `deploy/.env` or `deploy/images`; they are gitignored and excluded from builds.

## Behaviour to know

- State survives restarts: Postgres in the `pgdata` volume, the node's disks and runtime files in `nodedata`. Recreating
  the node container stops the guests; with the VM's desired state `RUNNING`, the reconciler boots them again.
- The node reports the host's CPU and memory totals, not the container's `cpus`/`mem_limit` caps, so the scheduler can
  place more than the caps allow. Size VMs within the caps yourself.
- Creating an agent fails until the model exists in Ollama (the name is validated); pull one with
  `docker compose exec ollama ollama pull <model>`.

## Production-style deployment

This section covers running Attic Cloud on real hosts. Addresses are documentation addresses (192.0.2.x), and
`/etc/attic` and `/var/lib/attic` are example paths. Component behaviour is in the linked READMEs; this section
covers how the parts are put together and operated. Host packages and the node's user and kernel requirements are in
[libs/qemu](../libs/qemu/README.md#host-requirements).

### Deployment shapes

| Shape | Processes | Use |
| --- | --- | --- |
| Single host | `vmd --role=all` (control plane and an in-process node), `agentd`, Postgres | One machine, small installs |
| Several hosts | `vmd --role=control` (one or more replicas), `vmd --role=node` on each VM host, `agentd`, Postgres | Production-style installs |
| Partitioned | As above, with several `--postgres-dsn` flags | Spreads projects over several databases; see [Sharding](#sharding-operations) |

Single host:

```
vmd --role=all --postgres-dsn=postgres://attic@192.0.2.20:5432/attic_a \
  --trusted-key=admin=/etc/attic/admin.pub \
  --ssh-key=/etc/attic/ssh_ed25519 --data-dir=/var/lib/attic --run-as=qemu
```

Control plane for two nodes:

```
vmd --role=control --listen=:8080 \
  --postgres-dsn=postgres://attic@192.0.2.20:5432/attic_a \
  --trusted-key=admin=/etc/attic/admin.pub \
  --trusted-key=agentd=/etc/attic/agentd.pub:service \
  --node=node-1=https://192.0.2.11:8443 \
  --node=node-2=https://192.0.2.12:8443 \
  --service-key=/etc/attic/vmd.key --service-key-id=vmd
```

Each node (one per host). A node has no database and no `/metrics`. Its name is the `--node id=URL` the control plane
gives it, so `--node-id` is not needed:

```
vmd --role=node --listen=:8443 \
  --trusted-key=vmd=/etc/attic/vmd.pub:node \
  --ssh-key=/var/lib/attic/ssh_ed25519 --data-dir=/var/lib/attic --run-as=qemu \
  --tls-cert=/etc/attic/node.crt --tls-key=/etc/attic/node.key
```

agentd (its own database, and the control plane's address):

```
agentd --listen=:8081 --postgres-dsn=postgres://attic@192.0.2.21:5432/attic_agentd \
  --trusted-key=admin=/etc/attic/admin.pub \
  --vmd-url=https://192.0.2.10:8080 --ollama-url=http://127.0.0.1:11434 \
  --service-key=/etc/attic/agentd.key --service-key-id=agentd --workers=4
```

Flags are in [vmd](../services/vmd/README.md#configuration) and [agentd](../services/agentd/README.md#configuration).
`vmd -h` and `agentd -h` print them. Migrations run at start-up, before the server listens, so Postgres must be
reachable when a replica starts.

### Network and firewall

- The node API (port 8443 above) accepts only `node`-role tokens. Let only the control plane reach it.
- The control plane listens on the API, `/healthz`, `/readyz` and `/metrics`. Put it on a private address or behind
  TLS. Do not expose it publicly.
- Postgres belongs on a private network with TLS. The development compose file uses `sslmode=disable`; do not copy
  that to production.
- With the firewall on, guests cannot reach private, link-local, shared-address or loopback addresses on the host.
  A resolver on a private address is blocked unless it is passed with `--allow-dns` on the node. See
  [libs/qemu](../libs/qemu/README.md#security-model).
- Prometheus scrapes the control plane only; nodes do not serve `/metrics`.

### TLS and reverse proxies

- `--tls-cert` and `--tls-key` make vmd and agentd serve HTTPS (HTTP/1.1 and HTTP/2). Without them, cleartext HTTP/1.1
  and cleartext HTTP/2 are served. Use cleartext only on a private network.
- The control plane calls nodes with Go's default TLS settings: the system certificate pool, and the standard
  `SSL_CERT_FILE` variable on Linux for a private CA. There is no flag to pin a certificate or skip verification.
- Every RPC is unary. JSON clients (curl, browsers, proxies) need only HTTP/1.1. gRPC clients need HTTP/2, so a proxy
  in front of them must speak HTTP/2 (ALPN `h2` over TLS).
- Set the proxy's upstream read timeout above 30 seconds. `wait_seconds` is capped at 30 seconds, and a proxy that gives
  up earlier returns an error for a call that would have succeeded.
- Allow request bodies of at least 12 MiB. `PutFile` accepts 8 MiB of content, which is about 11.2 MiB once base64
  encoded in JSON.

### Keys, tokens and roles in production

Keep one key id per purpose, so that each can be limited and rotated on its own:

| Key id | Private key | Trusted by | Role limit |
| --- | --- | --- | --- |
| `admin` | Signing workstation only | vmd, agentd | None: operators' tokens |
| `agentd` | agentd host | vmd | `service` |
| `vmd` | Control plane only | Every node | `node` |

- Verifiers hold only `.pub` files. A `.key` file stays on the machine that mints tokens with it.
- Mint short-lived tokens. Prometheus needs a token with `*:operator` (see [Prometheus](#prometheus)).
- Create the revocation file before starting vmd and agentd, because each refuses to start without it. An empty file
  is fine. Pass it with `--revoked-tokens`. A single token is revoked by appending its `jti`.
- Rotation and the reasons for each role are in [libs/authz](../libs/authz/README.md#key-rotation) and
  [What a leaked key allows](../libs/authz/README.md#what-a-leaked-key-allows).
- Record which key ids exist, which servers trust each one and with which roles.

### Prometheus

Mint a token for the scraper. The token needs `capacity.get` on `*`, which the `operator` role grants. Its lifetime
must not exceed the server's `--max-token-ttl`, which defaults to 24 hours:

```
atticctl token mint -key /etc/attic/admin.key -kid admin \
  -sub svc:prometheus -type service -bind '*:operator' -ttl 24h > /etc/prometheus/attic.token
chmod 0600 /etc/prometheus/attic.token
```

Re-mint the token daily, or raise `--max-token-ttl` on vmd for the scrape period. A missing or invalid token gets
`401`, and a token without `capacity.get` gets `403`.

Scrape configuration (standard Prometheus syntax):

```yaml
scrape_configs:
  - job_name: attic-vmd
    scheme: https                 # http if vmd runs without TLS
    metrics_path: /metrics
    scrape_interval: 30s
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/attic.token
    static_configs:
      - targets: ["192.0.2.10:8080"]
```

Metric names and labels are in [vmd](../services/vmd/README.md#observability) and
[agentd](../services/agentd/README.md#observability).

Example alert rules. The thresholds are starting points:

```yaml
groups:
  - name: attic
    rules:
      - alert: AtticNodeDown
        expr: attic_node_up == 0 and attic_node_draining == 0
        for: 2m
        labels: {severity: page}

      - alert: AtticNodeDrainedLongTerm
        expr: attic_node_draining == 1
        for: 24h
        labels: {severity: warn}
        annotations:
          summary: "A node has been draining for a day. Undrain it or plan its maintenance."

      - alert: AtticNoCapacity
        expr: attic_cluster_cpu_available_vcpus <= 0 or attic_cluster_memory_available_bytes < 1073741824
        for: 10m
        labels: {severity: warn}
        annotations:
          summary: "No CPU, or less than 1 GiB of memory, left to start VMs. Pending VMs wait for capacity."

      - alert: AtticVMInError
        expr: attic_vms{state="error"} > 0
        for: 5m
        labels: {severity: page}
        annotations:
          summary: "A VM is in ERROR (crash loop or failed step). Read its state_reason."

      - alert: AtticReconcileErrors
        expr: sum(rate(attic_reconcile_vm_total{outcome="error"}[10m])) > 0.05
        for: 10m
        labels: {severity: warn}

      - alert: AtticReconcilerStalled
        expr: rate(attic_reconcile_ticks_total[5m]) == 0
        for: 5m
        labels: {severity: page}

      - alert: AtticNodeDiskNearlyFull
        expr: attic_node_disk_free_bytes / attic_node_disk_total_bytes < 0.10
        for: 15m
        labels: {severity: warn}

      - alert: AtticGuestFilesystemNearlyFull
        expr: attic_vm_filesystem_used_bytes / attic_vm_filesystem_size_bytes > 0.90
        for: 15m
        labels: {severity: warn}

      - alert: AtticInternalErrors
        expr: sum(rate(attic_rpc_requests_total{code="internal"}[5m])) > 0
        for: 5m
        labels: {severity: warn}
        annotations:
          summary: "API calls return internal errors. Check Postgres and the server log."

      - alert: AtticScrapeErrors
        expr: rate(attic_vmd_scrape_errors_total[5m]) > 0
        for: 10m
        labels: {severity: warn}

      - alert: AtticGuestSamplesStale
        expr: time() - attic_vm_samples_last_timestamp_seconds > 300
        for: 5m
        labels: {severity: warn}

      - alert: AtticAgentLeaseLost
        expr: increase(attic_agent_lease_lost_total[10m]) > 0
        labels: {severity: warn}
        annotations:
          summary: "An agentd worker lost a task lease. Check worker health and Postgres latency."

      - alert: AtticAgentTaskError
        expr: increase(attic_agent_tasks_finished_total{state="error"}[30m]) > 0
        labels: {severity: warn}
        annotations:
          summary: "Agent tasks ended in ERROR. Read the task result."
```

Not covered by these rules: vmd exports no crash counter and no job or task queue gauge. A VM whose start fails
transiently (for example a missing `genisoimage`) stays `stopped` with a `state_reason`, so watch
`attic_reconcile_vm_total{outcome="error"}` for those.

### Capacity planning

Per node, with the defaults `--cpu-overcommit=1`, `--memory-overcommit=1` and `--reserve-memory-mb=2048`:

- CPU allocatable = floor(threads x cpu-overcommit).
- Memory allocatable = (RAM in MB - reserve-memory-mb) x memory-overcommit.
- Disk has no overcommit. A new disk needs its size free on the node.

Example: a host with 32 threads and 128 GiB of RAM (131072 MB) has 32 vCPUs and 129024 MB allocatable with the defaults.

Rules that matter when sizing:

- Only VMs whose desired state is running count against CPU and memory. A stopped VM takes no budget.
- CPU and memory are checked when a VM starts, not when it is created. A VM that does not fit waits in `PENDING` with
  `waiting for capacity: ...` and is checked every 3 seconds.
- A VM stays on the node that holds its boot disk. Nothing moves it, and `scheduler.Pick` is not used for placement.
  Plan capacity per node.
- An image exists only on the node where it was registered. Register a shared image on every node that will run its VMs.
  Images are not replicated.
- Overcommit above 1 raises density and removes the margin. Watch the available metrics before raising it:
  `attic_node_cpu_available_vcpus` and `attic_node_memory_available_bytes`.
- Quotas (`--quota-vms`, `--quota-vcpus`, `--quota-memory-mb`, `--quota-disk-gb`) limit each project. They are unlimited
  by default. See [vmd Quotas](../services/vmd/README.md#quotas).

### Sharding operations

- Each `--postgres-dsn` is one database. Projects are placed by rendezvous hashing over the DSNs in the order given.
  The first DSN is shard 0.
- The set and order of DSNs are the data layout. Choose them before the first write. Changing either re-routes projects,
  and rows already written become unreachable. There is no rebalancing tool. Moving data needs a manual migration.
- Node rows and shared images (empty project) live on shard 0. Operations that need them, such as placing a VM on a
  node or listing images, need shard 0 to be up.
- Each project's transactions stay on one database. If a database is down, calls for its projects fail until it
  returns. `/readyz` reports the failing `postgres-N` check.
- Resource IDs carry the shard, for example `vm_<ulid>-s02`. An ID without a suffix is looked up on every shard.
- Every database is migrated at start-up. Back up all of them together (see [Backup and restore](#backup-and-restore)).

### Backup and restore

Back up three things together, and restore them together:

1. Postgres: every vmd database (tables and `schema_migrations_vmd`) and the agentd database
   (`schema_migrations_agentd`), with `pg_dump` or continuous archiving. Restore the same set of databases in the
   same DSN order.
2. Each node's `--data-dir`: `images/`, `disks/`, `vms/` and `shared/`. An `images/<id>.qcow2` that is a symlink points
   outside the data directory, so back up the target too. Stop a VM before copying its disk, or use
   `DiskService.SnapshotDisk` on a stopped VM, which produces a consistent image.
3. Signing keys: the `.key` files. They cannot be recovered. Without them no new token can be minted for their key id.
   Existing public keys keep accepting tokens until those expire.

After a restore, start the control plane. The reconciler works towards the restored desired state. Check
`attic_reconcile_ticks_total` is rising and that VM `state_reason` values clear. Files on a node that the restored
database does not know about are not tracked by vmd and must be removed by hand. Guest job directories are short-lived
and are not backed up.

### Upgrades

- Migrations are forward-only. Only `NNN_name.up.sql` files exist and are applied in numeric order. There are no down
  migrations. Each runs in its own transaction, and its version is recorded in the database.
- Migrations run at start-up under a Postgres advisory lock. Several replicas may start together: one migrates and the
  others wait. Back up every database before an upgrade.
- Control plane replicas are stateless. Upgrade them one at a time: start the new binary (it migrates first), then stop
  an old one. Leases move the work to whichever replica is up.
- Node daemons can restart without stopping VMs. Stop the daemon, install the binary, and start it with the same
  `--data-dir`, `--run-as` and `--ssh-key`. QEMU processes are detached, so VMs keep running. Calls to that node fail
  with `unavailable` while it restarts, and the reconciler retries.
- The version is set at link time with `-ldflags "-X main.version=..."`. `vmd -version` prints it, and
  `attic_build_info` carries it.

### Draining a node for maintenance

Draining stops new starts on a node. It does not stop or move running VMs, so a host is emptied by stopping its VMs.

1. Drain the node. Its effective state becomes `draining` and `attic_node_draining` becomes 1:

   ```
   curl -sS -X POST http://192.0.2.10:8080/attic.vmd.v1.ClusterService/SetNodeDraining \
     -H "Authorization: Bearer $OPS_TOKEN" -H "Content-Type: application/json" \
     -d '{"nodeId":"node-1","draining":true}'
   ```

   The token needs `node.drain` on `*` (the `operator` role).
2. List the VMs on the node. `ListVMs` has no node filter, so list each project and keep those whose `nodeId` is the
   node. Write the IDs down.
3. Stop each VM and wait for it to stop. `StopVM` sets the desired state to stopped, so the VM stays stopped after the
   node is undrained:

   ```
   call VMService/StopVM '{"id":"vm_..."}'
   call VMService/GetVM '{"id":"vm_...","waitForState":"VM_STATE_STOPPED","waitSeconds":30}'
   ```

4. Confirm the node is empty. `attic_node_vms_running` for the node is 0, or `GetCapacity` shows `vmsRunning` 0 for it.
5. Do the maintenance. The node daemon can be restarted at this point.
6. Undrain (`"draining":false`), then start each VM from the list with `VMService/StartVM`.

If the host reboots with VMs still running, each VM counts a crash when the node answers. It is restarted, which is
why stopping VMs first matters.

## Troubleshooting

Start with the health endpoints. `/healthz` answers 200 whenever the process runs. `/readyz` answers 200 with
`{"status":"ok"}`, or 503 with the failed checks listed (`postgres-N` and `store` on the control plane, `postgres` on
agentd). The node daemon's `/readyz` has no checks.

| Symptom | Look at | Likely cause and action |
| --- | --- | --- |
| `401 unauthenticated` on every call | vmd log: `authentication failed` with `err` | Expired, revoked (`token revoked`), signed by an untrusted key id, or lifetime above `--max-token-ttl`. Mint a new token with `atticctl token inspect` to check it. |
| `403` on `/metrics` | Token bindings | The token lacks `*:operator`. |
| `permission_denied` on a create or list | Token bindings | No binding for that project, or the role lacks the permission. See [roles](../libs/authz/README.md#roles-and-permissions). |
| `not_found` for an ID you created | Project of the token | The token has no binding on the resource's project. Other tenants' IDs look the same. |
| `internal` errors, `/readyz` 503 | `/readyz` `failed` list; the log's `claim vms` or `update vm` errors | Postgres is down or unreachable. Restore it. Running VMs keep running. |
| VM `PENDING`, `waiting for capacity: node is draining` | `attic_node_draining`, `GetCapacity` | The node is drained. Undrain it when maintenance is done. |
| VM `PENDING`, `waiting for capacity: not enough memory: ...` | `GetCapacity` for the node | No room on the node. Stop VMs, add capacity, or change overcommit. |
| VM `PENDING`, `waiting for disk <id>` | Disk state | The disk is still `DISK_STATE_CREATING`. If it stays there, check the node. |
| VM `stopped` or `pending`, `node unreachable: <node>` | `attic_node_up`, the node's log | The node is down or silent for more than 15 s. Restore the node or its network. Reconciliation resumes. |
| Reconcile warnings `reconcile failed` | `err` field in the log; `attic_reconcile_vm_total{outcome="error"}` | Transient errors such as a missing `genisoimage` or `qemu-img`. Install the tool. Retries continue. |
| VM `ERROR` with `VM exited unexpectedly: ...` | `state_reason`; `vms/<id>/qemu.log` on the node | Crash loop: four crashes. Fix the cause, then `StartVM` (it clears the error and retries at once). |
| VM `ERROR` with `stop timed out, killed` | The guest's shutdown | The guest did not power off within 60 s. It was killed. Check for a hung process. |
| VM stays `STARTING` and the node reports `slow boot` | The guest image | The guest did not answer SSH within 5 minutes. Check the image for an SSH server, the `agent` user and cloud-init; see [libs/qemu](../libs/qemu/README.md#guest-requirements). |
| Exec or file calls return `unavailable` | Node reachability | The node is down or restarting. Calls resume when it answers. |
| Exec or file calls return `failed_precondition` | VM state | The VM is not `RUNNING`. Start it first. |
| Job `LOST` | `killed_reason` | The VM stopped or rebooted while the job ran, or the job directory was pruned. Run it again with a new `idempotencyKey`. |
| Job `FAILED` with `could not start: ...` | `killed_reason` | The command never reached the guest. Check the node and the VM. |
| Image `IMAGE_STATE_ERROR` | `stateReason`; the node's log | The file is missing or is not qcow2. Restore the file, or delete the image and register it again. |
| Disk `DISK_STATE_ERROR` | `stateReason` on the disk; the VM's `state_reason` | The node could not create or adopt the file. Fix the cause, or delete the disk and create it again. |
| `resource_exhausted` | Quota flags; the message names the limit | A project quota is reached. Raise the flag, or free resources. Lowering a flag does not stop existing VMs. |
| `aborted` on `UpdateVM` | `generation` | `expectedGeneration` is stale. Read the VM again and retry. |
| Agent task `ERROR` with `model error: ...` | Task `result`; the agentd log | Ollama returned an error or the model is missing. Install the model, or update the agent. |
| `attic_agent_lease_lost_total` rising | Worker and Postgres health | A worker stalled, for example through a Postgres failover. Another worker takes the task over. |
| Node `down` in logs and `attic_node_up` 0 | `node down` WARN log | Silent for more than 15 s. The drain flag survives the outage. |
