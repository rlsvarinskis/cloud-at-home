// Package cli is the toolkit of the Attic Cloud command-line tools. It defines command groups and their commands, a
// dispatcher with help output and exit codes, the global settings (vmd URL, project, token) and the Connect helpers
// that send the token. It knows no service's commands: a hub such as cmd/attic imports the groups it wants explicitly.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Exit codes returned by App.Run.
const (
	ExitOK      = 0
	ExitRuntime = 1 // the command failed
	ExitUsage   = 2 // the command line was wrong
)

// DefaultTimeout bounds one whole HTTP exchange of the default client, including a file transfer.
const DefaultTimeout = 5 * time.Minute

// ErrHelp is returned by a command's Run when -h or -help was given. The dispatcher prints the command's usage and
// exits 0.
var ErrHelp = flag.ErrHelp

// UsageError reports a wrong command line. The dispatcher prints it with the command's usage and exits 2.
type UsageError struct {
	Msg string
}

func (e *UsageError) Error() string { return e.Msg }

// Usagef returns a UsageError.
func Usagef(format string, args ...any) error {
	return &UsageError{Msg: fmt.Sprintf(format, args...)}
}

// Command is one action of a group, such as "vm list". Its Run receives the arguments after the command name.
type Command struct {
	Name    string // the word after the group name
	Summary string // one line, shown in the group's help
	Usage   string // arguments and flags after the command name, for example "[--mode OCTAL] SRC DST"
	Run     func(ctx context.Context, env *Env, args []string) error
}

// Group is a set of commands that belong together, such as "vm". A group lives next to the service it talks to.
type Group struct {
	Name     string // the word after the program name
	Summary  string // one line, shown in the top-level help
	Commands []Command
}

func (g Group) command(name string) (Command, bool) {
	for _, c := range g.Commands {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

// App is the hub: it parses the global flags and dispatches to a group.
type App struct {
	Name    string // program name in messages and help, for example "attic"
	Summary string // one line, shown in the top-level help
	Version string // printed by "version"; "dev" when empty
	Groups  []Group

	// Sources feeds the global settings. The zero value reads the process environment.
	Sources Sources
	// HTTP is the client for every call. Nil selects a client with DefaultTimeout that never follows redirects, so
	// the token is not sent to another host.
	HTTP *http.Client
}

// Run runs the command line args (without the program name) and returns the exit code. Output goes to stdout and
// stderr; stdin is the command's standard input.
func (a *App) Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var s Settings
	gfs := flag.NewFlagSet(a.Name, flag.ContinueOnError)
	gfs.SetOutput(io.Discard)
	gfs.Usage = func() {}
	gfs.StringVar(&s.VMDURL, "vmd-url", "", "vmd base URL")
	gfs.StringVar(&s.Project, "project", "", "project name")
	gfs.StringVar(&s.Project, "p", "", "project name (short form)")
	gfs.StringVar(&s.TokenFile, "token-file", "", "file holding the API token")
	if err := gfs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			a.writeHelp(stdout)
			return ExitOK
		}
		fmt.Fprintf(stderr, "%s: %v\n", a.Name, err)
		fmt.Fprintf(stderr, "run '%s help' for usage\n", a.Name)
		return ExitUsage
	}
	rest := gfs.Args()
	if len(rest) == 0 {
		a.writeHelp(stderr)
		return ExitUsage
	}
	switch rest[0] {
	case "help":
		a.writeHelp(stdout)
		return ExitOK
	case "version":
		fmt.Fprintln(stdout, a.version())
		return ExitOK
	}
	g, ok := a.group(rest[0])
	if !ok {
		fmt.Fprintf(stderr, "%s: unknown command %q\n", a.Name, rest[0])
		fmt.Fprintf(stderr, "run '%s help' for the commands\n", a.Name)
		return ExitUsage
	}
	env := &Env{Stdin: stdin, Stdout: stdout, Stderr: stderr, HTTP: a.HTTP}
	if env.HTTP == nil {
		env.HTTP = defaultHTTPClient()
	}
	return a.runGroup(ctx, g, rest[1:], s, env)
}

