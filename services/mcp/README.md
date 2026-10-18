# services/mcp

The MCP (Model Context Protocol) front end for Attic Cloud. It lets MCP clients such as Claude create machines, run
commands, move files and give tasks to local-LLM agents, by calling the public **vmd** and **agentd** APIs.

It adds no privileges. A caller presents a Attic Cloud bearer token (the same Ed25519 JWTs vmd and agentd accept); the
server verifies it and **forwards the same token** on every downstream call, so vmd and agentd apply their own roles
and project scoping. A viewer can read but not create; an editor of project A cannot see project B.

## Running

```
mcp -vmd-url http://vmd:8080 -agentd-url http://agentd:8080 -default-project p1 -default-image debian \
    -trusted-key admin=/keys/admin.pub [-tls-cert c.pem -tls-key k.pem]
```

MCP is served over streamable HTTP at `/mcp`; `/healthz` is the liveness probe. Without `-tls-cert` the server logs a
warning, because tokens travel in headers: terminate TLS in front of it. Other flags: `-listen` (default `:8082`),
`-max-wait` (longest one tool call may block, default 40 s, because MCP clients give up near 60 s),
`-max-token-ttl`, `-revoked-tokens`, `-log-level`.

## Tools

An MCP "machine" is a vmd VM with the tool's `name`. All tools take an optional `project` (default `-default-project`).

| Tool | What it does |
|---|---|
| `agent_create` | create a VM (image, CPUs, memory, disk) or start a stopped one; waits up to the cap for it to boot |
| `agent_list` / `agent_stop` / `agent_destroy` / `agent_resize` | list machines with their latest task; stop; delete (including disk); change CPUs/memory (restarts the VM, refuses if jobs run unless `force`) |
| `agent_exec` / `agent_exec_poll` / `agent_exec_cancel` / `agent_exec_list` | run a command inside the machine; long commands return a `job_id` after the wait cap and keep running; results are JSON with the same keys every time |
| `agent_upload` / `agent_download` / `agent_fetch` / `agent_ssh_pubkey` | files up to 8 MiB; `agent_download` returns JSON (`path`, `size`, `sha256` of the raw bytes, `encoding` auto/utf-8/base64, `content`); download a URL inside the VM; show the machine's SSH public key |
| `agent_task` / `agent_status` / `agent_cancel` / `agent_models` | give an LLM agent a task (the caller **must** pick an installed model; there is no default), follow it, interrupt it, list models as JSON with `fits_vram` |
| `agent_metrics` | CPU (top-style, 100 per busy vCPU), load, memory, uptime, per-disk usage inside the guest |
| `cluster_capacity` / `node_drain` | operators only: free CPU/memory/disk per node; stop or resume placing VMs on a node |

`agent_task` creates (or retargets) the agentd agent that works on the machine, so one machine can be driven by
different models over time. If the agent already has unfinished tasks the new one is reported as queued. A model that
does not fit the VRAM budget gets a warning line, because it will probably run partly on the CPU.

`agent_status` starts with a one-line JSON header (`header: {...}`: status, step, max_steps, elapsed_s, prompt_tokens,
gen_tokens, calls, first_command_s, verdict_source, reason, last_command). Poll cheaply with `since_step` (the last
step number you have seen; only newer commands are listed, and the output stays under 1 KB when nothing is new) or with
`brief=true` (the header, plus the final report once the task has ended). While a task runs, the model's partial answer
and raw reasoning are shown on their own labelled lines and never appear in the final report.

MCP servers declare the `tools.listChanged` capability (the go-sdk does this whenever tools are registered). The
server does not send `notifications/tools/list_changed` after initialisation, because the tool set does not change
while it runs and the go-sdk only sends that notification when tools are added or removed.

## Layout

* `mcpserver/`: the tool implementations (`*_tools.go`), token handling (`auth.go`), downstream clients (`clients.go`).
* `cmd/mcp/`: the daemon.

Allowed dependencies: `api`, `libs/common`, `libs/authz`, `libs/server`. It talks to vmd and agentd only through their
generated clients.

## Tests

`cd services/mcp && go test -race ./...` runs every tool through the real MCP protocol (in-memory transport) against
fake vmd/agentd clients. `e2e/mcp_test.go` runs an MCP client over HTTP against the real vmd and agentd handlers
(memory and Postgres stores): machine lifecycle, exec, files, an agent task, and the auth rules.

## Known limits

* agentd and vmd list jobs and tasks **oldest first**, so "the latest task" and ownership checks page through the
  whole history. A newest-first option in the APIs would remove that cost.
* `agent_download` reports `size` as the number of bytes returned: vmd does not say how large a truncated file is.
* `agent_destroy` returns once deletion is requested; vmd's reconciler removes the VM and disks afterwards.
