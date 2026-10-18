# cmd/hcctl

`cmd/hcctl` is homecloud's administrative command-line tool. It generates the Ed25519 signing keys that the servers
trust, mints signed tokens for users and services, inspects tokens against a public key, and lists the roles and
the permissions each one grants. It runs locally and never calls a server.

Module path: `github.com/rlsvarinskis/cloud-at-home/cmd/hcctl`.

## Packages

This module is one package, `main` (`main.go`), with its tests in `main_test.go`. The commands are:

- `hcctl key generate -out DIR -id KEYID`: writes `DIR/KEYID.key` (private) and `DIR/KEYID.pub` (public), and prints
  the two paths.
- `hcctl token mint -key FILE -kid KEYID -sub SUBJECT -type user|service -bind project:role [-bind ...] [-ttl 24h]
  [-max-ttl 720h]`: prints one signed JWT on stdout. `project` can be `*` for all projects. Bindings must name known
  roles.
- `hcctl token inspect -pub FILE -kid KEYID (TOKEN | -)`: verifies a token (or one read from stdin with `-`) and
  prints the subject, type, token ID, key ID, expiry and bindings.
- `hcctl roles [-v]`: lists the roles; `-v` also prints the permissions each role grants.
- `hcctl help` (or `-h`) prints the usage text.

Exit codes: 0 for success, 1 for a runtime failure (including an invalid token), 2 for a usage error.

## Public entry points

There are no exported Go APIs. The entry point is `main()`, which calls `run(args, stdin, stdout, stderr) int`.
`run` is what the tests call. Keys and tokens are produced with the `libs/authz` functions (`GenerateKey`,
`SavePrivateKey`, `SavePublicKey`, `Issuer.Mint`, `Verifier.Verify`).

## Dependencies

- Depends on `libs/authz` (key files, token issue and verification, role list). `go.mod` lists
  `golang-jwt/jwt/v5` directly and `libs/common` only indirectly.
- Must NOT depend on `services/*`, `libs/qemu`, `libs/server` or `e2e`. An admin tool should not need the server
  code. The `go.mod` `replace` lines for those modules are unused by the imports and must not be turned into
  imports.

## Build and test

```
cd cmd/hcctl && go build -o hcctl .
cd cmd/hcctl && go test -race ./...
```

`main_test.go` has 17 tests: the key round trip, refusal to overwrite existing keys, bad key IDs and bindings, unknown
roles, TTL limits, token inspection (including a token from another key and tokens read from stdin), and the role list.

## Notes and limitations

- Secrets are not meant to be printed. The package comment says that the only stdout output carrying sensitive data
  is the JWT from `token mint`, printed alone so it can be captured in a shell variable.
- Key files are written by `authz.SavePrivateKey` with mode 0600. `key generate` refuses to overwrite an existing
  key, including one where only the public half exists.
- Flags must come before positional arguments (the usage text says so). A token given as a positional argument is
  visible in the shell history and the process list. Use `-` to read it from stdin.
- `token mint` rejects a `-ttl` longer than `-max-ttl`, and a non-positive TTL, as a usage error.
- There is no command to revoke a token. Revocation is a file that the daemons read (see `libs/authz`).
- The comment on `knownRoles` in `main.go` refers to `internal/auth`, a path that no longer exists; the roles come
  from `libs/authz`.
