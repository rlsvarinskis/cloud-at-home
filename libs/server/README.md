# libs/server

`libs/server` is the plumbing shared by the long-running processes (vmd, agentd, mcp): the observability and error
handling for Connect servers (`obs`), and the daemon helpers (`daemon`) that parse trusted keys, build the verifier and
logger, serve HTTP with TLS or cleartext HTTP/2, and shut down gracefully.

Module: `github.com/rlsvarinskis/cloud-at-home/libs/server`

## Role

- `obs`: RPC metrics as a Connect interceptor; the protected `/metrics` handler; `/healthz` and `/readyz`; build info;
  an error interceptor that logs internal errors.
- `daemon`: the `-trusted-key` flag type and verifier construction; wiring of the revocation file; loading a service's
  signing key; the text logger; the HTTP server with graceful shutdown.
- Does not declare flags. Each `main` declares its own flags and passes the values in.
- Does not mount business handlers or routes. The services build their own `http.ServeMux` and add these handlers to it.
- Does not own state. It holds no database and no files other than the ones it is pointed at.

## Position

```
 cmd/main (flags) ─► daemon.TrustedKeys.Verifier ─► authz.Verifier ─┐
                  ─► daemon.WithRevocations ─────────────────────────┤
                  ─► daemon.LoadIssuer ─► authz.ServiceTokens        │
                  ─► daemon.NewLogger ─► slog.Logger                 │
                                                                     ▼
 service.NewHandler: obs.NewRPCMetrics → obs.ErrorInterceptor → authz.ServerInterceptor
                     /healthz  obs.HealthHandlers   /readyz   /metrics  obs.MetricsHandler
                                                                      │
 main: daemon.Serve(ctx, log, listener, mux, tlsCert, tlsKey, grace) ◄┘
```

- Used by `services/vmd`, `services/agentd`, `services/mcp` and the e2e tests.
- Imports allowed by `scripts/check-boundaries.sh`: `api`, `libs/common` and `libs/authz`.
- External: `connectrpc.com/connect`, `github.com/prometheus/client_golang`.

## Contract

### `obs`

#### Health

```go
type Check func(ctx context.Context) error // nil means healthy
type NamedCheck struct { Name string; Check Check }
func HealthHandlers(ready ...NamedCheck) (live, readyH http.Handler)
```

- `live` (`/healthz`) always answers `200` with the body `ok` (`text/plain`).
- `readyH` (`/readyz`) runs every check concurrently. Each check gets 2 s; a check that does not return in time counts
  as failed, even if it ignores its context. The answer is:
  - `200 {"status":"ok"}` when every check passes;
  - `503 {"status":"unavailable","failed":["name", ...]}` otherwise, with the failed names in the order given.
- Error text is never returned to the caller. Both responses carry `Cache-Control: no-store`.

#### Metrics endpoint

```go
func MetricsHandler(gatherer prometheus.Gatherer, v *authz.Verifier) http.Handler
```

Serves the Prometheus text exposition. The caller must send `Authorization: Bearer <token>`. The token must verify
and hold `capacity.get` on the global scope (a `*` binding):

| Case | Status | Body |
| --- | --- | --- |
| No bearer token, or the token does not verify | 401 | `unauthorized` |
| Valid token without `capacity.get` on `*` | 403 | `forbidden` |
| Valid operator token on `*` | 200 | the metrics |

If `v` is nil the endpoint serves without authentication. This is for local development only.

#### Registry and build info

```go
func NewRegistry() *prometheus.Registry
func RegisterBuildInfo(reg prometheus.Registerer, version string)
```

- `NewRegistry` returns a fresh registry with the Go runtime and process collectors. Servers use it instead of the
  global default registry, so only what they register is exposed.
- `RegisterBuildInfo` registers `attic_build_info{version, go_version}` with the value 1. It panics if already
  registered.

#### RPC metrics

```go
func NewRPCMetrics(reg prometheus.Registerer, subsystem string) *RPCMetrics // a connect.Interceptor
```

