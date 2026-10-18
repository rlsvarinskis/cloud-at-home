# api

`api` is the wire contract of Attic Cloud: the Protocol Buffers definitions and the Go code generated from them
for the Connect RPC protocol. It contains no hand-written logic. The `.proto` files under `proto/` are the source
of truth; `gen/` is generated code that is committed to the repository.

## Packages

| Import path | Package | Contents |
|---|---|---|
| `.../api/gen/attic/vmd/v1` | `vmdv1` | Messages for vmd's VMService, ImageService, DiskService, ExecService, ClusterService and MetricsService |
| `.../api/gen/attic/vmd/v1/vmdv1connect` | `vmdv1connect` | Connect client and handler constructors for the vmd API |
| `.../api/gen/attic/agentd/v1` | `agentdv1` | Messages for agentd's AgentService, TaskService and ModelService |
| `.../api/gen/attic/agentd/v1/agentdv1connect` | `agentdv1connect` | Connect client and handler constructors for the agentd API |
| `.../api/gen/attic/node/v1` | `nodev1` | Messages for NodeService, the host API behind `node.Driver` |
| `.../api/gen/attic/node/v1/nodev1connect` | `nodev1connect` | Connect client and handler constructors for the node API |

Module path: `github.com/rlsvarinskis/cloud-at-home/api`.

## Public entry points

Every service has generated constructors, for example `vmdv1connect.NewVMServiceHandler` (returns the path and
`http.Handler` to mount) and `vmdv1connect.NewVMServiceClient` (a typed client over a `connect.HTTPClient`).
The same pattern exists for `NewImageService*`, `NewDiskService*`, `NewExecService*`, `NewClusterService*`,
`NewMetricsService*`, `NewAgentService*`, `NewTaskService*`, `NewModelService*` and `NewNodeService*`.
Enum name maps (for example `vmdv1.VMState_name`) and the `File_*` descriptors are also exported by the generated
packages.

The proto packages are `attic.vmd.v1`, `attic.agentd.v1` and `attic.node.v1`. The `.proto` files are in
`proto/attic/<service>/v1/`.

## Dependencies

- Depends on nothing else in this repository. Its only external requirements are `connectrpc.com/connect` and
  `google.golang.org/protobuf`.
- Must NOT depend on any other module here. Every other module depends on `api`, so a dependency in the other
  direction would create a cycle.
- `api/go.mod` contains `replace` directives that point at sibling modules (libs, services, cmd, e2e). They are
  not needed to build `api` and do not create dependencies; similar blocks appear in the other modules' `go.mod`.

## Build and test

```
cd api && go build ./...
cd api && go test -race ./...     # no test files; this only confirms that the package compiles
```

Regenerating `gen/` needs `protoc`, `protoc-gen-go` and `protoc-gen-connect-go` on the PATH. From the repository
root run `make proto`. The target deletes `gen/` and rebuilds it from `proto/`. Do not edit files in `gen/` by hand.

## Notes and limitations

- `api` has no tests and no hand-written code. Compatibility of changes is checked only indirectly, by the tests of
  the modules that use it.
- The generated files record `protoc v6.33.5` in their headers. The Makefile does not pin the plugin versions, so a
  regeneration with other tool versions will change the generated files.
- Because the `.proto` files are the only source, a field added to a message must be added there and regenerated;
  the packages cannot be extended in place.
