# vmd

vmd is the Attic Cloud VM service: it manages virtual machines, disks, images, in-guest exec jobs, node
placement and metrics, and drives the node daemons (see [libs/qemu](../../libs/qemu/README.md)) towards the
state that clients ask for.

## Role

- Owns the VM, disk, image and job records, their desired and observed state, and the `state_reason` text that
  explains them. Persists them in PostgreSQL, or in memory when no DSN is given.
- Owns placement: a VM runs on the node that holds its boot disk or image, and a start is admitted only when
  `scheduler.Fits` passes for that node. Owns the node registry, the operator drain flag, and project quotas.
- Runs the reconciler: a lease-based loop that moves each VM, disk and image towards its desired state through
  the node API, retrying with backoff.
- Forwards exec and file calls to the guest through the node. Stores job metadata, never job output: output and
  job files live in the guest.
- Exposes Prometheus metrics for guests and cluster capacity.
- Does NOT run QEMU, build cloud-init seeds, or touch host files (that is the node driver in `libs/qemu`).
- Does NOT issue tokens or hold user accounts (`libs/authz`, `cmd/atticctl`), run agents (`services/agentd`), move
  VMs between nodes, or replicate images across nodes.

## Position

```
 clients: services/mcp, services/agentd (service token), operators (curl), Prometheus
        │  Connect over HTTP/1.1, h2c or TLS; Bearer JWT on every call
        ▼
 ┌──────────────────────────────┐   Postgres (one or more databases, projects sharded)
 │ vmd  --role=all | control    │ ─────────────────────────────────────────────────────►
 │ API + reconciler + sampler   │
 └──────────────┬───────────────┘
                │ NodeService (Connect, bearer token with role "node")
        ┌───────┴────────┬───────────────────┐
        ▼                ▼                   ▼
 vmd --role=node   vmd --role=node    (role=all: in-process node)
 QEMU/KVM, qcow2   QEMU/KVM, qcow2    libs/qemu/qemudriver
```

- Imports allowed by `scripts/check-boundaries.sh`: `api`, `libs/common`, `libs/authz`, `libs/qemu`,
  `libs/server`. No other service may be imported.
- `--role=node` runs only the node API and needs no database. `--role=control` and `--role=all` run the API,
  the reconciler and the sampler.

## Contract

### Wire conventions

- Protocol: Connect (gRPC, gRPC-Web and HTTP+JSON share the same paths). A unary call is
  `POST /<package>.<Service>/<Method>` with a JSON or protobuf body. Services: `attic.vmd.v1.VMService`,
  `attic.vmd.v1.DiskService`, `attic.vmd.v1.ImageService`, `attic.vmd.v1.ExecService`,
  `attic.vmd.v1.MetricsService`, `attic.vmd.v1.ClusterService`. Source: `api/proto/attic/vmd/v1/vmd.proto`.
- JSON encoding (protobuf JSON mapping): field names are lowerCamelCase (`projectId`), enums are written by name
  (`"VM_STATE_RUNNING"`), `int64` values are JSON strings in responses (requests accept numbers or strings), `bytes`
  fields are base64, and zero-valued fields are omitted from responses. No gRPC server reflection is registered.
- Authentication: `Authorization: Bearer <JWT>` on every RPC. A missing, invalid, expired or revoked token gives
  `unauthenticated` (the reason is only logged).
- Authorization: each RPC names one permission (tables below). Handlers check it against the project of the
  resource they touch. A caller with no binding on that project gets `not_found`, so other tenants' IDs cannot be
  probed. A caller who can see the project but lacks the permission gets `permission_denied`.
- Long-poll: `GetVM.wait_seconds`, `StartJob.wait_seconds` and `PollJob.wait_seconds` are clamped to 0..30. The
  server re-checks every 200 ms for VMs and every 250 ms doubling to 1 s for jobs. It returns the current
  state when the wait expires. No call blocks longer than 30 s.
- Pagination: `page_size` defaults to 50 and is capped at 200. `page_token` is the last ID of the previous page.
  Lists are ordered by ID ascending, which is creation order for ULIDs.
- IDs: `vm_`, `disk_`, `img_` and `job_` followed by a ULID. A sharded deployment appends `-sNN` (two hex digits,
  the shard number), so ID lookups go straight to the right database.
- Idempotency: `CreateVM.idempotency_key` and `StartJob.idempotency_key` are scoped to the project. A repeated
  `CreateVM` with the same key returns the existing VM and does not compare the body. A repeated `StartJob` with
  the same key returns the existing job, but a different VM, command or workdir gives `invalid_argument`.
  Other mutations are repeatable: deleting an image, disk or VM that is already being deleted, or setting a
  hold or drain that is already set, succeeds without change.
- Errors: domain errors map to Connect codes in `libs/common/errs`. Errors from the node are passed through only
  when they mean something to the caller; transport failures become `unavailable` with no host name or port.
  Other unknown errors, including database failures, become `internal` with the text hidden.

| Code | Returned when |
| --- | --- |
| `invalid_argument` | a field is missing or out of range; a shrink or same-size resize; an idempotency key reused for another request |
| `not_found` | the ID is unknown, or the caller has no binding on the resource's project |
| `permission_denied` | the caller has a binding on the project but lacks the permission; or the caller lacks a global permission |
| `already_exists` | a VM, disk or image name is taken in the project; a shared image name is taken |
| `failed_precondition` | the VM is not stopped or not running, is being deleted, is held, or has an attached disk; the image is not ready; the node is not registered |
| `resource_exhausted` | a quota would be exceeded; no ready node has room for a new disk |
| `aborted` | `UpdateVM.expected_generation` does not match, or a write kept conflicting after 8 retries |
| `unavailable` | the node is unreachable or failed in a way that is not shown (details are in the vmd log) |
| `unauthenticated` | no valid token |
| `internal` | an unexpected error, usually a database failure; the message is hidden |

Errors are sent as `{"code": "...", "message": "..."}`, and Connect maps the codes to HTTP statuses in the usual way
(`unauthenticated` 401, `not_found` 404, `invalid_argument` 400).

### VMService

