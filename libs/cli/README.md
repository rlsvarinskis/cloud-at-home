# libs/cli

`libs/cli` is the toolkit for the Attic Cloud command-line tools. It defines command groups and their commands, a
dispatcher with help output and exit codes, the global settings (vmd URL, project, token) and the Connect helpers that
send the token. It knows no service's commands. A hub such as [cmd/attic](../../cmd/attic/README.md) imports the groups
it wants, and each group lives next to the service it talks to (for example [services/vmd/cli](../../services/vmd/cli/README.md)).

Module: `github.com/rlsvarinskis/cloud-at-home/libs/cli`

## Role

- Groups and commands: a `Group` has a name, a one-line summary and commands; a `Command` has a name, a summary, a
  usage line and a `Run(ctx, env, args) error` function. Nothing is registered at init time and nothing uses reflection.
- Dispatch: `App.Run` parses the global flags, picks the group and command, prints help and maps the result to an exit
  code (see [Exit codes](#exit-codes)).
- Global settings: resolves the vmd URL, the project and the token from flags, environment variables and defaults.
- Calls: `Env.ClientOptions` returns the Connect options that send `Authorization: Bearer`. `RPCError` shows a Connect
  error as `code: message`.
- Does not know any service's messages, does not read a service's configuration, and holds no state between commands.

## Position

```
cmd/attic (hub)  ──imports──▶  libs/cli  ◀──imports──  services/vmd/cli (group "vm")
                                  │
                                  └── api, libs/common (generated code and error vocabulary only)
```

- Imports `connectrpc.com/connect` and the standard library. It may import `api` and `libs/common`; the boundaries
  script (`scripts/check-boundaries.sh`) allows nothing else. It does not import `libs/authz`: the bearer header is set
  here, in a small interceptor.
- Imported by every command group and by the hub.

## Contract

### Types

| Type | Meaning |
| --- | --- |
| `Command{Name, Summary, Usage, Run}` | One action. `Usage` is the text after the command name, for example `[--mode OCTAL] SRC DST`. |
| `Group{Name, Summary, Commands}` | A set of commands, for example `vm`. |
| `App{Name, Summary, Version, Groups, Sources, HTTP}` | The hub. `Run(ctx, args, stdin, stdout, stderr) int` returns the exit code. |
| `Env{Stdin, Stdout, Stderr, Config, HTTP, Command}` | What a command runs with. `Flags()` and `Parse()` build and parse its flag set. `ClientOptions()` gives the bearer interceptor. |
| `Config{VMDURL, Project, Token}` | The resolved global settings. `Token` is a `Secret`. |
| `Secret` | A credential. It prints as `<redacted>` under `%v`, `%+v`, `%#v` and `%q`. `Reveal()` returns the raw value, for the header only. |
| `UsageError` | A wrong command line: printed with the command's usage, exit 2. Make one with `Usagef`. |
| `ErrHelp` | Returned by a command when `-h` or `-help` was given: the usage goes to stdout, exit 0. |

### Global settings

Each setting takes the first of its sources that is set. Surrounding whitespace is trimmed from every value.

| Setting | Flag | Environment | Default |
| --- | --- | --- | --- |
| vmd URL | `--vmd-url URL` | `ATTIC_VMD_URL` | `http://127.0.0.1:8080` |
| project | `-p NAME`, `--project NAME` | `ATTIC_PROJECT` | `default` |
| token | `--token-file PATH` | `ATTIC_TOKEN_FILE`, then `ATTIC_TOKEN`, then the file `~/.config/attic/token` | none |

- The token file's contents are trimmed. An explicitly named file (flag or `ATTIC_TOKEN_FILE`) must exist and must not
  be empty; the default file may be absent.
- The vmd URL must be an `http` or `https` URL with a host. A trailing slash is removed.
- The token is sent only as `Authorization: Bearer <token>`, and only to the configured vmd URL. The default HTTP client
  does not follow redirects, so a redirect cannot send the token to another host. A command with no token fails before
  it sends anything.
- The token never appears in any output: not in help, in errors, or in `%v` of the configuration. Error messages name
  files and URLs, never their contents.

### Exit codes

| Code | When |
| --- | --- |
| 0 | Success, `help`, `version`, or `-h` on a command |
| 1 | A command failed at run time, or the configuration is unusable (for example an unreadable explicit token file) |
| 2 | The command line is wrong: unknown flag, group or command, wrong number of operands, or no command |

Global flags go before the group name. Flags of a command go after the command name.

### Help

`App.Run` prints the top-level help for `help`, `-h`, `-help` and `--help`, and for no arguments (to stderr, exit 2).
`<group> help` lists the commands of a group. `<group> <command> -h` prints the command's usage.

## Configuration

None beyond the global settings above. The package reads the environment and the home directory only through the
`Sources` of the `App`, so a test controls every input. Its defaults are the constants `DefaultVMDURL` and
`DefaultProject`.

## Known limits

- Global flags are parsed up to the group name. `attic vm list -p demo` does not work; write `attic -p demo vm list`.
- The default HTTP client times out after five minutes (`DefaultTimeout`), which covers one whole exchange.
- There is no configuration file other than the token file.

## Running and testing

```
cd libs/cli && go test -race -count=1 ./...
```

The tests cover flag, environment and file precedence, trimming, the redaction of `Secret`, the dispatcher's help and
exit codes, a token that is never printed, and the `Authorization` header together with the redirect refusal (against a
local `httptest` server).

## Examples

```go
app := &cli.App{
    Name:    "attic",
    Summary: "Attic Cloud command-line tool",
    Groups:  []cli.Group{vmcli.Group()},
}
code := app.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
os.Exit(code)
```

A command:

```go
cli.Command{
    Name:    "list",
    Summary: "list the VMs of the project, one per line",
    Run: func(ctx context.Context, env *cli.Env, args []string) error {
        fs := env.Flags()
        if err := env.Parse(fs, args); err != nil {
            return err // ErrHelp for -h, a UsageError for a bad flag
        }
        opts, err := env.ClientOptions() // fails without a token
        if err != nil {
            return err
        }
        _ = opts // pass to a generated client constructor
        return nil
    },
}
```
