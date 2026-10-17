# Deploying homecloud

This directory holds the container images, the local development stack and the admin tooling notes. For running
homecloud on real hosts, see [docs/OPERATIONS.md](../docs/OPERATIONS.md).

## How the pieces fit

| Binary | Role | Image |
| --- | --- | --- |
| `vmd` (`cmd/vmd`) | Control plane: Connect API, scheduler, state in Postgres. With `--role=node` it runs a VM host. | `Dockerfile.vmd` (control plane only), `Dockerfile.node` (node, `--role=node`) |
| `agentd` (`cmd/agentd`) | Agent daemon: runs LLM-backed tasks by calling vmd, with Ollama as the model server. | `Dockerfile.agentd` |
| `hcctl` (`cmd/hcctl`) | Admin CLI: generates signing keys, mints tokens, inspects tokens. Runs on your workstation. | not containerized |

Request flow:

```
client (curl, hcctl token) --Bearer JWT--> vmd API --(Postgres)
agentd --service JWT (signed with agentd.key)--> vmd API --(node role)--> qemu on the host
agentd --> ollama (LLM)
```

Authentication uses Ed25519-signed JWTs. Servers only hold public keys. Each key is trusted under a
key ID (`kid`), and can be restricted to certain roles:

- `admin` key: signs user tokens with `*:admin` bindings (created with `hcctl`).
- `agentd` key: signs agentd's service tokens. vmd trusts it only for the `service` role, so
  agentd cannot mint admin tokens even if its key leaks into a token.

Roles are listed with `go run ./cmd/hcctl roles -v`. See [docs/AUTH.md](../docs/AUTH.md).

## Dev stack

Prerequisites: Docker with Compose v2, and Go 1.26 for `hcctl`.

1. Create `deploy/.env` with a local database password. Compose refuses to start without it.

   ```
   HOMECLOUD_POSTGRES_PASSWORD=dev-only-change-me
   ```

2. Generate the keys (see the next section). They go in `deploy/keys/`, which is gitignored.

3. Start the stack from the `deploy/` directory:

   ```
   docker compose up -d --build            # postgres, vmd, agentd
   docker compose --profile llm up -d      # also start ollama for agentd
   docker compose logs -f vmd
   ```

   Ports are bound to `127.0.0.1` only:

   - vmd API: `http://127.0.0.1:8080`
   - agentd: `http://127.0.0.1:8081`
   - Postgres: `127.0.0.1:5432`
   - ollama (profile `llm`): `127.0.0.1:11434`

The stack's vmd runs `--role=all` with these flags, each set in `docker-compose.yml`:

- `--ssh-key=/var/lib/homecloud/node_ssh` (required for a node; created on first start).
- `--no-firewall`, `--kvm=false`, `--run-as=`: the container has no `iptables` rights, no `/dev/kvm` and no
  separate VM user.
- `--trusted-key=admin=/keys/admin.pub` and `--trusted-key=agentd=/keys/agentd.pub:service`.

What the stack can and cannot do:

- **Works:** the vmd API (authentication, authorization, projects, images, disks, VM records) and the database
  (vmd and agentd each have their own database; `postgres-init.sql` creates the `agentd` one). agentd accepts agents
  and tasks through its API.
- **Does not work:** booting guests. The vmd image has no QEMU, and the container cannot run the firewall or KVM. By
  the code's path, a VM that is started stays `STOPPED` with a `state_reason` of `node unreachable: <node id>`, and
  command and file calls to it fail with `failed_precondition`. A queued agent task cannot finish without a running VM.
  Run a real node on a host (`vmd --role=node`, or `Dockerfile.node` with `--privileged` and `/dev/kvm`) and point
  vmd at it with `--node id=URL` to run VMs.
- The stack has not been run in this repository's environment (Docker is not installed on the development host);
  the description above follows `docker-compose.yml` and the code.

## Generate keys

Run from the repository root. Keys go to `deploy/keys/`:

```
go run ./cmd/hcctl key generate -out deploy/keys -id admin
go run ./cmd/hcctl key generate -out deploy/keys -id agentd
```

This writes `admin.key` and `admin.pub` (private key mode 0600, public key mode 0644), and the same
pair for `agentd`. `hcctl` refuses to overwrite existing files, so rerunning it is safe. To rotate a
key, delete its two files, regenerate, and restart the services that use it.

The compose file expects `admin.pub` and `agentd.key`/`agentd.pub` in `deploy/keys/`. Keep the
`.key` files private. Only the `.pub` files are needed by vmd.

## Mint and inspect a token

Mint an admin token for user `robert`. The token is printed alone on stdout, so it can be captured:

```
TOKEN=$(go run ./cmd/hcctl token mint \
  -key deploy/keys/admin.key \
  -kid admin \
  -sub user:robert \
  -type user \
  -bind '*:admin' \
  -ttl 24h)
```

Bindings are `project:role`. The project `*` means every project. Flags:

- `-ttl` must be positive and at most 30 days by default. Use `-max-ttl` to raise that ceiling.
- At least one `-bind` is required. Repeat the flag for more bindings, for example `-bind proj-a:editor`.
- Unknown roles are rejected. Valid roles come from `go run ./cmd/hcctl roles`.

Check the token before using it:

```
go run ./cmd/hcctl token inspect -pub deploy/keys/admin.pub -kid admin "$TOKEN"
# or read it from stdin:
echo "$TOKEN" | go run ./cmd/hcctl token inspect -pub deploy/keys/admin.pub -kid admin -
```

Inspect prints the `token-id` (needed for revocation, see [docs/AUTH.md](../docs/AUTH.md#revoking-a-token)). It exits
1 and prints the reason on stderr if the signature, key ID or expiry is wrong.

## Call the API with curl

vmd speaks the Connect protocol. A unary call is a plain JSON `POST` to
`/<package>.<Service>/<Method>`, with the bearer token in the `Authorization` header:

```
curl -sS -X POST http://127.0.0.1:8080/homecloud.vmd.v1.VMService/ListVMs \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"projectId": "proj-a"}'
```

Request and response fields use the protobuf JSON names (camelCase). A token without a binding on
`proj-a` gets an error, not data. Errors come back as JSON with a `code` field. The API is described in
[docs/API.md](../docs/API.md).

## Files

- `Dockerfile.vmd`: control-plane image on distroless (nonroot), no qemu.
- `Dockerfile.agentd`: agentd image on distroless (nonroot).
- `Dockerfile.node`: Debian stable slim with qemu-system-x86, qemu-utils, genisoimage and openssh-client, running
  `vmd --role=node`. Nodes normally run on the bare host.
- `docker-compose.yml`: local dev stack (postgres, vmd, agentd, optional ollama).
- `postgres-init.sql`: creates the `agentd` database on first start of the Postgres container.
- `.gitignore`: ignores `keys/`.

Build any image from the repository root, for example:

```
docker build -f deploy/Dockerfile.vmd -t homecloud/vmd .
```
