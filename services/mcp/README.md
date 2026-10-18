# mcp

mcp is the MCP (Model Context Protocol) server of Attic Cloud. It exposes machines (vmd VMs), commands, files,
machine metrics and LLM agent tasks (agentd) as 20 MCP tools over streamable HTTP, and forwards the caller's own
bearer token to vmd and agentd for every call.

## Role

- Translates MCP tool calls into vmd and agentd Connect calls, and owns the tool names, argument checks, result
  formats and the wait budget that keeps each call under an MCP client's timeout.
- Adds no privileges. It verifies the bearer token, then sends that same token downstream, so vmd and agentd apply
  their own roles and project scoping. A viewer can read but not create; an editor of project A cannot see project B.
- Keeps no state of its own: no database, no cache, no machine or task records. The only in-memory state is the MCP
  SDK's sessions.
- Does not run VMs, models or jobs itself, and does not call Ollama or nodes.
- Serves tools only. It declares no resources or prompts.

## Position

```
  MCP client (Claude, other MCP hosts)
        |  streamable HTTP  POST/GET /mcp, Authorization: Bearer <token>
        v
  mcp  (requireBearer -> SDK session -> tool handlers)
        |  Connect, same bearer token forwarded
        +--> vmd:    VMService, ExecService, ImageService, MetricsService, ClusterService
        +--> agentd: AgentService, TaskService, ModelService
```

Connect clients are built in [`mcpserver/clients.go`](mcpserver/clients.go). `NewClients` also builds a
`DiskServiceClient`; no tool calls it.

Allowed imports (from `scripts/check-boundaries.sh`): `api`, `libs/common`, `libs/authz`, `libs/server`. The
server talks to vmd and agentd only through their generated clients. It has no other dependencies on the services.

Related: [vmd](../vmd/README.md) (machines, exec, files, capacity) and [agentd](../agentd/README.md) (agents, tasks,
models).

## Contract

### Transport and endpoints

| Path | Method | Behaviour |
| --- | --- | --- |
| `/mcp` and `/mcp/` | POST, GET, DELETE | MCP over streamable HTTP, served by the go-sdk v1.8.0 handler with default options. Sessions are stateful: the server returns an `Mcp-Session-Id` on `initialize`, and clients send it back. POST must accept both `application/json` and `text/event-stream`. |
| `/healthz` | GET | `200 ok` (text) while the process runs. It does not check vmd or agentd. |

There is no `/readyz`, no `/metrics`, and no RPC metrics. Logs are text (`slog`).

Server identity in `initialize`: name `attic`, version from the build (`-ldflags "-X main.version=..."`, default
`dev`). The server declares the `tools` capability with `listChanged: true`, which the SDK sets whenever tools are
registered. The server never sends `notifications/tools/list_changed`: all tools are registered in `New`, before any
session exists, and the tool set does not change while it runs.

### Authentication

Every request to `/mcp` needs `Authorization: Bearer <token>`. The token is a user or service token in the same
format vmd and agentd accept (Ed25519 JWT, see [AUTH](../../docs/AUTH.md)).

| Case | Response |
| --- | --- |
| No header, or not `Bearer ...` | `401`, body `missing bearer token`, header `WWW-Authenticate: Bearer realm="attic-mcp"`. |
| Token fails verification (signature, expiry, lifetime above `-max-token-ttl` plus one minute, unknown key, revoked) | `401`, body `invalid token`. |
| Valid | The principal and token are put in the request context. Each tool call forwards the token as the `Authorization` header of its vmd or agentd calls. |

The MCP server never mints tokens and never holds more power than the caller. Revocation is checked by this server
too (`-revoked-tokens`). vmd and agentd each check their own copy of that list.

Do not put tokens in URLs. Use the header only.

### Tool results and errors

- Each tool returns one text content block. Some tools put JSON in that text (documented per tool); the others give
  plain lines.
- A failed tool call is a normal result with `isError: true` and the text `<code>: <message>`, where `<code>` is the
  lowercased Connect code of the downstream error (for example `not_found`, `permission_denied`,
  `failed_precondition`, `invalid_argument`, `unavailable`), or plain text for errors the server itself raises. The
  model can read these and react. MCP protocol errors are not used for tool failures.
