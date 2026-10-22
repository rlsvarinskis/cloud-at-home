// Command attic is the Attic Cloud command-line tool. It is only a hub: it parses the global flags and dispatches to
// command groups. Each group lives next to the service it talks to (services/vmd/cli for "vm") and is imported here
// explicitly, so adding a group means adding one import and one entry in newApp.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rlsvarinskis/cloud-at-home/libs/cli"
	vmcli "github.com/rlsvarinskis/cloud-at-home/services/vmd/cli"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := newApp().Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// newApp returns the hub with every command group it knows.
func newApp() *cli.App {
	return &cli.App{
		Name:    "attic",
		Summary: "Attic Cloud command-line tool",
		Version: version,
		Groups: []cli.Group{
			vmcli.Group(),
		},
	}
}
