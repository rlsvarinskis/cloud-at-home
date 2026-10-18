# e2e

`e2e` runs the vmd and agentd servers together, in one test process, and drives them only through their real
Connect APIs over HTTP. It is the only module allowed to depend on both services. It has no production code: the
package is a comment in `doc.go` and everything else is `_test.go` files.

Module path: `github.com/rlsvarinskis/cloud-at-home/e2e`.

## Packages

The module is one test package, `e2e_test`. Its files:

- `world_test.go`: the harness. It starts vmd and agentd with `httptest` servers, a manual clock, the fake node
  driver (`libs/qemu/fake`) and a scripted LLM (`llmHub`, one `llm.Fake` per model). It runs with the memory or the
  Postgres backend and can split vmd across memory shards.
- `golden_test.go`: the main scenarios: the golden path (a task that succeeds and one whose check fails), the golden
  path on a sharded vmd, and many agents sharing one VM.
- `authz_test.go`: authorization across both services (scenario 4).
- `holds_test.go`: a VM that agents hold cannot be deleted until they are gone; a forced delete orphans them (3).
- `cancel_test.go`: cancelling a task, while the model is thinking and while a command runs in the VM (6).
- `failure_test.go`: failures in the middle of a task (5).
- `idem_test.go`: retried requests create one resource, and a retried StartJob does not run a command twice (7).
- `metrics_test.go`: both servers expose metrics behind the operator role (8).
- `main_test.go`: `TestMain` calls `pgtest.Run`, so the Postgres test server is started once.

## Public entry points

There are none. The tests are the interface: `go test -run TestGoldenPath ./...` and similar.

## Dependencies

- Depends on `api`, `libs/common` (`pg`, `pg/pgtest`), `libs/authz`, `libs/qemu` (`fake`), `libs/server` (`obs`),
  `services/vmd` (its `service`, `store`, `store/postgres`, `store/shard`, `scheduler`, `promcollect`) and
  `services/agentd` (its `llm`, `runner`, `service`, `store`, `store/postgres`, `vmclient`).
- E2E may depend on any module. In practice, nothing else may depend on `e2e`: no other module should import it.
- External: `connectrpc.com/connect`, `pgx/v5`, `prometheus/client_golang`, `embedded-postgres` through `pgtest`.

## Build and test

```
cd e2e && go vet ./...
cd e2e && go test -race ./...
```

Scenarios run on the memory backend without any setup. Postgres scenarios need `HOMECLOUD_TEST_POSTGRES` (an
admin DSN of a running server), or the embedded Postgres binaries, which are downloaded on first use. Set
`HOMECLOUD_TEST_POSTGRES=skip` to skip them. Use `-count=1` to avoid cached results.

## Notes and limitations

- The clock is manual and the LLM is scripted, so the tests check the interaction of the two servers, not model
  quality or real time.
- Nothing here boots a guest VM. Node behaviour comes from the fake driver, so the QEMU path is not covered.
- The sharded scenario uses in-memory shards only (`withShards` says so); the shard router is tested in the vmd module over
  in-memory shards only.
- Lease and long-poll behaviour in agentd and vmd is tied to real time in places, so the suite can be sensitive to
  machine load. An earlier review reported an intermittent failure under `-race` load. This review did not run the tests,
  so that report is not confirmed here.
- The harness does not start the real `cmd/vmd` or `cmd/agentd` binaries; it builds the handlers in-process.