- `project` (optional on every tool) defaults to the server's `-default-project`. Machines are addressed by `name`,
  which is the VM name in that project.
- A machine name that does not exist in the project gives `not_found` (`no machine named "x" in project "p"`). The
  machine list is read with the project as the scope, so a caller with no access to the project gets
  `permission_denied` before any name is compared.

### Waiting

Tools that wait do so inside one call, with a budget derived from `-max-wait` (default 40 s, because clients give
up near 60 s):

| Tool | Budget |
| --- | --- |
| `agent_exec`, `agent_exec_poll` | `min(wait_seconds, max-wait)`; default `wait_seconds` 45, so 40 s at the default. |
| `agent_fetch` | `min(wait_seconds, max-wait)`; default 45. |
| `agent_ssh_pubkey` | `min(20 s, max-wait)`. |
| `agent_create`, `agent_resize` | `max-wait` minus 5 s (35 s at the default). |
| `agent_status` | `wait_seconds` (default 30), capped at `max-wait`; the whole call is bounded by `max-wait`. |
| `agent_stop`, `agent_destroy`, `agent_task`, `agent_cancel`, `agent_list`, the operator tools | No wait (one or a few requests). |

Every single downstream wait is at most 30 s (the vmd and agentd limit), and at most `max-wait`. A wait that expires
is not an error: the tool returns the state it has, and the text says what to do next (poll again).

### Machine tools

A machine is a vmd VM. `name` must be 1 to 31 characters of lowercase letters, digits and hyphens, and vmd also
requires it to start with a letter (the MCP check accepts a leading digit; vmd then refuses it).

| Tool | Arguments | Result | Notes |
| --- | --- | --- | --- |
| `agent_create` | `name` (required); `cpus` (1-32, default 8); `memory_gb` (1-56, default 16); `disk_gb` (at least 1, default 40); `image` (name or ID, default `-default-image`); `project` | Text: `machine 'x' is running (8 CPUs, 16 GiB)`, or `is booting (agent_list shows when it is ready)`. | Creates the VM and starts it, or starts a stopped VM with that name (its sizes and image are then not changed). Refused if the VM is already running or starting. An image name that matches two images is refused (give the ID). |
| `agent_list` | `project` | One line per machine: `name: state, N CPUs / X GiB[, reason: ...], task: <label>`. `(no machines)` when empty. | State: pending, stopped, starting, running, stopping, error, deleting. `task` is the newest agentd task of the machine's agents: `queued`, `running`, `done`, `failed`, `gave_up`, `cancelled`, `error`, `none` (no agent or no task), or `unknown` (agentd unreachable, or a `succeeded_unverified` task, see [Known limits](#known-limits)). |
| `agent_stop` | `name`, `project` | `machine 'x' is stopped` or `is stopping`. | Graceful stop of the guest. Disk and data are kept. Does not wait. |
| `agent_destroy` | `name`, `project`, `force` (default false) | `machine 'x' destroyed`. | PERMANENT. Deletes the agentd agents on the VM first (one by one; a failure stops the call before the VM is deleted), then deletes the VM with its boot disk. `force` is passed to vmd, which then deletes a VM other services hold. Returns once deletion is requested. |
| `agent_resize` | `name`, `project`, `cpus` (0 or omitted keeps it; 1-32), `memory_gb` (0 or omitted keeps it; 1-56), `force` (default false) | `machine 'x' resized from A to B; running again`, `; booting again (...)`, or `; applied; machine is stopped (agent_create starts it)`. | Needs at least one of `cpus` and `memory_gb`. A running machine is refused while it runs jobs, unless `force`. Otherwise it is stopped gracefully, resized and started, waiting within the budget. With `force` the job check is skipped; the running jobs end when the VM stops (vmd marks them lost). |

### Exec tools

Commands run inside the machine as `bash -lc <command>`. Jobs live in the guest
([ADR 0003](../../docs/adr/0003-jobs-live-in-the-guest.md)): a command keeps running after the tool returns, and
across MCP server restarts.