| RPC | Request fields | Response | Permission | Errors beyond the common set | Semantics |
| --- | --- | --- | --- | --- | --- |
| `CreateVM` | `project_id`, `name`, `cpus`, `memory_mb`, `image_id` or `disk_id` (oneof boot), `disk_size_gb`, `labels`, `start`, `idempotency_key` | `VM` | `vm.create` on project | `not_found` image or disk not visible; `failed_precondition` image not READY, disk attached/ERROR/DELETING, node not registered; `resource_exhausted` quota | Returns at once with state `PENDING`. With `image_id` it creates a boot disk named `<vm id>-boot` (default 20 GB, max 4096) in CREATING. With `disk_id` the detached disk is attached. `start=true` sets desired RUNNING. Name: 1 to 63 characters, starts with a lower-case letter, then lower-case letters, digits or hyphens, and does not end with a hyphen. `cpus` 1..64, `memory_mb` 256..1048576 |
| `GetVM` | `id`, `wait_for_state`, `wait_seconds` | `VM` | `vm.get` | | Returns when the observed state equals `wait_for_state` or is `ERROR`, or when the wait expires. Without `wait_for_state` returns at once |
| `ListVMs` | `project_id`, `page_size`, `page_token`, `label_selector` | `vms`, `next_page_token` | `vm.list` on project | | `label_selector` is an exact match on each key (a missing key matches an empty value) |
| `UpdateVM` | `id`, `cpus` (optional), `memory_mb` (optional), `replace_labels`, `labels`, `expected_generation` | `VM` | `vm.update` | `failed_precondition` resize while the VM is not stopped and desired stopped; `aborted` generation mismatch | Resize changes `generation`; label edits do not. Labels merge unless `replace_labels`. A resize does not check quotas. No change returns the VM unchanged |
| `StartVM` | `id` | `VM` | `vm.start` | `failed_precondition` VM being deleted; `resource_exhausted` quota (running vCPUs and memory) | Sets desired RUNNING and returns at once. A VM in `ERROR` with desired RUNNING has its failure and crash counters reset and is retried now |
| `StopVM` | `id`, `force` | `VM` | `vm.stop` | `failed_precondition` VM being deleted | Sets desired STOPPED and returns at once. `force` skips the graceful power-off |
| `DeleteVM` | `id`, `force`, `delete_disks` | `DeleteVMResponse` | `vm.delete` | `failed_precondition` the VM has holds and `force` is false | Sets desired deleted; the reconciler stops and removes the VM. `delete_disks` also deletes its disks. Returns at once |
| `AddHold` | `id`, `holder` | `VM` | `vm.hold` (roles `admin`, `service`) | `invalid_argument` empty holder; `failed_precondition` VM being deleted | Adds a holder string (for example `agentd/agent-01H...`). Idempotent. Holds block `DeleteVM` unless forced |
| `RemoveHold` | `id`, `holder` | `VM` | `vm.hold` | `invalid_argument` empty holder | Removes the holder. Idempotent |

### ImageService

| RPC | Request fields | Response | Permission | Errors beyond the common set | Semantics |
| --- | --- | --- | --- | --- | --- |
| `RegisterImage` | `project_id`, `name`, `node_id`, `path` | `Image` | `image.register` (role `admin`) | `invalid_argument` `project_id` is `*`, or `path` is not a clean absolute path; `failed_precondition` node not registered | `project_id` empty creates a shared image, which needs a `*` binding. The image is CREATING; the node adopts the file (a symlink) and the image becomes READY |
| `GetImage` | `id` | `Image` | `image.get`; a shared image needs the permission in any project | | Shared images (empty project) are readable by any caller with the permission in some project |
| `ListImages` | `project_id`, `page_size`, `page_token` | `images`, `next_page_token` | `image.list` on project | | Returns the project's images and the shared images |
| `DeleteImage` | `id` | `DeleteImageResponse` | `image.delete` (role `admin`; shared images need `*`) | `failed_precondition` a disk is still created from the image | Sets DELETING. The reconciler removes the node file, then the row. Idempotent |

### DiskService

| RPC | Request fields | Response | Permission | Errors beyond the common set | Semantics |
| --- | --- | --- | --- | --- | --- |
| `CreateDisk` | `project_id`, `name`, `image_id`, `size_gb`, `node_id` | `Disk` | `disk.create` | `invalid_argument` size outside 1..4096, or `node_id` differs from the image's node; `failed_precondition` image not READY, node not registered; `resource_exhausted` quota, or no ready node has `size_gb` free when neither `image_id` nor `node_id` is set | Returns at once with state CREATING. With `image_id` the disk is a copy-on-write overlay on the image's node. The node creates the file asynchronously. An explicit `node_id` is not checked against drain |
| `GetDisk` | `id` | `Disk` | `disk.get` | | |
| `ListDisks` | `project_id`, `page_size`, `page_token` | `disks`, `next_page_token` | `disk.list` | | |
| `ResizeDisk` | `id`, `size_gb` | `Disk` | `disk.update` | `invalid_argument` not larger than the current size, or above 4096; `failed_precondition` disk not READY, or attached to a VM that is not stopped and desired stopped; `resource_exhausted` quota; `unavailable` node failure | Grow only. The node call is synchronous |
| `DeleteDisk` | `id` | `DeleteDiskResponse` | `disk.delete` | `failed_precondition` disk attached to a VM | Sets DELETING; the reconciler removes the file and the row. Idempotent |
| `SnapshotDisk` | `id`, `image_name` | `Image` | `disk.snapshot` | `invalid_argument` bad name; `failed_precondition` disk not READY, or attached to a VM that is not stopped; `already_exists` image name taken | Creates an image in CREATING. The node flattens the disk into a new file. No idempotency key: a retry gets `already_exists` |

### ExecService

Jobs run inside the guest as detached processes (see [ADR 0003](../../docs/adr/0003-jobs-live-in-the-guest.md)).
The job record in PostgreSQL holds only metadata.

| RPC | Request fields | Response | Permission | Errors beyond the common set | Semantics |
| --- | --- | --- | --- | --- | --- |
| `StartJob` | `vm_id`, `command`, `workdir`, `timeout_seconds`, `wait_seconds`, `max_output`, `idempotency_key` | `JobOutput` | `exec.run` | `invalid_argument` command empty or over 256 KiB, NUL byte, timeout over 7 days, reused key for another request; `failed_precondition` VM not RUNNING (a new job only) | `command` runs with `bash -lc`. Default `workdir` is the guest user's home. Default timeout 3600 s; a timeout ends with exit 124 and state `TIMED_OUT`. Waits up to `wait_seconds` for a final state. `max_output` defaults to 8192 bytes per stream, capped at 65536. Output is the last bytes (tail mode) |
| `PollJob` | `job_id`, `wait_seconds`, `stdout_offset`, `stderr_offset`, `max_output`, `tail` | `JobOutput` | `exec.get` | `invalid_argument` negative offset | Offset mode returns output from the given offsets; `tail` returns the last `max_output` bytes and sets `truncated`. If the VM is not RUNNING the stored record is returned with no output. A job whose process vanished becomes `LOST` |
| `CancelJob` | `job_id` | `JobOutput` | `exec.cancel` | | Sends TERM then KILL to the job's process group and records state `CANCELLED`. A finished job is returned unchanged. Waits up to 3 s |
| `ListJobs` | `vm_id`, `page_size`, `page_token` | `jobs`, `next_page_token` | `exec.list` on the VM's project | | Oldest first. Does not refresh job state |
| `PutFile` | `vm_id`, `path`, `content`, `mode` | `PutFileResponse` | `file.write` | `invalid_argument` empty path, NUL in path, content over 8 MiB, mode above 07777; `failed_precondition` VM not RUNNING | Atomic write (temp file, then rename). `mode` defaults to 0644 and creates parent directories |
| `GetFile` | `vm_id`, `path`, `max_bytes` | `content`, `truncated` | `file.read` | `invalid_argument` empty path or NUL; `failed_precondition` VM not RUNNING; `not_found` no regular file at `path` | `max_bytes` defaults to and is capped at 8 MiB; `truncated` is set when the file is longer |