func (a *App) runGroup(ctx context.Context, g Group, args []string, s Settings, env *Env) int {
	prog := a.Name + " " + g.Name
	if len(args) == 0 {
		writeGroupHelp(env.Stderr, a.Name, g)
		return ExitUsage
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		writeGroupHelp(env.Stdout, a.Name, g)
		return ExitOK
	}
	c, ok := g.command(args[0])
	if !ok {
		fmt.Fprintf(env.Stderr, "%s: unknown command %q\n", prog, args[0])
		fmt.Fprintf(env.Stderr, "run '%s help' for the commands\n", prog)
		return ExitUsage
	}
	cfg, err := Resolve(s, a.Sources)
	if err != nil {
		fmt.Fprintf(env.Stderr, "%s: %v\n", a.Name, err)
		return ExitRuntime
	}
	env.Config = cfg
	env.Command = prog + " " + c.Name
	err = c.Run(ctx, env, args[1:])
	return reportCommand(env, c, err)
}

func reportCommand(env *Env, c Command, err error) int {
	var ue *UsageError
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, ErrHelp):
		writeUsageLine(env.Stdout, env.Command, c.Usage)
		return ExitOK
	case errors.As(err, &ue):
		fmt.Fprintf(env.Stderr, "%s: %s\n", env.Command, ue.Msg)
		writeUsageLine(env.Stderr, env.Command, c.Usage)
		return ExitUsage
	default:
		fmt.Fprintf(env.Stderr, "%s: %v\n", env.Command, err)
		return ExitRuntime
	}
}

func (a *App) group(name string) (Group, bool) {
	for _, g := range a.Groups {
		if g.Name == name {
			return g, true
		}
	}
	return Group{}, false
}

func (a *App) version() string {
	if a.Version == "" {
		return "dev"
	}
	return a.Version
}

func (a *App) writeHelp(w io.Writer) {
	fmt.Fprintf(w, "%s - %s\n\n", a.Name, a.Summary)
	fmt.Fprintf(w, "Usage:\n  %s [global flags] <group> <command> [arguments]\n  %s help | %s version\n\n", a.Name, a.Name, a.Name)
	fmt.Fprintln(w, "Groups:")
	for _, g := range a.Groups {
		fmt.Fprintf(w, "  %-8s %s\n", g.Name, g.Summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Global flags (a flag overrides its environment variable, which overrides the default):")
	fmt.Fprintf(w, "  --vmd-url URL       vmd base URL (%s; default %s)\n", EnvVMDURL, DefaultVMDURL)
	fmt.Fprintf(w, "  -p, --project NAME  project (%s; default %q)\n", EnvProject, DefaultProject)
	fmt.Fprintf(w, "  --token-file PATH   file holding the API token (%s, then %s, then ~/%s)\n",
		EnvTokenFile, EnvToken, defaultTokenFile)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Global flags go before the group name. Run '%s <group> help' for the commands of a group.\n", a.Name)
}

func writeGroupHelp(w io.Writer, prog string, g Group) {
	fmt.Fprintf(w, "%s %s - %s\n\n", prog, g.Name, g.Summary)
	fmt.Fprintf(w, "Usage:\n  %s %s <command> [arguments]\n\nCommands:\n", prog, g.Name)
	for _, c := range g.Commands {
		fmt.Fprintf(w, "  %-8s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintf(w, "\nRun '%s %s <command> -h' for the flags of a command.\n", prog, g.Name)
}

func writeUsageLine(w io.Writer, command, usage string) {
	line := command
	if usage != "" {
		line += " " + usage
	}
	fmt.Fprintf(w, "usage: %s\n", line)
}

func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: DefaultTimeout,
		// A redirect could send the bearer token to another host, so the response of a redirect is returned as is.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
