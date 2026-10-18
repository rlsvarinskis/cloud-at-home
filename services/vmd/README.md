# services/vmd

`services/vmd` is the VM control plane. It manages virtual machines, disks, images, in-guest command jobs and
cluster capacity across one or more hosts. Clients record a desired state through its Connect API; a reconciler
drives each resource towards that state by calling the node drivers from `libs/qemu`. The same binary runs as the
control plane, as a node daemon on a host, or both.

Module path: `github.com/rlsvarinskis/cloud-at-home/services/vmd`.

## Packages

- `cmd/vmd`: the server binary. `--role=all` (control plane plus an in-process node, the default), `--role=control`
  (control plane talking to nodes given as `--node id=URL`) and `--role=node` (node daemon only).
- `domain`: the internal model (VM, Disk, Image, Job, Node, Usage) and its state enums. It is separate from the
  protobuf types.
- `service`: the public API (VM, Disk, Image, Exec, Metrics and Cluster servers, `NewHandler(Deps)`), the
  `Reconciler`, the `NodeManager` (registry of node drivers, which polls each node's capacity into the store), and
  `ClusterCapacity` / `SampleVM`.
- `scheduler`: pure placement arithmetic. `Fits` checks one node against a request with overcommit and host reserve;
  `Pick` chooses a node by load (tested, but unused in production code).
- `promcollect`: a Prometheus `Collector` for capacity, VM counts, cached guest metrics and reconciler activity.
- `store`: the `Store` interface (the persistence contract) and `Memory`, the in-memory reference.
- `store/postgres`: `Store` on PostgreSQL, with its own migrations (`Migrate`, `New`).
- `store/shard`: a `Store` that splits projects across several stores by rendezvous hashing (`New`, `WithProject`).
- `store/storetest`: the conformance suite every store must pass (`Run`).
- `store/mock`, `service/servicemock`: gomock mocks generated from the interfaces (`make mocks`).

## Public entry points

- The Connect services, created by `service.NewHandler(Deps)`, which implements the vmd API from `api`.
- `service.NewReconciler(store, nodes, cfg, owner)` and `service.NewNodeManager(store, log, now)`.
- `store.NewMemory()`, `postgres.New(pool)`, `shard.New(stores)`.
- `scheduler.Fits`, `scheduler.Avail`, `scheduler.Pick`.
- `promcollect.New(store, nodes, sched, options)`.

## Dependencies

- Depends on `api`, `libs/common` (`errs`, `ids`, `pg`), `libs/authz` (RBAC and tokens), `libs/qemu` (`node.Driver`,
  `fake`, `qemudriver`, `remote`, `nodeserver`, `sshrunner`) and `libs/server` (`daemon`, `obs`).
- Must NOT depend on `services/agentd`, `cmd/hcctl` or `e2e`. agentd uses only vmd's public API, and tests that run
  both live in `e2e`. The rule in `scripts/check-boundaries.sh` allows `libs/qemu` and `libs/server` here.
- External: `connectrpc.com/connect`, `pgx/v5`, `prometheus/client_golang`, `go.uber.org/mock` (for mocks).

## Build and test

```
cd services/vmd && go build ./...
cd services/vmd && go test -race ./...
```

Store tests run the `storetest` suite against memory, against Postgres (started by `libs/common/pg/pgtest` or
`HOMECLOUD_TEST_POSTGRES`), and against the shard router over one and three in-memory shards. Setting
`HOMECLOUD_TEST_SHARDS=3` runs the whole service suite on a sharded store. Reconciler behaviour is tested with the
fake node driver and a manual clock.

## Notes and limitations

- Placement is serialised per node by an in-process `sync.Map` of mutexes in `service/reconciler.go`. Two vmd
  replicas can still both pass the capacity check and overcommit a node; closing that needs a reservation in the store.
- `scheduler.Pick` exists and is tested, but no production code calls it. VM placement uses `Fits` on the node that
  holds the boot image.
- Changing the set of shard databases re-routes projects. There is no rebalancing tool.
- `store` documents "two implementations" (memory and postgres); the shard router is a third that also implements
  `store.Store`.
- The `libs/qemu` real-host path (`qemudriver`) has not been exercised against a booted guest in this repository.
- `scripts/check-boundaries.sh` also lists `services/auth`, which does not exist in this repository.
