package mcpserver

import (
	"net/http"

	"connectrpc.com/connect"

	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1/agentdv1connect"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1/vmdv1connect"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
)

// Clients are the downstream APIs the tools call. They are interfaces so tests can substitute fakes.
type Clients struct {
	VMs     vmdv1connect.VMServiceClient
	Disks   vmdv1connect.DiskServiceClient
	Images  vmdv1connect.ImageServiceClient
	Exec    vmdv1connect.ExecServiceClient
	Metrics vmdv1connect.MetricsServiceClient
	Cluster vmdv1connect.ClusterServiceClient
	Agents  agentdv1connect.AgentServiceClient
	Tasks   agentdv1connect.TaskServiceClient
	Models  agentdv1connect.ModelServiceClient
}

// NewClients dials vmd and agentd at the given base URLs; every call forwards the MCP caller's token.
func NewClients(hc *http.Client, vmdURL, agentdURL string) Clients {
	opts := []connect.ClientOption{connect.WithInterceptors(authz.ClientInterceptor(tokenFrom))}
	return Clients{
		VMs:     vmdv1connect.NewVMServiceClient(hc, vmdURL, opts...),
		Disks:   vmdv1connect.NewDiskServiceClient(hc, vmdURL, opts...),
		Images:  vmdv1connect.NewImageServiceClient(hc, vmdURL, opts...),
		Exec:    vmdv1connect.NewExecServiceClient(hc, vmdURL, opts...),
		Metrics: vmdv1connect.NewMetricsServiceClient(hc, vmdURL, opts...),
		Cluster: vmdv1connect.NewClusterServiceClient(hc, vmdURL, opts...),
		Agents:  agentdv1connect.NewAgentServiceClient(hc, agentdURL, opts...),
		Tasks:   agentdv1connect.NewTaskServiceClient(hc, agentdURL, opts...),
		Models:  agentdv1connect.NewModelServiceClient(hc, agentdURL, opts...),
	}
}
