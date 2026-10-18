# libs/qemu

`libs/qemu` is the host side of Attic Cloud: everything one physical host needs to run VMs for vmd. It defines the
`node.Driver` interface that vmd calls, a real implementation for Linux with QEMU/KVM, the command-line and
cloud-init builders, the scripts that run inside guests, and the node API (a server that exposes a driver and a
client that calls one over the network).

Module: `github.com/rlsvarinskis/cloud-at-home/libs/qemu`. Control plane: [vmd](../../services/vmd/README.md).

## Role

- Runs and supervises QEMU processes on one host, and owns the files of its images, disks and VMs under one data
  directory.
- Builds guest configuration (qemu arguments, cloud-init seed, the `agent` user) and runs guest commands, jobs,
  file transfers and metrics over SSH.
- Installs the host firewall that keeps guests off private networks (`LockDownNetwork`).
- Serves the node API, and provides the client that vmd uses for remote nodes.
- Does NOT decide placement, quotas or desired state. That is vmd, which retries failed steps.
- Does NOT store anything in a database, authenticate users, or know about projects. The node API is project-less;
  access is one role, `node`.
- Does NOT protect the host from a root attacker. It is one layer: unprivileged QEMU, the firewall, and key-only SSH.

## Position

```
 vmd (services/vmd)
   ├─ NodeManager / reconciler / ExecServer / MetricsServer ──► node.Driver
   │                                                             ├─ qemudriver.Driver   (role all, node: on this host)
   │                                                             └─ remote.Driver ──HTTP/Connect──► nodeserver
   │                                                                                    │
   │                                                       node daemon (vmd --role=node)│
   │                                                             qemudriver.Driver ◄────┘
   │                                                               │ qemu-system-x86_64 (one per VM, detached)
   │                                                               │ qemu-img, genisoimage, iptables, chown
   │                                                               ▼
   │                                              guest: sshd, bash jobs in /var/tmp/mcp-jobs/<id>
```

- Imports allowed by `scripts/check-boundaries.sh`: `api`, `libs/common`, `libs/authz`. Nothing else in the
  repository may be imported, and no service may be imported.
- `services/vmd` imports this module; the reverse is not allowed.

## Contract

### node.Driver

Package `libs/qemu/node`. Every method takes a context and a request from `api/gen/attic/node/v1` and returns the
matching response. Errors use the sentinels in `libs/common/errs`, so `errors.Is` works for every
implementation.

| Method | Purpose |
| --- | --- |
| `GetCapacity` | CPU threads, RAM and free disk of the host |
| `RegisterImage`, `DeleteImage` | Adopt or remove an image file |
| `CreateDisk`, `ResizeDisk`, `DeleteDisk`, `SnapshotDisk` | Disk files: overlay or blank create, grow, remove, flatten to an image |
| `StartVM`, `StopVM`, `GetVMRuntime`, `ForgetVM` | VM process lifecycle and observation |
| `StartJob`, `PollJob`, `CancelJob` | Jobs inside a guest |
| `PutFile`, `GetFile` | Files inside a guest |
| `GetVMMetrics` | Guest CPU, memory, load and filesystems; host allocation of each disk |

Implementations: `qemudriver.Driver` (real host), `remote.Driver` (another host), `fake.Driver` (tests).
`node/mock` is a gomock of the interface.

### Node API

Protocol: Connect, service `attic.node.v1.NodeService`, source `api/proto/attic/node/v1/node.proto`. Mounted by
`nodeserver.New(driver, verifier)` at the path it returns. Each RPC is a Connect unary call with a bearer token.
Every RPC requires the `node.manage` permission, which only the `node` role grants. The token must come from a
trusted key that is restricted to `node` (vmd: `--trusted-key=vmd=/etc/attic/vmd.pub:node`). A missing or invalid
token gives `unauthenticated`. Errors from the driver are converted with `errs.ToConnect`: sentinel errors keep
their code, and any other error is `internal` with its text hidden.

A daemon serves one host and has no node ID in its messages: vmd keeps the mapping from node name to address.

