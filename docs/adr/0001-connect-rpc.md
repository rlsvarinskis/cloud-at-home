# ADR 1: Connect RPC for gRPC and HTTP

**Decision.** Use `connectrpc.com/connect`. One handler serves the gRPC, gRPC-Web and Connect protocols, so
the same code answers `grpcurl`/generated gRPC clients *and* plain `curl -d '{...}' -H 'Content-Type:
application/json' https://host/homecloud.vmd.v1.VMService/GetVM`.

**Why not grpc-go + grpc-gateway.** Two servers and a generated REST layer for what Connect gives from one
handler; Connect also works through HTTP/1.1 proxies (reverse proxies such as nginx) without special configuration.

**Consequences.** REST-style URLs (`GET /v1/projects/p/vms/x`) are not served yet; they can be added by
annotating the protos with `google.api.http` and fronting with `connectrpc.com/vanguard`.