`JobOutput` carries `job` (`id`, `vm_id`, `project_id`, `command`, `workdir`, `state`, `exit_code`,
`timeout_seconds`, `killed_reason`, `started_at`, `finished_at`), `stdout`, `stderr`, `stdout_offset`,
`stderr_offset`, `truncated` and `more`.

Job states: `RUNNING`, `SUCCEEDED` (exit 0), `FAILED` (other non-zero exit, or the start never reached the guest with
`killed_reason` `could not start: ...`), `CANCELLED`, `TIMED_OUT`, `LOST`.

### MetricsService and ClusterService

| RPC | Request fields | Response | Permission | Errors beyond the common set | Semantics |
| --- | --- | --- | --- | --- | --- |
| `GetVMMetrics` | `vm_id` | `VMMetrics` | `vm.metrics` (roles viewer and up, `service`) | `failed_precondition` VM not RUNNING; `unavailable` the guest or node could not be sampled | One guest sample now. `cpu_percent` is like `top`: 100 per busy vCPU. Disk allocation comes from `qemu-img info` on the host |
| `GetCapacity` | none | `nodes` (`NodeCapacity` each), `total` | `capacity.get` on `*` (role `operator`) | | `total` (`node_id` `*`) sums READY nodes only. `*_available` is what the scheduler would still hand out, after overcommit and reserve |
| `SetNodeDraining` | `node_id`, `draining` | `NodeCapacity` | `node.drain` on `*` (role `operator`) | `invalid_argument` empty `node_id`; `not_found` unknown node | Persists the drain decision. Returns the node's capacity view |

`NodeCapacity` fields: `node_id`, `state` (`ready`, `draining` or `down`), `cpus_total`, `cpus_allocated`,
`cpus_available`, `memory_mb_total`, `memory_mb_allocated`, `memory_mb_available`, `disk_bytes_total`,
`disk_bytes_free`, `vms_total`, `vms_running`, `last_heartbeat`.

`VMMetrics` fields: `vm_id`, `sampled_at`, `cpu_percent`, `cpu_cores`, `load1/5/15`, `mem_total_bytes`,
`mem_used_bytes`, `mem_available_bytes`, `swap_total_bytes`, `swap_used_bytes`, `uptime_seconds`,
`filesystems[]` (`disk_id`, `device`, `mount_point`, `size_bytes`, `used_bytes`), `disks[]` (`disk_id`,
`virtual_size_bytes`, `allocated_bytes`).

### VM, disk, image and enum fields

- `VM`: `id`, `project_id`, `name`, `node_id`, `cpus`, `memory_mb`, `disk_ids` (first is boot), `desired_state`,
  `state`, `state_reason`, `holds`, `labels`, `generation`, `created_at`, `updated_at`.
- `Disk`: `id`, `project_id`, `name`, `node_id`, `size_gb`, `image_id`, `state`, `attached_vm_id`, `created_at`,
  `state_reason`.
- `Image`: `id`, `project_id` (empty = shared), `name`, `node_id`, `state`, `size_bytes`, `source_disk_id`,
  `created_at`, `state_reason`.
- `PowerState`: `RUNNING`, `STOPPED`. Internally a VM being deleted has desired `deleted`, which is reported as
  `STOPPED`.
- `VMState` (observed): `PENDING`, `STOPPED`, `STARTING`, `RUNNING`, `STOPPING`, `ERROR`, `DELETING`.
- `DiskState` and `ImageState`: `CREATING`, `READY`, `ERROR`, `DELETING`.

### HTTP endpoints

| Path | Served by | Behaviour |
| --- | --- | --- |
| `GET /healthz` | every role | `200 ok`, always |
| `GET /readyz` | every role | `200 {"status":"ok"}`, or `503 {"status":"unavailable","failed":[...]}`. Checks run in parallel with a 2 s timeout each. Control plane: `postgres-0`, `postgres-1`, ... (one per DSN) and `store` (a node list read). Node daemon: no checks |
| `GET /metrics` | control plane, when `--metrics` is true (default) | Needs `Authorization: Bearer` with `capacity.get` on `*`. `401` missing or invalid token; `403` token without the permission |

### Node API: what vmd calls

