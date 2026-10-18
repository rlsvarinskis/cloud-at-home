# agentd

agentd runs LLM agents on top of vmd's virtual machines: it stores agents (each bound to one VM) and their tasks,
and drives an Ollama model through a `bash` tool (and `apply_patch` for gpt-oss models) inside the VM until the
task ends.

## Role

- Owns agents (a model, a system prompt and a working directory, all bound to one VM), tasks, task steps and the
  stored conversation of each task.
- Owns the task queue: workers claim tasks under a database lease, renew it, and resume tasks whose worker died.
- Owns the verdict of a task: it decides the final state from the model's `VERDICT:` line, from `check_command`, or
  from its loop guards, and records why.
- Places and releases the agent's hold on its VM through vmd, so the VM cannot be deleted under a running agent.
- Does not run VMs, nodes or QEMU and does not import `libs/qemu`. Every command and file operation goes through
  vmd's public API ([vmd](../vmd/README.md)) as a service principal.
- Does not serve models. It lists the models Ollama has installed and reports whether each fits a VRAM budget.
- Does not authenticate users itself beyond verifying bearer tokens; projects and roles come from `libs/authz`.

## Position

```
  MCP server (../mcp) or any Connect client
          |  Connect RPC, bearer token
          v
  agentd: AgentService, TaskService, ModelService  +  workers (runner)
          |                         |                               |
          | Postgres (or memory)    | Connect, service token        | HTTP
          v                         v                               v
  agentd database            vmd API (GetVM, holds,           Ollama (/api/chat,
                             StartJob, PollJob, files)        /api/tags, /api/show)
```

agentd may import `api`, `libs/common`, `libs/authz` and `libs/server` only. It must not import `services/vmd` or
`libs/qemu`, and it shares no database with vmd. `scripts/check-boundaries.sh` enforces this; its rules are the
`ALLOWED` table in that script.

## Contract

The API is defined in [`api/proto/attic/agentd/v1/agentd.proto`](../../api/proto/attic/agentd/v1/agentd.proto) and
served by Connect on the same paths for gRPC, gRPC-Web and the Connect protocol (JSON or binary). Method paths are
`/attic.agentd.v1.<Service>/<Method>`. Field names are JSON camelCase in the Connect JSON form. The generated Go
types are in [api](../../api/README.md).

### Authorization and errors (all RPCs)