Creates and registers the metrics below. Every series carries the constant label `server=<subsystem>` (`vmd`,
`agentd`), so two servers can share one registry. It panics if the same subsystem is registered twice.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `attic_rpc_requests_total` | counter | `server`, `service`, `method`, `code` | Completed server RPCs. `code` is the Connect code, or `ok`. |
| `attic_rpc_duration_seconds` | histogram | `server`, `service`, `method` | Server-side duration. Buckets: 5 ms, 10, 25, 50, 100, 250, 500 ms, 1, 2.5, 5, 10, 30 s. Streams are timed for their whole life. |
| `attic_rpc_in_flight` | gauge | `server`, `service`, `method` | RPCs being handled now. |

The `service` and `method` labels come from the procedure `/package.Service/Method`. Client-side calls are not recorded.

#### Error interceptor

```go
func ErrorInterceptor(log *slog.Logger) connect.Interceptor
```

Converts any error a handler returns with `errs.ToConnect` (see [libs/common](../common/README.md)). When the result is
`internal`, it logs `internal error` with `procedure`, `caller` (the subject, or `anonymous`) and the original `err`.
The client receives only `internal error`.

Install order used by the services, outermost first: RPC metrics (when a registry is given), `ErrorInterceptor`,
`authz.ServerInterceptor`. The metrics then see the final status, and the authentication failure is still mapped.

### `daemon`

#### Trusted keys

```go
type TrustedKeys []string // implements flag.Value; each value is kid=path[:role,role]
func (k TrustedKeys) Verifier(maxTTL time.Duration) (*authz.Verifier, error)
```

`Verifier` builds an `authz.Verifier` with `MaxTTL = maxTTL` and one `TrustedKey` per flag value. It fails when:

- no keys are given;
- a value is not `kid=path`, or the `kid` or the path is empty;
- the key file cannot be read or is not an Ed25519 public key;
- a role in the list is unknown;
- a `kid` appears twice.

A missing role list means the key may grant any role.

#### Revocations

```go
func WithRevocations(ctx context.Context, v *authz.Verifier, path string, log *slog.Logger) error
```