| Tool | Arguments | Result | Notes |
| --- | --- | --- | --- |
| `agent_exec` | `name`, `command` (required, not blank); `timeout` (1-604800 s, default 3600, total run time); `wait_seconds` (default 45); `max_output` (1-65536 bytes per stream, default 8192); `workdir` (default: the guest user's home, created if missing); `project` | JSON object (below). | Starts the job and waits for it within the budget. If `done` is false, poll with `agent_exec_poll`. |
| `agent_exec_poll` | `name`, `job_id` (required); `wait_seconds` (default 45); `stdout_offset`, `stderr_offset` (default 0); `max_output`; `tail` (bool); `project` | JSON object. | `tail` defaults to true when both offsets are 0, else false. With `tail`, the newest `max_output` bytes of each stream are returned. Otherwise the output after the offsets is returned (up to `max_output` per stream in total across the polls of the call), and the returned offsets are the ones to pass next. `more` true means more output is waiting. A job of another machine is refused. |
| `agent_exec_cancel` | `name`, `job_id` (required); `project` | JSON object (state `cancelled` if it was running). | Checks that the job belongs to the machine (reads all of the machine's job pages), then cancels. Cancelling a finished job returns its final state. |
| `agent_exec_list` | `name`; `project` | One line per job: `job_id state exit=<code or -> <seconds>s <command>`, command clipped to 80 characters. `(no jobs)` when none. | Reads one page of up to 50 jobs. See [Known limits](#known-limits) for which 50. |

The JSON object of the exec tools always has these keys: `job_id`, `done` (false while running), `state`
(`running`, `succeeded`, `failed`, `cancelled`, `timed_out` or `lost`), `exit_code` (null until it exits), `seconds`,
`stdout`, `stderr` (invalid UTF-8 replaced), `truncated`, `stdout_offset`, `stderr_offset`, `more`. `killed_reason` is
added only when the system stopped the job. `lost` means the VM stopped or the job process vanished without an exit
code.

### File tools

| Tool | Arguments | Result | Notes |
| --- | --- | --- | --- |
| `agent_upload` | `name`, `path` (absolute, required), `content` (required), `base64` (bool, default false), `mode` (octal such as `0644`, default `0644`; `0000` refused), `project` | `wrote N bytes to <path>` | `content` is plain text, or base64 of the bytes (whitespace is ignored). At most 8 MiB after decoding. |
| `agent_download` | `name`, `path` (required), `encoding` (`auto` default, `utf-8`, `base64`), `base64` (deprecated alias of `encoding=base64`), `max_bytes` (default 1 MiB), `project` | JSON: `path`, `size` (bytes returned), `sha256` (of those raw bytes), `encoding` (`utf-8` or `base64`, the one used), `content`, and `truncated` when set. | `auto` gives UTF-8 text when the bytes are valid UTF-8, base64 otherwise. `utf-8` fails on binary files. `max_bytes` may be up to 1 MiB for `utf-8` and up to 8 MiB for `auto` and `base64`. A cut inside a UTF-8 character is removed before the check. |
| `agent_fetch` | `name`, `url` (absolute http or https, required), `path` (required; absolute, or relative to the home directory; directories are created), `wait_seconds` (default 45), `project` | `ok: <ls -l line>` when it finished; `download still running as job <id>: poll it with agent_exec_poll`; or an error with the exit code and the last 1000 bytes of stderr. | The machine downloads the URL itself with `curl -fsSL --create-dirs` (curl must exist in the image). The job lives up to 3600 s. |
| `agent_ssh_pubkey` | `name`, `project` | The public key of `~/.ssh/id_ed25519.pub`, plus a line saying the private key never leaves the machine. | Creates an ed25519 key pair with no passphrase if there is none (needs `ssh-keygen` in the image). Waits up to 20 s. |

### Agent tools (LLM tasks)

An agentd agent is named after its machine and bound to its VM. `agent_task` creates it on first use, with the
idempotency key `mcp-agent-<vm id>`, and changes its model with `UpdateAgent` when the model differs.
Agent system prompt and working directory are the agentd defaults. Behaviour of tasks is in
[agentd](../agentd/README.md#behaviour-and-guarantees).

| Tool | Arguments | Result | Notes |
| --- | --- | --- | --- |
| `agent_models` | none | JSON array: `name`, `size_gib` (one decimal), `params`, `quant`, `fits_vram`, `context` (null when unknown). | `fits_vram` is agentd's check against its `-vram-gib` budget (default 16 GiB less 0.5 GiB). The tool text says 16 GiB; that is only the default. |
| `agent_task` | `name` (required); `task` (required, not blank); `model` (required, an installed model; there is no default); `check_command` (run in the machine; exit 0 means success); `max_steps` (default 40; agentd caps at 200; negative refused); `max_minutes` (0 or omitted: no limit; negative refused); `max_call_tokens` (default 32768, 0 = no cap, negative refused); `check_each_step` (default true); `project` | `task <id> started with model <m>`, or `task <id> queued with model <m> behind N unfinished task(s) ...`, plus `warning: model ... exceeds the VRAM budget ...` when `fits_vram` is false. | Returns at once; follow with `agent_status`. `N` counts the agent's queued and running tasks (best effort: at most 50 of each state). An unknown model is refused and lists the installed ones. Nothing is changed when validation fails. |
| `agent_status` | `name`; `wait_seconds` (default 30, capped); `since_step` (last step number seen); `max_commands` (default 10, 0 lists none); `brief` (bool); `project` | Text, described below. | While the latest task is queued or running the call waits (long poll on agentd). Fails with `no agent/task yet on machine 'x' (use agent_task)`. |
| `agent_cancel` | `name`; `project` | `cancel requested for task <id>`, or `no running task`. | Stops a queued or running task. A running task stops within about 1 s. |

`agent_status` output, in order:

1. `status: <label>  (N commands run, model <m>[, verdict: V][, verdict_source: S][, reason: R])`. Labels: `running`
   (queued or running), `done`, `done_unverified`, `failed`, `gave_up`, `cancelled`, `error`, `unknown`.
2. `header: {...}`: a JSON object with keys `status`, `step`, `max_steps`, `elapsed_s`, `prompt_tokens`,
   `gen_tokens`, `calls`, `first_command_s` (null until a command ran), `verdict_source`, `reason`, `last_command`
   (null when none; clipped to 120 characters).
3. `timing:` (created, started, finished in UTC; elapsed) and `tokens:` lines, and `last calls:` (the latest five
   model calls: index, seconds, generated tokens, `partial`, and the end reason when not `ok`). Not in
   `since_step` mode.
4. `$ <command>` lines for the latest `max_commands` commands (each clipped to 200 characters). In `since_step` mode,
   only commands after that step, each as `[seq] $ <command>`.
5. While running and not in `since_step` mode: `[model answer so far: ...]` (last 600 characters) and
   `[model reasoning (raw, may be noise): ...]` (last 300 characters). These are never part of the result.
6. `=== RESULT ===` and the final report, or `(no result yet)` while running, or `(none)`.

Modes: `brief=true` gives the header, and the result once the task has ended. `since_step=N` is the cheap poll: the
commands after N, the header, and the result once ended; it leaves out the timing and live lines. A poll that
returns while the task still runs has no `=== RESULT ===` section.

### Ops tools

| Tool | Arguments | Result | Permission |
| --- | --- | --- | --- |
| `agent_metrics` | `name`, `project` | Lines: `cpu:` (top-style, 100% per busy vCPU), `load:`, `memory:` (and swap), `uptime:`, `disk <disk id or ?> <mount> <device>: used / size (pct%)` per guest filesystem, `host disk <id>: allocated X of Y virtual` per host image. | `vm.metrics` on the project. The machine must be running, otherwise the error says so. |
| `cluster_capacity` | none | One line per node: `node <id> [state]: cpus a/b allocated (c available), memory ..., disk X free of Y, vms r/t running`, then `total: ...` over ready nodes only. | `capacity.get` (operators). |
| `node_drain` | `node_id` (required); `draining` (bool, required) | The node's line after the change. | `node.drain` (operators). `true` stops new VM starts on the node; `false` resumes. Persists across restarts. |

## Behaviour and guarantees

- Authorization is decided downstream. This server adds no check of its own beyond authentication, so an error
  from vmd or agentd is the answer the caller gets.
- A tool call makes a bounded number of downstream calls: lookups page through the project's machines, agents,
  images, tasks and jobs (pages of up to 200, or 50 for some job and task lists; agent steps in pages of 500), then
  one or a few calls to act.
- Machine lookup is by name within the project (`vmByName`). Names are unique per project in vmd, so a name matches at
  most one machine.
- Long-running work is never held open by the server: `agent_exec` and `agent_fetch` return a job ID, and the caller
  polls. The job keeps running in the VM whether or not anyone polls.
- `agent_destroy` and `agent_stop` return after the request, not after the VM has changed state.

## Configuration

Flags of `services/mcp/cmd/mcp` (`mcp -h` prints them):

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `:8082` | Address for `/mcp`, `/mcp/` and `/healthz`. |
| `-vmd-url` | none (required) | Base URL of the vmd API, for example `http://vmd.internal:8080`. |
| `-agentd-url` | none (required) | Base URL of the agentd API. |
| `-default-project` | none (required) | Project used when a tool call names none. |
| `-default-image` | empty | Image (name or ID) `agent_create` boots when none is given. Without it, `agent_create` needs `image`. |
| `-max-wait` | `40s` | Longest one tool call may block. Downstream waits are capped at 30 s and at this value. |
| `-trusted-key` | none (at least one required) | `kid=path[:role,role]`, a public key whose tokens are accepted. Repeatable. |
| `-max-token-ttl` | `24h` | Tokens whose lifetime exceeds this (plus one minute) are refused. |
| `-revoked-tokens` | none | File of revoked token IDs, one per line, `#` comments; reloaded within 5 s of a change. |
| `-tls-cert`, `-tls-key` | none | Serve HTTPS. Without `-tls-cert` the server logs a warning: tokens travel in clear text. |
| `-log-level` | `info` | `debug`, `info`, `warn` or `error`. |
| `-version` | false | Print the version and exit. |

The server needs no database and no service key: it forwards the caller's token and does not mint its own.
The downstream HTTP client has 8 idle connections per host and an idle timeout of 90 s. Shutdown drains requests
for up to 20 s.

## Data

None. The server stores nothing. The MCP SDK keeps sessions in memory: a restart ends them, and clients have to
initialize again. Machines, jobs, agents and tasks are owned by vmd and agentd.

## Observability

Logs only (text handler). Notable lines: `agent_list: cannot list agents` and `agent_list: cannot list tasks` (agentd
unreachable; the list still shows machines, with `unknown` labels), and `no -tls-cert` at start. There are no metrics.
Use vmd's and agentd's `/metrics` and logs for the work done downstream.

## Failure modes and recovery

| Failure | What the caller sees | What to do |
| --- | --- | --- |
| vmd or agentd unreachable | Tool error `unavailable: ...`. Transport errors carry the client's error text, which can include the downstream address; treat tool output as possibly sensitive. `agent_list` still lists machines, with `task: unknown`. | Restore the service. Retry. |
| Ollama down | `agent_models` and `agent_task` fail with `unavailable` (agentd). Existing tasks retry inside agentd. | Restore Ollama. |
| Token expired or revoked | `401` at the HTTP layer (`invalid token`). The MCP client must present a new token. | Mint a new token with `atticctl`, update the client. |
| Token valid but no access | `not_found` for machines outside the caller's projects; `permission_denied` for missing permissions. | Ask an administrator for the binding. |
| Job still running when the tool returns | `done: false` with a `job_id` (or the `download still running` line). Not an error. | Call `agent_exec_poll` with the `job_id`. |
| Wait expired on `agent_create` or `agent_resize` | The machine is `booting` or `stopped`. Not an error. | Check with `agent_list`. |
| MCP server restarted | Sessions are gone. Jobs and tasks continue (they live in vmd and agentd). | Clients reconnect and re-initialize. |
| MCP server down | No tools. Running jobs and tasks continue. | Restart the process; it is stateless. |
| TLS not configured | Works, but tokens are sent in clear text. | Terminate TLS in front of the server, or pass `-tls-cert` and `-tls-key`. |

## Connecting a client

The endpoint is `https://<host>/mcp` (use `http://` only for local testing). Send the token as a header and never in
the URL:

```
Authorization: Bearer $TOKEN
```

A JSON configuration for an MCP host that supports HTTP servers (placeholders only):

```json
{
  "mcpServers": {
    "attic": {
      "type": "http",
      "url": "https://$MCP_HOST/mcp",
      "headers": { "Authorization": "Bearer $TOKEN" }
    }
  }
}
```

Get a user token with `atticctl` (see [AUTH](../../docs/AUTH.md)). Keep it out of shared files. Set
`-default-project` to the project most calls will use, so `project` can be left out.

Quick check with curl (the `initialize` request must succeed before any tool call):

```
curl -sS -i https://$MCP_HOST/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H "Accept: application/json, text/event-stream" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize",
       "params":{"protocolVersion":"2025-06-18","capabilities":{},
                 "clientInfo":{"name":"curl","version":"0"}}}'
```

The response carries `Mcp-Session-Id`. Send it on later requests.

## Known limits

- `agent_list` shows `unknown` for an agentd task in state `succeeded_unverified` (the label mapping does not include
  it). `agent_status` shows it as `done_unverified`.
- `agent_exec_list` reads one page of jobs in vmd's order, which is oldest first by ID. On a machine with more than 50
  jobs it lists the oldest 50 (newest first among them), not the newest, and says so at the end. The tool text says
  "most recent": that is wrong for such machines.
- The waits text of `agent_create` and `agent_resize` says "about 25 s". The code waits `max-wait` minus 5 s (35 s at
  the default).
- `agent_download` reports `size` as the bytes returned. vmd does not report the size of a larger file, so a
  truncated read shows `truncated: true` with the size read.
- `agent_destroy` deletes agents one by one; if one fails the VM is not deleted and the call reports the error.
- The MCP name check accepts a leading digit; vmd does not. Such a name fails at `agent_create` with vmd's error.
- `agent_task`'s queue count is best effort (first page of 50 per state; errors count as 0).
- The VRAM figure in the `agent_models` text is fixed at 16 GiB; the real budget is agentd's `-vram-gib`.
- `agent_status` looks up the latest task by reading every task page of the agent, because agentd lists oldest first.
  A newest-first option in agentd would remove that cost.

## Running and testing

```
cd services/mcp && go build ./...
cd services/mcp && go vet ./...
cd services/mcp && go test -race -count=1 ./...
make test         # every module, from the repository root
```

`mcpserver` tests run each tool through the real MCP protocol over an in-memory transport, against fake vmd and
agentd clients (`testutil_test.go`). `cmd/mcp/main_test.go` covers the binary's start-up and flags.

End to end: `cd e2e && go test -race -count=1 ./...`. `e2e/mcp_test.go` runs an MCP client over HTTP against the real
vmd and agentd handlers (memory and Postgres stores): machine lifecycle, exec, files, an agent task, and the auth rules.

Run a local server (placeholders for every value):

```
mcp -listen :8082 \
  -vmd-url http://127.0.0.1:8080 \
  -agentd-url http://127.0.0.1:8081 \
  -default-project $PROJECT \
  -default-image $IMAGE \
  -trusted-key admin=./keys/admin.pub
```

## Layout

```
cmd/mcp/             binary: flags, HTTP mux (/mcp, /healthz), graceful shutdown
mcpserver/server.go  Config, New (registers every tool), Handler (streamable HTTP behind requireBearer)
mcpserver/auth.go    requireBearer (verifies the token), token context and forwarding
mcpserver/clients.go Connect clients for vmd and agentd, each carrying the caller's token
mcpserver/helpers.go addTool (text results, errors as tool errors), project default, wait cap, VM lookup, clip
mcpserver/machine_tools.go  agent_create, agent_list, agent_stop, agent_destroy, agent_resize
mcpserver/exec_tools.go     agent_exec, agent_exec_poll, agent_exec_cancel, agent_exec_list
mcpserver/file_tools.go     agent_upload, agent_download, agent_fetch, agent_ssh_pubkey
mcpserver/agent_tools.go    agent_models, agent_task, agent_status, agent_cancel
mcpserver/ops_tools.go      agent_metrics, cluster_capacity, node_drain
mcpserver/*_test.go         tool tests over an in-memory MCP transport
```

## Related

- [agentd](../agentd/README.md): agents, tasks, steps and models that the agent tools call.
- [vmd](../vmd/README.md): machines, jobs, files, images, capacity that the other tools call.
- [AUTH](../../docs/AUTH.md): tokens, roles and permissions.
- [ADR 0003: jobs live in the guest](../../docs/adr/0003-jobs-live-in-the-guest.md).
- [e2e](../../e2e/README.md), [deploy](../../deploy/README.md), [top-level README](../../README.md).
