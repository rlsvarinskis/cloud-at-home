# API reference

This document describes the public APIs of vmd and agentd: what each RPC does, which permission it needs, the errors
it returns and the limits it enforces. The wire definitions are in [`api/proto/attic/`](../api/proto/attic/) (the
source of truth); the Go types are generated into [`api/gen/`](../api/gen/). Behaviour below was read from the handlers in
`services/vmd/service` and `services/agentd/service`, and the examples were run against a development vmd.

The system design (placement, reconciler, failure model) is in [ARCHITECTURE.md](ARCHITECTURE.md). Flags and
deployment are in [OPERATIONS.md](OPERATIONS.md). Roles and tokens are in [AUTH.md](AUTH.md).

## Contents

- [Conventions](#conventions)
- [Limits](#limits)
- [vmd services](#vmd-services): VMService, DiskService, ImageService, ExecService, MetricsService, ClusterService
- [agentd services](#agentd-services): AgentService, TaskService, ModelService
- [Calling the API](#calling-the-api): curl, grpcurl, Go client
- [Workflows](#workflows)

## Conventions

### Transport

Every RPC is available at `POST /<package>.<Service>/<Method>` with the three protocols that Connect serves
from one handler:

| Protocol | Use |
| --- | --- |
| Connect (JSON) | `curl`, browsers, any HTTP client. `Content-Type: application/json`. |
| gRPC | `grpcurl`, generated gRPC clients. Cleartext HTTP/2 is accepted without TLS. |
| gRPC-Web | Browser clients through a proxy. |

Requests and responses use the protobuf JSON mapping: field names are lowerCamelCase (`projectId`),
enums are written as their names (`"VM_STATE_RUNNING"`), and `int64` values are written as JSON strings in
responses (`"memoryMb": "2048"`). Requests accept numbers or strings for `int64`. `bytes` fields are base64.
Fields with zero values are omitted from responses.

Server reflection is not enabled. Clients must load the `.proto` files themselves.

### Authentication

Send `Authorization: Bearer <token>` on every call. Tokens are Ed25519-signed JWTs issued by `atticctl`
(users) or by a service (see [AUTH.md](AUTH.md)). A missing, malformed, expired, revoked or
untrusted token gets `unauthenticated` with the message `unauthenticated`; the server never says why.

### Authorization

Each handler checks a named permission (for example `vm.create`) against the project of the resource it
touches. Roles and their permissions are listed in [AUTH.md](AUTH.md#roles-and-permissions). Two rules
decide which error a caller sees:

- **Requests that name a project** (create, list): a caller with no binding on that project gets
  `permission_denied`.
- **Requests that address a resource by ID** (get, update, delete, jobs, disks, images): a caller with no
  binding on the resource's project gets `not_found`, the same answer as for a missing ID. A caller who has
  some role on the project but lacks the permission gets `permission_denied`.

Shared images (empty `project_id`) are readable by any caller with `image.get` in some project. Changing them
needs a binding on project `*`.

### Errors

Errors are JSON objects `{"code": "...", "message": "..."}` with the Connect code:

| Code | When (vmd and agentd) |
| --- | --- |
| `unauthenticated` | Missing, invalid, expired, revoked or untrusted token; a token whose lifetime exceeds the server's maximum. |
| `permission_denied` | The caller lacks the permission on the project named in the request or on the resource's project. Global permissions (`capacity.get`, `node.drain`, `image.register` with an empty project) need a `*` binding. |
| `not_found` | Unknown ID, or the caller has no binding on the resource's project. |
| `invalid_argument` | Validation failed: bad name, out-of-range number, wrong path, unknown model, idempotency key reused for a different request, and so on. |
| `failed_precondition` | The state does not allow the operation: VM not running for an exec call, VM not stopped for a resize, disk still attached, image still used by a disk, VM held without `force`, node not registered. |
| `already_exists` | A task's idempotency key was already used for another agent's task. |
| `resource_exhausted` | A project quota would be exceeded (quotas are set with `--quota-*` flags and are unlimited by default; see [OPERATIONS.md](OPERATIONS.md#quotas)), or no ready node has enough free disk for a disk created without an image. |
| `aborted` | Optimistic concurrency failed after retries, or `expected_generation` did not match. |
| `unavailable` | The node, vmd (as seen from agentd) or the LLM backend could not be reached. The message names the component, never its address, for example `unavailable: node n1 is unavailable`. Retry later. |
| `internal` | Anything unexpected, including database errors. The text is hidden from the client and logged by the server. |

Connect maps these codes to HTTP statuses (for example `unauthenticated` to 401, `not_found` to 404,
`invalid_argument` to 400). Server logs carry the details.

### Long-polling and waits

Calls that can take a long time accept `wait_seconds`. The server re-checks the state about every 200 ms
and returns when the condition holds or the wait expires. The wait is clamped to **30 seconds** everywhere,
because clients and proxies tend to give up around 60 seconds. A wait is never an error: when it expires, the
call returns the current state.

| RPC | Wait parameter | Returns early when |
| --- | --- | --- |
| `VMService.GetVM` | `wait_seconds` with `wait_for_state` | the VM reaches that state, or the VM is in `VM_STATE_ERROR` (any `wait_for_state`) |
| `ExecService.StartJob` | `wait_seconds` | the job is final |
| `ExecService.PollJob` | `wait_seconds` | the job is final (new output alone does not end the wait) |
| `TaskService.GetTask` | `wait_seconds` with `wait_for_not` | the task's state differs from `wait_for_not` |

### Pagination

`page_size` defaults to 50 and is capped at 200. A list response has `next_page_token` when there is another
page; pass it back as `page_token`. Tokens are opaque IDs. Lists are ordered by ID, which is time-ordered.

### Idempotency keys

Some creates accept `idempotency_key`. The key identifies one request. Retrying the same request with the same key
returns the result of the first call instead of doing the work again. Keys are scoped to the project.

| RPC | Effect of a retry with the same key |
| --- | --- |
| `VMService.CreateVM` | Returns the existing VM. The other fields of the retry are ignored. |
| `ExecService.StartJob` | If the VM, command and workdir match, returns the existing job. If any of them differ, `invalid_argument`. If the first attempt never reached the guest (the node could not be reached), the retry starts the same job again. |
| `AgentService.CreateAgent` | Returns the existing agent. |
| `TaskService.CreateTask` | Returns the existing task. A key reused for another agent's task gives `already_exists`. |

Keys are not accepted by the other creates (disks, images, snapshots).

## Limits

| Limit | Value | Where |
| --- | --- | --- |
| Wait per call (`wait_seconds`) | 30 s | all long-poll RPCs |
| Page size | default 50, max 200 | all lists |
| Command text (`StartJob.command`) | 256 KiB, no NUL bytes, not blank | vmd |
| Job timeout (`timeout_seconds`) | default 3600 s, max 604800 s (7 days) | vmd |
| Job output per call (`max_output`) | default 8 KiB, max 64 KiB, per stream | vmd |
| File content (`PutFile`) | 8 MiB | vmd |
| File read (`GetFile.max_bytes`) | default and max 8 MiB | vmd |
| VM vCPUs | 1 to 64 | vmd (service default, not settable by flag) |
| VM memory | 256 MB to 1 TiB | vmd (service default, not settable by flag) |
| Disk size | 1 to 4096 GB; default boot disk 20 GB | vmd (service default, not settable by flag) |
| Agent system prompt | 8 KiB | agentd |
| Task prompt | 64 KiB, not blank | agentd |
| Task `check_command` | 16 KiB | agentd |
| Task `max_steps` | default 40, max 200 | agentd |
| Task `max_call_tokens` | default 32768 per model reply; 0 = no cap | agentd |
| Task `max_minutes` | default 0 (no time limit) | agentd |
| Steps per `ListSteps` call (`limit`) | default 100, max 500 | agentd |
| Model calls kept per task (`calls`) | the latest 50 | agentd |

Resource names (VMs, disks, images, agents) must match `^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`.

---

## vmd services

### VMService

Virtual machines. A VM has a desired power state (`desired_state`) and an observed state (`state`). Calls
change the desired state; the reconciler moves the VM towards it. `state_reason` explains the last failure
or stop. The reconciler's steps are described in [OPERATIONS.md](OPERATIONS.md#vm-lifecycle-and-the-reconciler).

VM states: `VM_STATE_PENDING` (waiting for its disks, or for capacity), `VM_STATE_STOPPED`,
`VM_STATE_STARTING`, `VM_STATE_RUNNING`, `VM_STATE_STOPPING`, `VM_STATE_ERROR` (see `state_reason`; the
reconciler keeps retrying), `VM_STATE_DELETING`.

A VM's node is the node that holds its boot disk. A VM is never moved between nodes.

| RPC | Purpose | Key request fields | Permission | Notes and errors |
| --- | --- | --- | --- | --- |
| `CreateVM` | Create a VM. Optionally start it. | `project_id`, `name`, `cpus`, `memory_mb`, one of `image_id` (creates a boot disk of `disk_size_gb`) or `disk_id` (uses a detached disk), `start`, `labels`, `idempotency_key` | `vm.create` | The image must be `READY`, or `failed_precondition`. A disk must be detached and not in error or deleting. The node must be registered (`failed_precondition` otherwise); CPU and memory are checked when the VM starts, not here. Quotas are checked: the VM count and, for a new boot disk, the disk GB; running vCPUs and memory only when `start` is true. |
| `GetVM` | Read a VM, optionally waiting for a state. | `id`, `wait_for_state`, `wait_seconds` | `vm.get` | `not_found` for unknown or other-project VMs. |
| `ListVMs` | List a project's VMs. | `project_id`, `page_size`, `page_token`, `label_selector` | `vm.list` | Label selector: every given key must match exactly. There is no node filter; `node_id` is in each VM. |
| `UpdateVM` | Change CPUs, memory or labels. | `id`, `cpus`, `memory_mb`, `labels`, `replace_labels`, `expected_generation` | `vm.update` | Resizing needs the VM `VM_STATE_STOPPED` with desired stopped, else `failed_precondition`. Resizing does not check quotas. `expected_generation` (non-zero) must equal the current generation, else `aborted`. Each spec change bumps `generation`. Labels merge unless `replace_labels` is true. If nothing changes (identical values, including labels), the current VM is returned and nothing is written. |
| `StartVM` | Set desired state to running. | `id` | `vm.start` | The project's running vCPUs and memory are checked against its quota. A VM in `VM_STATE_ERROR` is retried immediately, and its crash and failure counts are reset. Deleting VMs get `failed_precondition`. |
| `StopVM` | Set desired state to stopped. | `id`, `force` | `vm.stop` | Without `force` the guest is powered off gracefully, and killed after 60 s if it does not stop. `force` kills at once. |
| `DeleteVM` | Delete a VM. | `id`, `force`, `delete_disks` | `vm.delete` | A held VM needs `force` (else `failed_precondition`). The VM stops, then its row is removed. Disks are detached; with `delete_disks` they are deleted too. |
| `AddHold` | Prevent deletion on behalf of another service. | `id`, `holder` | `vm.hold` | `holder` is required (`invalid_argument` if empty). Idempotent. Refused for deleting VMs. |
| `RemoveHold` | Release a hold. | `id`, `holder` | `vm.hold` | `holder` is required. Idempotent. |

`holds` on the VM lists the holders. `DeleteVM` with `force` ignores holds.

### DiskService

Disks are qcow2 files on one node. A disk created from an image is a copy-on-write overlay of that image.

| RPC | Purpose | Key request fields | Permission | Notes and errors |
| --- | --- | --- | --- | --- |
| `CreateDisk` | Create an empty disk or an overlay of an image. | `project_id`, `name`, `size_gb`, `image_id` (optional), `node_id` (optional placement hint) | `disk.create` | With an image, the disk goes on the image's node; a different `node_id` is `invalid_argument`. A `node_id` that is not registered is `failed_precondition`. Without an image and without `node_id`, the ready node with the most free space that has `size_gb` free is chosen, else `resource_exhausted`. The project's disk quota is checked. The disk is `DISK_STATE_CREATING` until the reconciler creates the file. |
| `GetDisk` | Read a disk. | `id` | `disk.get` | |
| `ListDisks` | List a project's disks. | `project_id`, `page_size`, `page_token` | `disk.list` | |
| `ResizeDisk` | Grow a disk. | `id`, `size_gb` | `disk.update` | Grow only: a smaller or equal size is `invalid_argument`. The disk must be `READY`, and if attached its VM must be stopped (`failed_precondition`). The project's disk quota is checked for the added GB. |
| `DeleteDisk` | Delete a disk. | `id` | `disk.delete` | Must be detached (`failed_precondition`). State becomes `DISK_STATE_DELETING`, and the reconciler removes the file. |
| `SnapshotDisk` | Create an image from a disk. | `id`, `image_name` | `disk.snapshot` | The disk must be `READY` and detached, or attached to a stopped VM. Returns the new image in `IMAGE_STATE_CREATING`; it becomes `READY` once the flattened copy exists. The image belongs to the disk's project. |

Disk states: `CREATING`, `READY`, `ERROR`, `DELETING`. The disk response has no reason field. A disk in `ERROR`
makes the VM that uses it `ERROR`, and the reason appears in that VM's `state_reason`.

### ImageService

Images are read-only qcow2 files on one node. Project images belong to one project. Shared images have an
empty `project_id`.

| RPC | Purpose | Key request fields | Permission | Notes and errors |
| --- | --- | --- | --- | --- |
| `RegisterImage` | Adopt an existing qcow2 file on a node as an image. | `project_id` (empty for shared), `name`, `node_id`, `path` | `image.register` (admin only) | `path` must be an absolute, clean path on the node. The node must be registered, else `failed_precondition`. An empty `project_id` needs a binding on project `*`. `project_id` `*` is `invalid_argument`. The image is `IMAGE_STATE_CREATING` until the node has adopted it. The file is symlinked, not copied. A missing file, or a file that is not qcow2, ends in `IMAGE_STATE_ERROR`. |
| `GetImage` | Read an image. | `id` | `image.get` | Shared images need `image.get` in some project. Other projects' images are `not_found`. |
| `ListImages` | List a project's images plus shared ones. | `project_id`, `page_size`, `page_token` | `image.list` | |
| `DeleteImage` | Delete an image. | `id` | `image.delete` (admin only) | Refused with `failed_precondition` while any disk was created from it. Deleting a shared image needs a binding on `*`. The image goes to `IMAGE_STATE_DELETING`, and the node removes the file. |

Image states: `CREATING`, `READY`, `ERROR`, `DELETING`. `size_bytes` is set when the image is ready. The image
response has no reason field; check the node's log for the cause of `ERROR`.

### ExecService

Commands run inside the VM as **jobs**. A job is started with `bash -lc <command>` in a detached session in
the guest. Its state and output live in the guest (under `/var/tmp/mcp-jobs/<id>/`), not in the control
plane. Job metadata (command, state, exit code) is kept by vmd. No call waits more than 30 seconds.

Jobs survive restarts of vmd and of the node daemon. The guest keeps the 50 most recent finished job directories:
when a new job starts, finished jobs older than those 50 are deleted.

A job's `state` is one of:

| State | Meaning |
| --- | --- |
| `JOB_STATE_RUNNING` | Still running. |
| `JOB_STATE_SUCCEEDED` | Exit code 0. |
| `JOB_STATE_FAILED` | Non-zero exit code, or the command could not be started (`killed_reason` starts with `could not start:`). |
| `JOB_STATE_CANCELLED` | Stopped by `CancelJob`; `killed_reason` is `cancelled`. |
| `JOB_STATE_TIMED_OUT` | Killed by the job timeout (exit code 124); `killed_reason` is `timed out and was killed`. |
| `JOB_STATE_LOST` | The VM stopped or rebooted while the job ran, or the job's files disappeared. `killed_reason` gives the cause. |

A job whose VM is no longer running is marked `LOST` the next time `PollJob` or `CancelJob` reads it. `ListJobs`
does not change state, and no background task marks jobs.

| RPC | Purpose | Key request fields | Permission | Notes and errors |
| --- | --- | --- | --- | --- |
| `StartJob` | Start a command and wait up to `wait_seconds`. | `vm_id`, `command`, `workdir`, `timeout_seconds`, `wait_seconds`, `max_output`, `idempotency_key` | `exec.run` | The VM must be `VM_STATE_RUNNING` (`failed_precondition` otherwise). An empty `workdir` is the home directory of the guest user the job runs as; a missing directory is created. Returns a `JobOutput` with the **tail** of the output (last `max_output` bytes per stream). Idempotency is described [above](#idempotency-keys). |
| `PollJob` | Read a job's state and new output. | `job_id`, `wait_seconds`, `stdout_offset`, `stderr_offset`, `max_output`, `tail` | `exec.get` | Offset mode (default) returns output from the given offsets. `tail` returns the last `max_output` bytes instead. The response's `stdout_offset` and `stderr_offset` are where the next poll starts. `more` is true when output remains. `truncated` is true when earlier output was omitted. For a job whose VM is not running, the call returns the record with no output. |
| `CancelJob` | Stop a running job. | `job_id` | `exec.cancel` | Sends the cancel to the guest and waits up to 3 seconds for the job to end. A finished job is returned unchanged. |
| `ListJobs` | List a VM's jobs, in creation order. | `vm_id`, `page_size`, `page_token` | `exec.list` | |
| `PutFile` | Write a file into a running VM. | `vm_id`, `path`, `content` (up to 8 MiB), `mode` (default `0644`, which is `420` in JSON) | `file.write` | The VM must be running. `path` must not contain NUL bytes. `mode` must be a permission mask up to `07777`; `0` means `0644`. |
| `GetFile` | Read a file from a running VM. | `vm_id`, `path`, `max_bytes` | `file.read` | The VM must be running. `max_bytes` defaults to and is capped at 8 MiB; `truncated` says whether the file was longer. |

### MetricsService

| RPC | Purpose | Key request fields | Permission | Notes and errors |
| --- | --- | --- | --- | --- |
| `GetVMMetrics` | Sample a running VM now. | `vm_id` | `vm.metrics` | The VM must be running (`failed_precondition` otherwise). The sample is taken live from the node. Units and meanings are in [OPERATIONS.md](OPERATIONS.md#metric-catalogue). `cpu_percent` is like `top`: 100 per busy vCPU, so an 8-vCPU guest can show 800. |
| `GetCapacity` | Cluster capacity for operators. | none | `capacity.get` (operator role, global) | Per node and in total. `total` covers ready nodes only. `cpus_available` and `memory_mb_available` are what the scheduler would still allocate, after overcommit and the memory reserve. |

`GetCapacity` node `state` is `ready`, `draining` or `down`. `draining` is set with `ClusterService.SetNodeDraining`.
When a node is both down and draining, `state` is `down`.

### ClusterService

Operator actions on the fleet. The RPC is global: it is not scoped to a project.

| RPC | Purpose | Key request fields | Permission | Notes and errors |
| --- | --- | --- | --- | --- |
| `SetNodeDraining` | Stop (`draining: true`) or resume (`draining: false`) placing VM starts on a node. Returns the node's `NodeCapacity`. | `node_id`, `draining` | `node.drain` (operator role, global) | An empty `node_id` is `invalid_argument`; an unknown node is `not_found`. The decision is stored apart from node health: it survives an outage and a restart of vmd. Semantics are in [OPERATIONS.md](OPERATIONS.md#draining-a-node). |

```
curl -sS -X POST http://127.0.0.1:8080/attic.vmd.v1.ClusterService/SetNodeDraining \
  -H "Authorization: Bearer $OPS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"nodeId":"node-1","draining":true}'
```

The response has the same fields as a `GetCapacity` node entry, with `"state":"draining"`.

---

## agentd services

agentd's handlers are in `services/agentd/service` and its task runner is in `services/agentd/runner`. The
entry point, with its flags, is `services/agentd/cmd/agentd`; see [OPERATIONS.md](OPERATIONS.md#running-agentd).

An agent is always bound to one VM (`vm_id` is required). Many agents can share a VM; each has its own working
directory (default `/home/agent/agents/<agent id>`). When an agent is created, agentd places a hold on the VM
named `agentd/<agent id>`, so the VM cannot be deleted from under the agent. Agent `state`: `AGENT_STATE_ACTIVE`,
`AGENT_STATE_ORPHANED` (set by the runner when a task finds its VM gone), `AGENT_STATE_DELETING`.

### AgentService

| RPC | Purpose | Key request fields | Permission | Notes and errors |
| --- | --- | --- | --- | --- |
| `CreateAgent` | Create an agent on a VM and place its hold. | `project_id`, `name`, `vm_id`, `model`, `system_prompt`, `workdir`, `labels`, `idempotency_key` | `agent.create` | `model` and `vm_id` are required. The VM must exist in the same project (`not_found` otherwise) and must not be deleting. The model must be installed on the LLM backend (`invalid_argument` names the installed models). If the backend cannot be reached the model check is skipped. `workdir` must be an absolute, clean path. If the hold cannot be placed the agent is removed again. |
| `GetAgent` | Read an agent. | `id` | `agent.get` | |
| `ListAgents` | List a project's agents, optionally one VM's. | `project_id`, `vm_id`, `page_size`, `page_token` | `agent.list` | |
| `UpdateAgent` | Change the model and/or system prompt. | `id`, `model`, `system_prompt` | `agent.update` | Unset fields are left alone. The VM and workdir cannot be changed. |
| `DeleteAgent` | Delete an agent. | `id` | `agent.delete` | Cancels its unfinished tasks, deletes the agent with its tasks, steps and messages, and releases the hold. A failed release is logged, not returned. |

### TaskService

A task is one prompt given to an agent. Workers claim queued tasks under a lease and run them.

| RPC | Purpose | Key request fields | Permission | Notes and errors |
| --- | --- | --- | --- | --- |
| `CreateTask` | Queue a task for an agent. | `agent_id`, `prompt`, `check_command`, `max_steps`, `max_minutes`, `max_call_tokens`, `check_each_step`, `idempotency_key` | `task.create` | The agent must be `ACTIVE` (`failed_precondition` otherwise). Limits are in the table above. `check_command` is run in the VM and its exit status decides the outcome (see below). `check_each_step` (default true) runs it after every batch of commands and ends the task as soon as it passes. `max_minutes` cancels the task after that long, with `reason` `timeout`. `max_call_tokens` caps one model reply. |
| `GetTask` | Read a task, optionally waiting. | `id`, `wait_for_not`, `wait_seconds` | `task.get` | Returns as soon as the state differs from `wait_for_not` (for example `TASK_STATE_QUEUED`), or after the wait. |
| `ListTasks` | List an agent's tasks or a project's tasks. | exactly one of `agent_id` or `project_id`; `state`; `page_size`; `page_token` | `task.list` | Giving both or neither is `invalid_argument`. |
| `CancelTask` | Stop a task. | `id` | `task.cancel` | A queued task becomes `TASK_STATE_CANCELLED` at once. A running task is flagged; within about a second its worker aborts the model call and cancels the command that is running in the VM. A finished task is returned unchanged. Cancelling twice is harmless. |
| `ListSteps` | List the commands a task ran. | `task_id`, `after_seq`, `limit` | `task.get` | Steps are numbered from 1. `after_seq` is the cursor: only steps with a higher number are returned, so pass the last `seq` you have seen (0 for the first page). `limit` is the page size (default 100, max 500). Each step has the command, an excerpt of its output (the first and last 2000 bytes) and its exit code. Output is cleaned before it is stored: NUL bytes are removed and invalid UTF-8 is replaced. |

**Task states**

| State | Meaning |
| --- | --- |
| `TASK_STATE_QUEUED` | Waiting for a worker. Also used after a transient failure, when the task is put back in the queue. |
| `TASK_STATE_RUNNING` | Claimed by a worker, which holds a lease that it renews. |
| `TASK_STATE_SUCCEEDED` | Verified success: the `check_command` exited 0. |
| `TASK_STATE_SUCCEEDED_UNVERIFIED` | The model's last line was `VERDICT: SUCCESS` but there is no `check_command`, so nothing verified it. The result says so. |
| `TASK_STATE_FAILED` | The `check_command` exited non-zero, or, without one, the model's last line was `VERDICT: FAILED - ...`. |
| `TASK_STATE_GAVE_UP` | No verdict: the model gave no verdict after one reminder, the step limit was reached, or the model's replies kept repeating themselves or hit the token cap (`reason` says which). |
| `TASK_STATE_CANCELLED` | Cancelled before it finished. |
| `TASK_STATE_ERROR` | The task could not be run to a verdict: the agent is not active, the VM is not running or is gone (the agent is then set to `ORPHANED`), the model failed with an error other than "unavailable", a stored message is corrupt, or the task was taken over or requeued more than three times. `result` says which. |

When a `check_command` is set, the model's verdict is recorded in `verdict` and noted in `result`, but the
exit status of the check decides the state. Transient failures (VM or model unreachable) requeue the task
and count as an attempt.

**Progress fields.** A task also reports how it is going, and why it ended. `reason` is set when the task ended
early or stopped on a guard: `repetition`, `token_limit`, `step_limit`, `timeout`, or `check passed` (a
`check_each_step` success). `verdict_source` is `check` or `model`: who decided the state. `prompt_tokens` and
`completion_tokens` count the tokens of completed model calls; an aborted call counts one token per streamed chunk and
is marked `partial`. `model_calls` and `partial_calls` count the calls, `first_token_seconds` and
`first_command_seconds` are measured from the start. `calls` lists the latest 50 model calls (`index`, `seconds`,
`prompt_tokens`, `gen_tokens`, `end`, `partial`, `note`). While a call streams, `live_answer` (the last ~600 characters
of the answer) and `live_reasoning` (the last ~300 characters of the raw reasoning) are updated about once a second.
They are cleared when the call ends and are never part of `result`.

A task never ends with an empty `result`: if none was written, a note saying so is stored. A task that would end as
a success without a result ends as `TASK_STATE_ERROR` instead.

### ModelService

| RPC | Purpose | Permission | Notes |
| --- | --- | --- | --- |
| `ListModels` | List models installed on the LLM backend. | `model.list` (any project) | Returns `models`, each with `name`, `size_bytes`, `size_gib`, `params` and `quant` (as the backend reports them, may be empty), `context_length` (0 when unknown) and `fits_vram`. `fits_vram` is `size_gib <= budget - 0.5`, where the budget is agentd's `-vram-gib` flag (default 16). It is a rough check of the weights only: the KV cache and the runtime are not counted. Errors are `unavailable` when the backend cannot be reached. |

---

## Calling the API

The examples use one project, `proj-a`, and one token for an editor or admin with a binding on `proj-a`.
Replace `img_...` with an image ID from `RegisterImage` or `ListImages`.

### curl (Connect JSON)

```
export TOKEN=...   # from atticctl token mint
curl -sS -X POST https://attic.example.com/attic.vmd.v1.VMService/CreateVM \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"projectId":"proj-a","name":"web-1","cpus":2,"memoryMb":2048,
       "imageId":"img_...","diskSizeGb":20,"start":true,
       "labels":{"role":"web"},"idempotencyKey":"web-1-create"}'
```

A successful call returns the VM as JSON; a failed one returns `{"code": ..., "message": ...}`.

### grpcurl

Reflection is not enabled, so pass the proto file. Use `-plaintext` for a cleartext server, and omit it for TLS.

```
grpcurl -plaintext \
  -proto api/proto/attic/vmd/v1/vmd.proto -import-path api/proto \
  -H "authorization: Bearer $TOKEN" \
  -d '{"projectId":"proj-a","name":"web-1","cpus":2,"memoryMb":2048,
       "imageId":"img_...","diskSizeGb":20,"start":true,
       "labels":{"role":"web"},"idempotencyKey":"web-1-create"}' \
  localhost:8080 attic.vmd.v1.VMService/CreateVM
```

### Go client

The generated Connect client takes an HTTP client, the base URL and options. `authz.ClientInterceptor` adds the
bearer token to each call. The snippet below is a `package main` for a module that requires `api` and `libs/authz`.
It lists the VMs in project `p1` when run against a development server.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1/vmdv1connect"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
)

func main() {
	client := vmdv1connect.NewVMServiceClient(
		http.DefaultClient,
		"http://127.0.0.1:8080",
		connect.WithInterceptors(authz.ClientInterceptor(authz.StaticToken(os.Getenv("TOKEN")))),
	)
	res, err := client.ListVMs(context.Background(),
		connect.NewRequest(&vmdv1.ListVMsRequest{ProjectId: "p1", PageSize: 10}))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("vms:", len(res.Msg.Vms))
}
```

`authz.StaticToken` suits a fixed token. For a service that mints its own tokens, use
`authz.ServiceTokens` (see [AUTH.md](AUTH.md#service-tokens-in-go)). Errors returned by the client are
`*connect.Error`; `connect.CodeOf(err)` gives the code. `errs.FromConnect(err)` converts them to the
`libs/common/errs` sentinels so that `errors.Is(err, errs.ErrNotFound)` works.

---

## Workflows

The steps below use curl with `$TOKEN` set and `$API=https://attic.example.com`. Each `call` is
`curl -sS -X POST "$API/attic.vmd.v1.<Service>/<Method>" -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '<json>'`.

### 1. Register a shared image

Only admins can register images. An empty `projectId` makes the image shared, which needs a binding on `*`.

```
call ImageService/RegisterImage '{"projectId":"","name":"debian-12","nodeId":"node-1",
  "path":"/var/lib/attic/base/debian-12.qcow2"}'
```

The response is `IMAGE_STATE_CREATING`. Poll until `IMAGE_STATE_READY`:

```
call ImageService/GetImage '{"id":"img_..."}'
```

`IMAGE_STATE_ERROR` means the node could not adopt the file. The response has no reason field, so check the node's
log. The path must be an absolute qcow2 file on the node named by `nodeId`.

### 2. Create a VM and wait for RUNNING

```
call VMService/CreateVM '{"projectId":"proj-a","name":"web-1","cpus":2,"memoryMb":2048,
  "imageId":"img_...","diskSizeGb":20,"start":true,"idempotencyKey":"web-1-create"}'
# returns the VM with state VM_STATE_PENDING and an id vm_...

call VMService/GetVM '{"id":"vm_...","waitForState":"VM_STATE_RUNNING","waitSeconds":30}'
```

Repeat the `GetVM` call while the state is not `VM_STATE_RUNNING`. The call also returns early in
`VM_STATE_ERROR`: read `stateReason` and fix the cause. A VM that is short of capacity stays in
`VM_STATE_PENDING` with a `stateReason` beginning `waiting for capacity`. A VM that was created with
`start: false` is started with `VMService/StartVM`.

### 3. Run a command

`StartJob` waits up to `waitSeconds` and returns the tail of the output. To read the rest, poll.

```
call ExecService/StartJob '{"vmId":"vm_...","command":"apt-get update && uname -a",
  "timeoutSeconds":600,"waitSeconds":25,"maxOutput":8192,"idempotencyKey":"run-1"}'
```

The response has `job.id`, `job.state`, `stdoutOffset`, `stderrOffset`, `more` and `truncated`. If
`job.state` is `JOB_STATE_RUNNING`, loop:

```
call ExecService/PollJob '{"jobId":"job_...","waitSeconds":25,
  "stdoutOffset":<stdoutOffset from the previous response>,
  "stderrOffset":<stderrOffset from the previous response>,"maxOutput":8192}'
```

Print the returned `stdout` and `stderr` (base64 in the JSON), store the new offsets, and continue until
`job.state` is one of `JOB_STATE_SUCCEEDED`, `JOB_STATE_FAILED`, `JOB_STATE_CANCELLED`,
`JOB_STATE_TIMED_OUT` or `JOB_STATE_LOST`. When `more` is false and the job is final, nothing is left.
`ExecService/CancelJob` stops a job. Use a new `idempotencyKey` for each new command. Reusing a key with a
different command is `invalid_argument`.

### 4. Copy files

```
call ExecService/PutFile '{"vmId":"vm_...","path":"/home/agent/app.conf",
  "content":"<base64 of the file, up to 8 MiB>","mode":420}'

call ExecService/GetFile '{"vmId":"vm_...","path":"/home/agent/app.conf","maxBytes":1048576}'
```

`mode` is decimal (420 is `0644`). Both calls need a running VM.

### 5. Snapshot a disk into an image

Stop the VM first, or detach the disk.

```
call VMService/StopVM '{"id":"vm_..."}'
call VMService/GetVM '{"id":"vm_...","waitForState":"VM_STATE_STOPPED","waitSeconds":30}'
call DiskService/SnapshotDisk '{"id":"disk_...","imageName":"web-1-snap"}'
```

The new image is `IMAGE_STATE_CREATING` until its flattened copy exists. Poll `ImageService/GetImage`. The image
can then be used with `CreateVM` or `CreateDisk`.

### 6. Resize

A disk grows while detached or while its VM is stopped; it cannot shrink.

```
call DiskService/ResizeDisk '{"id":"disk_...","sizeGb":40}'
```

A VM's CPUs and memory change only while it is stopped:

```
call VMService/StopVM '{"id":"vm_..."}'
call VMService/GetVM '{"id":"vm_...","waitForState":"VM_STATE_STOPPED","waitSeconds":30}'
call VMService/UpdateVM '{"id":"vm_...","cpus":4,"memoryMb":4096,"expectedGeneration":"2"}'
call VMService/StartVM '{"id":"vm_..."}'
```

`expectedGeneration` is the `generation` you last read. A mismatch is `aborted`.

### 7. Delete

```
call VMService/StopVM '{"id":"vm_..."}'
call VMService/DeleteVM '{"id":"vm_...","deleteDisks":true}'
```

`DeleteVM` is refused with `failed_precondition` while the VM has holds, unless `force` is set. With `force`,
the VM is stopped at once. Disks are deleted only with `deleteDisks`; otherwise they stay and can be attached to
another VM. A disk can be deleted only when it is detached; an image can be deleted only when no disk was
created from it.

```
call DiskService/DeleteDisk '{"id":"disk_..."}'
call ImageService/DeleteImage '{"id":"img_..."}'
```

### 8. Agents on a VM

Create an agent. Each agent gets its own working directory and its own hold on the VM.

```
call AgentService/CreateAgent '{"projectId":"proj-a","name":"builder","vmId":"vm_...",
  "model":"llama3.1:8b","systemPrompt":"You build the project in /home/agent/src.",
  "idempotencyKey":"builder-1"}'
```

Several agents can share one VM, for example one for builds and one for logs. Each has its own `workdir`
(default `/home/agent/agents/<agent id>`), and each places its own hold `agentd/<agent id>` on the VM.
`DeleteVM` on the VM is refused until all of its agents are deleted (or `force` is used).

Queue a task with a check that decides the outcome:

```
call TaskService/CreateTask '{"agentId":"agent_...",
  "prompt":"Make the unit tests in /home/agent/src pass.",
  "checkCommand":"cd /home/agent/src && go test ./...","maxSteps":60,
  "idempotencyKey":"tests-1"}'
```

Wait for the task to leave the queue, then wait for it to finish:

```
call TaskService/GetTask '{"id":"task_...","waitForNot":"TASK_STATE_QUEUED","waitSeconds":30}'
call TaskService/GetTask '{"id":"task_...","waitForNot":"TASK_STATE_RUNNING","waitSeconds":30}'
```

Repeat the second call until the state is final (`SUCCEEDED`, `FAILED`, `GAVE_UP`, `CANCELLED` or `ERROR`).
Read the commands with `TaskService/ListSteps` and `afterSeq` set to the last `seq` you have. To stop a task,
call `TaskService/CancelTask`. Its state becomes `TASK_STATE_CANCELLED` at once if it was queued, or within about
a second if it was running.