An empty `path` does nothing. Otherwise the file is loaded (it must exist), attached as `v.Revocations`, and
watched every 5 s until `ctx` ends. It logs `token revocation list loaded` with the entry count.
`log` must not be nil. Format and
behaviour are in [libs/authz](../authz/README.md#revocation).

#### Signing key

```go
func LoadIssuer(path, kid string) (*authz.Issuer, error)
```

Loads the PKCS#8 private key at `path` and returns an issuer for `kid`. Both arguments are required. Used by services
that mint their own service tokens.

#### Logger

```go
func NewLogger(level string, w io.Writer) (*slog.Logger, error)
```

A `slog` text logger at `level`: `debug`, `info`, `warn` or `error` (any form `slog.Level` accepts is allowed). An
unknown level is an error.

#### Serving

```go
func Serve(ctx context.Context, log *slog.Logger, ln net.Listener, h http.Handler,
    certFile, keyFile string, grace time.Duration) error
```

- Protocols: HTTP/1.1, HTTP/2 over TLS, and cleartext HTTP/2 (`h2c`) when no certificate is given. gRPC clients
  therefore work behind a proxy without TLS.
- Timeouts: `ReadHeaderTimeout` 10 s, `IdleTimeout` 2 min. There is no `WriteTimeout`, because long-poll and streaming
  calls keep a response open.
- TLS: with `certFile` and `keyFile` it calls `ServeTLS`. No client certificate is required. The Go default TLS settings
  apply.
- Logs `listening` with `addr` and `tls`.
- Shutdown: when `ctx` ends it logs `shutting down` and calls `Shutdown`, which waits up to `grace` for in-flight
  requests. If they do not finish in time, the server is closed and `shutdown: ...` is returned.
- Returns `nil` after a clean shutdown. The services pass a 20 s grace period.

## Behaviour and guarantees

- Readiness is evaluated per request. Checks run in parallel, so the slowest check sets the time, up to 2 s.
- Metrics are updated on every completed call, including failed ones, and the in-flight gauge returns to zero when a
  call ends.
- `RPCMetrics` and `ErrorInterceptor` never change a successful response.
- Shutdown is graceful within `grace`; after it, open connections are closed and in-flight calls fail. A shutdown
  does not stop work already running in a VM: agent tasks and VM jobs carry on (see
  [services/vmd](../../services/vmd/README.md) and [services/agentd](../../services/agentd/README.md)).

## Configuration

These flags are declared by the services and passed to this library:

| Flag | Services | Default | Meaning |
| --- | --- | --- | --- |
| `-listen` | vmd `:8080`, agentd `:8081`, mcp `:8082` | see left | Address for the API and the health and metrics endpoints. |
| `-tls-cert`, `-tls-key` | all three | empty | Serve TLS. Without them, HTTP/1.1 and cleartext HTTP/2 are served. |
| `-metrics` | vmd, agentd | `true` | Serve `/metrics` (operator token required). |
| `-log-level` | all three | `info` | `debug`, `info`, `warn` or `error`. |
| `-trusted-key` | all three | none | Public key to trust, `kid=path[:role,role]`. Repeatable. |
| `-max-token-ttl` | all three | `24h` | Longest token lifetime accepted. |
| `-revoked-tokens` | all three | empty | Revocation file. |

The mcp server warns at start-up when no `-tls-cert` is given, because bearer tokens travel in clear text then.

## Data

None. The library reads key files, the revocation file and the TLS files it is given, and writes nothing.

## Observability

| Name | Type | Where |
| --- | --- | --- |
| `attic_rpc_requests_total`, `attic_rpc_duration_seconds`, `attic_rpc_in_flight` | see above | Every server |
| `attic_build_info` | gauge | When the service calls `RegisterBuildInfo` |
| Go runtime and process collectors | | From `NewRegistry` |

| Log message | Level | Meaning |
| --- | --- | --- |
| `listening` | info | The listener is up. Fields `addr`, `tls`. |
| `shutting down` | info | Shutdown started. Field `grace`. |
| `token revocation list loaded` | info | Revocation file in force. Field `entries`. |
| `internal error` | error | A handler returned an unexpected error. Fields `procedure`, `caller`, `err`. |
| `ErrorLog` from `net/http` | warn | Server-level errors (for example TLS handshake failures). |

Health: `/healthz` (liveness) and `/readyz` (readiness, failed check names). Metrics: `/metrics`, when enabled.

## Failure modes and recovery

| Situation | Effect | Operator action |
| --- | --- | --- |
| A readiness dependency is down | `/readyz` returns 503 with its name; `/healthz` stays 200. | Restore the dependency. Load balancers should use `/readyz`. |
| A readiness check hangs | Counted as failed after 2 s. | Investigate the dependency. |
| TLS files are missing or invalid | `Serve` returns an error and the process stops. | Fix the paths. |
| The address is in use | `Serve` returns the listen error. | Free the port or change `-listen`. |
| A stop takes longer than `grace` | Connections are closed; `Serve` returns `shutdown: ...`. | Expect in-flight calls to fail; callers retry. |
| The revocation file is removed while running | Warning in the log; the last list stays in force. | Restore the file. |

## Running and testing

```
cd libs/server && go build ./...
cd libs/server && go test -race -count=1 ./...
```

`daemon_test.go` covers the trusted-key syntax and its errors, `LoadIssuer`, `NewLogger`, and `Serve` speaking
HTTP/1.1 and cleartext HTTP/2 and shutting down gracefully. `obs_test.go` covers the RPC metrics over Connect (and
that client calls are not recorded), the `/metrics` authorization matrix, the liveness and readiness answers (including
a slow check), build info, and the registry.

## Layout

```
libs/server/
  obs/rpc.go        RPCMetrics: Connect interceptor and its three metrics
  obs/handlers.go   MetricsHandler, HealthHandlers, RegisterBuildInfo, NewRegistry
  obs/errors.go     ErrorInterceptor
  obs/obs_test.go   tests of obs
  daemon/daemon.go  TrustedKeys, WithRevocations, LoadIssuer, NewLogger, Serve
  daemon/daemon_test.go
```

## Related

- [libs/authz/README.md](../authz/README.md): tokens, the verifier, `ServerInterceptor`, revocation file format.
- [libs/common/README.md](../common/README.md): `errs.ToConnect`, the error codes.
- [api/README.md](../../api/README.md): the RPC paths (`/<package>.<Service>/<Method>`) and the protocols.
- [services/vmd/README.md](../../services/vmd/README.md), [services/agentd/README.md](../../services/agentd/README.md),
  [services/mcp/README.md](../../services/mcp/README.md): the processes that use this library.
- [cmd/atticctl/README.md](../../cmd/atticctl/README.md): mints the tokens these servers verify.
