# Authentication and authorization

homecloud authenticates callers with signed bearer tokens and authorizes them with project-scoped roles.
Servers hold only public keys, so no server can mint a token it is not configured to trust with a key
it holds privately. This document describes the token model, the roles, revocation, service-to-service calls,
key rotation and hardening. The code is in [`libs/authz`](../libs/authz/) (package `authz`), the admin tool is
[`cmd/hcctl`](../cmd/hcctl/). The system design is in [ARCHITECTURE.md](ARCHITECTURE.md#identity-and-authorization).

> **Planned, not implemented:** a `services/auth` identity service, which would hold users, API keys and service
> accounts, offer OIDC login for Google and GitHub, keep an audit log and publish JWKS, so that services verify its
> tokens offline. It does not exist yet. The rest of this document describes what is implemented today.

## Contents

- [Tokens](#tokens)
- [Keys](#keys)
- [Projects and bindings](#projects-and-bindings)
- [Roles and permissions](#roles-and-permissions)
- [Revoking a token](#revoking-a-token)
- [Service-to-service](#service-to-service)
- [Key rotation](#key-rotation)
- [What a leaked key allows](#what-a-leaked-key-allows)
- [Hardening checklist](#hardening-checklist)

## Tokens

A token is a JWT signed with Ed25519 (`alg: EdDSA`).

| Part | Content |
| --- | --- |
| Header `kid` | Id of the signing key. |
| `iss` | The same key id as the header. |
| `sub` | Subject, for example `user:robert` or `svc:vmd`. |
| `typ` | `user` or `service`. |
| `b` | Bindings: a list of `{"p": project, "r": role}`. `p` may be `*`. |
| `jti` | Token id (`tok_...`), used for audit and for revocation. |
| `iat`, `nbf`, `exp` | Issued at, not before (issued at minus 30 s), expiry. `exp` is required. |

### What a server checks

The verifier (`authz.Verifier`) accepts a token only if all of these hold:

1. The algorithm is `EdDSA`.
2. The header `kid` is a trusted key on this server.
3. The signature verifies with that key.
4. `exp` is present and has not passed (30 s leeway).
5. `iss` equals the header `kid`.
6. `exp - iat` is at most the server's maximum lifetime plus one minute. Both vmd and agentd set the maximum with
   `-max-token-ttl`, which defaults to 24 hours.
7. The `jti` is not on the server's revocation list, if the server has one (see [Revoking a token](#revoking-a-token)).
8. If the key is restricted to certain roles, every binding's role is one of them.
9. Every binding names a known role and a non-empty project.

Any failure returns `unauthenticated` with the message `unauthenticated`. The reason is logged on the server
as an `authentication failed` warning with the procedure name, and is not sent to the client.

### Lifetimes

| Setting | Default | Effect |
| --- | --- | --- |
| `hcctl token mint -ttl` | 24 h | Lifetime of the token. Must be positive and not above `-max-ttl`. |
| `hcctl token mint -max-ttl` | 720 h (30 days) | Ceiling for `-ttl` at mint time. |
| `vmd -max-token-ttl`, `agentd -max-token-ttl` | 24 h | The longest lifetime the server accepts. A token minted with a longer `-ttl` is refused with `unauthenticated`. |

The server's `-max-token-ttl` is the effective limit. Raise it on the server to accept longer tokens. Short
lifetimes are safer: a leaked token is useful until it expires.

### Inspecting a token

```
hcctl token inspect -pub keys/admin.pub -kid admin "$TOKEN"
# or from stdin:
echo "$TOKEN" | hcctl token inspect -pub keys/admin.pub -kid admin -
```

The output lists `subject`, `type`, `token-id`, `key-id`, `expires` and `bindings`. It exits with 1 and an error on
stderr if the signature, key id or expiry is wrong.

`hcctl token mint` prints only the token. Its id appears in `token inspect`, so inspect a token when you need its
id, for example before you revoke it.

## Keys

`hcctl key generate -out DIR -id KEYID` writes two files and refuses to overwrite either one:

| File | Content | Mode |
| --- | --- | --- |
| `KEYID.key` | Private key, PKCS#8 PEM. Only the signing service has it. | 0600 |
| `KEYID.pub` | Public key, PKIX PEM. Verifiers get this. | 0644 |

The key id must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`.

Verifiers are configured with `-trusted-key kid=path[:role,role]`, which may be repeated:

- The key id must be unique on the server, or the server refuses to start.
- The optional role list limits the roles this key may grant. An unknown role refuses to start.
- Without a role list, the key may grant any role, including `node`.

`hcctl` (for user tokens) and the services that call other services (for service tokens) hold private keys.
Servers never need a private key except the one they sign with.

## Projects and bindings

A token carries bindings `(project, role)`. Project `*` is every project. A binding applies to a request when:

- the binding's project is `*`, or
- the binding's project equals the project of the resource (or of the request, for requests that name one).

Requests that do not belong to a project (for example `capacity.get`, `node.drain`, `image.register` with an empty
project) are satisfied only by a `*` binding.

Two rules decide the error a caller sees when access is refused:

- Requests that name a project (create, list) return `permission_denied` when the caller lacks the
  permission on that project.
- Requests that address a resource by ID return `not_found` when the caller has no binding at all on the
  resource's project. This is the same answer as for an ID that does not exist, so IDs of other tenants
  cannot be probed. A caller who has some role on the project but lacks the permission gets `permission_denied`.

Shared images (empty project) are readable by any caller who has the permission in some project
(`RequireAnyProject`). Only a `*` binding can change them.

## Roles and permissions

Roles are fixed sets of permissions in [`libs/authz/rbac.go`](../libs/authz/rbac.go). The table below is
the output of `cd cmd/hcctl && go run . roles -v`, reformatted as a table. Regenerate it with that command after any
change to `rbac.go`.

Notes on the table:

- `viewer` is the base of `editor`, and `editor` is the base of `admin`.
- `vm.hold` is not granted to `editor`. Agents place holds through the `service` role.
- `image.register` and `image.delete` are admin only. A shared image needs a `*` binding for either.
- `capacity.get` (cluster capacity, and the `/metrics` endpoint) and `node.drain` (`ClusterService.SetNodeDraining`)
  are granted only by `operator`, and only on project `*`.
- `node.manage` (the node API) is granted only by `node`.

| Permission | viewer | editor | admin | service | node | operator |
|---|---|---|---|---|---|---|
| `agent.create` |  | yes | yes |  |  |  |
| `agent.delete` |  | yes | yes |  |  |  |
| `agent.get` | yes | yes | yes |  |  |  |
| `agent.list` | yes | yes | yes |  |  |  |
| `agent.update` |  | yes | yes |  |  |  |
| `capacity.get` |  |  |  |  |  | yes |
| `disk.create` |  | yes | yes |  |  |  |
| `disk.delete` |  | yes | yes |  |  |  |
| `disk.get` | yes | yes | yes |  |  |  |
| `disk.list` | yes | yes | yes |  |  |  |
| `disk.snapshot` |  | yes | yes |  |  |  |
| `disk.update` |  | yes | yes |  |  |  |
| `exec.cancel` |  | yes | yes | yes |  |  |
| `exec.get` | yes | yes | yes | yes |  |  |
| `exec.list` | yes | yes | yes | yes |  |  |
| `exec.run` |  | yes | yes | yes |  |  |
| `file.read` |  | yes | yes | yes |  |  |
| `file.write` |  | yes | yes | yes |  |  |
| `image.delete` |  |  | yes |  |  |  |
| `image.get` | yes | yes | yes |  |  |  |
| `image.list` | yes | yes | yes |  |  |  |
| `image.register` |  |  | yes |  |  |  |
| `model.list` | yes | yes | yes | yes |  |  |
| `node.drain` |  |  |  |  |  | yes |
| `node.manage` |  |  |  |  | yes |  |
| `task.cancel` |  | yes | yes |  |  |  |
| `task.create` |  | yes | yes |  |  |  |
| `task.get` | yes | yes | yes |  |  |  |
| `task.list` | yes | yes | yes |  |  |  |
| `vm.create` |  | yes | yes |  |  |  |
| `vm.delete` |  | yes | yes |  |  |  |
| `vm.get` | yes | yes | yes | yes |  |  |
| `vm.hold` |  |  | yes | yes |  |  |
| `vm.list` | yes | yes | yes | yes |  |  |
| `vm.metrics` | yes | yes | yes | yes |  |  |
| `vm.start` |  | yes | yes |  |  |  |
| `vm.stop` |  | yes | yes |  |  |  |
| `vm.update` |  | yes | yes |  |  |  |

Which RPC needs which permission is listed per service in [API.md](API.md).

### Example bindings

| Who | Typical token |
| --- | --- |
| Engineer with full access to one project | `-bind proj-a:admin` |
| Engineer who only reads one project | `-bind proj-a:viewer` |
| Operator (drains nodes, reads capacity), Prometheus | `-bind '*:operator'` |
| agentd, calling vmd | `*:service`, minted by agentd itself (see below) |
| vmd control plane, calling a node | `*:node`, minted by vmd itself (see below) |

## Revoking a token

Tokens are valid until they expire unless the server is told otherwise. `-revoked-tokens FILE` is accepted by both
`vmd` and `agentd`. The file lists token ids (`jti`), one per line; `#` starts a comment:

```
# revoked 2026-10-18: laptop lost
tok_01m5611vsv4gv1njmdxk8jymgp
```

- **Finding the id.** `hcctl token inspect` prints it as `token-id`. The id is not printed at mint time.
- **Revoking.** Append the id to the file. Each server reloads the file when its modification time changes; the
  check runs every 5 s. Afterwards the token is refused with `unauthenticated` (logged as
  `token revoked`).
- **Each server has its own copy.** Every `vmd` and `agentd` process that should refuse the token must read the same
  file (or its own copy of it).
- **Start-up.** The file must exist when the server starts, otherwise the server does not start. Create it empty first.
- **Failed reloads.** If the file disappears or cannot be read, the server logs a warning and keeps the last good list.
  A failed reload never un-revokes a token.

Revocation applies to one token. To invalidate every token from a key, [rotate the key](#key-rotation).

## Service-to-service

### agentd to vmd

agentd calls vmd as a service principal. vmd trusts agentd's public key with a role restriction, so an agentd
key can never mint an admin token even if it leaks. The example in `deploy/docker-compose.yml` is:

```
--trusted-key=agentd=/keys/agentd.pub:service
```

agentd signs its own service tokens with its private key (in the compose file: `--service-key`,
`--service-key-id agentd`). The `service` role grants `vm.get`, `vm.list`, `vm.metrics`, `vm.hold`,
`exec.*`, `file.read`, `file.write` and `model.list`. It does not grant `vm.create`, `vm.delete`, disk or
image changes, and it does not grant `node.drain`.

agentd mints its service tokens with subject `svc:agentd`, type `service`, binding `*:service` and a lifetime of
five minutes, and refreshes them as the vmd client needs them. The `*` binding lets that service act in every
project. Restricting the role on vmd's side does not restrict projects; only the binding does, so a narrower
deployment would need agentd to mint per-project bindings (not implemented).

### vmd to node

The control plane calls each node with its own service key. The key id defaults to `vmd`:

```
# on the control plane
vmd --role=control --node node-1=http://node-1:8080 \
  --service-key=/etc/homecloud/vmd.key --service-key-id=vmd

# on each node
vmd --role=node --trusted-key=vmd=/etc/homecloud/vmd.pub:node ...
```

The control plane mints tokens with subject `svc:vmd`, type `service` and binding `*:node`, valid for five
minutes. It caches a token and mints a new one once four fifths of its lifetime have passed. A node accepts
only the `node` role from that key, so the key cannot be used for anything else on the node.

### Service tokens in Go

`authz.ServiceTokens(issuer, principal, ttl)` returns a `TokenSource` that mints and caches tokens. Pass it
to `authz.ClientInterceptor` on a Connect client. See the snippet in [API.md](API.md#go-client).

## Key rotation

The `kid` is part of each token's header and issuer, and each server maps a `kid` to one public key. Rotate by
adding a new `kid`, then removing the old one:

1. Generate the new key pair with a new id: `hcctl key generate -out keys -id admin-2026-11`.
2. Add `-trusted-key admin-2026-11=keys/admin-2026-11.pub` to every server that must accept the new tokens,
   and restart them (keys are read at start-up).
3. Mint new tokens with `-kid admin-2026-11` and move clients over. For service keys, change the
   `-service-key` path and `-service-key-id`. Make sure every node trusts the new kid before you switch.
4. Wait until the old tokens expire. Their lifetime is bounded by the server's `-max-token-ttl`.
5. Remove the old `-trusted-key` flag and restart the servers. Delete the old private key from the signing
   machine.

Replacing the public key behind an existing `kid` invalidates every token signed with that kid at once, since
their signatures no longer verify. Use this only as an emergency measure, and then mint new tokens with the
same kid from the new private key.

## What a leaked key allows

| Leaked | What the attacker can do |
| --- | --- |
| A private key of a key trusted with no role restriction | Any role on any project, and the global roles. Includes `node`, which is direct control of the hypervisor on every node that trusts the key: start and stop any VM, and read or write any disk file on that host. Treat as full compromise. |
| The vmd service key (`svc:vmd`, role `node`) | Direct control of every node that trusts it: VMs, disks, images and files on those hosts, bypassing the project model. |
| The agentd service key (restricted to `service` on vmd) | Within the projects its tokens are bound to: run commands in VMs, read and write files in them, place holds. With a `*` binding this is every VM in every project. Cannot create or delete VMs, disks or images, cannot drain nodes, and cannot mint anything. |
| An admin user key | Everything the admin role allows in the bound projects, and image registration and deletion. Restrict its `-trusted-key` roles on the servers if the key does not need every role. |
| An operator key | Cluster capacity, the `/metrics` endpoint, and draining or undraining nodes (`node.drain`). No VM or file access. |
| A viewer or editor user key | Reads, and for editors changes, in the bound projects only. |

Mitigations: short token lifetimes, restricted `-trusted-key` role lists, `*` bindings only where needed,
revocation of individual tokens with `-revoked-tokens`, and rotation after any suspected exposure.

## Hardening checklist

- **TLS.** Start vmd and each node with `-tls-cert` and `-tls-key`, or terminate TLS at a proxy in front of
  them. gRPC needs HTTP/2, so the proxy must support it.
- **Short token lifetimes.** Keep `-max-token-ttl` at the lowest value that works (the default is 24 hours).
  Mint interactive tokens with short `-ttl`. Automation should mint short-lived tokens when it needs them.
- **Revocation file.** Run `vmd` and `agentd` with `-revoked-tokens` so that a single token can be cut off without
  a key rotation.
- **Minimal trusted roles.** On every server, give each `-trusted-key` only the roles it must grant. Never
  trust the vmd service key on a host other than the nodes it manages.
- **Node API reachable only from the control plane.** `node.manage` is the only check on the node API. Use
  firewall rules so that only the control plane can reach a node's listen address.
- **Firewall on guests.** Run nodes without `-no-firewall`. That flag disables the rules that stop guests
  from reaching the host's private networks.
- **Key files.** Keep `.key` files mode 0600 on the machine that signs with them. Verifiers need only `.pub`
  files. Keep the `deploy/keys/` directory out of version control (it is git-ignored).
- **Metrics.** Scrape `/metrics` with an operator token over a private network or TLS. See
  [OPERATIONS.md](OPERATIONS.md#prometheus).
- **Databases.** Keep Postgres on a private network with TLS. The compose file uses `sslmode=disable` for local
  development only.
- **Logs.** Alert on the `authentication failed` warning. A spike means scans or misconfigured clients.
- **Inventory.** Record which key ids exist, which servers trust each one, and with which roles. Rotate when
  someone with access leaves or a host is compromised.
