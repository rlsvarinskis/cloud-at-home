# Operations

This document covers running homecloud: deployment shapes, the host setup a node needs, how the reconciler
behaves, what happens when parts fail, metrics and alerts, capacity, backups and upgrades. Flags are those of
`cmd/vmd` (`vmd -h`), `cmd/agentd` (`agentd -h`) and `cmd/hcctl`.

## Contents

- [Deployment shapes](#deployment-shapes)
- [Node host requirements](#node-host-requirements)
- [Running the control plane](#running-the-control-plane)
- [Running a node](#running-a-node)
- [Running agentd](#running-agentd)
- [VM lifecycle and the reconciler](#vm-lifecycle-and-the-reconciler)
- [Node lifecycle](#node-lifecycle)
- [Failure behaviour](#failure-behaviour)
- [Prometheus](#prometheus)
- [Metric catalogue](#metric-catalogue)
- [Example alert rules](#example-alert-rules)
- [Capacity planning](#capacity-planning)
- [Backups](#backups)
- [Upgrades](#upgrades)

## Deployment shapes

homecloud has three roles for `vmd`, chosen with `--role`:

| `--role` | Runs | Use |
| --- | --- | --- |
| `all` (default) | Control plane and an in-process node. | One host. Development, or a small single-host install. |
| `control` | Control plane only. Nodes are reached over the node API with `--node id=URL`. | Several hosts. Run one or more replicas. |
| `node` | Node daemon only. Serves the node API for one host. | One per physical host that runs VMs. |

`agentd` is a separate process (`cmd/agentd`). It calls vmd's API with a service token that it mints with its own
key. See [Running agentd](#running-agentd).

### 1. Single host, all in one

```
vmd --role=all --postgres-dsn=postgres://... \
  --trusted-key=admin=/etc/homecloud/admin.pub \
  --ssh-key=/etc/homecloud/ssh_ed25519 \
  --data-dir=/var/lib/homecloud --run-as=qemu
```

`--role=all` requires `--ssh-key` and, unless `--no-firewall` is given, root or `CAP_NET_ADMIN` for the
network rules. Without `--postgres-dsn` (or `HOMECLOUD_POSTGRES_DSN`) state is kept in memory and lost on
restart; vmd logs a warning.

The repository's `deploy/docker-compose.yml` starts `vmd --role=all` without `--ssh-key`. By the code in
`cmd/vmd`, that exits at start-up with `-ssh-key is required for a node`. Add `--ssh-key` (with a key file in
the container) before using the compose stack to run guests. The compose file also expects an `agentd` image
built from `cmd/agentd`.

### 2. Control plane and several nodes

Control plane (one or more replicas; they share one Postgres):

```
vmd --role=control \
  --postgres-dsn=postgres://... \
  --trusted-key=admin=/etc/homecloud/admin.pub \
  --trusted-key=agentd=/etc/homecloud/agentd.pub:service \
  --node=node-1=http://node-1.internal:8443 \
  --node=node-2=http://node-2.internal:8443 \
  --service-key=/etc/homecloud/vmd.key --service-key-id=vmd
```

At least one `--node` is required with `--role=control`. Each `--node id=URL` names a node. The id is the
control plane's name for that node; a node's own `--node-id` is informational. The control plane needs
`--service-key` (and `--service-key-id`, default `vmd`) to call nodes. Each node must trust that key.

Each node (one per host):

```
vmd --role=node --listen=:8443 \
  --trusted-key=vmd=/etc/homecloud/vmd.pub:node \
  --ssh-key=/var/lib/homecloud/ssh_ed25519 \
  --data-dir=/var/lib/homecloud --run-as=qemu \
  --tls-cert=/etc/homecloud/node.crt --tls-key=/etc/homecloud/node.key
```

Node daemons do not serve `/metrics`; the control plane collects everything operators need.

### 3. Postgres partitioned by project

The package `internal/vmd/store/shard` implements a store that spreads projects over N databases with
rendezvous hashing. Node rows, shared images and rows with an empty project live on shard 0. An ID created by
the shard store carries its shard number, so ID lookups go straight to the right database. Operations that cannot
name a project (for example `ClaimVMs`) fan out and merge.

This package is covered by tests but is **not wired into `cmd/vmd`**: there is no flag for it. Today a
deployment uses one Postgres database.

## Node host requirements

A node needs the following on the host. The names are the programs the node runs.

| Requirement | Why | Source |
| --- | --- | --- |
| Linux on x86_64 | The QEMU command is `qemu-system-x86_64`. | `internal/vmd/node/qemu` |
| `qemu-system-x86_64` | Runs the guests. | `qemu/args.go` |
| `/dev/kvm` and `--kvm=true` (default) | Hardware acceleration. `--kvm=false` uses TCG emulation, which is much slower. | `cmd/vmd` flag `-kvm` |
| `qemu-img` | Creates disks, reads image metadata, resizes, flattens snapshots. | `qemudriver/disks.go` |
| `genisoimage` | Builds the cloud-init seed ISO at every VM start. Missing, starts fail with the message `build seed ISO: genisoimage ... not found`. | `qemudriver/vm.go` |
| `iptables` and `ip6tables` | Guest network lockdown, unless `--no-firewall`. | `qemudriver/firewall.go` |
| Root, or `CAP_NET_ADMIN` for the firewall and `CAP_CHOWN` for the `chown` of the shared folder when `--run-as` is set | Installs the firewall rules and hands `shared/` to the run-as user. | `qemudriver/driver.go`, `qemudriver/firewall.go` |
| A user named by `--run-as` (default `qemu`) | QEMU processes run as this user, and the shared folder belongs to it. Use `--run-as=` (empty) to run as the current user, for development. | `cmd/vmd` flag `-run-as` |
| `--data-dir` (default `/var/lib/homecloud`) writable by the node | All state (below). | `qemudriver/driver.go` |
| An SSH key at `--ssh-key` | The control plane's guest access key. Created with mode 0600 if missing, and its public half is written to `<path>.pub`. The public key is injected into each guest for user `agent`. | `sshrunner/sshrunner.go` |

### Network lockdown

With the firewall on, the node installs a chain (`SBX`) in `OUTPUT` that applies to the run-as user's UID. It
rejects traffic from guests to `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `169.254.0.0/16`,
`100.64.0.0/10`, `127.0.0.0/8`, `::1/128`, `fc00::/7` and `fe80::/10`. `--allow-dns` (repeatable) names resolvers
that guests may still reach. Accept rules come before the rejects.

### Data directory

```
<data-dir>/
  images/<id>.qcow2      a symlink to a registered image, or a flattened snapshot
  disks/<id>.qcow2       VM disks, optionally overlays on an image
  shared/                mounted at /shared in every guest (--shared-dir overrides the location)
  vms/<id>/spec.json     what the VM was started with
  vms/<id>/runtime.json  pid, SSH port, start time, stop request, mount state
  vms/<id>/seed.iso, user-data, meta-data, console.log, qemu.log
```

`images/<id>.qcow2` is a symlink when the image was registered from an existing file, so the file it points to is
outside the data directory. See [Backups](#backups).

### Guest image

The image must be a qcow2 file with a cloud-init-capable Linux that the node can boot. The node's cloud-init
(`qemu/args.go`) does the following, so the image must support it:

- creates user `agent` with passwordless `sudo` and the node's SSH public key, and disables password logins;
- runs an SSH server that listens on port 22 (the node forwards a loopback port to it);
- supports the 9p filesystem over virtio, for `/shared`.

The job scripts and the mount script run these programs in the guest: `bash`, `setsid`, `timeout`, `base64`,
`stat`, `tail`, `cat`, `mkdir`, `mv`, `kill`, `pgrep`, `mountpoint`, `sudo`, `date`, `chmod`, `poweroff`. Guests
stop with `sudo poweroff`. Extra packages can be installed through `GuestPackages` in code; `cmd/vmd` does not
set it.

Each running VM gets a free loopback SSH port on `127.0.0.1` (at 1024 or above), forwarded to its port 22. The
node keeps the ports it has handed out, and a restarted node re-reserves the ports of VMs still running.

## Running the control plane

Flags that matter in production (see `vmd -h` for all):

| Flag | Default | Meaning |
| --- | --- | --- |
| `--listen` | `:8080` | Address for the API, `/healthz`, `/readyz` and `/metrics`. |
| `--postgres-dsn` | `$HOMECLOUD_POSTGRES_DSN` | Postgres for the control plane. Empty means memory. |
| `--trusted-key` | none | `kid=path[:role,role]`, repeatable. At least one is required. |
| `--max-token-ttl` | `24h` | Longest token lifetime accepted. |
| `--tls-cert`, `--tls-key` | none | Serve TLS. Without them, HTTP/1.1 and cleartext HTTP/2 are served. |
| `--metrics` | `true` | Serve `/metrics` (needs an operator token). |
| `--cpu-overcommit`, `--memory-overcommit`, `--reserve-memory-mb` | `1`, `1`, `2048` | Capacity rules, see [Capacity planning](#capacity-planning). |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error`. |

Migrations run at start-up, before the server listens. Postgres must be reachable then.

### Health endpoints

- `/healthz` always answers `200 ok`.
- `/readyz` answers `200 {"status":"ok"}` when its checks pass, or `503 {"status":"unavailable","failed":[...]}`
  naming the failed checks. The checks are `postgres` (a ping, when a DSN is set) and `store` (a read of the node
  list). Each check has a 2 s timeout. Error text is not returned.

### Database schema

vmd keeps its tables in its own database under the migration record `schema_migrations_vmd`. Migrations are
the files `NNN_name.up.sql` embedded in the binary, applied in numeric order. Each runs in its own transaction.

## Running a node

See [Node host requirements](#node-host-requirements) and the `--role=node` example above. The node has no
database. Its state is in `--data-dir`, which must be an absolute path. A node logs `listening` when it
starts. Reconcile failures appear in the control plane's log as `reconcile failed`.

## Running agentd

`agentd` (from `cmd/agentd`) serves the Agent, Task and Model APIs and runs task workers. It reaches vmd over
vmd's API with a service token. It needs its own private key for that, and it verifies user tokens with its own
trusted keys.

```
agentd --listen=:8081 \
  --postgres-dsn=postgres://... \
  --trusted-key=admin=/etc/homecloud/admin.pub \
  --vmd-url=https://vmd.internal:8080 \
  --ollama-url=http://127.0.0.1:11434 \
  --service-key=/etc/homecloud/agentd.key --service-key-id=agentd \
  --workers=4
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--listen` | `:8081` | Address for the API, `/healthz`, `/readyz` and `/metrics`. |
| `--postgres-dsn` | `$HOMECLOUD_POSTGRES_DSN` | Postgres for agents and tasks. Empty means memory. Migrations run at start-up. |
| `--trusted-key` | none | `kid=path[:role,role]` for user tokens. Repeatable. |
| `--max-token-ttl` | `24h` | Longest user token lifetime accepted. |
| `--vmd-url` | none (required) | Base URL of vmd's API. |
| `--ollama-url` | `http://127.0.0.1:11434` | Base URL of the Ollama model server. |
| `--service-key` | none (required) | Private key that agentd signs its service tokens with. |
| `--service-key-id` | `agentd` | Key id of `--service-key`. vmd must trust this kid with the `service` role. |
| `--workers` | `2` | Tasks run concurrently by this process. |
| `--metrics` | `true` | Serve `/metrics` (operator token). |
| `--tls-cert`, `--tls-key` | none | Serve TLS. |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error`. |

vmd side: vmd must trust agentd's public key with the `service` role, for example
`--trusted-key=agentd=/etc/homecloud/agentd.pub:service`.

Service tokens: agentd mints tokens with subject `svc:agentd`, type `service`, binding `*:service` and a five-minute
lifetime. The `service` role on vmd is what lets agentd place holds, run commands and read files, and list models.

Readiness: `/readyz` checks Postgres only (when a DSN is set).

Workers: each worker claims one queued task (or a task whose lease expired) at a time. The lease lasts 30 s and is
renewed every 10 s. The lease owner is the host name and process id. Workers poll the queue every 500 ms when
idle. A task keeps its lease while it runs; if the process dies, another worker takes the task over after the lease
expires.

Models: `CreateAgent` and `UpdateAgent` check that the model is installed on the Ollama server. If Ollama cannot be
reached at that moment the check is skipped, and the task fails later if the model is missing.

## VM lifecycle and the reconciler

A VM has two power states. The **desired** state (`desired_state`) is set by the API: `running` or `stopped`
(a VM being deleted is internally `deleted`). The **observed** state (`state`) is what the reconciler has seen:
`PENDING`, `STOPPED`, `STARTING`, `RUNNING`, `STOPPING`, `ERROR`, `DELETING`. The reconciler does one step at a
time to move observed towards desired. Each step is idempotent.

### Loop

Every second (`ReconcileInterval`) one tick runs:

1. Images in `CREATING` are registered or snapshotted on their node; images in `DELETING` are removed.
2. Disks in `CREATING` are created; disks in `DELETING` are removed.
3. Up to 32 VMs whose next check is due and whose lease has expired are claimed (`lease 30 s`), and up to 8 of
   them are reconciled in parallel.

Leases make several control-plane replicas safe: each VM is worked on by one replica at a time, and a
replica that dies loses its leases after 30 s.

### Steps for one VM

- **Pending.** Waits until all its disks are `READY`. A disk in error makes the VM `ERROR`. Then the VM becomes
  `STOPPED`.
- **Desired running.**
  - Runtime `RUNNING`: state `RUNNING`, checked again every 10 s (`ResyncInterval`).
  - Runtime `BOOTING`: state `STARTING`.
  - Runtime absent or exited: the node has to start it. If the node lacks CPU or memory, the VM goes to
    `PENDING` with `state_reason` `waiting for capacity: ...` and is re-checked every 3 s. Otherwise the node
    starts QEMU and the VM is `STARTING`.
- **Desired stopped.** A running guest is stopped: gracefully, or with a kill if `StopVM` used `force`. The state
  is `STOPPING` until the process is gone, then `STOPPED`. A graceful stop that takes longer than 60 s is killed,
  and the reason becomes `stop timed out, killed`. The reason after a normal stop is `stopped`.
- **Desired deleted.** The guest is killed. Its runtime files are removed. Disks are detached, and with
  `delete_disks` they are marked for deletion. The VM row is removed.

### Crashes and backoff

- A VM whose QEMU process has exited while it was `RUNNING` or `STARTING` counts a crash (`Crashes`), and the
  reason becomes `VM exited unexpectedly: ...` with the last lines of `qemu.log`. This includes a node or host
  reboot, which leaves the process gone.
- A VM is restarted after a crash, unless it has crashed more than three times. Then it is `ERROR` and retried
  with backoff. The backoff doubles from 1 s and is capped at 60 s, counted from the crash count.
- A healthy `RUNNING` VM forgives one crash each time it is checked after a resync interval (10 s).
- `StartVM` on a VM in `ERROR` clears the crash count and retries at once.
- An error from the node is retried with backoff (the same schedule as above). Errors classed as unavailable get
  the `state_reason` prefix `node unreachable:`. That includes host-tool failures such as a missing `genisoimage`.
- An error that is not transient (not found, invalid argument, failed precondition, already exists, permission
  denied) puts the VM in `ERROR`. The VM is checked again every 60 s, in case the cause was fixed outside homecloud.

Note: a start that fails with a transient error (for example, `genisoimage` missing) leaves the VM in its
previous observed state (`STOPPED`) with a `state_reason`, not in `ERROR`. Alerts on `state="error"` do not see
it; alert on `state_reason` or on reconcile failures instead (see [Example alert rules](#example-alert-rules)).

There is no log line at `ERROR` level for a crash loop. The crash loop is visible as the VM's `ERROR` state.
Reconcile failures are logged at `WARN` with the VM id and failure count.

### Jobs and VMs

Jobs live in the guest. A job whose VM is no longer `RUNNING` is marked `LOST` the next time a job call reads it
(`PollJob` or `CancelJob`). Nothing marks it in the background. The `killed_reason` says why, for example
`vm stopped: ...`.

## Node lifecycle

A node is `ready`, `draining` or `down`.

- `ready`: takes new VMs and disks.
- `down`: the control plane has not heard from it for more than 15 s. The control plane polls each node
  every 5 s with a 5 s timeout. A node that answers again becomes `ready`.
- `draining`: meant to keep its existing VMs and take no new ones. **No API or flag sets this state.** Only the
  store can write it. Do not rely on draining in this version.

A node's VMs stay in their last observed state while it is down. The reconciler retries them with backoff.

## Failure behaviour

| Failure | What happens | What to do |
| --- | --- | --- |
| **Node daemon unreachable** (host down, network, daemon stopped) | The control plane marks the node `down` after 15 s of silence. Its VMs keep their last observed state, with `state_reason` `node unreachable: ...`. The reconciler retries with backoff (1 s to 60 s). Exec, file and metrics calls to its VMs fail with `unavailable`. The node's capacity is left out of cluster totals. A VM can still be created on it, but its disks are not created until the node answers. | Restore the node. Reconciliation resumes on its own. |
| **Node daemon restarted** | QEMU processes are detached, so VMs keep running. On start, the new driver re-reserves the SSH ports of live VMs and reads their runtime files. Guest jobs continue. Calls to that node fail with `unavailable` while it restarts, and the reconciler retries. | Nothing, normally. Check that `run-as` and `data-dir` are unchanged. |
| **Control-plane replica dies** | Its leases on VMs and tasks expire after 30 s, and another replica takes them over. Requests it was serving fail. Clients retry; create calls are safe to retry with an idempotency key. | Restart it. Other replicas keep serving. |
| **All control-plane replicas down** | No API calls and no reconciliation. VMs keep running. | Restore at least one replica. |
| **Postgres down** | `/readyz` returns `503`, listing `postgres` and/or `store`. API calls that touch the database return `internal`, because database errors are not mapped to `unavailable`. The reconciler logs `claim vms` errors each tick. Running VMs keep running. No state is lost. | Restore Postgres. Reconciliation resumes. |
| **agentd worker dies mid-task** | Its lease expires after 30 s, and another worker claims the task. The attempt count rises. The task resumes from the stored messages. A command that was running in the guest is found again by its idempotency key, so it is not started twice. After more than three takeovers the task ends in `TASK_STATE_ERROR`. | Nothing, normally. |
| **VM crashes** (QEMU exits) | The reconciler restarts it, counting the crash. After more than three crashes the VM is `ERROR`, retried with backoff. Guest jobs become `LOST` when they are next read. | Read `state_reason` and the VM's `qemu.log` on the node. Use `StartVM` after fixing the cause. |
| **Guest powers itself off** | The QEMU process exits without a stop request from vmd. The reconciler treats that as a crash and restarts the VM, as long as its desired state is `RUNNING`. Its running jobs become `LOST`. | Stop the VM through the API (`StopVM`) instead of powering off inside the guest. |
| **Host reboot** | QEMU processes are gone, so VMs that were running count as crashed and are restarted by the reconciler when the node answers. | Check that the VMs come back. |
| **Image or disk file missing on the node** | An image registered from a missing file goes to `IMAGE_STATE_ERROR`. A VM whose disk is `DISK_STATE_ERROR` goes to `ERROR` with the disk's reason. A VM whose disk is still being created waits in `PENDING` with `waiting for disk ...`. | Restore the file, or delete the resource and register or create it again. |

## Prometheus

The control plane exposes `/metrics` on the API listener when `--metrics` is on (the default). Each request
must carry `Authorization: Bearer <token>`. A missing or invalid token gets `401`. A valid token without
the `capacity.get` permission on project `*` gets `403`. The `operator` role grants that permission.

Mint a token for Prometheus:

```
hcctl token mint -key /etc/homecloud/admin.key -kid admin \
  -sub svc:prometheus -type service -bind '*:operator' -ttl 24h > /etc/prometheus/homecloud.token
chmod 0600 /etc/prometheus/homecloud.token
```

The `-ttl` must not exceed the server's `--max-token-ttl`, which is 24 hours by default. Either re-mint the
token daily, or raise `--max-token-ttl` on vmd for the scrape period.

Scrape configuration (standard Prometheus syntax; not part of this repository):

```yaml
scrape_configs:
  - job_name: homecloud-vmd
    scheme: https                 # http if vmd runs without TLS
    metrics_path: /metrics
    scrape_interval: 30s
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/homecloud.token
    static_configs:
      - targets: ["vmd.internal:8080"]
```

## Metric catalogue

All metric names start with `homecloud_`, except the standard Go and process collectors. The control
plane registers only the collectors listed here.

The names and meanings below are from `internal/vmd/promcollect` (`descs.go`, `promcollect.go`, `capacity.go`
and `sampler.go`).

### RPC metrics (every API server)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `homecloud_rpc_requests_total` | counter | `server`, `service`, `method`, `code` | Completed server-side RPCs, by Connect code. `code="ok"` is success. |
| `homecloud_rpc_duration_seconds` | histogram | `server`, `service`, `method` | Duration in seconds. Buckets from 5 ms to 30 s. Streaming calls are timed for the whole stream. |
| `homecloud_rpc_in_flight` | gauge | `server`, `service`, `method` | RPCs being handled now. |
| `homecloud_build_info` | gauge | `version`, `go_version` | Always 1. Identifies the build. |

`server` is `vmd` or `agentd`.

### Node and cluster capacity (per scrape, from the store)

Units: CPU counts are threads or vCPUs; memory is MiB; disk is bytes.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `homecloud_node_up` | `node` | 1 if the node is `ready`, 0 if draining or down. |
| `homecloud_node_draining` | `node` | 1 if the node is draining. |
| `homecloud_node_cpus_total` | `node` | Physical CPU threads on the node. |
| `homecloud_node_cpus_allocated` | `node` | vCPUs of VMs that should be running on the node. |
| `homecloud_node_cpus_available` | `node` | vCPUs the scheduler can still allocate, after overcommit. |
| `homecloud_node_memory_mb_total` | `node` | Physical memory, MiB. |
| `homecloud_node_memory_mb_allocated` | `node` | Memory of VMs that should be running, MiB. |
| `homecloud_node_memory_mb_available` | `node` | Memory the scheduler can still allocate, MiB, after the reserve and overcommit. |
| `homecloud_node_disk_bytes_total` | `node` | Disk capacity on the node. |
| `homecloud_node_disk_bytes_free` | `node` | Free disk on the node for new disks. |
| `homecloud_node_vms_total` | `node` | VMs placed on the node, any state. |
| `homecloud_node_vms_running` | `node` | VMs on the node in state `RUNNING`. |
| `homecloud_node_last_heartbeat_timestamp_seconds` | `node` | Unix time of the last successful poll; 0 if it never answered. |
| `homecloud_cluster_cpus_total`, `..._cpus_allocated`, `..._cpus_available` | none | The same, summed over ready nodes. |
| `homecloud_cluster_memory_mb_total`, `..._memory_mb_allocated`, `..._memory_mb_available` | none | Summed over ready nodes, MiB. |
| `homecloud_cluster_disk_bytes_total`, `..._disk_bytes_free` | none | Summed over ready nodes. |
| `homecloud_cluster_vms_total`, `..._vms_running` | none | Summed over ready nodes. |
| `homecloud_cluster_nodes_ready` | none | Nodes in state `ready`. |
| `homecloud_vms` | `state` | VMs per observed state across all nodes. Every state is present, zeros included. |
| `homecloud_vms_desired_running` | none | VMs whose desired state is running. |

`state` values for `homecloud_vms`: `pending`, `stopped`, `starting`, `running`, `stopping`, `error`, `deleting`.

### Guest metrics (sampled, per running VM)

Labels: `vm_id`, `project`, `name` (the VM name), `node`. Filesystem series add `disk_id`, `device` and
`mount_point`. Disk series add `disk_id`. Values come from a cache filled every 30 s
(`SampleInterval`). A VM's sample expires after three intervals without a new one, and VMs that have stopped
are removed.

| Metric | Unit | Meaning |
| --- | --- | --- |
| `homecloud_vm_cpu_percent` | percent | Guest CPU use, like `top`: 100 per busy vCPU. An 8-vCPU guest can reach 800. |
| `homecloud_vm_cpu_cores` | count | vCPUs the guest sees. |
| `homecloud_vm_load1`, `_load5`, `_load15` | load average | Guest load over 1, 5 and 15 minutes. |
| `homecloud_vm_memory_total_bytes` | bytes | Guest memory. |
| `homecloud_vm_memory_used_bytes` | bytes | Guest memory in use. |
| `homecloud_vm_memory_available_bytes` | bytes | Guest memory available for new work. |
| `homecloud_vm_swap_total_bytes`, `homecloud_vm_swap_used_bytes` | bytes | Guest swap. |
| `homecloud_vm_uptime_seconds` | seconds | Guest uptime. |
| `homecloud_vm_metrics_age_seconds` | seconds | Age of the cached sample. |
| `homecloud_vm_filesystem_size_bytes`, `homecloud_vm_filesystem_used_bytes` | bytes | A guest filesystem, attributed to the VM disk it lives on (`disk_id`; empty when it cannot be attributed). |
| `homecloud_vm_disk_virtual_size_bytes` | bytes | Virtual size of a VM disk. |
| `homecloud_vm_disk_allocated_bytes` | bytes | Host space the disk's qcow2 file occupies. Thin provisioning makes this smaller than the virtual size. |

### Self-instrumentation

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `homecloud_vmd_scrape_errors_total` | counter | `source` | Scrapes that could not read the store (`source="capacity"`). |
| `homecloud_vm_sample_errors_total` | counter | none | Guest samples that failed. |
| `homecloud_vm_samples_last_duration_seconds` | gauge | none | How long the last sampling pass took. |
| `homecloud_vm_samples_last_timestamp_seconds` | gauge | none | Unix time at which the last sampling pass finished. |
| `homecloud_reconcile_vm_total` | counter | `outcome` (`ok`, `error`) | VM reconciliation steps, by outcome. |
| `homecloud_reconcile_vm_duration_seconds` | histogram | none | Time to reconcile one VM. |
| `homecloud_reconcile_ticks_total` | counter | none | Reconciler ticks completed. |
| `homecloud_reconcile_tick_duration_seconds` | histogram | none | Time per tick. |
| `homecloud_reconcile_last_tick_claimed` | gauge | none | VMs claimed by the last tick. |

### agentd metrics

agentd serves the RPC metrics (`server="agentd"`) and these task metrics:

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `homecloud_agent_tasks_finished_total` | counter | `state` (`succeeded`, `failed`, `gave_up`, `cancelled`, `error`) | Tasks that reached a final state. |
| `homecloud_agent_task_duration_seconds` | histogram | none | Time from start to final state. |
| `homecloud_agent_steps_total` | counter | none | Commands run by agents inside VMs. |
| `homecloud_agent_lease_lost_total` | counter | none | Tasks whose lease was taken over while this worker ran them. |

There is no gauge for the queue depth. Count queued tasks with `TaskService.ListTasks` (`state=TASK_STATE_QUEUED`)
or with a query on the database.

## Example alert rules

These are examples in Prometheus rule syntax. Thresholds are starting points.

```yaml
groups:
  - name: homecloud
    rules:
      - alert: HomecloudNodeDown
        expr: homecloud_node_up == 0 and homecloud_node_draining == 0
        for: 2m
        labels: {severity: page}

      - alert: HomecloudNoCapacity
        expr: homecloud_cluster_cpus_available <= 0 or homecloud_cluster_memory_mb_available < 1024
        for: 10m
        labels: {severity: warn}
        annotations:
          summary: "No CPU or memory left to start VMs. Pending VMs wait with 'waiting for capacity'."

      - alert: HomecloudVMInError
        expr: homecloud_vms{state="error"} > 0
        for: 5m
        labels: {severity: page}
        annotations:
          summary: "A VM is in ERROR (crash loop or failed step). Read its state_reason."

      - alert: HomecloudReconcileErrors
        expr: sum(rate(homecloud_reconcile_vm_total{outcome="error"}[10m])) > 0.05
        for: 10m
        labels: {severity: warn}

      - alert: HomecloudReconcilerStalled
        expr: rate(homecloud_reconcile_ticks_total[5m]) == 0
        for: 5m
        labels: {severity: page}

      - alert: HomecloudNodeDiskNearlyFull
        expr: homecloud_node_disk_bytes_free / homecloud_node_disk_bytes_total < 0.10
        for: 15m
        labels: {severity: warn}

      - alert: HomecloudGuestFilesystemNearlyFull
        expr: homecloud_vm_filesystem_used_bytes / homecloud_vm_filesystem_size_bytes > 0.90
        for: 15m
        labels: {severity: warn}

      - alert: HomecloudInternalErrors
        expr: sum(rate(homecloud_rpc_requests_total{code="internal"}[5m])) > 0
        for: 5m
        labels: {severity: warn}
        annotations:
          summary: "API calls return internal errors. Check Postgres and the server log."

      - alert: HomecloudScrapeErrors
        expr: rate(homecloud_vmd_scrape_errors_total[5m]) > 0
        for: 10m
        labels: {severity: warn}

      - alert: HomecloudGuestSamplesStale
        expr: time() - homecloud_vm_samples_last_timestamp_seconds > 300
        for: 5m
        labels: {severity: warn}
```

Not covered: a VM crash counter is not exported. Use `state="error"`, and the reconcile failure rate. vmd
exports no task queue gauge. For agentd, use the [agentd metrics](#agentd-metrics) (for example,
`increase(homecloud_agent_lease_lost_total[10m]) > 0` shows workers losing their leases).

A transient start failure (for example, a missing `genisoimage`) does not show as `error`; the VM stays
`stopped` with a `state_reason`. Watch `homecloud_reconcile_vm_total{outcome="error"}` for those.

## Capacity planning

### What the flags do

| Flag | Default | Effect |
| --- | --- | --- |
| `--cpu-overcommit` | `1` | vCPUs the scheduler may allocate per host thread. |
| `--memory-overcommit` | `1` | Multiplier for usable memory. |
| `--reserve-memory-mb` | `2048` | Memory kept back for the host, subtracted before the overcommit factor. |

For a node:

- CPU allocatable = floor(threads × `cpu-overcommit`).
- Memory allocatable = (RAM in MB − reserve) × `memory-overcommit`.
- Disk: no overcommit. A new disk needs its size free on the node.

The allocated amount counts VMs whose desired state is running. A stopped VM takes no CPU or memory budget.

### Where capacity is checked

- **At start.** A VM that should run but has no CPU or memory on its node waits in `PENDING` with
  `state_reason` `waiting for capacity: ...`, and is checked every 3 s.
- **At create.** `CreateVM` checks that the node is registered and the project quota (when one is configured).
  It does not check CPU or memory.
- **Disks without an image** go to the ready node with the most free disk that has the requested size.

### Placement

A VM runs on the node of its boot disk, and that node is the node of its image. A VM is never moved. The
scheduler has a `Pick` function that chooses the best node by headroom, but no service code calls it at present.

### Quotas

The service has quotas per project (VMs, running vCPUs, running memory, disk GB). They are enforced when set in
the service configuration. `cmd/vmd` sets none, so **quotas are unlimited in the shipped binary**. There is no
flag for them.

### Per-VM and per-disk limits

These are the service defaults, which `cmd/vmd` does not change:

- vCPUs per VM: 1 to 64.
- Memory per VM: 256 MB to 1 TiB.
- Disk size: 1 to 4096 GB; a boot disk made from an image defaults to 20 GB.

## Backups

Back up three things. Restoring them together is what keeps the control plane and the nodes in agreement.

1. **Postgres.** Back up the vmd database (its tables and `schema_migrations_vmd`) and, if used, agentd's
   database. Use `pg_dump` or continuous archiving, as usual for Postgres. A restore brings back VM and disk
   records; the reconciler then works towards them. Files on nodes that are not in the restored database are
   not known to vmd and must be cleaned up by hand.
2. **Node data directory** (`--data-dir`): `images/`, `disks/`, `vms/` and `shared/`. Remember that
   `images/<id>.qcow2` may be a symlink to a file outside the data directory: back up the target too.
   A copy of a disk that a running VM writes to can be inconsistent. Stop the VM first, or use
   `DiskService.SnapshotDisk` on a stopped VM, which produces a consistent image.
3. **Signing keys.** Keep the private keys (`.key` files) in a secure store. They cannot be recovered if they are
   lost, and without them new tokens cannot be minted for their kid. Existing public keys keep accepting
   tokens until they expire.

Guest job directories are not backed up. They are short-lived.

## Upgrades

- **Migrations are forward-only.** Only `NNN_name.up.sql` files are applied, in numeric order. There are no
  down migrations. Each migration runs in its own transaction, and its version is recorded in
  `schema_migrations_vmd`.
- **Migrations run at start-up**, before the server listens, under a Postgres advisory lock. Several replicas
  can start together: one applies the migration and the rest wait. Take a database backup before an upgrade.
- **Control plane replicas are stateless.** Upgrade them one at a time: start the new binary (it migrates first),
  then stop an old one. Leases mean that work is picked up by whichever replica is up.
- **Node daemons can restart without killing VMs.** Stop the daemon, install the new binary, and start it with
  the same `--data-dir`, `--run-as` and `--ssh-key`. The new process re-reserves the ports and reads the runtime
  files of the running VMs. Calls to that node fail with `unavailable` while it restarts, and the reconciler
  retries.
- The build version is set at link time (`-ldflags "-X main.version=..."`). `vmd -version` prints it, and the
  `homecloud_build_info` metric carries it.
