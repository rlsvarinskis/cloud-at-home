# atticctl

`atticctl` is the Attic Cloud admin CLI. It generates Ed25519 signing keys, mints signed tokens, inspects a token
against a public key and lists the roles and their permissions. It works only on local files: it never contacts a
server.

Module: `github.com/rlsvarinskis/cloud-at-home/cmd/atticctl`

## Role

- Creates key pairs as files, and never overwrites an existing key.
- Mints user and service tokens signed with a private key, and prints the token alone on stdout.
- Inspects a token: checks its signature, key id, expiry and binding shape, and prints its claims.
- Prints the roles and the permissions each grants (`roles -v`). This is the source of the table in
  [libs/authz](../../libs/authz/README.md#roles-and-permissions).
- Does not revoke tokens. Revocation is a line in each server's revocation file (see [Revoke a token](#revoke-a-token)).
- Does not store keys, users or tokens, and does not read any configuration file or environment variable.

## Position

```
operator's machine (holds *.key)
  atticctl key generate ──► KID.key (0600), KID.pub (0644)
  atticctl token mint   ──► token on stdout ──► Authorization: Bearer on vmd, agentd, mcp
  atticctl token inspect ◄── token; reads KID.pub only
```

- Imports `libs/authz` (`Issuer`, `Verifier`, key files, `Roles`) and `github.com/golang-jwt/jwt/v5`.
  `scripts/check-boundaries.sh` allows `api`, `libs/common` and `libs/authz`. It imports no server code.
- Distributed as a binary. Build it with `go build` in `cmd/atticctl` (see [Running and testing](#running-and-testing)).

## Contract

### Usage

```
usage:
  atticctl key generate -out DIR -id KEYID
  atticctl token mint -key FILE -kid KEYID -sub SUBJECT -type user|service -bind project:role [-bind ...] \
    [-ttl 24h] [-max-ttl 720h]
  atticctl token inspect -pub FILE -kid KEYID (TOKEN | -)
  atticctl roles [-v]        list roles (-v: with the permissions each grants)

Flags must come before positional arguments.
```

`atticctl help`, `-h`, `-help` and `--help` print the usage to stdout and exit 0. Running with no arguments prints it to
stderr and exits 2.

### Commands

#### `key generate`

`atticctl key generate -out DIR -id KEYID`

| Flag | Required | Meaning |
| --- | --- | --- |
| `-out DIR` | yes | Directory to write into. Created with mode 0700 if it does not exist. |
| `-id KEYID` | yes | Key id. Must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`. |

Writes `DIR/KEYID.key` (PKCS#8 PEM, private, mode 0600) and `DIR/KEYID.pub` (PKIX PEM, public, mode 0644). Both files
are reserved with `O_EXCL` before the key is generated. If either exists, nothing is written and the command fails.
On success it prints:

```
wrote DIR/KEYID.key
wrote DIR/KEYID.pub
```

#### `token mint`

`atticctl token mint -key FILE -kid KEYID -sub SUBJECT -type user|service -bind project:role [-bind ...]`
`[-ttl 24h] [-max-ttl 720h]`

| Flag | Required | Default | Meaning |
| --- | --- | --- | --- |
| `-key FILE` | yes | | Private signing key (`KEYID.key`). |
| `-kid KEYID` | yes | | Key id written to the header `kid` and the `iss` claim. Same pattern as `key generate -id`. |
| `-sub SUBJECT` | yes | | Subject, for example `user:alice` or `svc:agentd`. Surrounding spaces are trimmed. |
| `-type user\|service` | yes | | Principal type. |
| `-bind project:role` | yes, at least one | | A grant. Repeatable. `project` is a project id or `*` (every project). `role` must be a known role. The project part cannot contain `:`. |
| `-ttl DURATION` | no | `24h` | Token lifetime. Must be greater than zero and not greater than `-max-ttl`. |
| `-max-ttl DURATION` | no | `720h` | Largest allowed `-ttl`. Must be greater than zero. |

On success it prints only the token, followed by a newline, so the output can be captured:

```
TOKEN=$(atticctl token mint -key keys/admin.key -kid admin -sub user:alice -type user -bind proj-a:admin)
```

The token's id (`jti`) is not printed. Get it with `token inspect`.

#### `token inspect`

`atticctl token inspect -pub FILE -kid KEYID (TOKEN | -)`

| Argument or flag | Required | Meaning |
| --- | --- | --- |
| `-pub FILE` | yes | Public key (`KEYID.pub`). |
| `-kid KEYID` | yes | The key id the token must carry. |
| `TOKEN` | yes, exactly one | The token. |
| `-` | instead of TOKEN | Read the token from stdin. Surrounding whitespace is trimmed. |

On success it prints the claims:

```
subject:  user:alice
type:     user
token-id: tok_<ulid>
key-id:   admin
expires:  2026-10-18T19:25:55Z
bindings:
  proj-a:admin
  *:operator
```

`expires` is the `exp` claim in UTC, or `never` if the token has none. With no bindings the last line is
`bindings: (none)`. An invalid token prints `atticctl: invalid token: <reason>` on stderr.

What inspect checks: the algorithm, the key id against `-kid`, the signature, that `iss` equals the key id, `exp`
(required, with the 30 s leeway), and that every binding has a known role and a project. The checks are those of
[`Verifier.Verify`](../../libs/authz/README.md#verifier). What it does **not** check:

- the server's `-max-token-ttl` (inspect has no maximum);
- the server's revocation list. A token that a server refuses because its `jti` is revoked is shown as valid here.
- per-server key role restrictions (`-trusted-key kid=path:role`).

#### `roles`

`atticctl roles [-v]`

Prints the role names, sorted. With `-v`, each role is followed by its permissions, indented four spaces, sorted:

```
admin
    agent.create
    ...
```

Any other argument is a usage error.

### Flags and arguments: general rules

- Flags must come before positional arguments. `token inspect TOKEN -pub FILE` fails with a usage error.
- `-h` after a subcommand prints that subcommand's flags and exits 0.
- An unknown flag or a missing value is a usage error (exit 2).

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success, or help requested. |
| 1 | Runtime failure: a file cannot be read or written, a key file is not a valid Ed25519 key, a file would be overwritten, the token is empty or invalid. |
| 2 | Usage error: unknown command or subcommand, missing or invalid flag, wrong number of positional arguments, `-ttl` above `-max-ttl`, a non-positive duration, an unknown role, a bad `-type`. |

### Output and secrets

- The only output that carries a secret is the token printed by `token mint`, on stdout, alone.
- Error messages name files and values but never print key material.
- Private keys are written with mode 0600 and never printed.

## Behaviour and guarantees

- `key generate` is race-safe: `O_EXCL` on both files means two concurrent runs cannot both succeed for one id.
  If the second file cannot be created, the first is removed again.
- A directory that already exists keeps its mode. Only a directory that `-out` creates gets 0700.
- `token mint` writes nothing. It reads the private key, signs and prints. A minted token is valid from `nbf`
  (`iat` minus 30 s) until `exp`.
- `token mint` does not know the servers' settings. A token minted with `-ttl` longer than a server's `-max-token-ttl`
  is refused by that server.
- The subject is free text. The role and type are validated; the subject and project are not checked against any
  registry.

## Configuration

No environment variables, configuration files or defaults other than the flag defaults above.

## Data

| File | Written by | Read by | Notes |
| --- | --- | --- | --- |
| `KEYID.key` | `key generate` | `token mint` | PKCS#8 PEM, mode 0600. Keep it on the signing machine only. |
| `KEYID.pub` | `key generate` | `token inspect`, and copied to every server as `-trusted-key` | PKIX PEM, mode 0644. |

## Observability

None. Errors go to stderr with the prefix `atticctl:`. There are no logs and no metrics.

## Failure modes and recovery

| Symptom | Cause | Action |
| --- | --- | --- |
| `refusing to overwrite DIR/KEYID.key` (exit 1) | Key id already used | Choose a new id (see [key rotation](../../libs/authz/README.md#key-rotation)). |
| `load signing key: ...` (exit 1) | Wrong path, a public key passed as `-key`, or a corrupt file | Check the path. The signing key is `KEYID.key`. |
| `invalid token: ... unknown key id` (exit 1) | `-kid` is not the key the token was signed with | Use the right `.pub` and `-kid`. |
| `invalid token: ... token is expired` (exit 1) | Token past `exp` | Mint a new token. Expired tokens cannot be inspected. |
| `no token given` (exit 1) | Empty argument or empty stdin | Pass the token. |
| `-ttl ... exceeds -max-ttl ...` (exit 2) | Lifetime above the mint ceiling | Lower `-ttl` or raise `-max-ttl` deliberately. |

## Examples

Use placeholders in scripts and notes. Never paste a real token or a private key into a shared document.

### Generate a signing key

```
atticctl key generate -out keys -id admin
```

Copy `keys/admin.pub` to every server that must accept the key, and pass it there with
`-trusted-key admin=/path/to/admin.pub[:roles]`.

### Mint a user token

```
TOKEN=$(atticctl token mint -key keys/admin.key -kid admin \
  -sub user:alice -type user -bind proj-a:admin -bind '*:operator' -ttl 1h)
```

Use it as `Authorization: Bearer $TOKEN`. Keep `-ttl` short for interactive use.

### Mint a service token by hand

```
atticctl token mint -key keys/ops.key -kid ops -sub svc:backup -type service -bind '*:operator' -ttl 15m
```

Services that call other services mint their own tokens in code (see
[libs/authz](../../libs/authz/README.md#service-to-service-calls)).

### Inspect a token

```
atticctl token inspect -pub keys/admin.pub -kid admin "$TOKEN"
echo "$TOKEN" | atticctl token inspect -pub keys/admin.pub -kid admin -
```

### Revoke a token

1. Get its id: `atticctl token inspect -pub keys/admin.pub -kid admin "$TOKEN"` and read `token-id`.
2. Append it to the revocation file of every `vmd`, `agentd` and `mcp` process that must refuse it:

   ```
   echo "tok_<ulid>  # reason: laptop lost" >> /etc/attic/revoked-tokens
   ```

3. Wait a few seconds. Each server reloads the file when its modification time changes (checked every 5 s).

Details of the file format are in [libs/authz](../../libs/authz/README.md#revocation).

### List roles

```
atticctl roles            # names only
atticctl roles -v         # names with their permissions
```

## Running and testing

```
cd cmd/atticctl && go build -o ../../bin/atticctl .
cd cmd/atticctl && go test -race -count=1 ./...
```

`main_test.go` covers the key round trip (generate, mint, inspect), reading the token from stdin, refusal to overwrite,
bad key ids, binding syntax, unknown roles, required flags, the TTL limits, a missing key file, invalid tokens, a token
from another key, usage errors, and the roles listing.

After changing `libs/authz/rbac.go`, regenerate the role table in [libs/authz](../../libs/authz/README.md) with
`cd cmd/atticctl && go run . roles -v`.

## Layout

```
cmd/atticctl/
  main.go       command dispatch, flag parsing, key generation, mint, inspect, roles
  main_test.go  tests of the commands, through run(args, stdin, stdout, stderr)
  go.mod
```

## Related

- [libs/authz/README.md](../../libs/authz/README.md): token format, verifier rules, roles, revocation, key rotation.
- [api/README.md](../../api/README.md): how tokens are sent (`Authorization: Bearer`) and the error codes they produce.
- [services/vmd/README.md](../../services/vmd/README.md) and
  [services/agentd/README.md](../../services/agentd/README.md):
  the servers that take `-trusted-key`, `-max-token-ttl` and `-revoked-tokens`.
- [deploy/README.md](../../deploy/README.md): where keys live in the compose setup.