| RPC | Request fields | Response fields | Errors (Connect code) | Semantics |
| --- | --- | --- | --- | --- |
| `GetCapacity` | none | `capacity`: `cpus_total`, `memory_mb_total`, `memory_mb_available`, `disk_bytes_total`, `disk_bytes_free` | `internal` if `/proc/meminfo` or `statfs` fails | Threads from the runtime; RAM from `MemTotal` and `MemAvailable`; disk from the file system holding the data directory, free space for an unprivileged user |
| `RegisterImage` | `image_id`, `path` | `size_bytes` (virtual size) | `invalid_argument` bad ID, relative path, or not qcow2; `not_found` file missing; `already_exists` ID taken | Creates `images/<id>.qcow2` as a symlink to `path`. The file is not copied or chowned |
| `DeleteImage` | `image_id` | none | `not_found` | Removes the link or flattened file. A registered file is left in place |
| `CreateDisk` | `disk_id`, `backing_image_id`, `size_gb` | none | `invalid_argument` size below 1, or disk smaller than its image; `not_found` backing image missing; `already_exists`; `unavailable` `qemu-img` failed | Creates `disks/<id>.qcow2`. With a backing image it is a copy-on-write overlay (`-F qcow2`). Built as a temporary file, then renamed |
| `ResizeDisk` | `disk_id`, `size_gb` | none | `invalid_argument`; `not_found`; `failed_precondition` shrink, or a live VM uses the disk; `unavailable` | Grow only. The same size is a no-op |
| `DeleteDisk` | `disk_id` | none | `not_found`; `failed_precondition` a live VM uses the disk | Removes the file |
| `SnapshotDisk` | `disk_id`, `image_id` | `size_bytes` (virtual size) | `not_found` disk; `already_exists` image; `failed_precondition` disk in use; `unavailable` | Flattens the disk into `images/<image_id>.qcow2` (a regular file). Temporary file, then rename |
| `StartVM` | `vm_id`, `name`, `cpus`, `memory_mb`, `disk_ids` (first is boot) | `runtime` | `invalid_argument` bad ID, name not `^[a-zA-Z0-9._-]+$`, CPUs below 1, memory below 256, no disks, a disk listed twice; `not_found` a disk file missing; `unavailable` seed ISO or QEMU could not be started | If the VM is already alive, returns its current runtime. Otherwise writes spec and a new cloud-init seed, reserves a loopback SSH port, starts QEMU, and returns `BOOTING` |
| `StopVM` | `vm_id`, `force` | none | `invalid_argument` bad ID | Graceful: records the request and asks the guest to `sudo poweroff` (10 s, best effort). Forced: SIGKILL at once, reason `killed`. A VM that is not alive is a no-op |
| `GetVMRuntime` | `vm_id` | `runtime`: `state`, `reason` | `invalid_argument` bad ID | The observation step; see [Behaviour](#behaviour-and-guarantees). May probe SSH, run the one-time mount, and kill an overdue stop |
| `ForgetVM` | `vm_id` | none | `failed_precondition` process still alive | Removes `vms/<id>` (spec, runtime, seed, logs). Disks are kept. Forgetting an unknown VM is a no-op |
| `StartJob` | `vm_id`, `spec`: `job_id`, `command`, `workdir`, `timeout_seconds` | none | `failed_precondition` VM not RUNNING; `invalid_argument` bad job ID; `already_exists` job ID exists in the guest | Launches the command detached in the guest (see [Jobs](#jobs-in-the-guest)). Returns once it is running |
| `PollJob` | `vm_id`, `job_id`, `stdout_offset`, `stderr_offset`, `max_output`, `tail` | `state` (`MISSING`, `RUNNING`, `EXITED`, `LOST`), `exit_code` (optional), `cancelled`, `elapsed_seconds`, `stdout`, `stderr`, `stdout_from`, `stderr_from`, `stdout_offset`, `stderr_offset`, `more`, `truncated` | `invalid_argument` negative offset or `max_output`, bad job ID | A VM that is not RUNNING gives `MISSING`. `max_output` defaults to 8192 and is capped at 65536 per stream. `tail` returns the last bytes. `more` means output remains past the returned bytes |
| `CancelJob` | `vm_id`, `job_id` | none | `invalid_argument` bad job ID | Kills the job's process group. A finished job, or a VM that is not RUNNING, is a no-op |
| `PutFile` | `vm_id`, `path`, `content`, `mode` | none | `failed_precondition` VM not RUNNING; `invalid_argument` empty path; `unavailable` guest failed | Writes atomically (temporary file in the same directory, `chmod`, rename). Creates parent directories. Mode 0 means 0644 |
| `GetFile` | `vm_id`, `path`, `max_bytes` | `content`, `truncated` | `failed_precondition` VM not RUNNING; `not_found` no regular file at `path`; `invalid_argument` empty path | `max_bytes` defaults to and is capped at 8 MiB. `truncated` is set when the file is longer |
| `GetVMMetrics` | `vm_id`, `disk_ids` (in attach order: first is `vda`) | `cpu_percent`, `cpu_cores`, `load1/5/15`, `mem_total_bytes`, `mem_used_bytes`, `mem_available_bytes`, `swap_total_bytes`, `swap_used_bytes`, `uptime_seconds`, `filesystems[]` (`disk_id`, `device`, `mount_point`, `size_bytes`, `used_bytes`), `disks[]` (`disk_id`, `virtual_size_bytes`, `allocated_bytes`) | `failed_precondition` VM not RUNNING; `not_found` a listed disk file missing; `unavailable` the guest did not answer or the script failed; `invalid_argument` bad disk ID | Samples the guest once (about 1 s, the CPU delta) within 30 s. `cpu_percent` is like `top`: 100 per busy vCPU. Allocation is the qcow2 `actual-size` on the host |

`runtime` is `state` (`ABSENT`, `BOOTING`, `RUNNING`, `EXITED`) and a `reason` string. The reasons vmd can show
include `slow boot`, `stopped`, `killed`, `stop timed out, killed`, and `QEMU exited unexpectedly: <last qemu.log
lines>`.

### Exported Go API

| Package | Exported identifiers | Notes |
| --- | --- | --- |
| `node` | `Driver` (interface, 17 methods) | The contract above |
| `node/mock` | `MockDriver`, `NewMockDriver` | Generated by `make mocks` |
| `qemu` | `VMSpec` (`Name`, `CPUs`, `MemoryMB`, `DiskPaths`, `SeedISO`, `SSHPort`, `ConsolePath`, `SharedDir`, `KVM`), `(VMSpec).Validate`, `Binary`, `Args`, `UserData(key, packages)`, `MetaData(instanceID, hostname)`, `ImageInfo`, `ParseImageInfo`, `ImgCreateArgs`, `ImgResizeArgs`, `ImgConvertArgs` | Pure: returns argv and text, runs nothing. `Validate` requires SSH port 1024..65535 and memory of at least 256 MB |
| `qemudriver` | `Config`, `New(Config) (*Driver, error)`, `Driver` (implements `node.Driver`), `Commander` (`Run`, `Start`), `Procs` (`Alive`, `Signal`), `KeySource` (`PublicKeyLine`), `LockDownNetwork(ctx, cmd, runAsUser, allowDNS...)`, `OSCommander()` | `New` creates the directory layout, chowns `shared/` to `RunAs`, and adopts live VMs |
| `guestexec` | `Runner` (`Run(ctx, command, stdin)`), `Spec` (`JobID`, `Command`, `Workdir`, `TimeoutSeconds`), `Exec` (fields `R`, `JobsDir`; methods `Start`, `Poll`, `Cancel`, `PutFile`, `GetFile`) | Guest job protocol. `JobsDir` defaults to `/var/tmp/mcp-jobs` |
| `guestmetrics` | `Script` (const, bash), `Sample`, `Filesystem`, `Parse(output, diskIDs)` | `Parse` names the section that is missing or malformed |
| `sshrunner` | `Config` (`Addr`, `User`, `Signer`, `HostKeyCallback`, `DialTimeout`, `MaxOutput`), `Runner` (implements `guestexec.Runner`), `New(Config)`, `LoadSigner(path)`, `PublicKeyLine(signer)`, `GenerateKey(path)` | Dials per call. Default user `agent`, dial timeout 5 s, output cap 16 MiB |
| `remote` | `Driver` (implements `node.Driver`), `New(httpClient, baseURL, tokens, opts...)` | Errors come back as `errs` sentinels |
| `nodeserver` | `New(driver, verifier, opts...) (path, http.Handler)` | Requires `node.manage` on every call |
| `fake` | `Config` (`CPUs`, `MemoryMB`, `DiskBytesTotal`, `DiskBytesFree`, `BootPolls`), `Driver` (fields `Exec`, `Now`; implements `node.Driver`), `New(Config)`, methods `Crash`, `SetDown`, `FailNext`, `Calls`, `SetMetrics` | In memory. `Exec` runs under the driver lock and must not call back into it |

## Behaviour and guarantees

### VM runtime

The node has no desired state. `StartVM` and `StopVM` record a request, and `GetVMRuntime` reports what the
process looks like now. Each call computes the runtime in this order:

1. No `runtime.json` for the VM: `ABSENT`.
2. The recorded PID is not a live `qemu-system-x86_64` process: `EXITED`. The reason is the stop reason
   (`stopped` after a requested stop), or `QEMU exited unexpectedly: <last 3 lines of qemu.log>`.
3. A kill was recorded (`killed` or `stop timed out, killed`): `EXITED` with that reason.
4. A graceful stop is older than `StopGrace` (60 s): SIGKILL, record `stop timed out, killed`, return `EXITED`.
5. Otherwise an SSH probe (`true`, 3 s timeout; cached for 1 s per PID). A failed probe is `BOOTING`, with reason
   `slow boot` once the VM is more than `BootTimeout` (5 min) old.
6. Reachable: run the shared-folder mount once per boot, and return `RUNNING`. A failed mount is retried on a later
   call, and the VM is reported `RUNNING` in the meantime.

The driver runs no background goroutine. A stop that has overrun `StopGrace` is killed only when the next
`GetVMRuntime` (or any call that observes the VM) happens. A guest that never answers stays `BOOTING` until it is
stopped; the driver does not kill it by itself.

Starting: a new cloud-init instance ID (the VM ID plus a random 4-byte nonce) is written on every start, so
cloud-init runs again on each boot. The disks are attached in order: `vda` is the first. A start holds the VM's
lock and the locks of its disks, so no disk can be deleted between the check and the boot.

`ForgetVM` refuses while the process is alive. `ResizeDisk`, `DeleteDisk` and `SnapshotDisk` refuse while a live
VM has the disk in its `spec.json`.

### QEMU command line

`qemu.Args` produces, in order:

- Acceleration: `-enable-kvm -cpu host` with KVM, or `-accel tcg -cpu max` without.
- `-machine q35 -smp <cpus> -m <memory>M -display none`.
- Serial console to `console.log`, or `-serial none`.
- One `virtio` qcow2 drive per disk (`vda`, `vdb`, ...), then the seed ISO as a read-only raw drive.
- User-mode networking, IPv6 off, with the only inbound path `hostfwd=tcp:127.0.0.1:<port>-:22`.
- Optional 9p share: `-fsdev local,...,security_model=mapped-xattr` with mount tag `shared`.

`optValue` doubles commas in paths. Paths beginning with `-` are rejected for `qemu-img`.

### Guest provisioning

- cloud-init (`UserData`): creates user `agent` with bash, passwordless `sudo`, the node's SSH public key, and
  `ssh_pwauth: false`. Optional `GuestPackages` are installed with `package_update`. vmd does not set them. The
  key must be one printable line with a key type and body, and must not contain quotes or backslashes.
- `MetaData`: `instance-id` and `local-hostname`. Both must match `^[a-zA-Z0-9._-]+$`.
- The guest must run sshd on port 22, support 9p, and have the commands listed in
  [Guest requirements](#guest-requirements).
- Mount (`mountScript`): mounts tag `shared` at `/shared` with `trans=virtio,version=9p2000.L`, adds a `nofail`
  fstab line, and chowns `/shared` to `agent`.

### Jobs in the guest

`guestexec` keeps each job in a directory `<JobsDir>/<job id>/` (default `/var/tmp/mcp-jobs`) with `cmd`,
`started`, `pgid`, `stdout`, `stderr`, `exit_code` and `cancelled`.

- Start: validates the ID, creates the directory with `mkdir` (an existing directory is `already_exists`), stores
  the command from stdin, creates the workdir when missing, and launches `setsid bash -c` which runs
  `timeout -k 2 <seconds> bash -lc "$(cat cmd)"`. Output goes to files, so no pipe keeps the job alive. Default
  workdir: `$HOME` of the SSH user. Timeout below 1 becomes 1 s. The launch also deletes all but the newest 50
  finished job directories.
- Exit code: written with `mv -n` once the command ends. `124` is a timeout. Cancel writes `130`.
- Poll: a job is `lost` when its `pgid` is recorded, there is no `exit_code`, and `/proc/<pgid>` is gone or a
  zombie. A missing directory is `missing`. The reply is one header line (state, exit code, cancelled flag,
  elapsed seconds, sizes, offsets) and two base64 chunks.
- Cancel: waits up to 2 s for the `pgid`, sends TERM to the session, waits up to 2 s, sends KILL, and records
  exit `130` if no exit code exists yet. Cancelling a finished job does nothing.
- `PutFile`: a temporary file next to the target, `chmod`, then `mv -f`. `GetFile`: `head -c` on a regular file;
  exit 3 means not a regular file.

### Guest metrics

`guestmetrics.Script` reads `/proc/stat` twice with a one-second sleep between, then `/proc/meminfo`,
`/proc/loadavg`, `/proc/uptime`, `nproc`, and `df -B1` (without tmpfs, devtmpfs, squashfs or overlay). `Parse`
attributes a filesystem to a disk by device name: `/dev/vda*` is the first disk ID passed in, `/dev/vdb*` the
second, and so on. An out-of-range device has an empty `disk_id`.

### Host capacity

`GetCapacity` reports: CPUs as `runtime.NumCPU()` (threads), RAM from `/proc/meminfo` (`MemTotal`,
`MemAvailable`, in MiB), and disk through `statfs` on the data directory. vmd applies overcommit and reserve on top.

### Node-side concurrency

- Each VM has a mutex that guards its `runtime.json`, probe cache and mount flag. It is not held across an SSH
  probe or a guest command.
- Each image and disk ID has its own lock. Lock order: VM, then disks (sorted), then image.
- `writeJSON` writes a temporary file and renames it, so a reader never sees a partial `runtime.json`.
- Host commands run through `Commander`. `OSCommander` uses `os/exec`; QEMU is started with `Setsid` and its
  output appended to `qemu.log`. A goroutine reaps the child.

### Adoption on restart

`New` scans `vms/` and reserves the SSH port of every VM whose process is alive, so a new VM cannot take it. Each
QEMU process runs in its own session, so it keeps running while the daemon is down.
Liveness is checked through `/proc/<pid>/stat` (not a zombie) and `/proc/<pid>/cmdline` (contains
`qemu-system-x86_64`).

## Configuration

### Host requirements

What a Linux host needs before `vmd --role=node` (or `--role=all`) can run guests. Package names are Debian's, as
used by [deploy/Dockerfile.node](../../deploy/Dockerfile.node). The driver does not install anything.

| Requirement | Used for | Notes |
| --- | --- | --- |
| Linux on x86_64 | Guests run `qemu-system-x86_64` | The binary name is fixed in `qemu/args.go` (`Binary`). |
| `qemu-system-x86_64` (Debian `qemu-system-x86`) | Runs each guest | Started as the `-run-as` user. |
| `qemu-img` (Debian `qemu-utils`) | Creates, resizes and converts disks and images; reads image metadata | Called with `info`, `create`, `resize` and `convert`. |
| `genisoimage` (Debian `genisoimage`) | Builds the cloud-init seed ISO at every VM start | Missing, every start fails with `node unreachable`. |
| `iptables` and `ip6tables` (Debian `iptables`) | Guest network lockdown | Not needed with `-no-firewall`. |
| `/dev/kvm` | Hardware acceleration with `-kvm=true`, the default | Without it QEMU fails to start. `-kvm=false` uses TCG, which is much slower. When `/dev/kvm` exists, the `-run-as` user is also put in its group. |
| Root, or `CAP_NET_ADMIN` plus `chown` rights | Installs the firewall rules; hands `shared/`, VM directories and disks to `-run-as` | Start-up fails if the firewall cannot be installed. |
| A user named by `-run-as` (default `qemu`) | Owns the shared folder, VM directories and disks; runs QEMU | The lookup fails if the user does not exist. Use `-run-as=` only for development. Debian: `useradd --system qemu`. |
| Images readable by the `-run-as` user | Images are not chowned, so the user must be able to read them | Put them where that user can read them. |
| Absolute `-data-dir` (default `/var/lib/attic`) | All state; see [On-disk layout](#on-disk-layout) | Created with mode 0755. |
| Free disk on the data directory | Disks are qcow2 files sized at creation | No overcommit: a new disk needs its size free. |
| `-ssh-key` file | The control plane's key for guests | Created with mode 0600 when missing; the public half goes to `<path>.pub` (0644) and is injected into each guest. The SSH client is in-process; no `ssh` binary is run. |
| Private network or TLS for the node API | The node API accepts `node`-role tokens only | `-tls-cert` and `-tls-key` on `vmd --role=node`, or a private network. |

`deploy/Dockerfile.node` installs `qemu-system-x86`, `qemu-utils`, `genisoimage`, `openssh-client` and `iptables`
on Debian stable and creates the `qemu` user. The container needs `--privileged` and `/dev/kvm`. The Dockerfile
installs `openssh-client`, but the driver does not use it.

`qemudriver.Config` (set by `services/vmd/cmd/vmd`; the flags that feed it are listed in the
[vmd README](../../services/vmd/README.md#configuration)):

| Field | Default | Meaning |
| --- | --- | --- |
| `NodeID` | none | Name of the host. Informational |
| `DataDir` | required, absolute | Root of all state |
| `SharedDir` | `<DataDir>/shared` | Exported to guests at `/shared` |
| `SSHKeyPath` | none | Private key; the public key is read from `<path>.pub` unless `Keys` is set |
| `Keys` | from `SSHKeyPath` | Source of the public key line injected into guests |
| `KVM` | `false` as a zero value | Hardware acceleration. vmd sets it from `-kvm` (default true) |
| `RunAs` | empty (current user) | User that QEMU runs as, and that owns `shared/`, VM directories and disks |
| `GuestPackages` | none | Packages cloud-init installs |
| `StopGrace` | 60 s | Time for a graceful power-off before SIGKILL |
| `BootTimeout` | 5 min | After this, a guest that still does not answer SSH is reported `slow boot` |
| `Cmd` | `OSCommander()` | Host command runner |
| `Procs` | `/proc` and `kill(2)` | Process inspection and signals |
| `NewRunner` | required | Returns the guest runner for an SSH port |
| `FS` | the real host | Host facts: `/proc/meminfo`, `statfs`, CPU count, a free loopback port |
| `Now` | `time.Now` | Clock |

Fixed values: probe timeout 3 s, probe cache 1 s, mount timeout 30 s, power-off request 10 s, metrics timeout
30 s, `qemu.log` tail 3 lines, minimum guest SSH port 1024.

Host commands the driver runs: `qemu-img info --output=json`, `qemu-img create -q -f qcow2 [-b <base> -F qcow2]
<path> [<n>G]`, `qemu-img resize`, `qemu-img convert -q -O qcow2`, `genisoimage -quiet -output ... -volid cidata
-joliet -rock`, `chown`, `iptables` and `ip6tables` (`-nL`, `-N`, `-F`, `-A`, `-C`), and `qemu-system-x86_64`.

## Data

### On-disk layout

```
<data-dir>/                      absolute, created 0755 by New
  images/<id>.qcow2              symlink to a registered file, or a flattened snapshot (regular file)
  disks/<id>.qcow2               VM disks, optionally overlays on an image
  shared/                        mounted at /shared in every guest; owned by RunAs
  vms/<id>/spec.json             name, CPUs, memory, disk IDs as started
  vms/<id>/runtime.json          pid, SSH port, started_at, stop_requested_at, mounted, kill_reason
  vms/<id>/user-data, meta-data  cloud-init documents of the last start
  vms/<id>/seed.iso              cloud-init NoCloud seed (cidata)
  vms/<id>/console.log           serial console
  vms/<id>/qemu.log              QEMU stdout and stderr
```

Temporary files `.<id>.tmp.qcow2` and `.tmp-*` are written beside the final name and renamed; a failed create
removes them. A registered image is outside `<data-dir>` when the symlink points elsewhere, so backups must follow
it (see the vmd README).

### Guest layout

- `/var/tmp/mcp-jobs/<job id>/`: job files, described above.
- `/shared`: the 9p share, owned by `agent`.
- The guest user `agent`, with home directory `$HOME` used as the default job workdir.

### Permissions the driver sets

- `chown RunAs` on `shared/`, on each VM directory at start, and on each disk file at start. Images are not
  chowned; the RunAs user must be able to read them.
- The SSH private key is created with mode 0600 and its public half with 0644 (only when missing).

## Security model

- Unprivileged QEMU: with `RunAs` set, QEMU is started with that user's UID and GID, and the group of `/dev/kvm`
  as a supplementary group. The daemon itself needs root or `CAP_NET_ADMIN`, and `chown` rights, to do this.
  `RunAs` empty runs QEMU as the daemon's user, which is for development only.
- Guest network lockdown (`LockDownNetwork`): in both `iptables` and `ip6tables`, a chain `SBX` is filled and
  jumped to from `OUTPUT` for NEW connections of the RunAs UID. Accepts for `allow-dns` resolvers come first, then
  rejects for `10/8`, `172.16/12`, `192.168/16`, `169.254/16`, `100.64/10` and `127/8`, and for `::1/128`,
  `fc00::/7` and `fe80::/10`. The chain is rebuilt on each start of the daemon. Failure is fatal (fail closed)
  unless the daemon runs with `-no-firewall`. The rules are rebuilt by flushing the chain and refilling it, so for a
  moment the chain is empty and allows traffic. Call it only at start-up, before any VM runs (the code's comment
  says the same).
- Ingress: the only inbound path to a guest is the loopback SSH forward (`127.0.0.1`).
- SSH: public-key authentication only. The guest's host key is not verified (`InsecureIgnoreHostKey`), because the
  connection goes to the local forward of a VM this process started. A caller that reaches a guest over a network
  must supply a verifying callback; `sshrunner.Config.HostKeyCallback` allows that.
- Key material: the driver injects only the public key. The private key lives at `SSHKeyPath` and is not sent
  anywhere.
- IDs are validated (`^[a-zA-Z0-9_-]{1,64}$`) before they become file names. Commands sent to the guest are passed
  as shell-quoted arguments, and job IDs are validated again in the guest scripts.
- Guest commands run as `agent`, which has passwordless `sudo` (set by cloud-init). Anything a job runs can become
  root inside its guest. The guest is the isolation boundary for jobs; it is not a boundary for the host.
- Node API: bearer tokens with role `node` only. Put the node API on TLS or a private network (vmd `--tls-cert`,
  `--tls-key`); this package does not add TLS.

## Observability

This package exports no metrics and writes no logs of its own. The `vmd` process reports:

- Node capacity and VM state through vmd's metrics (see the vmd README).
- Failed driver calls as `reconcile failed` and `node unreachable` in vmd.
- Start failures, QEMU exits and their last `qemu.log` lines in the VM's `state_reason`.
- Full details of a VM's process in `vms/<id>/qemu.log` and `console.log` on the host.

## Failure modes and recovery

| Failure | What the driver does | What an operator does |
| --- | --- | --- |
| `qemu-img` or `genisoimage` missing, or fails | Start or disk step returns `unavailable`. vmd reports `node unreachable` and retries with backoff | Install the tool; retries resume |
| `qemu-system-x86_64` fails to start | `unavailable`; the VM's files stay on disk | Read `qemu.log`, check KVM and the RunAs user |
| QEMU exits | Observed as `EXITED` with the last three `qemu.log` lines; vmd restarts it | Read `state_reason` and `qemu.log` |
| Guest never answers SSH | `BOOTING`, reason `slow boot` after 5 min; no kill | Stop the VM (`StopVM` with `force` if needed) |
| Graceful stop does not finish | Killed after 60 s, reason `stop timed out, killed`; the kill happens on the next observation | Nothing, unless the stop is urgent: use `force` |
| Mount fails in the guest | VM reported `RUNNING` and mount retried on each observation | Check sudo, the 9p device and `/etc/fstab` in the guest |
| Node daemon restarts | Live VMs are re-adopted; their SSH ports are reserved again | Check that `RunAs` and `DataDir` did not change |
| Host reboots | Every VM is `EXITED` (`QEMU exited unexpectedly`), and vmd restarts them | Stop VMs before a planned reboot |
| Firewall rules cannot be installed | The daemon refuses to start | Run as root, or with `CAP_NET_ADMIN`, or `-no-firewall` for development only |
| Disk in use by a VM | `ResizeDisk`, `DeleteDisk`, `SnapshotDisk` return `failed_precondition` | Stop the VM first |
| Job's process group survives cancel | TERM then KILL after about 2 s; exit `130` recorded | Check for processes that leave the group (`setsid`) |
| A job's directory was pruned | Poll reports `MISSING`; vmd marks the job `LOST` | Expected after 50 newer finished jobs |

## Running and testing

Run from the repository root, or from `libs/qemu`.

```
cd libs/qemu && go build ./...
cd libs/qemu && go vet ./...
cd libs/qemu && go test -race -count=1 ./...
make mocks            # regenerate node/mock (from the repository root)
make test             # all modules, including this one
```

Test coverage by package:

| Package | Test files | Basis |
| --- | --- | --- |
| `qemudriver` | `driver_test.go`, `disks_test.go`, `firewall_test.go`, `metrics_test.go`, `fakes_test.go` | A recording fake `Commander` and `Procs`, a fake guest runner, a fake node (no real `qemu-img`, `iptables` or QEMU). Firewall tests check the recorded commands; they do not run `iptables` |
| `qemudriver` (real) | `real_qemuimg_test.go` | Real `qemu-img`: overlay and resize. Skipped without it |
| `qemudriver` (real) | `real_qemu_test.go` | Real `qemu-system-x86_64` with KVM: the generated command line is accepted; the cloud-init seed is a real ISO labelled `cidata` (built by `genisoimage` or `xorriso`); the SSH forward listens on loopback only; a forced stop kills the process. Skipped without QEMU, `/dev/kvm` or an ISO builder |
| `qemu` | `args_test.go` | Pure builders |
| `guestexec` | `guestexec_test.go` | Scripts run with local `bash` through a runner |
| `guestmetrics` | `guestmetrics_test.go` | `Parse` with recorded output |
| `sshrunner` | `sshrunner_test.go` | An in-process SSH server |
| `remote`, `nodeserver` | `remote_test.go`, `nodeserver_test.go` | Round trip through Connect, tokens and error mapping |
| `fake` | `fake_test.go` | The fake's own behaviour |

What the real-host tests do not prove: see [Not verified on real hardware](#not-verified-on-real-hardware).

## Not verified on real hardware

No guest has been booted by this code. The real QEMU test starts a VM with no operating system on its disk, so
the guest never answers SSH and the VM stays `BOOTING`. What that test checks is the host side of a boot: the
command line, the SSH forward, the seed ISO and the stop path.

Not verified in this repository:

- A real guest image: cloud-init user creation, `sudo` for `agent`, and the SSH server on port 22.
- The guest scripts (`guestexec`, `guestmetrics`, the mount script) running inside a guest rather than a local shell.
- `sshrunner` against a real `sshd`, and graceful power-off timing (`sudo poweroff` and the 60 s grace).
- The 9p share and `/shared` mount in a guest.
- `iptables` and `ip6tables` rules on a live host: the firewall tests record commands only.
- Image provisioning, and `GuestPackages` installation.
- The `docker compose` stack and Dockerfiles (not built here).

### Guest requirements

A guest image must be a qcow2 file with a Linux that:

- runs sshd on port 22 and accepts cloud-init (NoCloud, ISO labelled `cidata`);
- has `sudo` for user `agent` without a password;
- supports 9p (`virtio`, `9p2000.L`) for the `shared` tag;
- has in the default path `bash`, `setsid`, `timeout`, `base64`, `stat`, `tail`, `head`, `cat`, `mkdir`, `mv`,
  `kill`, `pkill`, `pgrep`, `seq`, `sleep`, `mktemp`, `mountpoint`, `date`, `chmod`, `df`, `nproc`, `dirname`,
  `grep` and `poweroff` (the scripts call them directly).

## Layout

```
node/                 Driver interface (driver.go); mock/ (gomock)
qemu/                 pure builders: VMSpec and Args, cloud-init, qemu-img arguments, image info
qemudriver/           the real Driver for a Linux host
  driver.go           Config, New, adoption, per-VM and per-disk locks, guest runner factory
  vm.go               StartVM, StopVM, GetVMRuntime, ForgetVM, observation, probe, mount, seed
  disks.go            images, disks, snapshots (qemu-img)
  jobs.go             StartJob, PollJob, CancelJob, PutFile, GetFile on the driver
  metrics.go          GetCapacity, GetVMMetrics
  firewall.go         LockDownNetwork (SBX chain, iptables and ip6tables)
  system.go           host commands, processes, /proc, statfs, free port (OSCommander and friends)
  export.go           OSCommander()
guestexec/            guest job protocol (launch, poll, cancel, files) as shell scripts
guestmetrics/         guest sampling script and parser
sshrunner/            SSH runner for guests; key generation and loading
remote/               node.Driver over the node API (client)
nodeserver/           node API server (Connect handler for any node.Driver)
fake/                 in-memory node.Driver for tests
```

## Related

- [vmd](../../services/vmd/README.md): the only caller; its contract for placement, reconciliation and the
  `state_reason` texts.
- [API definitions](../../api/README.md): `api/proto/attic/node/v1/node.proto` and the generated code.
- [libs/authz](../../libs/authz/README.md): tokens, trusted keys and the `node` role.
- [libs/common](../../libs/common/README.md): `errs` sentinels used by every driver.
- [ADR 0003: jobs live in the guest](../../docs/adr/0003-jobs-live-in-the-guest.md).
