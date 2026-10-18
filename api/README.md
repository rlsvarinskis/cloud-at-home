# api

`api` is the wire contract of Attic Cloud: the Protocol Buffers definitions and the Go code generated from them for
the Connect RPC protocol. It contains no hand-written logic. The `.proto` files under `proto/` are the source of truth;
`gen/` is generated code that is committed.

Module: `github.com/rlsvarinskis/cloud-at-home/api`

## Role

- Defines every RPC, message and enum of the three APIs: vmd (`attic.vmd.v1`), agentd (`attic.agentd.v1`) and the
  node daemon (`attic.node.v1`).
- Generates Go messages (`protoc-gen-go`) and Connect clients and handlers (`protoc-gen-connect-go`) into `gen/`.
- States the wire conventions every service follows: transport, JSON, errors, pagination, long-polling, idempotency
  keys, identifiers and timestamps (see [Wire conventions](#wire-conventions)).
- Does not implement behaviour. What each RPC does, its permission and its errors are in the service READMEs
  ([vmd](../services/vmd/README.md), [agentd](../services/agentd/README.md)) and [libs/qemu](../libs/qemu/README.md)
  for the node API.
- Does not describe the MCP server. `services/mcp` exposes MCP tools, not these RPCs; its README covers it.

## Position

```
api/proto/attic/*/v1/*.proto --make proto--> api/gen/attic/*/v1          (messages)
                                           \--> api/gen/attic/*/v1/*connect   (clients, handlers)

  services/vmd     (VM, image, disk, exec, metrics, cluster)
  services/agentd  (agent, task, model)
  libs/qemu        (NodeService, served by vmd --role=node)
  services/mcp     (MCP tools; calls vmd and agentd through their clients)
  e2e              (tests)
```

- Imports nothing from this repository. Every other module depends on `api`, so `api` must not depend on any of them
  (`scripts/check-boundaries.sh` allows no sibling imports).
- External: `connectrpc.com/connect` v1.21.1, `google.golang.org/protobuf` v1.36.12.

## Contract

### Packages

| Proto package | Source | Go import path | Go package | Contents |
| --- | --- | --- | --- | --- |
| `attic.vmd.v1` | `proto/attic/vmd/v1/vmd.proto` | `.../api/gen/attic/vmd/v1` | `vmdv1` | Messages and enums for vmd's six services |
| `attic.vmd.v1` (connect) | same | `.../api/gen/attic/vmd/v1/vmdv1connect` | `vmdv1connect` | Clients and handlers for vmd |
| `attic.agentd.v1` | `proto/attic/agentd/v1/agentd.proto` | `.../api/gen/attic/agentd/v1` | `agentdv1` | Messages and enums for agentd's three services |
| `attic.agentd.v1` (connect) | same | `.../api/gen/attic/agentd/v1/agentdv1connect` | `agentdv1connect` | Clients and handlers for agentd |
| `attic.node.v1` | `proto/attic/node/v1/node.proto` | `.../api/gen/attic/node/v1` | `nodev1` | Messages and `NodeService` |
| `attic.node.v1` (connect) | same | `.../api/gen/attic/node/v1/nodev1connect` | `nodev1connect` | Client and handler for the node API |

Every generated Connect package exports `New<Service>Handler(impl, opts...)`, which returns the path and handler to
mount, and `New<Service>Client(httpClient, baseURL, opts...)`. Generated packages also export the enum name and
value maps (for example `vmdv1.VMState_name`) and the `File_*` descriptors.

### Calling from Go

```go
client := vmdv1connect.NewVMServiceClient(
    http.DefaultClient,
    "https://vmd.example.internal:8080", // placeholder address
    connect.WithInterceptors(authz.ClientInterceptor(authz.StaticToken(token))),
)
res, err := client.ListVMs(ctx, connect.NewRequest(&vmdv1.ListVMsRequest{ProjectId: "proj-a", PageSize: 10}))
if err != nil {
    // connect.CodeOf(err) is the Connect code, for example connect.CodeNotFound.
    // errs.FromConnect(err) wraps the matching sentinel, so errors.Is(err, errs.ErrNotFound) works.
    return fmt.Errorf("list vms: %w", errs.FromConnect(err))
}
fmt.Println("vms:", len(res.Msg.Vms))
```

`authz.StaticToken` suits a fixed token. A service that mints its own tokens uses `authz.ServiceTokens` (see
[libs/authz](../libs/authz/README.md#service-to-service-calls)). Client errors are `*connect.Error`.

### Service index

Every service and RPC in the API. The RPC behaviour, permission, request fields and errors are in the implementing
README.

| Service | Proto package | Implemented by | RPCs |
| --- | --- | --- | --- |
| `VMService` | `attic.vmd.v1` | [services/vmd](../services/vmd/README.md) | `CreateVM`, `GetVM`, `ListVMs`, `UpdateVM`, `StartVM`, `StopVM`, `DeleteVM`, `AddHold`, `RemoveHold` |
| `ImageService` | `attic.vmd.v1` | [services/vmd](../services/vmd/README.md) | `RegisterImage`, `GetImage`, `ListImages`, `DeleteImage` |
| `DiskService` | `attic.vmd.v1` | [services/vmd](../services/vmd/README.md) | `CreateDisk`, `GetDisk`, `ListDisks`, `ResizeDisk`, `DeleteDisk`, `SnapshotDisk` |
| `ExecService` | `attic.vmd.v1` | [services/vmd](../services/vmd/README.md) | `StartJob`, `PollJob`, `CancelJob`, `ListJobs`, `PutFile`, `GetFile` |
| `MetricsService` | `attic.vmd.v1` | [services/vmd](../services/vmd/README.md) | `GetVMMetrics`, `GetCapacity` |
| `ClusterService` | `attic.vmd.v1` | [services/vmd](../services/vmd/README.md) | `SetNodeDraining` |
| `AgentService` | `attic.agentd.v1` | [services/agentd](../services/agentd/README.md) | `CreateAgent`, `GetAgent`, `ListAgents`, `UpdateAgent`, `DeleteAgent` |
| `TaskService` | `attic.agentd.v1` | [services/agentd](../services/agentd/README.md) | `CreateTask`, `GetTask`, `ListTasks`, `CancelTask`, `ListSteps` |
| `ModelService` | `attic.agentd.v1` | [services/agentd](../services/agentd/README.md) | `ListModels` |
| `NodeService` | `attic.node.v1` | [libs/qemu](../libs/qemu/README.md) (`nodeserver`), served by `vmd --role=node` | `GetCapacity`, `RegisterImage`, `DeleteImage`, `CreateDisk`, `ResizeDisk`, `DeleteDisk`, `SnapshotDisk`, `StartVM`, `StopVM`, `GetVMRuntime`, `ForgetVM`, `StartJob`, `PollJob`, `CancelJob`, `PutFile`, `GetFile`, `GetVMMetrics` |

`vmd` serves the vmd services in its control-plane roles. The node API is served only by a node daemon. It is
called only by the vmd control plane, with the `node` role. Its RPCs are used by vmd, not by clients.

### Wire conventions

#### Transport

Every RPC is a `POST` to `/<package>.<Service>/<Method>`, for example `/attic.vmd.v1.VMService/GetVM`. One
handler serves three protocols on the same paths:

| Protocol | Use |
| --- | --- |
| Connect (JSON or binary protobuf) | `curl`, browsers and any HTTP client. Use `Content-Type: application/json`. |
| gRPC | Generated gRPC clients and `grpcurl`. Cleartext HTTP/2 is accepted without TLS (see [libs/server](../libs/server/README.md)). |
| gRPC-Web | Browser clients through a proxy. |

Server reflection is not enabled. Clients load the `.proto` files themselves.

#### JSON encoding

The standard protobuf JSON mapping applies:

- Field names are lowerCamelCase (`projectId`, `memoryMb`).
- Enums are written by name (`"VM_STATE_RUNNING"`).
- `int64` values are written as JSON strings (`"memoryMb": "2048"`). Requests accept numbers or strings.
- `bytes` fields are base64.
- Fields with zero values are omitted from responses. An absent field means zero, empty or false.
- `optional` fields distinguish "not set" from zero. Used in update requests such as `UpdateVMRequest.cpus`.
- Timestamps are `google.protobuf.Timestamp`: RFC 3339 strings in UTC with fractional seconds.
- Durations are not used on the wire. They are integers with the unit in the field name (`timeout_seconds`, `size_gb`,
  `memory_mb`, `size_bytes`, `max_bytes`).

#### Authentication

Every call carries `Authorization: Bearer <token>`. A missing, invalid or revoked token gets `unauthenticated`.
The rules for tokens and the permissions behind each RPC are in [libs/authz](../libs/authz/README.md).

#### Errors

A failed call returns a Connect error. In the JSON protocol the body is `{"code": "<code>", "message": "<text>"}`. The
code is one of the Connect codes below. The mapping from the sentinel errors of `libs/common/errs` is fixed:

| `errs` sentinel | Connect code | HTTP status (Connect protocol) | Meaning |
| --- | --- | --- | --- |
| `ErrUnauthenticated` | `unauthenticated` | 401 | No valid token. |
| `ErrPermissionDenied` | `permission_denied` | 403 | The token lacks the permission on the project. |
| `ErrNotFound` | `not_found` | 404 | Unknown id, or the caller has no access to its project. |
| `ErrInvalidArgument` | `invalid_argument` | 400 | The request is malformed or fails validation. |
| `ErrFailedPrecondition` | `failed_precondition` | 400 | The resource is in a state that does not allow the call. |
| `ErrAlreadyExists` | `already_exists` | 409 | The resource or idempotency key already exists for another request. |
| `ErrResourceExhausted` | `resource_exhausted` | 429 | A quota or capacity limit was reached. |
| `ErrConflict` | `aborted` | 409 | Optimistic concurrency failed. Retry after reading again. |
| `ErrUnavailable` | `unavailable` | 503 | A dependency (node, vmd, model backend) could not be reached. Retry later. |
| `context.DeadlineExceeded` | `deadline_exceeded` | 504 | The call ran out of time. |
| `context.Canceled` | `canceled` | 499 | The caller went away. |
| anything else | `internal` | 500 | Unexpected. The message is `internal error`; the details are only in the server log. |

Other Connect codes that can appear: `unimplemented` (501) and `unknown` (500). Clients that use gRPC receive the same
code names in `grpc-status`; the HTTP status is 200 for gRPC.

Errors from a downstream system are rewritten before they reach the client (`errs.Sanitize`): answers with a meaning
for the caller pass through, and transport failures become `unavailable` naming the component (for example
`node n1 is unavailable`), never an address.

#### Pagination

Every list RPC takes `page_size` and `page_token`:

- `page_size` defaults to 50 when 0 or negative, and is capped at 200.
- A response has `next_page_token` when another page exists. Pass it back as `page_token`. An empty token means the last
  page.
- Tokens are opaque. Do not construct them.
- Lists are ordered by `id`. Ids sort by creation time (see [Identifiers](#identifiers)).

`ListSteps` is the exception: it uses a cursor and a limit. `after_seq` returns steps with a larger `seq` (0
means from the start). `limit` defaults to 100 when 0, and is capped at 500.

#### Long-polling

Calls that may take a while accept `wait_seconds`. The server checks the state about every 200 ms and returns when the
condition holds, or when the wait ends. The wait is clamped to 30 seconds, because clients and proxies often give up
around 60 seconds. A wait that ends is not an error: the call returns the current state.

| RPC | Wait fields | Returns early when |
| --- | --- | --- |
| `VMService.GetVM` | `wait_for_state`, `wait_seconds` | The VM reaches `wait_for_state`, or it is in `VM_STATE_ERROR`. |
| `TaskService.GetTask` | `wait_for_not`, `wait_seconds` | The task leaves the state `wait_for_not`. |
| `ExecService.StartJob` | `wait_seconds` | The job is final. |
| `ExecService.PollJob` | `wait_seconds` | The job is final. New output alone does not end the wait. |

#### Idempotency keys

Creates that may be retried take an `idempotency_key` string. The four RPCs that have one are `VMService.CreateVM`,
`ExecService.StartJob`, `AgentService.CreateAgent` and `TaskService.CreateTask`. A retry with the same key gets the
result of the first call instead of creating a second resource. An empty key does not match anything. The exact
effect of a retry, including reuse of a key with different fields, is in each service's README.

Other creates (disks, images, snapshots) take no key.

#### Identifiers

IDs are strings `<prefix>_<ulid>`. The ULID is 26 lowercase characters in Crockford base32, time-ordered.

| Prefix | Entity |
| --- | --- |
| `vm_` | Virtual machine |
| `disk_` | Disk |
| `img_` | Image (also snapshots) |
| `job_` | Job (command run in a VM) |
| `agent_` | Agent |
| `task_` | Task |
| `tok_` | Token id (`jti`); it appears in `token inspect`, not in the API |

When vmd runs with more than one database (repeated `-postgres-dsn`), projects are sharded across them and IDs carry a
suffix `-sNN`, where `NN` is the shard number in two lowercase hex digits. Clients treat the whole string as opaque.

Names of VMs, disks, images and agents (the `name` field) must match a lowercase letter, then up to 61 lowercase
letters, digits or hyphens, ending in a letter or digit. Project ids and
other free-text fields have no format rule in this API.

#### Enums

Every enum has a zero value named `<TYPE>_UNSPECIFIED`, which means "not set". Names carry the type as a prefix
(`VM_STATE_`, `JOB_STATE_`, `TASK_STATE_`). State enums describe what is observed; desired-state enums describe what was
asked (`PowerState`).

#### Units

Memory is `_mb` (MiB) in requests, and `_bytes` in metrics and images. Disk size is `size_gb`. CPU counts are `cpus`
(vCPUs). Metric percentages follow `top`: 100 per busy vCPU.

### Versioning policy

- The version is in the package name: `attic.<service>.v1`. `v1` is the only version in use.
- Additive changes are allowed in `v1`: new RPCs, new messages, new optional fields, and new enum values. Clients must
  ignore unknown fields and enum values.
- Never reuse a field number or an enum number. Never change a field's type. Never rename a field or an RPC on the wire.
  Mark a removed field `reserved`.
- A breaking change goes into a new package, `attic.<service>.v2`, with its own `.proto` file and `gen/` directory. The
  old version is served alongside it until clients have moved.
- Behaviour changes that keep the wire format (for example a new default) are documented in the service README and
  are not versioned.

This is the policy the project follows. No code enforces it. A change to an existing `.proto` file is reviewed against
it.

## Behaviour and guarantees

- The generated code is deterministic for a given input and toolchain. CI regenerates it and fails if `api/gen` differs
  from the committed files.
- The package has no runtime state, goroutines or I/O.

## Configuration

None.

## Data

The `.proto` files are the only source. `gen/` is derived from them and committed.

## Observability

None in this package. RPC metrics are provided by the servers (see [libs/server](../libs/server/README.md)).

## Failure modes and recovery

Not applicable at run time. A client built from older generated code calls an RPC or a field that does not exist on the
server: the server answers with a Connect error, and the caller must regenerate.

## Generating the code

### Tools

| Tool | Version | Notes |
| --- | --- | --- |
| `protoc` | 33.5 (`libprotoc 33.5`; generated headers record `protoc v6.33.5`) | Installed in CI from the protobuf release zip. |
| `protoc-gen-go` | v1.36.11 | `go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11` |
| `protoc-gen-connect-go` | v1.21.1 | `go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.21.1` |

The plugins must be on `PATH` (usually in `$HOME/go/bin`). The runtime libraries in `go.mod` are
`google.golang.org/protobuf` v1.36.12 and `connectrpc.com/connect` v1.21.1. Install the same tool versions
locally, or the output will differ from CI.

Well-known types such as `google/protobuf/timestamp.proto` come from the protoc include directory (`/usr/include`).

### Workflow

From the repository root:

```
make proto
```

The target deletes `api/gen/` and regenerates it from `api/proto/` with
`protoc -I proto -I /usr/include --go_out=gen --connect-go_out=gen --<plugin>_opt=paths=source_relative`. Commit the
regenerated `api/gen/` together with the `.proto` change. Never edit files in `gen/` by hand.

Adding an RPC:

1. Add the messages and the `rpc` line to the service in `proto/attic/<svc>/v1/*.proto`.
2. Run `make proto`.
3. Implement the handler in the service that owns the RPC, and add the permission check (see
   [libs/authz](../libs/authz/README.md)).
4. Add the RPC to the service index above and to the service's README.

## Build and test

```
cd api && go build ./...
cd api && go vet ./...
cd api && go test -race -count=1 ./...
```

The module has no test files. The test command only confirms that the package compiles. Compatibility is checked by the
tests of the modules that use the generated code.

## Layout

```
api/
  proto/attic/vmd/v1/vmd.proto        vmd services and messages
  proto/attic/agentd/v1/agentd.proto  agentd services and messages
  proto/attic/node/v1/node.proto      node daemon service and messages
  gen/attic/<svc>/v1/*.pb.go          generated messages
  gen/attic/<svc>/v1/<svc>v1connect/  generated Connect clients and handlers
  go.mod, go.sum
  README.md
```

## Related

- [services/vmd/README.md](../services/vmd/README.md): VMService, ImageService, DiskService, ExecService,
  MetricsService,
  ClusterService.
- [services/agentd/README.md](../services/agentd/README.md): AgentService, TaskService, ModelService.
- [libs/qemu/README.md](../libs/qemu/README.md): the node API and the node daemon.
- [libs/authz/README.md](../libs/authz/README.md): tokens, `Authorization: Bearer`, permissions per role.
- [libs/common/README.md](../libs/common/README.md): the `errs` sentinels and their mapping to Connect codes.
- [libs/server/README.md](../libs/server/README.md): HTTP server, protocols, health and metrics endpoints.
- [cmd/atticctl/README.md](../cmd/atticctl/README.md): minting the tokens used in calls.
- [e2e/README.md](../e2e/README.md): tests that call the APIs over HTTP.
