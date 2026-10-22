# services/vmd/cli

`services/vmd/cli` is the `vm` command group of the `attic` command-line tool. It lists the VMs of a project and copies
files to and from a VM through vmd's `VMService` and `ExecService`. It is a module nested inside
[services/vmd](../README.md), and it does not import vmd's code: it talks to vmd over the same HTTP API as any client.

Module: `github.com/rlsvarinskis/cloud-at-home/services/vmd/cli` (package `vmcli`)

## Role

- `attic vm list`: one line per VM of the project.
- `attic vm cp SRC DST`: copy a file to a VM (upload) or from a VM (download), with the 8 MiB limit enforced on both
  sides.
- Resolves a VM name to its id in the project. Names must be unique in a project for `cp`.
- Does not manage VMs (create, start, stop, delete) and does not run commands in a guest. Those are other tools' jobs.

## Position

```
attic (cmd/attic) ── vm group ──▶ services/vmd/cli ──Connect──▶ vmd  VMService.ListVMs
                                        │                         ExecService.PutFile / GetFile
                                        └── libs/cli (Env, flags, bearer token, RPCError)
```

- Imports `api` (the generated `vmdv1` messages and `vmdv1connect` clients), `libs/cli`, and `connectrpc.com/connect`.
- It must not import `services/vmd` (the service module itself). `scripts/check-boundaries.sh` enforces this.
- Imported by [cmd/attic](../../../cmd/attic/README.md), which registers `Group()`.

## Contract

### Usage

```
attic vm list
attic vm cp [--mode OCTAL] SRC DST
attic vm help
```

Global flags go before `vm`, for example `attic --project demo vm list`. See
[libs/cli](../../../libs/cli/README.md#global-settings) for the flags and environment variables.

### `vm list`

Prints one line per VM in the project: name, state, CPUs and memory in GiB, separated by spaces. There is no header.

```
$ attic vm list
alpha    running  2  1.5
beta     stopped  1  0.5
```

The state is the lower-case word of the VM state: `pending`, `running`, `stopped`, `starting`, `stopping`, `error` or
`deleting`. Memory is shown with up to three decimals. The output goes to stdout and nothing goes to stderr.

### `vm cp`

```
attic vm cp [--mode OCTAL] SRC DST
```

Exactly one of `SRC` and `DST` is a VM path, written `NAME:/absolute/path`. The other is a local path, or `-`.

| Operand | Meaning |
| --- | --- |
| `NAME:/abs/path` | A file inside the VM. `NAME` is a VM name in the project. The path must be absolute. |
| `path` | A local file. A local path that contains a colon needs a leading `./`, for example `./notes:v1`. |
| `-` | Standard input when it is `SRC`, standard output when it is `DST`. |

| Flag | Meaning |
| --- | --- |
| `--mode OCTAL` | Permission bits of an uploaded file, in octal, for example `0600`. Default: the local file's permission bits, or `0644` for stdin. Upload only: with a download it is a usage error. |

Behaviour:

- **Upload** reads the local file (or stdin) and sends it with `PutFile`. A file larger than 8 MiB is refused before
  any call is made. Stdin is refused as soon as it exceeds 8 MiB.
- **Download** asks `GetFile` for at most 8 MiB. If the file is larger, vmd reports it as truncated, and the command
  fails with `... is larger than 8 MiB; nothing was written`. Nothing is written in that case.
- **Local writes are atomic.** A download is written to a temporary file in the destination's directory and renamed into
  place. A failed download leaves no partial file. An existing destination is replaced only by a complete download.
- A download writes the file with mode `0644`, because `GetFile` does not return the file's mode.
- Stdout carries the data only. The confirmation goes to stderr: `wrote 12 bytes to alpha:/root/in.txt` for an upload,
  `read 12 bytes from alpha:/root/in.txt` for a download.

### Errors

Server errors are shown as `code: message`, the Connect code and the server's message:

```
attic vm cp: not_found: no such file
attic vm list: permission_denied: no role in project demo
```

Client-side refusals name the operand and the reason, for example `no VM named "alpha" in project "demo"`, or `2 VMs
are named "alpha" in project "demo" (ids ...)`.

Exit codes follow the hub: 2 for a wrong command line, 1 for everything else. See
[libs/cli](../../../libs/cli/README.md#exit-codes).

## Configuration

Only the global settings of `attic` (vmd URL, project, token). A command with no token fails before it calls vmd.

## Known limits

- **8 MiB per file, in both directions.** This is vmd's limit for `PutFile` content and `GetFile` replies. Larger files
  are not transferred in pieces by this command.
- A download does not overwrite a directory, and it replaces an existing file without asking.
- VM names are matched exactly and must be unique within the project. `cp` refuses an ambiguous name.
- A download's file mode is always `0644`; the remote mode is not read.
- Flags must come before the operands: `attic vm cp --mode 0600 SRC DST`.
- No recursive copy, no progress output and no resumption.

## Running and testing

```
cd services/vmd/cli && go test -race -count=1 ./...
```

The tests run against `vmclitest`, a fake vmd that serves `VMService` and `ExecService` over an `httptest` server with
the generated Connect handlers. They cover:

- name resolution with no match, one match, several matches and a match on a later page;
- the upload and download round trip, the file mode, and the stdin and stdout forms;
- the 8 MiB limit in both directions, including exactly 8 MiB (allowed) and one byte more (refused);
- a truncated download that leaves no file and keeps an existing file unchanged;
- the operand rules (a relative remote path, two remote or two local operands, `--mode` on a download), which must make
  no call at all;
- server errors shown as `code: message`, and a missing token that makes no call.

`vmclitest` is also used by [cmd/attic](../../../cmd/attic/README.md) for its end-to-end tests. It is test support only.

## Examples

Documentation addresses are used throughout (vmd at `192.0.2.10`):

```
export ATTIC_VMD_URL=http://192.0.2.10:8080
export ATTIC_TOKEN_FILE=$HOME/.config/attic/token   # or ATTIC_TOKEN, or ~/.config/attic/token

attic vm list
attic vm cp ./deploy.sh alpha:/root/deploy.sh          # upload, mode from the local file
attic vm cp --mode 0600 ./id.txt alpha:/root/id.txt    # upload with an explicit mode
attic vm cp alpha:/var/log/app.log ./app.log           # download
cat notes.txt | attic vm cp - alpha:/root/notes.txt    # upload from stdin
attic vm cp alpha:/root/notes.txt - | wc -c            # download to stdout
attic -p staging vm list                               # another project
```