Every RPC needs a bearer token. Permissions are checked against the project of the resource. A caller with no binding
on the resource's project gets `not_found` for an ID-addressed resource and `permission_denied` for a request that
names a project. The permission each RPC needs is in its table; roles are listed in [libs/authz](../../libs/authz/README.md#roles-and-permissions).

Errors are Connect codes mapped from `libs/common/errs`
([libs/common](../../libs/common/README.md)). Those agentd produces on purpose:

| Code | When |
| --- | --- |
| `invalid_argument` | Bad name, missing required field, value out of range, NUL byte, unknown or uninstalled model, relative `workdir`, too-long prompt. |
| `not_found` | Unknown ID, or no access to the resource's project. For a VM named in `CreateAgent`, also a VM of another project. |
| `already_exists` | A second agent with the same name in a project; an idempotency key reused for another agent's task. |
| `failed_precondition` | The agent is not `ACTIVE` (`CreateTask`); the VM is being deleted (`CreateAgent`). |
| `permission_denied` | The caller has access to the project but lacks the permission. |
| `unavailable` | vmd or Ollama could not be reached. The message names the component, never its address. Retry later. |
| `internal` | Unexpected errors. The text is hidden and logged. |

Long-polling waits are capped at 30 seconds (`maxWaitSeconds`). A wait that expires is not an error.

### Agent service

| RPC | Request fields | Response | Permission | Errors and notes |
| --- | --- | --- | --- | --- |
| `CreateAgent` | `project_id` (required), `name`, `vm_id` (required), `model` (required), `system_prompt`, `workdir`, `labels`, `idempotency_key` | `Agent` | `agent.create` | `name` must match `^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`. `system_prompt` is at most 8 KiB. `workdir` defaults to `/home/agent/agents/<agent id>` and must be absolute and clean. The VM must exist in the project (else `not_found`) and must not be `DELETING`. `model` must be installed; if Ollama cannot be reached the check is skipped. Then the agent is stored and a hold `agentd/<agent id>` is placed on the VM; if the hold fails the agent is deleted again and vmd's error is returned (`unavailable` when vmd cannot be reached). A retry with the same `idempotency_key` returns the existing agent. |
| `GetAgent` | `id` | `Agent` | `agent.get` | `not_found` for an unknown ID or no access. |
| `ListAgents` | `project_id` (required), `vm_id` (optional filter), `page_size`, `page_token` | `agents`, `next_page_token` | `agent.list` | Keyset paging by ID (oldest first). `page_size` defaults to 50, capped at 200. |
| `UpdateAgent` | `id`, `model` (optional, non-empty), `system_prompt` (optional) | `Agent` | `agent.update` | Unset fields are kept; an empty `system_prompt` clears it. A new `model` is checked against the backend like `CreateAgent`. The VM, workdir and name cannot change. A running task keeps the agent it loaded when it was claimed, so a model change applies to later claims. |
| `DeleteAgent` | `id` | empty | `agent.delete` | Requests cancellation of every unfinished task, deletes the agent with its tasks, steps and messages, then releases the hold. A failed release is logged, not returned. |

`Agent` fields: `id`, `project_id`, `name`, `vm_id`, `model`, `system_prompt`, `workdir`, `state`, `labels`,
`created_at`, `updated_at`. `state` is `AGENT_STATE_ACTIVE`, `AGENT_STATE_ORPHANED` or `AGENT_STATE_DELETING`.
`ORPHANED` is set by the runner when a task finds its VM gone. It is terminal: nothing sets it back, so the agent
refuses new tasks and must be deleted. `DELETING` is defined but no code path sets it.

### Task service

A task is one prompt for one agent. Its states and the rules that set them are in
[Behaviour](#behaviour-and-guarantees).

| RPC | Request fields | Response | Permission | Errors and notes |
| --- | --- | --- | --- | --- |
| `CreateTask` | `agent_id` (required), `prompt` (required, not blank, at most 64 KiB), `check_command` (at most 16 KiB), `max_steps`, `check_each_step`, `max_minutes`, `max_call_tokens`, `idempotency_key` | `Task` (state `QUEUED`) | `task.create` | `max_steps`: default 40, above 200 is clamped to 200, negative is `invalid_argument`. `check_each_step`: default true. `max_minutes`: default 0 (no limit), negative is invalid. `max_call_tokens`: default 32768, 0 means no cap, negative is invalid. The agent must be `ACTIVE` (`failed_precondition`). A retry with the same key returns the first task; the same key for another agent is `already_exists`. |
| `GetTask` | `id`, `wait_for_not`, `wait_seconds` | `Task` | `task.get` | Long poll. Returns as soon as the state differs from `wait_for_not`, or when `wait_seconds` (max 30) runs out, then with the current task. With no `wait_for_not` it returns at once. Re-checks every 200 ms. |
| `ListTasks` | `agent_id` or `project_id` (exactly one), `state` (optional), `page_size`, `page_token` | `tasks`, `next_page_token` | `task.list` | Oldest first by ID. Both or neither is `invalid_argument`. Page size as for agents. |
| `CancelTask` | `id` | `Task` | `task.cancel` | A queued task becomes `CANCELLED` at once (result `[cancelled]`). A running task gets `cancel_requested`; its worker stops within about a second and finishes it `CANCELLED`. A finished task is returned unchanged. Repeating is harmless. |
| `ListSteps` | `task_id`, `after_seq`, `limit` | `steps` | `task.get` | The cursor is `after_seq`: returns steps with `seq` greater than it (0 = from the start). `limit` defaults to 100, capped at 500. There is no next token: pass the last `seq` you have. Steps are dense from 1. |

`Task` fields, in short: identity (`id`, `agent_id`, `project_id`), input (`prompt`, `check_command`, `max_steps`,
`check_each_step`, `max_minutes`, `max_call_tokens`), outcome (`state`, `verdict`, `verdict_source`, `reason`,
`result`), progress (`steps`, `attempts`, `model_calls`, `partial_calls`, `prompt_tokens`, `completion_tokens`,
`first_token_seconds`, `first_command_seconds`, `calls`), live text (`live_answer`, `live_reasoning`), and the
timestamps `created_at`, `started_at`, `finished_at`. `cancel_requested` is also returned.

- `verdict` is `SUCCESS`, `FAILED` or empty, as the model last stated it.
- `verdict_source` is `check` or `model`. It is empty while the task is queued or running and for tasks that ended
  in `error` or `cancelled` before a decision.
- `reason` is set when the task ended early or stopped on a guard: `repetition`, `token_limit`, `step_limit`,
  `timeout`, or `check passed`. It is empty for an ordinary final report.
- `calls` holds the latest 50 model calls, oldest first, each with `index` (1-based), `seconds`, `prompt_tokens`,
  `gen_tokens`, `end` (`ok`, `cancel`, `timeout`, `repetition`, `length`, `parse`, `error`), `partial` and `note`.
- `live_answer` (last 600 characters of the reply) and `live_reasoning` (last 300 characters of the raw reasoning)
  are written about once a second while a call streams. They are cleared when the call ends and are never part of
  `result`.
- `first_token_seconds` and `first_command_seconds` are seconds from `started_at`; 0 means not yet.
- `prompt_tokens` and `completion_tokens` are summed from completed calls. An aborted call adds one token per
  streamed piece to `completion_tokens` and is marked `partial`. `model_calls` counts every call that reached the
  model, failed ones included, and `partial_calls` counts the aborted ones.

### Model service

| RPC | Request | Response | Permission | Notes |
| --- | --- | --- | --- | --- |
| `ListModels` | none | `models` | `model.list` on any project | Lists the models installed on Ollama. Each `Model`: `name`, `size_bytes`, `size_gib`, `params` (for example `8.0B`), `quant` (for example `Q4_K_M`), `context_length` (0 when unknown), `fits_vram`. `fits_vram` is `size_gib <= vram_gib - 0.5`, using the `-vram-gib` flag. It is weights-only: the KV cache and runtime are not counted. Backend failure is `unavailable`. |

`params` and `quant` come from Ollama's `/api/tags` details; `context_length` comes from the `<arch>.context_length`
key of `/api/show`, one call per model made concurrently. A failed show call leaves it 0.

### HTTP endpoints

| Path | Behaviour |
| --- | --- |
| `/healthz` | Always `200 ok` (text). |
| `/readyz` | `200 {"status":"ok"}`, or `503 {"status":"unavailable","failed":["postgres"]}`. The only check is Postgres, and only when a DSN is set. Each check has a 2 s timeout; error text is not returned. |
| `/metrics` | Prometheus text. Present only with `-metrics=true` (the default). Requires a bearer token whose principal holds `capacity.get` on the global scope (an operator token). Missing or invalid token: `401`. No permission: `403`. |

### Calls agentd makes to vmd

Through `vmclient` only. Each call uses a service token minted by agentd (subject `svc:agentd`, five-minute life).
vmd must trust agentd's key with the `service` role; see [Running](#running-and-testing).

| agentd call | vmd RPC | vmd permission used |
| --- | --- | --- |
| `GetVM` | `VMService.GetVM` | `vm.get` |
| `AddHold`, `RemoveHold` | `VMService.AddHold`, `RemoveHold` | `vm.hold` |
| `StartJob` | `ExecService.StartJob` | `exec.run` |
| `PollJob` | `ExecService.PollJob` | `exec.get` |
| `CancelJob` | `ExecService.CancelJob` | `exec.cancel` |
| `PutFile` | `ExecService.PutFile` | `file.write` |
| `GetFile` | `ExecService.GetFile` | `file.read` |

Each vmd call gets at most 60 s (`defaultTimeout` in `vmclient`), or less if the caller's deadline is sooner.

### Go API (for in-process use and tests)

- `service.NewHandler(service.Deps) http.Handler`: the Connect services and `/healthz`, `/readyz`, `/metrics`.
  `Deps` holds `Store`, `VM`, `LLM`, `Verifier`, `Config` (`VRAMGiB`, `HoldPrefix`, `Log`, `Now`), `Registry`, `Ready`.
- `runner.New(runner.Config) *Runner`, `Run(ctx)` (blocks until `ctx` ends and workers return), `RunOnce(ctx)`
  (claims and runs at most one task). `Config` is in [Configuration](#configuration). `Hooks` are `OnTaskFinished`,
  `OnStep`, `OnLeaseLost`.
- `llm.Client`: `Chat(ctx, model, msgs, tools, ChatOptions{MaxTokens}, onChunk) (Message, error)` and
  `ListModels(ctx) ([]ModelInfo, error)`. A malformed tool call is `*llm.ToolParseError` (`Raw`, `Msg`).
- `llm.NewOllama(baseURL) *Ollama`. Fields: `BaseURL`, `NumCtx` (default 100000), `HTTP`.
- `llm.Fake`: a scripted client for tests (`Say`, `CallTool`, `FakeReply` with `Chunks`, `Err`, `Block`, `Endless`,
  `DoneReason`, token counts). It records every call in `Calls`.
- `runner/toolcall`: `LenientJSON`, `Normalize`, `ParseHarmony`. `runner/applypatch`: `Parse`, `Apply(patch, fs, root)`.
- `vmclient.New(httpClient, baseURL, tokens) VM`: the `VM` interface (GetVM, AddHold, RemoveHold, StartJob, PollJob,
  CancelJob, PutFile, GetFile).
- `store.NewMemory()`, `postgres.New(pool)`, `postgres.Migrate(ctx, pool)`.

## Behaviour and guarantees

### Task states

| State | Set when | Decided by |
| --- | --- | --- |
| `QUEUED` | Created; put back after a transient failure (`attempts` +1). | - |
| `RUNNING` | Claimed by a worker (lease held). Also after a takeover of an expired lease. | worker |
| `SUCCEEDED` | `check_command` exited 0 (after a batch, or at the end). | check |
| `SUCCEEDED_UNVERIFIED` | No `check_command`; the model's last verdict was `SUCCESS` and no guard ended the task. | model |
| `FAILED` | With a check: the check failed and no guard ended the run. Without a check: the verdict was `FAILED` and no guard ended the run. | check or model |
| `GAVE_UP` | Without a check: no verdict line, or any guard ended the run. With a check: a guard ended the run and the check failed. | model or guard |
| `CANCELLED` | A cancel was requested, or `max_minutes` ran out (`reason` `timeout`, result `[cancelled: timeout]`). | request or timer |
| `ERROR` | The task cannot reach a verdict; see the list below. `result` says why. | runtime |

Transitions:

- `QUEUED` to `RUNNING`: a worker claims the oldest queued task, or a running task whose lease expired. A takeover
  increments `attempts`. Tasks with `cancel_requested` are not claimed.
- `RUNNING` to `QUEUED`: a transient failure (vmd unavailable, Ollama connection failed) releases the lease and
  increments `attempts`. Retried after the poll interval.
- `RUNNING` or `QUEUED` to `CANCELLED`: see `CancelTask` above. A running task is finished by its worker. A worker
  that holds no lease writes nothing.
- `RUNNING` to `ERROR`: the agent is not `ACTIVE` (it is then marked `ORPHANED` if the VM is gone); the VM is not
  `RUNNING` (`result` names its state); the VM is gone (agent becomes `ORPHANED`); the stored conversation is corrupt;
  more than three consecutive unusable tool calls; a non-transient VM failure; a non-transient Ollama error (HTTP
  error, bad stream); `attempts` above 3 (`gave up after 3 takeovers`, or `gave up after 3 attempts: <cause>` when
  requeued); a panic in the run; a `SUCCEEDED` or `SUCCEEDED_UNVERIFIED` with no result text.

An empty result is never stored. A missing result gets the note `[no result recorded; state was <state>]`. A success
with no result is stored as `ERROR` instead.

### Who decides, and the verdict

- With `check_command`, the check decides: exit 0 is `SUCCEEDED`, anything else `FAILED` (or `GAVE_UP` when a guard
  ended the run). The model's `VERDICT:` line is kept in `verdict` and noted in `result`
  (`note: the model reported FAILED but the check passed`, and the like).
- Without a check, the model's last `VERDICT:` line decides: `SUCCESS` is `SUCCEEDED_UNVERIFIED` (noted in the
  result as the model's own claim), `FAILED - <reason>` is `FAILED`. No verdict line is `GAVE_UP`. A guard always
  ends a check-less run as `GAVE_UP`, even with a `SUCCESS` line.
- A check run after each batch (see below) that passes ends the task as `SUCCEEDED` at once, with
  `verdict_source` `check`, `reason` `check passed` and no `verdict`.
- `verdict_source` is `model` whenever the loop concluded from the model's reply, even when the reply had no
  verdict line.

### The loop

Each worker runs one task at a time in this order:

1. Load the stored conversation. On the first run store the system message (the built-in text, the working
   directory rule and the agent's `system_prompt`) and the user prompt.
2. If the last assistant message has tool calls without results, run the missing calls first. This is how a
   resumed task continues.
3. If the last message is a text reply, decide it (step 6).
4. If `steps` has reached `max_steps`, conclude with reason `step_limit`. The model is not called again.
5. Otherwise call the model once (a turn). The reply is stored before anything uses it.
   - A reply with tool calls is run call by call (below).
   - A text reply with a `VERDICT:` line concludes the task. A text reply without one gets one nudge
     (`Give your final report now...`); a second text reply without a verdict concludes it.
6. Conclude: decide the state (verdict, check, guards) and write the final state, verdict, result and progress
   in one fenced write.

Tool calls:

- Models are offered `bash` (`command`). Models whose name starts with `gpt-oss` are also offered `apply_patch`
  (`patch`).
- Each call is normalised by `runner/toolcall.Normalize` before it runs. Tool names `shell`, `exec`, `run`,
  `terminal`, `functions.bash` and the like become `bash`; `python`, `python3`, `ipython` and
  `code_interpreter` run their code through a `python3` heredoc; `patch`, `edit` and `applypatch` become
  `apply_patch`; a list argument is an argv (`["bash","-lc",X]` becomes `X`); a `workdir` or `cwd` key prefixes
  `cd`. Unknown tools and missing text are errors.
- A call the backend cannot parse is repaired when its raw text holds a JSON object (`LenientJSON` escapes raw
  control characters, closes open brackets and tolerates text around the object). The call is recorded with
  `end` `parse` and the note `repaired a malformed tool call`. An unrepairable call is answered with the parser
  error and a request to re-emit it as valid JSON, and is not run.
- A harmony-format call (`to=functions.bash {...}`) leaked into a text reply without tool calls is run as a call.
- Each call that runs is stored with its step and tool message in one transaction, and the step counter moves.
  `apply_patch` runs inside the VM relative to the working directory; a patch is atomic (no file changes if any
  part fails). Each command runs under a job key `<task id>-<step>`, so a command started by a dead worker is
  found again rather than started twice.
- Each command has a 10-minute limit (`CommandTimeout`), enforced by vmd. The check command has 5 minutes.
- After the last call of a reply, if `check_each_step` is on, a `check_command` is set and a command ran in that
  batch, the check runs (key `<task id>-check-<step>`). Exit 0 ends the task as described above.
- `max_steps` is counted in commands. Calls past the limit are refused with an error result, not run.

### Guards

Constants are in `runner/loop.go`; they are not flags. Design rationale is in
[ADR 0004](../../docs/adr/0004-agent-loop-guards.md).

| Guard | Rule | Outcome |
| --- | --- | --- |
| Unusable tool calls | After more than 3 in a row (`maxParseFails` = 3) | `ERROR`. A call that runs resets the count. Counted per worker run. |
| Token cap | Each reply is capped at `max_call_tokens` (Ollama `num_predict`, default 32768; 0 sends no cap). A reply that stops on the cap is discarded. | The model is asked to wrap up or make a shorter call. |
| Repetition | While a reply streams, every 16 pieces the last 500 characters are checked (once at least 250 characters exist). A repeat is a 24-character phrase with at least 8 letters or digits that occurs 8 or more times. Positions are sampled every 4 characters. | The call is aborted (the connection is closed so Ollama stops), the reply is discarded, the model is asked to wrap up. |
| Cut replies | More than 2 discarded replies (`maxCuts` = 2) per worker run. | The task ends with reason `repetition` or `token_limit`: `GAVE_UP`, or `SUCCEEDED` if a check ran and passed. |
| Step limit | `steps` reaches `max_steps` (default 40, max 200). | Reason `step_limit`, concluded as above. |
| Time limit | `max_minutes` (default 0, none) counts from `started_at`. Checked every second. | The running command is killed and the task ends `CANCELLED` with reason `timeout`. |
| Takeovers | `attempts` above 3 (`defaultMaxAttempts`). | `ERROR`. |

A cut reply or a repetitive reply still counts as a model call. Its streamed pieces count as generated tokens and it
is marked `partial`, so token counts for those calls are estimates.

### Leases, takeover and cancellation

- A worker claims with `ClaimTasks`: it sets `lease_owner` to `<hostname>-<pid>`, `lease_until` to now plus 30 s,
  and the state to `RUNNING`. The claim is `FOR UPDATE SKIP LOCKED` in Postgres.
- The worker renews the lease every 10 s (one third of the TTL). If the renewal finds another owner, the run is
  cancelled, the lease is treated as lost and the worker writes nothing more (`agentd_lease_lost_total` increments).
- Every write goes through a fenced transaction that checks the lease owner and that the task is not final. A worker
  that lost its lease cannot write a final state.
- A worker stops by shutdown (context ends): the task is left `RUNNING`; its lease expires after 30 s and another
  worker takes it over. A command that was running keeps running in the VM and the next worker reattaches to it by
  its job key.
- A cancel is noticed within about 1 s. A running command is killed only for a cancel or a time limit, never for a
  shutdown or a lost lease.
- Workers poll for work every 500 ms when idle; a transient failure also waits one poll interval.

### Limits

| Limit | Value | Where |
| --- | --- | --- |
| Agent `system_prompt` | 8 KiB | `CreateAgent`, `UpdateAgent` |
| Task `prompt` | 64 KiB, not blank | `CreateTask` |
| Task `check_command` | 16 KiB | `CreateTask` |
| `max_steps` | default 40, max 200 (clamped) | `CreateTask` |
| `max_call_tokens` | default 32768; 0 = no cap | `CreateTask` |
| `max_minutes` | default 0 (none) | `CreateTask` |
| `GetTask` wait | 30 s | `GetTask` |
| Page size | default 50, max 200 | lists of agents and tasks |
| Steps per `ListSteps` | default 100, max 500 | `ListSteps` |
| Stored step output | first 2000 and last 2000 bytes | runner |
| Command output shown to the model | first and last 4000 bytes | runner |
| Check output kept in result | last 1500 bytes | runner |
| Stored result | 32 KiB (head kept) | runner |
| Stored command text | 8 KiB (the executed command is never cut) | runner |
| Patch: file read from VM | 4 MiB | `apply_patch` |

## Configuration

Flags of `services/agentd/cmd/agentd` (`agentd -h` prints them):

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `:8081` | Address for the API, `/healthz`, `/readyz` and `/metrics`. |
| `-postgres-dsn` | `$ATTIC_POSTGRES_DSN` | Postgres DSN. Empty uses the in-memory store (development only; all state is lost on restart). Migrations run at start. |
| `-log-level` | `info` | `debug`, `info`, `warn` or `error`. Text log format. |
| `-max-token-ttl` | `24h` | Tokens whose lifetime exceeds this (plus one minute) are refused. |
| `-trusted-key` | none (at least one required) | `kid=path[:role,role]`: a public key whose tokens are trusted, optionally limited to roles. Repeatable. |
| `-revoked-tokens` | none | File of revoked token IDs (`jti`), one per line, `#` comments. Reloaded within 5 s of a change. |
| `-metrics` | `true` | Serve `/metrics` (operator token). |
| `-tls-cert`, `-tls-key` | none | Serve HTTPS (HTTP/1.1 and HTTP/2) with this certificate. |
| `-vmd-url` | none (required) | Base URL of vmd's API, for example `http://vmd.internal:8080`. |
| `-ollama-url` | `http://127.0.0.1:11434` | Ollama base URL. |
| `-service-key` | none (required) | PKCS#8 private key agentd signs its vmd service tokens with. |
| `-service-key-id` | `agentd` | Key id of `-service-key`. vmd must trust it with the `service` role. |
| `-workers` | `2` | Tasks run concurrently in this process. |
| `-vram-gib` | `16` | GPU memory budget in GiB for `fits_vram` (less 0.5 GiB headroom). |
| `-version` | false | Print the version (set with `-ldflags "-X main.version=..."`) and exit. |

Environment: `ATTIC_POSTGRES_DSN` is the default of `-postgres-dsn`. Tests use `ATTIC_TEST_POSTGRES` (see below).

Not configurable, set in code: lease TTL 30 s (heartbeat 10 s), idle poll 500 ms, cancel poll 1 s, command timeout
10 min, check timeout 5 min, start wait 2 s, start request timeout 20 s, vmd poll wait 25 s, cancel grace 10 s,
max attempts 3, Ollama `num_ctx` 100000, and the loop constants in the [Loop](#the-loop) and [Limits](#limits)
tables. The service also has a 20 s shutdown grace and a 10 s header read timeout.

vmd side: vmd must trust agentd's public key with the `service` role, for example
`--trusted-key=agentd=/etc/attic/agentd.pub:service`. The `service` role grants what agentd needs and nothing
destructive ([libs/authz](../../libs/authz/README.md#roles-and-permissions)).

## Data

Postgres, schema `001_init` (`store/postgres/migrations/001_init.up.sql`), tracked in `schema_migrations_agentd`.
Migrations run at start and are safe to run from several replicas.

| Table | Holds | Key points |
| --- | --- | --- |
| `agents` | One row per agent. | Unique `(project_id, name)`; unique `(project_id, idempotency_key)` when set; index on `vm_id`. `state` checked to `active`, `orphaned`, `deleting`. Row `version` for compare-and-swap. |
| `tasks` | One row per task, with its progress. | Unique `(project_id, idempotency_key)` when set; claim index on `(state, id)` for queued and running rows; `agent_id` and `project_id` indexes. `calls` is JSON. `lease_owner`, `lease_until`, `attempts`, `cancel_requested`, `version`. No foreign key to `agents`: `DeleteAgent` removes the tasks itself. |
| `steps` | Commands run, `(task_id, seq)` primary key. | `output` is the excerpt stored (first and last 2000 bytes). `exit_code` nullable. Cascades with the task. |
| `messages` | The stored conversation, `(task_id, seq)` primary key. | `json` holds one `llm.Message`. Replaying them resumes a task. Cascades with the task. |

Sequence numbers for steps and messages are dense per task and assigned under a row lock on the task.

Persistence: everything in the tables above is durable. The in-memory store (`store.Memory`) holds the same data for
tests and development and loses it on restart. Per-run loop counters (unusable calls, cut replies, whether a batch
ran) are in memory only and start again when a task is taken over.

## Observability

RPC metrics (Connect interceptor, `server="agentd"`), from `libs/server/obs`:

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `attic_rpc_requests_total` | counter | `server`, `service`, `method`, `code` | Completed RPCs by Connect code. |
| `attic_rpc_duration_seconds` | histogram | `server`, `service`, `method` | RPC duration (5 ms to 30 s buckets). |
| `attic_rpc_in_flight` | gauge | `server`, `service`, `method` | RPCs being handled. |
| `attic_build_info` | gauge | `version`, `go_version` | Always 1. |

Runner metrics (`services/agentd/metrics`):

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `attic_agent_tasks_finished_total` | counter | `state` | Tasks that reached a final state, including requeued tasks that became `ERROR`. The labels `succeeded`, `failed`, `gave_up`, `cancelled` and `error` are created at start; `succeeded_unverified` appears on first use. |
| `attic_agent_task_duration_seconds` | histogram | none | Time from `started_at` to the final state. Buckets 1 s to 1 h. |
| `attic_agent_steps_total` | counter | none | Commands run inside VMs. |
| `attic_agent_lease_lost_total` | counter | none | Tasks whose lease was taken over while this process ran them. |

Go and process collectors are registered too. `/metrics` is absent when `-metrics=false`.

Logs (text handler, `slog`): `task finished` (`task`, `state`), `task requeued after transient failure` (`task`,
`attempts`, `err`), `task not finished: lease lost or already final`, `lease lost`, `no -postgres-dsn: using the
in-memory store`, `downstream call failed` (service, err), `panic in handler`, `run task` errors. Errors that become
`internal` are logged with the procedure and caller.

Counts of queued tasks are not exported: there is no cluster-wide count in the store. Use `ListTasks` with
`state=TASK_STATE_QUEUED`.

## Failure modes and recovery

| Failure | What happens | What to do |
| --- | --- | --- |
| Ollama unreachable during a turn | The turn fails as unavailable and the task is requeued (`attempts` +1). After more than 3 attempts it ends `ERROR`. | Restore Ollama. Tasks resume from the stored conversation. |
| Ollama returns an HTTP error or a broken stream | The task ends `ERROR` with `model error: ...`. No retry. | Read the message (for example a model missing from the server). |
| Ollama down at `CreateAgent` or `UpdateAgent` | The model check is skipped; the agent is created anyway. A task later fails if the model is missing. | Check `ListModels` once Ollama is back. |
| `ListModels` while Ollama is down | `unavailable`. | Restore Ollama. |
| vmd unreachable | Starting a command or reading a file is requeued; the task waits and retries. `CreateAgent` returns `unavailable`. | Restore vmd. |
| VM stopped (not `RUNNING`) when a task starts | The task ends `ERROR`, result `vm is <state>`. | Start the VM with vmd, then queue a new task. The agent stays `ACTIVE`. |
| VM deleted | The agent is marked `ORPHANED` (terminal) and the task ends `ERROR`, result `the VM no longer exists`. New tasks are refused with `failed_precondition`. | Delete the agent (`DeleteAgent`) and create a new one on another VM. |
| VM stopped while a command runs | vmd marks the job `LOST`. The command's result ends with `[lost]` and the model continues from there. | Queue a new task if the work is not done. |
| Worker process dies mid-task | Its lease expires after 30 s; another worker claims the task, `attempts` +1, and it resumes from the stored conversation. The running command is found by its job key. | Nothing normally. More than 3 takeovers end the task `ERROR`. |
| Worker loses its lease (network stall, Postgres failover) | The run is cancelled and writes nothing more. Another worker takes over. | Nothing normally. Watch `attic_agent_lease_lost_total`. |
| Postgres unavailable | Store calls fail. A running task's writes fail, and the worker leaves the task to its lease. `/readyz` returns `503`. | Restore Postgres. |
| Cancel requested for a task whose worker has died | Nothing finishes it: claims skip cancel-requested tasks and no process reaps them. The task stays `RUNNING` with `cancel_requested` true. Deleting the agent removes it. | Known gap. Delete the agent, or fix the database row by hand. |
| agentd restart with the memory store | All agents and tasks are lost. | Use Postgres in any deployment that matters. |
| Model stops making tool calls or repeats itself | Guards end the task (see [Guards](#guards)). | Read `reason` and `calls`. Adjust the prompt or model. |
| A VM name that agentd cannot use as an agent name | `CreateAgent` returns `invalid_argument` (agent names must start with a letter). | Rename the VM. |

## Running and testing

Run these from the repository root.

```
cd services/agentd && go build ./...
cd services/agentd && go vet ./...
cd services/agentd && go test -race -count=1 ./...
make test         # every module, race detector
make boundaries   # import rules, scripts/check-boundaries.sh
make mocks        # regenerate gomock mocks (needs network: go run mockgen@v0.6.0)
```

Postgres tests start an embedded Postgres once per test process (the binaries are downloaded on first use), or use
`ATTIC_TEST_POSTGRES=postgres://admin-dsn` for an existing server. `ATTIC_TEST_POSTGRES=skip` skips them. The store
conformance suite (`store/storetest`) runs against both the memory and Postgres stores. The runner and service tests
use `llm.Fake` and the mock VM client. The lease tests use short real-time TTLs and can be flaky under heavy load.

End to end: `cd e2e && go test -race -count=1 ./...` runs vmd and agentd in one process over their HTTP APIs.

Run a local instance (placeholders for paths and DSNs):

```
agentd -listen :8081 \
  -postgres-dsn "postgres://agentd:$PASSWORD@localhost:5432/agentd" \
  -vmd-url http://127.0.0.1:8080 \
  -ollama-url http://127.0.0.1:11434 \
  -service-key ./keys/agentd.key -service-key-id agentd \
  -trusted-key admin=./keys/admin.pub
```

A one-off run without a database (development only) omits `-postgres-dsn`.

## Layout

```
cmd/agentd/        binary: flags, store and runner wiring, HTTP server
domain/            Agent, Task, Step, Message, CallRecord, states, verdict sources
service/           Connect handlers (agent.go, task.go, model.go), validation and paging (server.go),
                   config and VRAM budget (config.go), domain-to-wire conversion (convert.go)
runner/            worker loop (runner.go: claim, lease, fenced writes; loop.go: turns, guards, verdicts;
                   command.go: vmd jobs; sanitize.go: text limits and cleaning; vmfs.go: apply_patch file system)
runner/toolcall/   lenient JSON, tool-call normalisation, harmony parsing
runner/applypatch/ the apply_patch format: parser and atomic applier over an FS interface
llm/               Client interface (llm.go), Ollama client (ollama.go), scripted Fake (fake.go), mock/
metrics/           runner Prometheus series
store/             Store interface and Memory; postgres/ (migrations/, logs.go, tasks.go); storetest/; mock/
vmclient/          VM interface and the vmd Connect client; mock/
```

## Examples

Agents and tasks through the API, with curl. The VM must already exist in the project (see
[vmd examples](../vmd/README.md#examples)). The token must be trusted by agentd and carry a binding on the project,
for example `-bind proj-a:editor`. The model name must be installed on the Ollama server (`ListModels` shows them).

```
export AGENTD=http://127.0.0.1:8081
export TOKEN=...                    # from atticctl token mint
call() {                            # call <Method> '<json>', for example call TaskService/GetTask '{...}'
  curl -sS -X POST "$AGENTD/attic.agentd.v1.$1" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d "$2"
}
```

### 1. List the models

```
call ModelService/ListModels '{}'
```

Each model has `name`, `sizeGib`, `params`, `quant`, `contextLength` and `fitsVram`.

### 2. Create an agent on a VM

Several agents can share one VM. Each gets its own working directory (default `/home/agent/agents/<agent id>`)
and its own hold `agentd/<agent id>` on the VM, so the VM cannot be deleted under it.

```
call AgentService/CreateAgent '{"projectId":"proj-a","name":"builder","vmId":"vm_...",
  "model":"llama3.1:8b","systemPrompt":"You build the project in /home/agent/src.",
  "idempotencyKey":"builder-1"}'
```

A retry with the same `idempotencyKey` returns the same agent.

### 3. Queue a task with a check

The `checkCommand` runs in the VM. Its exit status decides the outcome: 0 is `TASK_STATE_SUCCEEDED`.

```
call TaskService/CreateTask '{"agentId":"agent_...",
  "prompt":"Make the unit tests in /home/agent/src pass.",
  "checkCommand":"cd /home/agent/src && go test ./...","maxSteps":60,
  "idempotencyKey":"tests-1"}'
```

### 4. Wait for the task

Wait while the task is queued, then while it runs. Repeat the last call until the state is final
(`SUCCEEDED`, `SUCCEEDED_UNVERIFIED`, `FAILED`, `GAVE_UP`, `CANCELLED` or `ERROR`).

```
call TaskService/GetTask '{"id":"task_...","waitForNot":"TASK_STATE_QUEUED","waitSeconds":30}'
call TaskService/GetTask '{"id":"task_...","waitForNot":"TASK_STATE_RUNNING","waitSeconds":30}'
```

The task's `result`, `verdict`, `reason` and `verdictSource` say how it ended. `calls` lists the latest 50 model
calls.

### 5. Read the commands the task ran

Pass the last `seq` you have as `afterSeq`. Start with 0.

```
call TaskService/ListSteps '{"taskId":"task_...","afterSeq":0,"limit":100}'
call TaskService/ListSteps '{"taskId":"task_...","afterSeq":"<last seq seen>","limit":100}'
```

### 6. Cancel a task, then delete the agent

A queued task becomes `TASK_STATE_CANCELLED` at once. A running task is cancelled within about a second.

```
call TaskService/CancelTask '{"id":"task_..."}'
call AgentService/DeleteAgent '{"id":"agent_..."}'
```

`DeleteAgent` cancels the agent's unfinished tasks, removes the agent with its tasks, steps and messages, and
releases the hold on the VM. After that the VM can be deleted.

## Related

- [MCP server](../mcp/README.md): the MCP front end that creates agents and tasks through this API.
- [vmd](../vmd/README.md): the VM service agentd calls.
- [API](../../api/README.md): the generated Go types for the agentd protos.
- [libs/authz](../../libs/authz/README.md), [libs/common](../../libs/common/README.md),
  [libs/server](../../libs/server/README.md): the shared libraries used here.
- ADRs: [0001 Connect RPC](../../docs/adr/0001-connect-rpc.md),
  [0002 state and leases](../../docs/adr/0002-state-and-leases.md),
  [0003 jobs live in the guest](../../docs/adr/0003-jobs-live-in-the-guest.md),
  [0004 agent loop guards](../../docs/adr/0004-agent-loop-guards.md).
- [Deploy](../../deploy/README.md), [e2e](../../e2e/README.md), [top-level README](../../README.md).