vmd calls each node with its own service token (subject `svc:vmd`, binding `*:node`, five-minute lifetime). The
node accepts only the `node` role. The node protocol is `api/proto/attic/node/v1/node.proto`, service
`attic.node.v1.NodeService`. Its semantics are in [libs/qemu](../../libs/qemu/README.md#node-api).

| Node RPC | Called by | When |
| --- | --- | --- |
| `GetCapacity` | `NodeManager` | Every 5 s per node (5 s timeout). Sets health and capacity. A node silent for more than 15 s is `down` |
| `RegisterImage` | reconciler | Image in CREATING with a `path` |
| `SnapshotDisk` | reconciler | Image in CREATING with a `source_disk_id` |
| `DeleteImage` | reconciler | Image in DELETING |
| `CreateDisk` | reconciler | Disk in CREATING; `backing_image_id` is the disk's image |
| `ResizeDisk` | `DiskService.ResizeDisk` | Synchronously, from the API |
| `DeleteDisk` | reconciler | Disk in DELETING |
| `GetVMRuntime` | reconciler | Each reconcile step of a VM |
| `StartVM` | reconciler | Desired RUNNING, VM not alive, and `Fits` passes |
| `StopVM` | reconciler | Desired STOPPED and VM alive; forced during delete |
| `ForgetVM` | reconciler | Delete step, after the VM is no longer alive |
| `StartJob`, `PollJob`, `CancelJob` | `ExecService` | Exec calls |
| `PutFile`, `GetFile` | `ExecService` | File calls |
| `GetVMMetrics` | `MetricsService`, sampler | API call and the 30 s sampler for running VMs |

## Behaviour and guarantees

### VM states

```
desired STOPPED                                  desired RUNNING
 PENDING ──disks READY──► STOPPED ◄──────────────────────────────┐
   │                        │ StartVM (fits)                     │
   │                        ▼                                    │
   │                    STARTING ──runtime RUNNING──► RUNNING ──┤ crash: restart
   │                        │                            │      │ (fourth crash: ERROR)
   │                        ▼                            │      │
   │                    STOPPING ◄──── StopVM ───────────┘      │
   │                        │ process gone                       │
   └─ any ── DELETING ◄─────┘ (desired deleted)                  ERROR (retry with backoff)
             removed when the process is gone and disks are detached
```

- `PENDING`: created, waiting until every disk is READY. A disk in ERROR makes the VM `ERROR`.
- `STARTING`: the node's runtime is `BOOTING` (QEMU is up but the guest does not answer SSH yet).
- `RUNNING`: the guest answers SSH and the shared folder is mounted. The reconciler re-checks every 10 s.
- `STOPPING`: a graceful stop has been requested (or a forced one is in progress). If the guest has not exited
  after 60 s, the node kills it and the reason becomes `stop timed out, killed`. After a normal stop the reason is
  `stopped`; after a forced stop it is `killed`.
- `ERROR`: set after the fourth crash in a row, or by a non-transient step error. `StartVM` clears it at once.

### Disk and image states

- Disk: CREATING -> READY (node `CreateDisk` succeeded). A non-transient node error makes it ERROR with the node's
  message. A transient error is retried on every tick (about 1 s, without backoff). DELETING -> removed after the
  node's `DeleteDisk`.
- Image: CREATING -> READY after `RegisterImage` (the node symlinks the file, which is the image's size) or
  `SnapshotDisk` (flattens the disk). Non-transient errors give ERROR, for example a missing file or a disk that
  is in use when the snapshot runs. Transient errors are retried every tick, with no backoff. DELETING -> removed.

### Reconciler

- One tick runs every 1 s (`ReconcileInterval`). It first handles images and disks (up to 20 of each state per
  tick), then claims up to 32 due VMs (`BatchSize`) and reconciles up to 8 of them in parallel.
- A VM is due when `next_reconcile <= now` and its lease has expired. Claiming takes a 30 s lease
  (`LeaseTTL`) with `FOR UPDATE SKIP LOCKED`, so any number of replicas share the work. A replica that dies
  loses its leases after 30 s. Image and disk steps are not leased: two replicas may run the same step, which is
  harmless because an `already_exists` or `not_found` from the node is treated as done.
- Per step, a VM is:
  - desired RUNNING, runtime `RUNNING`: state RUNNING, re-check in 10 s.
  - desired RUNNING, runtime `BOOTING`: state STARTING, re-check in 1 s.
  - desired RUNNING, runtime absent or exited: check capacity; if it does not fit, state PENDING with
    `waiting for capacity: <reason>` and re-check in 3 s. Otherwise call `StartVM`.
  - desired STOPPED, runtime alive: call `StopVM` (forced when `StopForce`), state STOPPING, re-check in 1 s.
  - desired STOPPED, not alive: state STOPPED, reason `stopped` or `killed`, no further check until the API changes it.
  - desired deleted: force-stop if alive, then `ForgetVM`, detach disks (delete them if `delete_disks`), delete the row.
- A VM's placement lock is held from the capacity check to the persisted state, so two starts on one node in this
  process do not both see the same free room. The lock is per process (see Failure modes).

### Jobs

- `StartJob` stores the job as RUNNING, asks the node to launch it detached in the guest, then waits up to
  `wait_seconds` for a final state. If the launch fails, the job becomes `FAILED` with
  `could not start: <reason>` and the error is returned.
- A replay with the same idempotency key returns the stored job. If that job never reached the guest
  (`could not start`), the replay launches it again, using a compare-and-swap so that only one of several
  concurrent retries wins.
- `PollJob` and `CancelJob` apply the node's answer to the stored job: exit 0 is `SUCCEEDED`, exit 124 is
  `TIMED_OUT`, a cancelled job is `CANCELLED`, another non-zero exit is `FAILED`, and a vanished process or
  missing job directory is `LOST`. Reading a job of a VM that is not RUNNING marks it `LOST` with
  `vm stopped: <reason>` or `the VM was deleted`.

### Capacity and placement

- `scheduler.Fits` admits a start when the node is READY and: requested vCPUs <= `floor(threads * cpu-overcommit)`
  minus vCPUs of VMs in STARTING, RUNNING or STOPPING; and requested memory <= `(RAM - reserve-memory-mb) *
  memory-overcommit` minus memory of those VMs. A draining node gives `node is draining`; a down node gives
  `node is down`.
- Reported capacity (`GetCapacity`, metrics) counts a VM as allocated when its desired state is RUNNING. The two
  views can differ briefly while a VM moves between them.
- Disks without an image go to the ready node with the most free disk that has the requested size. Disks with an
  image go to the image's node.
- `scheduler.Pick` (least-loaded node) is tested but not called by the service.

### Crashes, failures and backoff

- Crash: a QEMU process that exits while the VM is RUNNING or STARTING counts one crash. The reason becomes
  `VM exited unexpectedly: <last lines of qemu.log>`. Crashes 1 to 3 restart the VM at the next step. The fourth
  crash sets ERROR and the next attempt waits `min(2^crashes s, 60 s)` (16 s, 32 s, then 60 s).
- A RUNNING VM that is still running at a re-check loses one crash count, so a VM that stays up is forgiven.
- Failure: an error from the node is counted separately and reset when the node answers. Retries wait
  `min(2^failures s, 60 s)`. An unavailable node sets the reason `node unreachable: <node id>`. A non-transient
  error (not found, invalid argument, failed precondition, already exists, permission denied) sets ERROR and
  re-checks every 60 s.
- A start that fails transiently leaves the VM in its earlier state (STOPPED or PENDING) with a reason. It does not
  become ERROR. Alert on `attic_reconcile_vm_total{outcome="error"}` to see these.

### Node health and drain

- The node manager polls `GetCapacity` every 5 s with a 5 s timeout. A node unheard from for more than 15 s is
  `down`. A node that has never answered is `down` with no heartbeat.
- Drain is an operator decision stored apart from health. The effective state is `down` if the node is down,
  else `draining` if drain is set, else `ready`. An outage cannot clear a drain.
- `draining`: running VMs keep running and nothing is moved. A VM that needs a start waits in PENDING with
  `waiting for capacity: node is draining`. A new VM is still created (`CreateVM` checks only that the node is
  registered), and it waits.
- `down`: the VM keeps its last state with `node unreachable` and is retried with backoff.

### Quotas

Per project; `0` means unlimited.

| Flag | Limit | Checked by |
| --- | --- | --- |
| `--quota-vms` | VMs in any state | `CreateVM` |
| `--quota-vcpus` | vCPUs of VMs whose desired state is RUNNING | `CreateVM` with `start`, `StartVM` |
| `--quota-memory-mb` | memory of VMs whose desired state is RUNNING | `CreateVM` with `start`, `StartVM` |
| `--quota-disk-gb` | size of all the project's disks | `CreateVM` with `image_id`, `CreateDisk`, `ResizeDisk` |

Lowering a quota does not stop existing resources. `UpdateVM` does not check quotas.

### Sharding

- `--postgres-dsn` may be given several times. Projects are placed on databases by rendezvous hashing over the
  DSN's position in the list. The first DSN is shard 0. Node rows and shared images (empty project) live on
  shard 0.
- IDs created for a sharded store carry the shard (`-sNN`). An ID without a suffix is looked up on every shard.
- A transaction stays on one shard; the service sets the project context before a transaction.
- The list and order of databases are part of the data layout. Changing either re-routes projects. There is no
  rebalancing tool. Every database is migrated at start-up.

### Limits and defaults (not flags)

| Setting | Value |
| --- | --- |
| VM vCPUs | 1..64 |
| VM memory | 256 MB..1 TiB |
| Disk size | 1..4096 GB; boot disk default 20 GB |
| Reconcile tick, resync, lease, batch | 1 s, 10 s, 30 s, 32 VMs |
| Node poll, poll timeout, node down after | 5 s, 5 s, 15 s |
| Sampler interval; sample expires after | 30 s; 3 intervals |
| Shutdown grace (SIGINT, SIGTERM) | 20 s |
| Guest command size; timeout | 256 KiB; default 3600 s, maximum 7 days |
| Job output per call | default 8 KiB, maximum 64 KiB per stream |
| File transfer | 8 MiB each way |

## Configuration

All flags are for `services/vmd/cmd/vmd`. Run `vmd -h` for the same list. The environment variable
`ATTIC_POSTGRES_DSN` is used only when no `-postgres-dsn` flag is given.

| Flag | Default | Role | Meaning |
| --- | --- | --- | --- |
| `-role` | `all` | all | `all` (control plane and in-process node), `control` (control plane; needs `-node`), or `node` (node daemon) |
| `-listen` | `:8080` | all | Address for the API, `/healthz`, `/readyz` and `/metrics` |
| `-postgres-dsn` | none (env `ATTIC_POSTGRES_DSN`) | control, all | Database DSN. Repeat to shard. None means the in-memory store, which loses state on restart |
| `-trusted-key` | none, at least one | all | `kid=path[:role,role]`: an Ed25519 public key that verifies user and service tokens. Repeatable. Roles restrict what the key may grant |
| `-max-token-ttl` | `24h` | all | Longest token lifetime accepted |
| `-revoked-tokens` | none | all | File of revoked token IDs, one per line. Reloaded when it changes; must exist at start |
| `-metrics` | `true` | control, all | Serve `/metrics` (needs an operator token) |
| `-tls-cert`, `-tls-key` | none | all | Serve TLS; without them, HTTP/1.1 and cleartext HTTP/2 |
| `-log-level` | `info` | all | `debug`, `info`, `warn` or `error` |
| `-node` | none | control | `id=URL` of a node daemon. Repeatable. Required for `control` |
| `-service-key` | none | control | Private Ed25519 key that signs the control plane's node tokens |
| `-service-key-id` | `vmd` | control | Key ID of `-service-key`; nodes must trust it with role `node` |
| `-node-id` | host name, lower-cased, dots replaced with `-` | all | Name of the in-process node (`all` only) |
| `-data-dir` | `/var/lib/attic` | node, all | Root of images, disks and VM state. Must be absolute |
| `-shared-dir` | `<data-dir>/shared` | node, all | Folder mounted at `/shared` in every guest |
| `-ssh-key` | none | node, all | Ed25519 key used to reach guests. Created with mode 0600 if missing, and its public half written to `<path>.pub`. Required |
| `-run-as` | `qemu` | node, all | User that QEMU processes run as. Empty runs as the current user (development only) |
| `-kvm` | `true` | node, all | Use KVM. `false` uses TCG emulation |
| `-no-firewall` | `false` | node, all | Skip the guest network lockdown. Development only |
| `-allow-dns` | none | node, all | Resolver IP guests may still reach through the lockdown. Repeatable |
| `-cpu-overcommit` | `1` | control, all | vCPUs per host thread the scheduler may hand out |
| `-memory-overcommit` | `1` | control, all | Multiplier on usable RAM |
| `-reserve-memory-mb` | `2048` | control, all | RAM kept back for the host |
| `-quota-vms` | `0` | control, all | Per-project VM count (see Quotas) |
| `-quota-vcpus` | `0` | control, all | Per-project running vCPUs |
| `-quota-memory-mb` | `0` | control, all | Per-project running memory, MB |
| `-quota-disk-gb` | `0` | control, all | Per-project disk, GB |
| `-version` | false | all | Print the version (set with `-ldflags "-X main.version=..."`) and exit |

`-role=node` and `-role=all` need `-ssh-key`. Without `-no-firewall` they also need root or `CAP_NET_ADMIN`:
start-up fails if the lockdown cannot be installed. `-role=control` without `-node` fails at start.

## Data

Schema: `services/vmd/store/postgres/migrations/001_init.up.sql`, applied at start-up under the migration record
`schema_migrations_vmd`. Each migration runs in its own transaction under a Postgres advisory lock, so several
replicas may start together.

| Table | Rows | Key columns and indexes |
| --- | --- | --- |
| `nodes` | one per node | `id` (PK); `state` (health), `drain`, `cpus`, `memory_mb`, `disk_bytes_total`, `disk_bytes_free`, `last_heartbeat`. Drain is not overwritten by health updates |
| `images` | one per image | `id` (PK); `project_id` (empty = shared), `name` unique per project, `node_id`, `state`, `state_reason`, `size_bytes`, `source_disk_id`, `path`, `version` |
| `disks` | one per disk | `id` (PK); `project_id`, `name` unique per project, `node_id`, `size_gb`, `image_id`, `state`, `state_reason`, `attached_vm_id`, `version` |
| `vms` | one per VM | `id` (PK); `name` unique per project; `desired`, `state`, `state_reason`, `disk_ids[]`, `holds[]`, `labels` (JSONB), `generation`, `stop_force`, `delete_disks`, `idempotency_key` (unique per project when set), `next_reconcile`, `lease_owner`, `lease_until`, `failures`, `crashes`, `version`. Indexes on `node_id` and `next_reconcile` |
| `jobs` | one per job | `id` (PK); `vm_id`, `project_id`, `command`, `workdir`, `state`, `exit_code`, `timeout_seconds`, `killed_reason`, `idempotency_key` (unique per project when set), `started_at`, `finished_at`. Indexes on `(vm_id, id)` and on running jobs |

- Every row carries a `version`. Writes are compare-and-swap on it; a lost race gives `conflict` internally.
- Job rows are never deleted. Nothing removes them. Job output is not stored.
- Node rows are created by the first successful poll of a node. Nodes are not registered through the API.
- The memory store (`store/memory.go`) implements the same contract and is used by tests and by the no-DSN mode.

## Observability

### Metrics

All names start with `attic_`. Each server registers only the collectors below.

RPC metrics (control plane, `server="vmd"`, from `libs/server/obs`):

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `attic_rpc_requests_total` | counter | `server`, `service`, `method`, `code` | Completed RPCs by Connect code (`ok` is success) |
| `attic_rpc_duration_seconds` | histogram | `server`, `service`, `method` | Duration; buckets 5 ms to 30 s; streams timed for the whole stream |
| `attic_rpc_in_flight` | gauge | `server`, `service`, `method` | RPCs being handled |
| `attic_build_info` | gauge | `version`, `go_version` | Always 1 |

Cluster and node (computed at scrape time from the store, `promcollect`):

| Metric | Labels | Meaning |
| --- | --- | --- |
| `attic_node_up` | `node` | 1 if the effective state is ready |
| `attic_node_draining` | `node` | 1 if draining |
| `attic_node_cpu_threads`, `attic_node_cpu_allocated_vcpus`, `attic_node_cpu_available_vcpus` | `node` | Threads; vCPUs allocated (desired RUNNING); vCPUs still available after overcommit |
| `attic_node_memory_total_bytes`, `attic_node_memory_allocated_bytes`, `attic_node_memory_available_bytes` | `node` | Memory; available is after reserve and overcommit |
| `attic_node_disk_total_bytes`, `attic_node_disk_free_bytes` | `node` | Disk on the node's data directory |
| `attic_node_vms`, `attic_node_vms_running` | `node` | VMs placed on the node, and those RUNNING |
| `attic_node_last_heartbeat_timestamp_seconds` | `node` | Unix time of the last successful poll; 0 if none |
| `attic_cluster_cpu_threads`, `attic_cluster_cpu_allocated_vcpus`, `attic_cluster_cpu_available_vcpus` | none | Sums over ready nodes |
| `attic_cluster_memory_total_bytes`, `attic_cluster_memory_allocated_bytes`, `attic_cluster_memory_available_bytes` | none | Sums over ready nodes |
| `attic_cluster_disk_total_bytes`, `attic_cluster_disk_free_bytes` | none | Sums over ready nodes |
| `attic_cluster_vms`, `attic_cluster_vms_running`, `attic_cluster_nodes_ready` | none | Counts over ready nodes |
| `attic_vms` | `state` | VMs per observed state, all states present (`pending`, `stopped`, `starting`, `running`, `stopping`, `error`, `deleting`) |
| `attic_vms_desired_running` | none | VMs whose desired state is RUNNING |

Guest metrics (sampled every 30 s for RUNNING VMs; labels `vm_id`, `project`, `name`, `node`; filesystem series add
`disk_id`, `device`, `mount_point`; disk series add `disk_id`):

| Metric | Meaning |
| --- | --- |
| `attic_vm_cpu_percent` | Guest CPU like `top`: 100 per busy vCPU |
| `attic_vm_cpu_cores`, `attic_vm_load1`, `attic_vm_load5`, `attic_vm_load15`, `attic_vm_uptime_seconds` | vCPUs, load averages, uptime |
| `attic_vm_memory_total_bytes`, `attic_vm_memory_used_bytes`, `attic_vm_memory_available_bytes` | Guest memory |
| `attic_vm_swap_total_bytes`, `attic_vm_swap_used_bytes` | Guest swap |
| `attic_vm_metrics_age_seconds` | Age of the cached sample |
| `attic_vm_filesystem_size_bytes`, `attic_vm_filesystem_used_bytes` | Guest filesystem, attributed to the VM disk it lives on (`disk_id`, empty if unknown) |
| `attic_vm_disk_virtual_size_bytes`, `attic_vm_disk_allocated_bytes` | Virtual size of a VM disk; host space its qcow2 file occupies |

vmd self-instrumentation:

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `attic_reconcile_vm_total` | counter | `outcome` (`ok`, `error`) | VM reconcile steps; `error` includes transient errors |
| `attic_reconcile_vm_duration_seconds` | histogram | none | Time per VM step |
| `attic_reconcile_ticks_total` | counter | none | Completed ticks |
| `attic_reconcile_tick_duration_seconds` | histogram | none | Time per tick |
| `attic_reconcile_last_tick_claimed` | gauge | none | VMs claimed by the last tick |
| `attic_vmd_scrape_errors_total` | counter | `source` (`capacity`) | Scrapes that could not read the store |
| `attic_vm_sample_errors_total` | counter | none | Failed guest samples |
| `attic_vm_samples_last_duration_seconds` | gauge | none | Duration of the last sampling pass |
| `attic_vm_samples_last_timestamp_seconds` | gauge | none | Unix time the last sampling pass finished |

Not exported: a queue depth for jobs, or a crash counter. Use `attic_vms{state="error"}` and the reconcile
error rate.

### Logs

Logs go to stderr in the format chosen by `daemon.NewLogger`. Messages worth alerting on:

| Level | Message | Fields |
| --- | --- | --- |
| WARN | `reconcile failed` | `vm`, `failures`, `err` |
| WARN | `node down` | `node`, `err` (on the transition) |
| WARN | `node call failed` | `node`, `err` (the sanitized error reached the client) |
| WARN | `image step failed, will retry`, `disk step failed, will retry` | `image` or `disk`, `reason` |
| WARN | `authentication failed` | `procedure`, `err` |
| WARN | `scrape source failed` | `source`, `err` |
| ERROR | `claim vms`, `update vm`, `upsert node`, `panic in handler` | `err` or `panic` |
| INFO | `node drain changed` | `node`, `draining`, `by` (the caller's subject) |
| INFO | `listening`, `sharding projects across databases`, `guest network lockdown active` | address, shard count, run-as user |

Logs do not carry hostnames or ports in API errors; those are only in the log.

## Failure modes and recovery

| Failure | What happens | What to do |
| --- | --- | --- |
| Node daemon unreachable | Node marked `down` after 15 s. Its VMs keep their last state with `node unreachable: <node>`. Retries back off to 60 s. Exec, file and metrics calls to its VMs return `unavailable`. Its capacity is left out of cluster totals. Disks for new VMs are created only when the node answers | Restore the node. Reconciliation resumes |
| Node daemon restarted | Guest QEMU processes are detached, so VMs keep running. The new driver re-reserves their SSH ports and reads their runtime files. Calls fail until it is back | Check `-run-as` and `-data-dir` are unchanged |
| Host reboot | QEMU processes are gone. Each running VM counts a crash and restarts when the node answers. A fourth crash in a row puts it in ERROR | Stop VMs through the API before maintenance (see Running and testing) if they must stay down |
| Control-plane replica dies | Its leases expire after 30 s and another replica takes the VMs. In-flight requests fail; create calls can be retried with an idempotency key | Restart the replica |
| All control-plane replicas down | No API calls and no reconciliation. VMs keep running | Restore one replica |
| Postgres down | `/readyz` returns 503 with the failed `postgres-N` and/or `store`. Database errors become `internal`. The reconciler logs `claim vms` each tick. Nothing committed is lost | Restore Postgres |
| VM crashes (QEMU exits) | Restarted, counted as a crash. Fourth in a row: ERROR, retried every 16 s, 32 s, then 60 s. Running jobs become LOST when next read | Read `state_reason` and the node's `vms/<id>/qemu.log`, fix the cause, call `StartVM` |
| Guest powers itself off | QEMU exits without a stop request, so it counts as a crash and restarts while desired RUNNING | Use `StopVM` instead of `sudo poweroff` inside the guest |
| Image file missing on the node | Image ERROR with the node's message. A pending VM whose disk is ERROR also goes to ERROR | Restore the file, or delete and register again |
| Disk still being created | VM waits in PENDING with `waiting for disk <id>` | Nothing, unless the disk stays CREATING (check the node) |
| Start blocked by capacity or drain | VM waits in PENDING with `waiting for capacity: ...` and is checked every 3 s | Free capacity, or undrain the node |
| Node drained for maintenance | Running VMs keep running; starts wait | Stop its VMs (`StopVM`, then wait for STOPPED), do the work, undrain, start them again |

Known limits, from the current code:

- Two vmd replicas can overcommit a node. The placement lock is per process; closing the gap needs a capacity
  reservation in the store.
- Disk and image steps retry transient errors every tick without backoff. A node that stays unreachable
  produces a warning every second for each affected disk or image.
- `CreateDisk` with an explicit `node_id` does not check drain.
- Deleting an image can race with creating a disk from it in a narrow window.
- A `SnapshotDisk` whose disk is attached to a running VM by the time the reconciler runs ends in image ERROR.
- Job rows are never deleted. Only the guest keeps the newest 50 finished job directories.
- `ListJobs` returns oldest first, and there is no single-job lookup.
- Image and disk steps have no lease. Several replicas may run them; this is harmless but duplicates work.
- Crash loops are not logged at ERROR. The VM becomes `ERROR` with a reason and is retried with backoff; alert on
  the state, not on a log line.
- No live resize: `UpdateVM` changes CPU and memory only while the VM is stopped and desired stopped. A disk can
  grow only, and only while detached or while its VM is stopped.
- `scheduler.Pick` is not called by the service; capacity is checked when a VM starts, not when it is created.
- `Store.ListRunningJobs` exists, but no service calls it. Running jobs are marked `LOST` only when a read reaches
  them, not when a VM stops.

## Running and testing

Run from the repository root. Each module is tested on its own; there is no root module.

```
make build vet boundaries      # build, vet and layering check of every module
make test                      # every module, -race, Postgres tests start a real PostgreSQL
make mocks                     # regenerate gomock mocks for store, nodes and node driver
make proto                     # regenerate api/gen (needs protoc 33.5 and the connect plugin)
```

vmd only:

```
cd services/vmd && go build -o vmd ./cmd/vmd          # binary in services/vmd/
cd services/vmd && go test -race -count=1 ./...
cd services/vmd && ATTIC_TEST_SHARDS=3 go test -race -count=1 ./service   # whole service suite on 3 shards
cd services/vmd && ATTIC_TEST_POSTGRES=skip go test -race -count=1 ./...  # skip Postgres-backed tests
cd e2e && go test -race -count=1 ./...                 # vmd and agentd together over their HTTP APIs
```

Postgres-backed tests use `libs/common/pg/pgtest`. It starts a real server, downloading PostgreSQL binaries to
`$TMPDIR/attic-pg-cache` on first use. Set `ATTIC_TEST_POSTGRES` to a DSN of an existing admin connection to use
that server instead. A killed test run can leave a `postgres -D` process: find it with
`pgrep -af 'attic-pg-cache/bin/postgres -D'` and kill it by PID.

Start a control plane with memory state (development):

```
vmd --role=all --listen=127.0.0.1:8080 --node-id=n1 \
  --trusted-key=admin=/etc/attic/admin.pub \
  --ssh-key=/var/lib/attic/ssh_ed25519 --data-dir=/var/lib/attic \
  --no-firewall --kvm=false --run-as=
```

Production layouts, with TLS, a Postgres DSN and a separate node per host, are described by the flag table.
Example: a control plane for two nodes.

```
vmd --role=control --listen=:8080 \
  --postgres-dsn=postgres://attic@db/attic \
  --trusted-key=admin=/etc/attic/admin.pub \
  --trusted-key=agentd=/etc/attic/agentd.pub:service \
  --node=node-1=https://node-1.internal:8443 --node=node-2=https://node-2.internal:8443 \
  --service-key=/etc/attic/vmd.key --service-key-id=vmd
```

Each node: `vmd --role=node --listen=:8443 --trusted-key=vmd=/etc/attic/vmd.pub:node --ssh-key=... --data-dir=...
--tls-cert=... --tls-key=...`.

Tokens come from `atticctl`. An operator token for capacity and drain, valid one hour:

```
atticctl token mint -key /etc/attic/admin.key -kid admin -sub user:ops -type user -bind '*:operator' -ttl 1h
```

Call an RPC:

```
curl -sS -X POST http://127.0.0.1:8080/attic.vmd.v1.ClusterService/SetNodeDraining \
  -H "Authorization: Bearer $OPS_TOKEN" -H "Content-Type: application/json" \
  -d '{"nodeId":"node-1","draining":true}'
```

Draining a node for maintenance: call `SetNodeDraining` with `draining:true`; find the node's VMs with `ListVMs`
(there is no node filter) and call `StopVM` on each; wait with `GetVM` and `wait_for_state` for `VM_STATE_STOPPED`;
do the work; undrain; call `StartVM` for each VM you stopped.

## Layout

```
cmd/vmd/            main.go: flags, roles, store opening, node registry, daemon start
domain/             internal model: VM, Disk, Image, Job, Node, Usage, states
service/            Connect handlers (vm, disk, image, exec, metrics, cluster), NewHandler (service.go, server.go),
                    reconciler.go, nodes.go (NodeManager), convert.go (domain <-> proto), config.go, servicemock/
scheduler/          Fits, Avail, Pick: capacity arithmetic
promcollect/        Prometheus collector: capacity, guest samples, reconcile counters (promcollect.go, sampler.go)
store/              Store interface and memory.go (reference implementation); storetest/ conformance suite; mock/
store/postgres/     Postgres implementation, migrations/001_init.up.sql, Migrate
store/shard/        router that spreads projects over several stores by rendezvous hashing
```

## Examples

Each example is a JSON `POST` with curl. The shell helper below sends one call; the placeholders (`$TOKEN`,
`vm_...`, `img_...`) stand for values from `atticctl token mint` or from earlier responses. A token with
`-bind proj-a:editor` can run the VM and exec calls; image registration needs an admin token with `*:admin`.

```
export API=http://127.0.0.1:8080
export TOKEN=...                    # from atticctl token mint
call() {                            # call <Service>/<Method> '<json>'
  curl -sS -X POST "$API/attic.vmd.v1.$1" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d "$2"
}
```

Errors come back as `{"code": "...", "message": "..."}`. Connect codes map to HTTP statuses as usual.

### 1. Register a shared image

An empty `projectId` makes the image shared, which needs a `*` binding. The path must be an absolute qcow2
file on the node named by `nodeId`.

```
call ImageService/RegisterImage '{"projectId":"","name":"debian-12","nodeId":"node-1",
  "path":"/var/lib/attic/base/debian-12.qcow2"}'
call ImageService/GetImage '{"id":"img_..."}'     # repeat until "state":"IMAGE_STATE_READY"
```

The node symlinks the file, so the image stays where it is. `IMAGE_STATE_ERROR` carries a `stateReason`.

### 2. Create a VM and wait for RUNNING

```
call VMService/CreateVM '{"projectId":"proj-a","name":"web-1","cpus":2,"memoryMb":2048,
  "imageId":"img_...","diskSizeGb":20,"start":true,"labels":{"role":"web"},
  "idempotencyKey":"web-1-create"}'
call VMService/GetVM '{"id":"vm_...","waitForState":"VM_STATE_RUNNING","waitSeconds":30}'
```

`CreateVM` returns at once with `VM_STATE_PENDING`. A retry with the same `idempotencyKey` returns the same VM.
The `GetVM` call returns early in `VM_STATE_ERROR`. Read `stateReason` and fix the cause. A VM short of capacity
stays `PENDING` with a reason that begins `waiting for capacity`. A VM created with `"start":false` is started
with `VMService/StartVM`.

### 3. Run a command and read its output

`StartJob` waits up to `waitSeconds` and returns the tail of the output (the last `maxOutput` bytes per stream).
Poll for the rest.

```
call ExecService/StartJob '{"vmId":"vm_...","command":"apt-get update && uname -a",
  "timeoutSeconds":600,"waitSeconds":25,"maxOutput":8192,"idempotencyKey":"run-1"}'

call ExecService/PollJob '{"jobId":"job_...","waitSeconds":25,
  "stdoutOffset":"<stdoutOffset from the previous response>",
  "stderrOffset":"<stderrOffset from the previous response>","maxOutput":8192}'
```

Print `stdout` and `stderr` (base64 in the JSON), keep the new offsets, and poll again while `job.state` is
`JOB_STATE_RUNNING` and `more` is true. The final states are `JOB_STATE_SUCCEEDED`, `JOB_STATE_FAILED`,
`JOB_STATE_CANCELLED`, `JOB_STATE_TIMED_OUT` and `JOB_STATE_LOST`. Use a new `idempotencyKey` for each new
command. Reusing a key with a different command is `invalid_argument`. `ExecService/CancelJob` with
`{"jobId":"job_..."}` stops a running job.

### 4. Copy a file in and out

The VM must be running. `mode` is decimal: 420 is `0644`. The content limit is 8 MiB.

```
CONTENT=$(base64 -w0 app.conf)
call ExecService/PutFile '{"vmId":"vm_...","path":"/home/agent/app.conf",
  "content":"'"$CONTENT"'","mode":420}'
call ExecService/GetFile '{"vmId":"vm_...","path":"/home/agent/app.conf","maxBytes":1048576}'
```

### 5. Snapshot a disk into an image

The disk must be `DISK_STATE_READY` and detached, or attached to a stopped VM. The first entry of the VM's
`diskIds` is the boot disk.

```
call VMService/StopVM '{"id":"vm_..."}'
call VMService/GetVM '{"id":"vm_...","waitForState":"VM_STATE_STOPPED","waitSeconds":30}'
call DiskService/SnapshotDisk '{"id":"disk_...","imageName":"web-1-snap"}'
call ImageService/GetImage '{"id":"img_..."}'     # CREATING until the flattened copy exists
```

When the image is `IMAGE_STATE_READY`, use it with `CreateVM` or `CreateDisk`.

### 6. Grow a disk and resize a VM

A disk only grows. If it is attached, its VM must be stopped. CPU and memory change only while the VM is stopped
and its desired state is stopped. `expectedGeneration` is the `generation` you last read; a mismatch is `aborted`.

```
call DiskService/ResizeDisk '{"id":"disk_...","sizeGb":40}'

call VMService/StopVM '{"id":"vm_..."}'
call VMService/GetVM '{"id":"vm_...","waitForState":"VM_STATE_STOPPED","waitSeconds":30}'
call VMService/UpdateVM '{"id":"vm_...","cpus":4,"memoryMb":4096,"expectedGeneration":"2"}'
call VMService/StartVM '{"id":"vm_..."}'
```

### 7. Delete a VM, its disks and an image

`DeleteVM` is refused with `failed_precondition` while the VM has holds (agents hold their VM). Delete the agents
first, or pass `"force":true`. `DeleteVM` force-stops a running guest. Call `StopVM` and wait for
`VM_STATE_STOPPED` first if the guest should power off cleanly.

```
call VMService/StopVM '{"id":"vm_..."}'
call VMService/GetVM '{"id":"vm_...","waitForState":"VM_STATE_STOPPED","waitSeconds":30}'
call VMService/DeleteVM '{"id":"vm_...","deleteDisks":true}'

call DiskService/DeleteDisk '{"id":"disk_..."}'          # only for a detached disk
call ImageService/DeleteImage '{"id":"img_..."}'         # refused while a disk was created from it
```

With `deleteDisks` false the disks are detached and stay, and can be attached to another VM.

### 8. Read VM metrics and cluster capacity

```
call MetricsService/GetVMMetrics '{"vmId":"vm_..."}'     # the VM must be running
call MetricsService/GetCapacity '{}'                      # needs an operator token (*:operator)
```

### 9. Drain a node

```
call ClusterService/SetNodeDraining '{"nodeId":"node-1","draining":true}'     # operator token
```

Running VMs keep running. Starts wait. For a full maintenance procedure see
[Draining a node for maintenance](../../deploy/README.md#draining-a-node-for-maintenance).

### 10. The same call with grpcurl

Reflection is not enabled, so pass the proto file. Use `-plaintext` for a cleartext server and omit it for TLS.

```
grpcurl -plaintext -proto api/proto/attic/vmd/v1/vmd.proto -import-path api/proto \
  -H "authorization: Bearer $TOKEN" -d '{"projectId":"proj-a"}' \
  127.0.0.1:8080 attic.vmd.v1.VMService/ListVMs
```

For Go, see [api](../../api/README.md#calling-from-go).

## Related

- [API definitions](../../api/README.md): the vmd and node protos and generated code.
- [libs/qemu](../../libs/qemu/README.md): the node side (driver, node API server and client).
- [libs/authz](../../libs/authz/README.md): tokens, roles, project bindings and the RBAC matrix.
- [libs/server](../../libs/server/README.md): daemon flags, health and RPC metrics.
- [libs/common](../../libs/common/README.md): `errs`, `ids`, `pg`.
- [agentd](../agentd/README.md): calls vmd with a service token.
- [MCP server](../mcp/README.md): calls vmd on behalf of Claude.
- [atticctl](../../cmd/atticctl/README.md): mints tokens and keys.
- [e2e](../../e2e/README.md): tests running vmd and agentd together.
- [ADR 0001: Connect RPC](../../docs/adr/0001-connect-rpc.md),
  [ADR 0002: state and leases](../../docs/adr/0002-state-and-leases.md),
  [ADR 0003: jobs live in the guest](../../docs/adr/0003-jobs-live-in-the-guest.md).
