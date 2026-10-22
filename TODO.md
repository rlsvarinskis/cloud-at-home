# TODO and future plans

What is planned or known to be missing. This is a roadmap, not a session log: it holds no deployment details (hosts,
addresses, domains, secrets), because the repository is public. Component limits are also listed in each component's
README; this page collects the cross-cutting plans.

Priorities: **P1** soon, **P2** next, **P3** someday.

## Authentication and identity

- **P2: OAuth for Claude connectors.** Claude's web and app connectors authenticate MCP servers with OAuth, not with a
  static bearer header, so the MCP server cannot be added there with a plain token today (Claude Code and other clients
  that let you set headers work with a signed token). Plan: make the MCP server an OAuth-protected resource that follows
  the MCP authorization specification (protected-resource and authorization-server metadata discovery, dynamic client
  registration, authorization code flow with PKCE), and issue the same signed tokens the services already verify.
  Check the current spec before building. This depends on having something that can authenticate a person, which is the
  identity service below. A static shared secret is deliberately not supported: it never expires and
  cannot be revoked per token.
- **P2: identity service** (`services/auth`), designed in [docs/design/auth-graph.md](docs/design/auth-graph.md):
  nodes, groups, ownership, policies, delegation, expiry. Phases 0 to 6 of the rollout table there. Nothing is
  implemented yet.
- **P1: token workflow.** Today tokens are minted by hand with `atticctl`. Add: a documented revocation procedure
  (`-revoked-tokens` file) that is easy to run, key rotation without downtime (several trusted keys per `kid`), and a
  place to keep signing keys safely that is not a temporary folder.

## Deployment

- **P1: a real production deployment.** The running Go stack is the test project from the first end-to-end run
  (throwaway keys and database password, test project names). Move it to a properly named deployment with its own keys,
  a backup of those keys, and a documented upgrade procedure. This needs a data migration (Postgres volume, node volume
  with the VM disks), so do it deliberately.
- **P1: backups.** Postgres dumps, the node's disks and images, and the signing keys. The README has a procedure in
  [deploy](deploy/README.md); automate and test a restore.
- **P2: finish retiring the Python gateway.** Its containers are gone; its volumes, images and folder are still kept as
  a rollback. Decide when to delete them. Its source is not part of this repository.
- **P2: run CI.** The GitHub Actions workflow has never run. The Docker images were built and run by hand only.
- **P2: GPU path under test.** The compose GPU override works by hand; add it to a documented smoke test.
- **P3: multi-node.** Placement across several real hosts has not been exercised.

## Correctness and robustness

- **P1: node capacity vs. container limits.** The node reports the host's CPU and memory, not the container's cgroup
  limits, so the scheduler can place more than the caps allow. Read the cgroup limits.
- **P1: two vmd replicas can overcommit a node** (the placement lock is per process). Needs a reservation in the store.
- **P1: stuck tasks.** A task with a cancel request whose worker died stays `RUNNING` forever: nothing reaps it.
- **P2: agentd states.** `DELETING` is never set, and `ORPHANED` has no recovery path other than deleting the agent.
- **P2: agentd runs two tasks of one agent at once.** Each worker claims one task without excluding a second task of the
  same agent, so they share one VM and workdir. Serialise per agent in the claim query.
- **P2: guest-side failures are masked.** Any failure from a guest command (permission denied, bad workdir) reaches the
  caller as "node is unavailable" because of error sanitising in the node service. Pass a safe message through.
- **P2: upload to a directory path** writes a hidden temp file (`mv -f` into the directory; should be `mv -fT`) in the
  guest exec scripts (libs/qemu). The guest also prunes job directories after the newest 50, so older output is lost.
- **P3: vmd exec details.** A command exiting 124 is reported as timed out; ListJobs has no newest-first order (the MCP
  pages through up to 2000 jobs); StartJob's idempotency key is not sent by the MCP; stored job state is not reconciled
  for jobs nobody polls.
- **P2: missing test** for the "model not installed" check in agentd.
- **P3: deleting an image can race with a concurrent disk creation from it** in a narrow window.

## Node and VM management

- **P2: talk to QEMU properly.** The driver never uses QEMU's control socket, so state comes from pid files and
  `/proc`. Add a small QMP client (graceful stop, status, resize) and consider the QEMU guest agent to replace the SSH
  layer for exec and metrics. Libvirt was considered and postponed: it replaces the lifecycle code but not the guest
  side, and it adds a daemon per node.
- **P3: live migration and snapshots** need the control socket above.
- **P3: import API.** Moving a VM in from another system was done by hand (flatten the disk, register it as an image,
  create a VM). A first-class "import disk" call would make it repeatable and keep layers.
- **P3: layered images.** Disks were flattened for the move; keeping a base-plus-overlay chain is allowed by the Go
  driver but was not exercised.

## Observability

- Export the agent task queue depth as a metric.
- Per-model-call timings and token counts are stored on tasks; add dashboards and alert examples.

## Code health

- Consolidate duplicated helpers (`validName`/`validProject`, `recoverPanic` in vmd and agentd; `shellQuote` in four
  places). This needs a shared package and a decision on the layering rule.
- Each `go.mod` lists replace lines for every sibling, mostly unused; trim them.
- Install and run `staticcheck`/`deadcode`; the dead-code check so far was a grep.
- Keep the component READMEs in step with the code; every contract change should update its README and tests.

## Not planned

- A static shared bearer secret as a login method.
- Compatibility shims for the old Python gateway's API beyond the existing tool names.
