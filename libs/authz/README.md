# libs/authz

`libs/authz` (Go package `authz`) is how homecloud decides who a caller is and what it may do. It issues and verifies
Ed25519-signed JWTs, maps a token's role bindings to permissions per project, provides Connect interceptors that
authenticate requests and attach the caller to the context, and keeps a file-based revocation list of token IDs.

Module path: `github.com/rlsvarinskis/cloud-at-home/libs/authz`.

## Packages

This module is a single package, `authz`, spread over these files:

- `token.go`: `Issuer` mints tokens, `Verifier` checks them (signature, expiry, maximum lifetime, strict decoding),
  `TrustedKey` lists the public keys a server accepts, and key files are generated and read/written here.
- `principal.go`: `Principal` (type `user` or `service`, subject, bindings `(project, role)`), `Can` and `Visible`
  checks, and `WithPrincipal` / `FromContext` for passing the caller through `context.Context`.
- `rbac.go`: the `Permission` and `Role` constants, the role-to-permission table, and the `Require*` helpers that
  handlers call.
- `connect.go`: `ServerInterceptor` (authenticates unary and streaming calls), `ClientInterceptor`, the `TokenSource`
  type, `StaticToken` and `ServiceTokens` (short-lived service tokens minted by the caller).
- `revocations.go`: the `Revocations` interface and `FileRevocations`, which reloads and watches a file of revoked
  token IDs.

## Public entry points

- Keys: `GenerateKey`, `LoadPrivateKey`, `SavePrivateKey`, `LoadPublicKey`, `SavePublicKey`.
- Tokens: `Issuer.Mint(principal, ttl)`, `Verifier.Verify(token)`, `Verifier.MaxTTL`.
- Context: `WithPrincipal`, `FromContext`, `Principal.Can`, `Principal.Visible`.
- Authorization: `Require(ctx, perm, project)`, `RequireAnyProject(ctx, perm)`, `RequireResource(ctx, perm, project,
  kind, id)`. `AnyProject` is the `*` binding.
- Connect: `ServerInterceptor{Verifier, Log}`, `ClientInterceptor(src)`, `StaticToken`, `ServiceTokens(iss, p, ttl)`.
- Revocation: `NewFileRevocations(path)`, then `Revoked(jti)`, `Reload()` and `Watch(ctx, every, log)`.
- Roles: `Roles()`, `KnownRole`, `Role.Grants(perm)`. The roles are `viewer`, `editor`, `admin`, `service` (agentd to
  vmd), `node` (control plane to node daemon) and `operator` (cluster capacity and drain, global only).

## Dependencies

- Depends on `libs/common` (`errs` and `ids`) in its code. Its test file imports the `api` vmd stubs.
- External: `connectrpc.com/connect`, `golang-jwt/jwt/v5`.
- Must NOT depend on `libs/qemu`, `libs/server`, any service or `cmd/hcctl`. Its `go.mod` has `replace` lines for
  those modules, but they are not imports; do not turn them into imports. The servers depend on authz, not the
  other way round.

## Build and test

```
cd libs/authz && go build ./...
cd libs/authz && go test -race ./...
```

`auth_test.go` covers the role/permission matrix, project scoping, token round trips and rejections, key files,
the interceptors over HTTP, and file-based revocation.

## Notes and limitations

- The package comment at the top of `rbac.go` says `Package auth` while the package is named `authz`. Its text also
  names `RequireVisible`, which does not exist; the functions are `Require`, `RequireAnyProject` and
  `RequireResource`. Visibility is `Principal.Visible`.
- Servers hold only public keys. Each `TrustedKey` can be limited to some roles (`AllowedRoles`, empty means any
  role), so a compromised service key cannot mint admin tokens if it is restricted.
- `Verifier.MaxTTL` is enforced with one minute of slack.
- Revocation is by token ID through a file that daemons re-read. It is not a distributed store, so a revocation takes
  effect on each replica only after that replica reloads the file.
- `ServerInterceptor` checks `Authorization: Bearer` headers. Unauthenticated calls are rejected, so any new
  procedure needs a `Require*` call in its handler; the interceptor does not check permissions.
