# attic

`attic` is the Attic Cloud command-line tool for users and operators. It is a hub: it parses the global flags and
dispatches to command groups. Each group lives next to the service it talks to, in its own module. Today there is one
group, `vm`, from [services/vmd/cli](../../services/vmd/cli/README.md). Agents will get `attic agent ...` the same way,
from `services/agentd/cli`, without changes to the hub's shared code.

Module: `github.com/rlsvarinskis/cloud-at-home/cmd/attic`

For the administrative tasks that work on local files (keys, tokens, roles), use [atticctl](../atticctl/README.md).
`attic` talks to a running vmd.

## Role

- Parses the global flags (`--vmd-url`, `-p`/`--project`, `--token-file`) and resolves the configuration (see
  [libs/cli](../../libs/cli/README.md#global-settings)).
- Dispatches `attic <group> <command>` to the group's commands, and prints help: `attic help`, `attic <group> help`,
  `-h` on any command.
- Prints its version with `attic version`. The version is set at build time.
- Does not implement any command itself. Adding a group is one import and one entry in `newApp`.

## Position

```
cmd/attic (main)
  ├── imports libs/cli                 the toolkit: dispatcher, help, settings, bearer token
  └── imports services/vmd/cli         group "vm": list, cp
                                         └── talks to vmd over Connect
```

- Imports `libs/cli` and `services/*/cli` modules only (`scripts/check-boundaries.sh`). It does not import a service's
  code, and it does not import `api`.
- The groups are imported explicitly. There is no `init()` registration and no reflection.

## Contract

### Usage

```
attic [global flags] <group> <command> [arguments]
attic help | attic version

Groups:
  vm       virtual machines: list them, copy files in and out

Global flags (a flag overrides its environment variable, which overrides the default):
  --vmd-url URL       vmd base URL (ATTIC_VMD_URL; default http://127.0.0.1:8080)
  -p, --project NAME  project (ATTIC_PROJECT; default "default")
  --token-file PATH   file holding the API token (ATTIC_TOKEN_FILE, then ATTIC_TOKEN, then ~/.config/attic/token)
```

Global flags go before the group name. A command's flags go after its name.

### Commands

| Command | What it does | Details |
| --- | --- | --- |
| `attic vm list` | One line per VM of the project: name, state, CPUs, memory in GiB | [services/vmd/cli](../../services/vmd/cli/README.md#vm-list) |
| `attic vm cp [--mode OCTAL] SRC DST` | Copy a file to or from a VM, `NAME:/absolute/path`, up to 8 MiB | [services/vmd/cli](../../services/vmd/cli/README.md#vm-cp) |

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success, or help |
| 1 | A runtime error: the server refused the call (shown as `code: message`), the network failed, or a local file could not be read or written |
| 2 | A usage error: unknown flag, group or command, or the wrong number of operands. Also running with no arguments |

### Output

Data goes to stdout and messages go to stderr. A failed command prints `attic <group> <command>: <message>` to stderr.
A usage error also prints the command's usage line. The token is never printed, in any output.

## Configuration

| Setting | Flag | Environment | Default |
| --- | --- | --- | --- |
| vmd URL | `--vmd-url` | `ATTIC_VMD_URL` | `http://127.0.0.1:8080` |
| project | `-p`, `--project` | `ATTIC_PROJECT` | `default` |
| token | `--token-file` | `ATTIC_TOKEN_FILE`, then `ATTIC_TOKEN`, then `~/.config/attic/token` | none |

The token file is trimmed of surrounding whitespace. The token is sent only as `Authorization: Bearer` to the
configured URL, and the HTTP client refuses redirects, so it is never sent to another host.

## Known limits

- The 8 MiB limit on files applies to `attic vm cp`, in both directions. See
  [services/vmd/cli](../../services/vmd/cli/README.md#known-limits).
- There is one group. The `agent` group is not yet built.
- Global flags are read only before the group name.
- No configuration file other than the token file, and no shell completion.

## Build and version

Build from this directory (the binary is ignored by git: `cmd/attic/attic`):

```
go build -o ../../bin/attic .
go build -ldflags "-X main.version=0.1.0" -o ../../bin/attic .
```

`bin/` is gitignored. Run `attic version` to see the version the binary was built with (`dev` when it was not set).

## Running and testing

```
cd cmd/attic && go test -race -count=1 ./...
```

The tests run the hub end to end against the fake vmd (`services/vmd/cli/vmclitest`): `attic vm list`, an upload and a
download, stdin and stdout, the exit codes of usage and runtime errors, global flags, and a check that the token never
appears in output.

## Examples

Documentation addresses are used (vmd at `192.0.2.10`):

```
export ATTIC_VMD_URL=http://192.0.2.10:8080
export ATTIC_TOKEN_FILE=~/.config/attic/token

attic vm list
attic -p demo vm list
attic vm cp ./app.tar.gz alpha:/srv/app.tar.gz
attic vm cp --mode 0600 ./key.pem alpha:/root/key.pem
attic vm cp alpha:/var/log/syslog ./syslog
attic help
attic vm cp -h
```
