// Package vmcli is the "vm" command group of the attic command-line tool: it lists the VMs of a project and copies
// files to and from them through vmd's VMService and ExecService. It builds its Connect clients from the generated
// vmdv1connect code and the Env the hub provides, and it imports no vmd code.
package vmcli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1/vmdv1connect"
	"github.com/rlsvarinskis/cloud-at-home/libs/cli"
)

// listPageSize is the page size of the ListVMs calls the group makes.
const listPageSize = 100

// Group returns the "vm" command group, for a hub to import and register.
func Group() cli.Group {
	return cli.Group{
		Name:    "vm",
		Summary: "virtual machines: list them, copy files in and out",
		Commands: []cli.Command{
			{
				Name:    "list",
				Summary: "list the VMs of the project, one per line",
				Run:     runList,
			},
			{
				Name:    "cp",
				Summary: "copy a file to or from a VM (NAME:/absolute/path)",
				Usage:   "[--mode OCTAL] SRC DST",
				Run:     runCp,
			},
		},
	}
}

// clients are the generated Connect clients of vmd, each sending the configured token.
type clients struct {
	vms  vmdv1connect.VMServiceClient
	exec vmdv1connect.ExecServiceClient
}

func newClients(env *cli.Env) (clients, error) {
	opts, err := env.ClientOptions()
	if err != nil {
		return clients{}, err
	}
	return clients{
		vms:  vmdv1connect.NewVMServiceClient(env.HTTP, env.Config.VMDURL, opts...),
		exec: vmdv1connect.NewExecServiceClient(env.HTTP, env.Config.VMDURL, opts...),
	}, nil
}

// listAll returns every VM of the project, following the pages of ListVMs.
func listAll(ctx context.Context, vms vmdv1connect.VMServiceClient, project string) ([]*vmdv1.VM, error) {
	var all []*vmdv1.VM
	token := ""
	seen := map[string]bool{}
	for {
		res, err := vms.ListVMs(ctx, connect.NewRequest(&vmdv1.ListVMsRequest{
			ProjectId: project, PageSize: listPageSize, PageToken: token,
		}))
		if err != nil {
			return nil, cli.RPCError(err)
		}
		all = append(all, res.Msg.GetVms()...)
		token = res.Msg.GetNextPageToken()
		if token == "" {
			return all, nil
		}
		if seen[token] {
			return nil, errors.New("ListVMs repeated a page token; stopping")
		}
		seen[token] = true
	}
}

// resolveVM returns the id of the only VM called name in the project. No match and several matches are errors.
func resolveVM(ctx context.Context, vms vmdv1connect.VMServiceClient, project, name string) (string, error) {
	all, err := listAll(ctx, vms, project)
	if err != nil {
		return "", err
	}
	var ids []string
	for _, vm := range all {
		if vm.GetName() == name {
			ids = append(ids, vm.GetId())
		}
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("no VM named %q in project %q", name, project)
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("%d VMs are named %q in project %q (ids %s); VM names must be unique for cp",
			len(ids), name, project, strings.Join(ids, ", "))
	}
}
