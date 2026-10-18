# libs/qemu

`libs/qemu` is everything a physical host needs to run virtual machines for vmd. It defines the `node.Driver`
interface that the control plane talks to, a real implementation that runs QEMU/KVM on Linux, the command-line and
cloud-init builders behind it, the scripts that run inside guests, and the pieces that let a control plane reach a
node over the network (a client and a server for the node API). It also ships an in-memory fake for tests.

Module path: `github.com/rlsvarinskis/cloud-at-home/libs/qemu`.

## Packages

- `node`: the `Driver` interface (the contract between vmd and any host). `node/mock` is the generated gomock.
- `qemu`: pure builders. `Args` (qemu-system command line), `ImgCreateArgs` / `ImgResizeArgs` / `ImgConvertArgs`
  (qemu-img), `UserData` / `MetaData` (cloud-init), `ParseImageInfo`. Nothing here runs a process.
- `qemudriver`: the real `node.Driver` for a Linux host. Images and disks are qcow2 files under `Config.DataDir`;
  each VM is a `qemu-system-x86_64` process with a cloud-init seed ISO, a loopback SSH forward and a 9p share. Also
  firewall rules (`LockDownNetwork`) and `Commander` / `Procs` seams for testing.
- `guestexec`: runs shell commands as detached background jobs inside a guest (`Start`, `Poll`, `Cancel`, `PutFile`,
  `GetFile`), through a `Runner` interface.
- `guestmetrics`: the bash `Script` that samples CPU, memory, load, uptime and disk usage in a guest, and `Parse`
  turning its output into a `Sample`.
- `sshrunner`: the production `guestexec.Runner`. It dials the guest's forwarded port on 127.0.0.1 with an Ed25519
  key. Also key generation and loading.
- `remote`: a `node.Driver` that calls a node daemon over the node API (the client half).
- `nodeserver`: exposes any `node.Driver` over the node API (the server half). Every RPC requires `node.manage`.
- `fake`: an in-memory `node.Driver` for tests. Time is injected, and failures are scripted with `Crash`, `SetDown`
  and `FailNext`; `Calls` lists what was called.

## Public entry points

- `node.Driver`: the interface the control plane uses: capacity, images (register, delete), disks (create, resize, delete, snapshot), VMs (start,
  stop, runtime, forget, metrics), jobs (start, poll, cancel) and guest files (put, get).
- `qemudriver.New(Config) (*Driver, error)`, with `Config.NewRunner` to choose how guests are reached.
- `remote.New(httpClient, baseURL, tokens, ...)` and `nodeserver.New(driver, verifier, ...)`.
- `fake.New(Config) *Driver`.
- `sshrunner.New(Config)`, `sshrunner.LoadSigner`, `sshrunner.GenerateKey`.
- `guestexec` (`Exec`, `Spec`, `Runner`) and `guestmetrics.Parse` / `guestmetrics.Script`.

## Dependencies

- Depends on `api` (node API stubs), `libs/common` (`errs`) and `libs/authz` (token source and verifier for the node
  API).
- Must NOT depend on `libs/server`, `services/*`, `cmd/hcctl` or `e2e`. Service code reaches hosts only through
  `node.Driver`, so the driver code stays free of control-plane logic. `services/vmd` depends on this module, not the
  reverse.
- External: `connectrpc.com/connect`, `golang.org/x/crypto/ssh`, and `go.uber.org/mock` for the generated mock.

## Build and test

```
cd libs/qemu && go build ./...
cd libs/qemu && go test -race ./...
```

Most tests use the fake, local shells or an in-process SSH server. The tests `real_qemu_test.go` and
`real_qemuimg_test.go` run real `qemu-img` and QEMU. They skip when `qemu-img`, qemu-system, `/dev/kvm` or an ISO
builder (`genisoimage` or `xorriso`) is missing.

## Notes and limitations

- The real QEMU path has not been run against a guest image in this repository's CI. The guest-side scripts are
  tested with local shells, not inside a booted guest.
- `qemudriver.LockDownNetwork` flushes the firewall chain and refills it. For a moment the chain is empty and allows
  traffic, so it must be called only at startup, before VMs run (its doc comment says so).
- `sshrunner.LoadSigner` accepts unencrypted OpenSSH or PKCS#8 keys only; an encrypted key makes it return a parse error.
- The `guestexec` package comment says it is a port of `local-agent/gateway/gateway.py`. That file is not in this
  repository, so the claim cannot be checked here.
- The `remote` package comment says the server half is `internal/vmd/node/nodeserver`. The actual package is
  `libs/qemu/nodeserver`. The comment predates the module split.
- `fake` runs `Exec` under its lock, so an `Exec` implementation must not call back into the driver.
