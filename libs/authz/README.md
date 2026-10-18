# libs/authz

`libs/authz` (package `authz`) is how Attic Cloud decides who a caller is and what it may do. It signs and verifies
Ed25519 JWTs, maps token bindings to permissions per project, provides the Connect interceptors that authenticate
calls, and holds a file-based revocation list of token IDs.

Module: `github.com/rlsvarinskis/cloud-at-home/libs/authz`

## Role

- Defines the token format and every rule a server applies before it trusts a token (see [Verifier](#verifier)).
- Defines the roles, their permissions and the matching rules between bindings and projects
  ([RBAC](#roles-and-permissions)).
- Authenticates every Connect call through `ServerInterceptor` and stores the caller in the request context.
  It does not check permissions: each handler must call `Require`, `RequireResource` or `RequireAnyProject`.
- Mints tokens for admins (`atticctl`) and for services calling services (`ServiceTokens`).
- Does not store users, API keys or sessions. There is no login flow. Keys are files on disk.
- Does not decide who may use which key: a trusted public key is accepted with whatever roles its configuration allows.

## Position

```
 atticctl ──(private key)──► token ──► Authorization: Bearer ──► vmd / agentd / mcp / node API
                                                                  │
 agentd ──ServiceTokens──► token ──► vmd (trusts agentd.pub:service)│
 vmd (control plane) ──ServiceTokens──► token ──► node (trusts vmd.pub:node)
                                                                  │
                                 ServerInterceptor: Verifier ─────┘
                                   │ Principal in context
                                   ▼
                              handler: Require / RequireResource (perm, project)
```

- Callers: `cmd/atticctl` (mint and inspect), `services/vmd` and `services/agentd` (server and client side),
  `services/mcp` (server side; forwards the caller's token), `libs/qemu/nodeserver` (node API).
- Imports allowed by `scripts/check-boundaries.sh`: `api` and `libs/common` only. The servers import authz, never the
  reverse.
- External: `connectrpc.com/connect`, `github.com/golang-jwt/jwt/v5`.

## Contract

### Token

A token is a JWT signed with Ed25519. The header carries `alg: EdDSA` and `kid` (the key id). The claims are:

| Claim | Go field | Content |
| --- | --- | --- |
| `iss` | `RegisteredClaims.Issuer` | Must equal the header `kid`. |
| `sub` | `Subject` | Subject, for example `user:alice` or `svc:vmd`. |
| `jti` | `TokenID` | Token id, `tok_<ulid>`. Used for audit and revocation. |
| `iat` | `IssuedAt` | Time of minting. |
| `nbf` | `NotBefore` | `iat` minus 30 s. |
| `exp` | `ExpiresAt` | Required. `iat` plus the lifetime. |
| `typ` | `Type` | `user` or `service`. |
| `b` | `Bindings` | List of `{"p": project, "r": role}`. `p` may be `*`. |

Example payload (placeholder values):

```
{"iss":"admin","sub":"user:alice","jti":"tok_<ulid>","iat":1792347955,"nbf":1792347925,
 "exp":1792351555,"typ":"user","b":[{"p":"proj-a","r":"admin"},{"p":"*","r":"operator"}]}
```

#### Lifetimes

| Setting | Default | Effect |
| --- | --- | --- |
| `atticctl token mint -ttl` | 24 h | Lifetime of the token. Must be positive and not above `-max-ttl`. |
| `atticctl token mint -max-ttl` | 720 h (30 days) | Ceiling for `-ttl` at mint time. |
| `-max-token-ttl` on vmd, agentd, mcp | 24 h | Longest lifetime the server accepts (`Verifier.MaxTTL`). |
| Service tokens (agentd and vmd pass `5m` to `ServiceTokens`) | 5 min | Lifetime of tokens a service mints for itself. |

The server limit is the effective one. A token whose `exp - iat` exceeds `MaxTTL` plus one minute is refused. Short
lifetimes limit the damage of a leaked token.

### Issuer

```go
type Issuer struct {
    KeyID string             // written to the header kid and the iss claim
    Key   ed25519.PrivateKey // PKCS#8 private key
    Now   func() time.Time   // optional clock, for tests
}
func (i *Issuer) Mint(p Principal, ttl time.Duration) (string, error)
```

`Mint` fails when `ttl <= 0` or when a binding names an unknown role. It assigns a fresh `jti` and sets `iat`, `nbf` and
`exp`. It does not enforce a maximum lifetime; `atticctl` does that with `-max-ttl`. It does not validate `KeyID`.

### Verifier

```go
type Verifier struct {
    Keys        map[string]TrustedKey // kid -> trusted public key
    MaxTTL      time.Duration         // 0 = no limit
    Revocations Revocations           // optional; Revoked(jti) bool
    Now         func() time.Time      // optional clock, for tests
}
func (v *Verifier) Verify(token string) (*Principal, error)

type TrustedKey struct {
    Public       ed25519.PublicKey
    AllowedRoles []Role // empty = any role
}
```

`Verify` accepts a token only if every check below holds. The checks run in this order:

1. The algorithm is `EdDSA`, the decoding is strict and `exp` is present (`WithExpirationRequired`).
2. The header `kid` is a key in `Keys`.
3. The signature verifies with that key.
4. `nbf` and `exp` hold, with 30 s of leeway.
5. `iss` equals the header `kid`.
6. If `MaxTTL > 0`: `exp - iat <= MaxTTL + 1 min`.
7. The `jti` is not revoked (when `Revocations` is set).
8. If the key has `AllowedRoles`, every binding's role is one of them.
9. Every binding has a known role and a non-empty project.

Every failure returns an error that wraps `errs.ErrUnauthenticated`. Over Connect the client only sees
`unauthenticated` with the message `unauthenticated`. The reason goes to the server log (see
[Observability](#observability)).

`atticctl token inspect` uses the same verifier with one trusted key, no `MaxTTL` and no revocation list. See
[the CLI README](../../cmd/atticctl/README.md#token-inspect).

### Keys

| Function | Behaviour |
| --- | --- |
| `GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error)` | Uses `crypto/rand`. |
| `SavePrivateKey(path, k)` | PKCS#8 PEM, type `PRIVATE KEY`, mode `0600`. Overwrites an existing file. |
| `SavePublicKey(path, k)` | PKIX PEM, type `PUBLIC KEY`, mode `0644`. Overwrites an existing file. |
| `LoadPrivateKey(path)` | Parses PKCS#8 and requires an Ed25519 key. |
| `LoadPublicKey(path)` | Parses PKIX and requires an Ed25519 key. |

`atticctl key generate` is the safe way to create files: it refuses to overwrite and writes the private key first.
The key id rule `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` is enforced by `atticctl`, not by this package.

### Principal and authorization API

| Item | Meaning |
| --- | --- |
| `type Binding struct { Project string; Role Role }` | One grant. JSON `p` and `r`. |
| `type Principal struct { Subject string; Type PrincipalType; Bindings []Binding; TokenID string }` | The caller. |
| `const AnyProject = "*"` | Binding project meaning every project, and the global scope for requests without a project. |
| `const User = "user"`, `const Service = "service"` | Values of `PrincipalType`. |
| `(*Principal).Can(perm Permission, project string) bool` | True if a binding that matches `project` grants `perm`. |
| `(*Principal).Visible(project string) bool` | True if any binding matches `project`, whatever its role. |
| `WithPrincipal(ctx, *Principal) context.Context` | Stores the caller in the context. |
| `FromContext(ctx) (*Principal, bool)` | Reads it back. |
| `Require(ctx, perm, project) error` | For requests that name a project (create, list). Unauthenticated, or `permission_denied`. |
| `RequireResource(ctx, perm, project, kind, id) error` | For resources addressed by ID. No binding on the project gives `not_found`; a binding without `perm` gives `permission_denied`. |
| `RequireAnyProject(ctx, perm) error` | True if some binding, on any project or `*`, grants `perm`. Used for reads of shared images and for the model list. |

Matching rule: a binding applies to a project when `binding.Project == "*"`, or when the project is not empty and
`binding.Project == project`. A request with an empty project (global) is therefore satisfied only by a `*` binding.

### Roles and permissions

Roles are fixed sets in [`rbac.go`](rbac.go). Their meaning:

| Role | Scope | Meaning |
| --- | --- | --- |
| `viewer` | project | Read-only. Base of `editor`. |
| `editor` | project | Viewer plus create, change and delete of VMs, disks and agents; start and stop VMs; create and cancel tasks; run commands and file operations in VMs. Base of `admin`. |
| `admin` | project | Editor plus holds, image registration and deletion. |
| `service` | project or `*` | Used by agentd to call vmd: commands, files, holds, reads. No destructive VM, disk or image changes. |
| `node` | `*` only | Used by the vmd control plane to call a node daemon (`node.manage`). |
| `operator` | `*` only | Cluster capacity, `/metrics`, and draining nodes. Nothing inside projects. |

`service` and `node` are for machines, not people. `vm.hold` is not granted to `editor`: agents place holds through
`service`. Shared images (empty project) need a `*` binding to be changed. The table below was produced by
`atticctl roles -v` (see [Verifying the table](#verifying-the-table)).

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

Which RPC needs which permission is listed in each service's README, and in the API index in
[api/README.md](../../api/README.md#service-index).

#### Verifying the table

```
cd cmd/atticctl && go run . roles -v
```

Any change to `rbac.go` must be followed by an update of this table. The command prints each role followed by its
indented permissions; the table is that output with one row per permission and one column per role.

### Connect interceptors

| Item | Behaviour |
| --- | --- |
| `ServerInterceptor{Verifier, Log}` | Unary and streaming handler interceptor. Reads `Authorization: Bearer <jwt>`, verifies it and calls `WithPrincipal`. A missing header, a bad token or a wrong scheme gives `unauthenticated`. It does not check permissions. |
| `ClientInterceptor(src TokenSource) connect.Interceptor` | Unary client interceptor. Sets `Authorization: Bearer` on every outgoing call. If `src` fails, the call fails with `unauthenticated`. |
| `type TokenSource func(ctx context.Context) (string, error)` | Supplies a token for outgoing calls. |
| `StaticToken(t string) TokenSource` | A fixed token, for example from an environment variable. |
| `ServiceTokens(iss *Issuer, p Principal, ttl time.Duration) TokenSource` | Mints a token for `p` and caches it. It mints a new one once four fifths of `ttl` have passed. Safe for concurrent use. |

The servers install `obs.ErrorInterceptor` in front of `ServerInterceptor`, so a rejected token is logged there as well
(see [libs/server](../server/README.md)). A Go client example is in
[api/README.md](../../api/README.md#calling-from-go).

### Service-to-service calls

Two patterns exist. Both use the same verifier on the receiving side.

| Pattern | Who | Token | Example |
| --- | --- | --- | --- |
| Service identity | agentd to vmd; vmd control plane to a node | The service signs its own short-lived token with its private key (`ServiceTokens`). Subject `svc:agentd` or `svc:vmd`, type `service`, binding `*:service` or `*:node`. | `services/agentd/vmclient` (agentd to vmd); `libs/qemu/remote` (vmd to nodes). |
| Token forwarding | mcp to vmd and agentd | The MCP server forwards the bearer token of its caller unchanged (`tokenFrom` in `services/mcp/mcpserver/auth.go`). | Downstream services apply the caller's own roles and projects. The MCP server holds no extra power. |

Restricting a service key on the receiving side with `-trusted-key kid=path:service` limits the role it can grant,
but not the projects: a `*` binding still reaches every project. Per-project service bindings are not implemented.

The example in `deploy/docker-compose.yml`:

```
# vmd (receiver): trust agentd's key for the service role only
--trusted-key=agentd=/keys/agentd.pub:service
# agentd (sender): sign its own tokens
--service-key=/keys/agentd.key --service-key-id=agentd
```

Control plane to node:

```
# on the control plane
vmd --role=control --node node-1=http://node-1:8080 \
  --service-key=/etc/attic/vmd.key --service-key-id=vmd
# on each node
vmd --role=node --trusted-key=vmd=/etc/attic/vmd.pub:node
```

The node accepts only the `node` role from that key, so a leaked control-plane key cannot do anything else on that node
beyond what `node` allows (see [What a leaked key allows](#what-a-leaked-key-allows)).

### Revocation

`FileRevocations` is a deny list kept in a text file:

- One token id (`jti`) per line. `#` starts a comment. Blank lines are ignored.
- `NewFileRevocations(path)` loads the file and fails if it does not exist or cannot be read.
- `Revoked(jti) bool` and `Len() int` read the current list. Reads are lock-free.
- `Reload() error` re-reads the file and replaces the list atomically.
- `Watch(ctx, every, log)` reloads when the file's modification time changes. It checks every `every`;
  the daemons use 5 s.

```
# revoked 2026-10-18: laptop lost
tok_01m5611vsv4gv1njmdxk8jymgp
```

- Find the id with `atticctl token inspect` (`token-id`). It is not printed by `token mint`.
- Each server keeps its own list. A replica acts on a revocation only after it reloads the file.
- A failed reload (file missing, unreadable, unparsable) logs a warning and keeps the last good list. A failure never
  un-revokes a token.
- Revocation is per token. To invalidate everything signed with a key, rotate the key.

The daemons wire it with `-revoked-tokens FILE`, which vmd, agentd and mcp all accept (see
[libs/server](../server/README.md)). An empty flag disables it.

### Trusted keys

Servers are started with `-trusted-key kid=path[:role,role]`, repeatable:

- `kid` must be unique in the server; a duplicate, a malformed spec, an unreadable key or an unknown role stops the
  server at start-up.
- `role,role` limits the roles that key may grant. Without a list, the key may grant any role, including `node`.
- Keys are read once at start-up. Adding or removing a key needs a restart.

Each server holds only public keys (`.pub` files). Only the process that mints tokens holds a `.key` file.

### Key rotation

Rotate by adding a new `kid`, then removing the old one:

1. Generate the new pair with a new id: `atticctl key generate -out keys -id admin-2026-11`.
2. Add `-trusted-key admin-2026-11=keys/admin-2026-11.pub` to every server that must accept the new tokens, and restart
   them.
3. Mint new tokens with `-kid admin-2026-11`, and move clients over. For service keys, change `-service-key` and
   `-service-key-id`. Every node must trust the new kid before you switch.
4. Wait until the old tokens expire. Their lifetime is bounded by the server's `-max-token-ttl`.
5. Remove the old `-trusted-key` and restart. Delete the old private key from the signing machine.

Replacing the public key behind an existing `kid` invalidates every token signed with that kid at once, because their
signatures no longer verify. Use this only as an emergency measure, then mint new tokens with the same kid.

### What a leaked key allows

| Leaked | What the attacker can do |
| --- | --- |
| A private key trusted without a role list | Any role on any project, including `node`. With `node`: start and stop any VM on every host that trusts the key, and use the node API for disks, images and guest files on those hosts. Treat as full compromise. |
| The vmd service key (`svc:vmd`, role `node`) | The node API of every node that trusts it: VMs, disks, images and guest files on those hosts, bypassing the project model. |
| The agentd service key (restricted to `service` on vmd) | Within the projects its tokens are bound to: run commands in VMs, read and write files there, place holds. With a `*` binding, every VM in every project. It cannot create or delete VMs, disks or images, cannot drain nodes and cannot mint other tokens. |
| An admin user key | Everything `admin` allows in the bound projects, including image registration and deletion. Limit its `-trusted-key` roles if the key does not need all of them. |
| An operator key | Cluster capacity, `/metrics`, and draining or undraining nodes. No VM or file access. |
| A viewer or editor user key | Reads, and for editors changes, in the bound projects only. |

Mitigations: short lifetimes, restricted `-trusted-key` role lists, `*` bindings only where needed, revocation of single
tokens, and rotation after any suspected exposure.

### Hardening checklist

- **TLS.** Start vmd, agentd and each node with `-tls-cert` and `-tls-key`, or terminate TLS at a proxy that supports
  HTTP/2 (gRPC needs it).
- **Short lifetimes.** Keep `-max-token-ttl` at the lowest value that works (default 24 h). Mint interactive tokens with
  a short `-ttl`.
- **Revocation file.** Run vmd and agentd with `-revoked-tokens`, so one token can be cut off without a rotation.
- **Minimal trusted roles.** Give each `-trusted-key` only the roles it must grant. Never trust the vmd service key on a
  host other than the nodes it manages.
- **Node API reachable only from the control plane.** `node.manage` is the only check on the node API. Use firewall
  rules so only the control plane reaches a node's listen address.
- **Guest firewall.** Run nodes without `-no-firewall`. That flag disables the rules that stop guests reaching the
  host's private networks.
- **Key files.** Keep `.key` files mode 0600 on the signing machine. Verifiers need only `.pub` files. `deploy/keys/` is
  git-ignored; keep it out of version control.
- **Metrics.** Scrape `/metrics` with an operator token over a private network or TLS.
- **Databases.** Keep Postgres on a private network with TLS. The compose file uses `sslmode=disable` for local
  development only.
- **Logs.** Alert on the `authentication failed` warning. A spike means scans or misconfigured clients.
- **Inventory.** Record which key ids exist, which servers trust each one and with which roles. Rotate when someone with
  access leaves or a host is compromised.

## Behaviour and guarantees

- Tokens are stateless. A valid token is accepted until `exp`, unless its `jti` is on the server's revocation list.
- Verification is local: a server needs only public keys and never calls another service to check a token.
- `ServiceTokens` is goroutine-safe. It refreshes at four fifths of its lifetime, so a call never sends a token
  that is about to expire.
- Authentication and authorization are separate. A handler that forgets its `Require` call is not protected by
  `ServerInterceptor`.
- Time checks (`nbf`, `exp`, `MaxTTL`) use the verifier's clock. If the minting host and a verifying host differ by
  more than 30 s, tokens are refused as not yet valid or as expired.

## Configuration

This package reads no environment variables and no flags. The services expose the following flags:

| Flag | Services | Default | Meaning |
| --- | --- | --- | --- |
| `-trusted-key kid=path[:role,role]` | vmd, agentd, mcp | none (required) | A public key to trust. Repeatable. |
| `-max-token-ttl` | vmd, agentd, mcp | `24h` | Longest token lifetime accepted. |
| `-revoked-tokens FILE` | vmd, agentd, mcp | empty (off) | Revocation list. Must exist at start-up. |
| `-service-key FILE` | vmd, agentd | empty | Private key used to sign this service's tokens. |
| `-service-key-id KID` | vmd, agentd | `vmd` / `agentd` | Key id written into those tokens. |

`atticctl` flags are in [cmd/atticctl](../../cmd/atticctl/README.md).

## Data

The package owns no storage. Its file formats are:

| File | Format | Written by | Read by |
| --- | --- | --- | --- |
| `KID.key` | PKCS#8 PEM, `PRIVATE KEY`, mode 0600 | `atticctl key generate` | `atticctl token mint`, services with `-service-key` |
| `KID.pub` | PKIX PEM, `PUBLIC KEY`, mode 0644 | `atticctl key generate` | `-trusted-key` on every server |
| revocation file | text, one `jti` per line, `#` comments | operator | `-revoked-tokens` |

## Observability

| Signal | When |
| --- | --- |
| Warning `authentication failed` with `procedure` and `err` | A call was rejected. The `err` text gives the reason (`unknown key id`, `token revoked`, `token lifetime exceeds the allowed maximum`, and so on). |
| Info `revocation list reloaded` with `path` and `entries` | The file changed and loaded. Logged by the daemon, not by this package. |
| Warning `revocation list unreadable` or `reload failed` | The file could not be read. The last good list stays in force. |

There are no metrics in this package. Rejections are counted only through the RPC metrics of the server (the call's
Connect code `unauthenticated`).

## Failure modes and recovery

| Situation | Effect | Operator action |
| --- | --- | --- |
| A trusted key file is missing or malformed | The server does not start. | Fix the path and restart. |
| The revocation file is missing at start-up | The server does not start. | Create the file, even empty. |
| The revocation file disappears while running | Warning; the last list stays. | Restore the file. |
| Clocks of the minting and verifying hosts differ by more than 30 s | Tokens fail with `unauthenticated`. | Fix time sync (NTP) on both hosts. |
| A key is leaked | Every token signed with it is valid until it expires or is rotated away. | Rotate, see [above](#key-rotation). |
| A token is revoked | Refused on each server after its next reload (at most about 5 s later). | None. |

## Running and testing

```
cd libs/authz && go build ./...
cd libs/authz && go test -race -count=1 ./...
```

`auth_test.go` covers the role and permission matrix, project scoping, `Require` and `RequireResource`, token
round trips
and rejections, role-restricted keys, key files, the interceptors over HTTP, `ServiceTokens` caching, the operator and
metrics permissions, `RequireAnyProject`, and file-based revocation.

## Layout

```
libs/authz/
  token.go        Issuer (Mint), Verifier (Verify), TrustedKey, key file helpers
  principal.go    Principal, Binding, Can, Visible, Require*, context helpers
  rbac.go         Permission and Role constants, role-to-permission table, Roles, Grants
  connect.go      ServerInterceptor, ClientInterceptor, TokenSource, StaticToken, ServiceTokens
  revocations.go  Revocations interface, FileRevocations (load, reload, watch)
  auth_test.go    tests of all of the above
```

## Related

- [api/README.md](../../api/README.md): wire contract, service index, error codes.
- [cmd/atticctl/README.md](../../cmd/atticctl/README.md): mint, inspect, key generation, roles.
- [libs/server/README.md](../server/README.md): the daemon flags that load trusted keys and the revocation list.
- [libs/common/README.md](../common/README.md): `errs`, the error sentinels that `Require*` return.
- [services/vmd/README.md](../../services/vmd/README.md), [services/agentd/README.md](../../services/agentd/README.md),
  [services/mcp/README.md](../../services/mcp/README.md): the servers that verify tokens.
- [deploy/README.md](../../deploy/README.md): key layout in the compose setup.
- [docs/design/auth-graph.md](../../docs/design/auth-graph.md): **proposal, not implemented.** A graph-based identity
  service (`services/auth`) with groups, signed statements, OIDC sign-in and offline verification via published keys.
  Implemented today: signed tokens, roles, project bindings, trusted keys and the revocation file, as described above.
  None of the proposal exists in the code.
