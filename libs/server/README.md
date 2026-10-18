# libs/server

`libs/server` is the part of the vmd and agentd daemons that is the same in both: Prometheus metrics for RPCs,
the protected `/metrics` endpoint and health probes, the mapping from handler errors to Connect errors, command-line
helpers (trusted keys, the signing key, logging) and the HTTP server with TLS and graceful shutdown.

Module path: `github.com/rlsvarinskis/cloud-at-home/libs/server`.

## Packages

- `obs`: observability. `NewRPCMetrics` / `RPCMetrics` (a server-side Connect interceptor that counts and times
  RPCs), `ErrorInterceptor` (turns handler errors into Connect errors and logs the ones that become Internal),
  `NewRegistry` (a private Prometheus registry, not the global default), `MetricsHandler` (the `/metrics` endpoint),
  `HealthHandlers` (liveness and readiness handlers; readiness runs the named `Check`s) and `RegisterBuildInfo`.
- `daemon`: what both servers share at start-up. `LoadIssuer` (signing key and key ID), `TrustedKeys` (a repeatable
  `flag.Value` of trusted public keys), a verifier builder, `NewLogger` (slog), `WithRevocations` (rejects the token
  IDs listed in a file and keeps the list fresh until its context ends), and `Serve` (HTTP/1.1, HTTP/2 over TLS, or
  cleartext HTTP/2 for gRPC behind a proxy, with graceful drain).

## Public entry points

- `obs.NewRegistry()`, `obs.NewRPCMetrics(reg, subsystem)`, `obs.ErrorInterceptor(log)`.
- `obs.MetricsHandler(gatherer, verifier)`: a nil verifier serves metrics without authentication. With a verifier,
  the caller needs a bearer token that holds `capacity.get` (the operator role).
- `obs.HealthHandlers(checks...)` returns `(live, ready http.Handler)`.
- `obs.RegisterBuildInfo(reg, version)`: panics if called twice on the same registry.
- `daemon.Serve(ctx, log, listener, handler, certFile, keyFile, grace)`.
- `daemon.LoadIssuer(path, kid)`, `daemon.WithRevocations(ctx, verifier, path, log)`, `daemon.NewLogger(level, w)`.

## Dependencies

- Depends on `libs/authz` (verifier, issuer, permissions for `/metrics`, revocation file) and `libs/common/errs`.
- Depends on `api` only in its test file (`obs/obs_test.go`) for the vmd Connect stubs.
- External: `connectrpc.com/connect`, `prometheus/client_golang`.
- Must NOT depend on `libs/qemu`, `services/*`, `cmd/atticctl` or `e2e`. The servers depend on this module; it must not
  know about any particular service.

## Build and test

```
cd libs/server && go build ./...
cd libs/server && go test -race ./...
```

`obs/obs_test.go` covers the RPC metrics (including client calls not being counted), the `/metrics` authorization,
health handlers and build info. `daemon/daemon_test.go` covers trusted-key parsing, `LoadIssuer`, the logger, and
`Serve` over HTTP/1.1 and cleartext HTTP/2 with graceful shutdown.

## Notes and limitations

- `RPCMetrics` records server-side calls only; client-side calls are not counted (its doc comment says so).
- `Serve` uses `ServeTLS` when `certFile` is set. Without a certificate it serves cleartext HTTP/2, which is meant
  for a TLS-terminating proxy in front. Nothing here stops cleartext being exposed directly.
- `RegisterBuildInfo` panics on a duplicate registration instead of returning an error.
- `LoadIssuer` requires both the key file and the key ID; it does not check that the key ID matches the public key
  the verifiers trust.
- The `obs` package doc calls `/metrics` a protected endpoint. The code requires a bearer token holding
  `capacity.get`, which only the operator role has, so in practice it is an operator endpoint.
