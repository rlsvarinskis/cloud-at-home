package vmcli

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/cli"
)

// runList prints one line per VM of the project: name, state, CPUs and memory in GiB. There is no header, so the
// output can be read by a script.
func runList(ctx context.Context, env *cli.Env, args []string) error {
	fs := env.Flags()
	if err := env.Parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return cli.Usagef("list takes no arguments, got %q", fs.Arg(0))
	}
	c, err := newClients(env)
	if err != nil {
		return err
	}
	vms, err := listAll(ctx, c.vms, env.Config.Project)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	for _, vm := range vms {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", vm.GetName(), stateName(vm.GetState()), vm.GetCpus(), gibString(vm.GetMemoryMb()))
	}
	return tw.Flush()
}

// stateName is the lower-case word of a VM state, for example "running".
func stateName(s vmdv1.VMState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "VM_STATE_"))
}

// gibString renders mebibytes as GiB with up to three decimals, for example "1.5".
func gibString(mb int64) string {
	gib := math.Round(float64(mb)/1024*1000) / 1000
	return strconv.FormatFloat(gib, 'f', -1, 64)
}
