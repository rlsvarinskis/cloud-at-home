// Package remote is a node.Driver that calls a node daemon over the network. It is the client
// half of the node API; the server half is libs/qemu/nodeserver.
package remote

import (
	"context"

	"connectrpc.com/connect"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1/nodev1connect"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/node"
)

var _ node.Driver = (*Driver)(nil)

// Driver implements node.Driver by calling a remote NodeService. Errors come back as the
// libs/common/errs sentinels, so errors.Is works the same as it does for a local driver. A host that
// cannot be reached yields an error matching errs.ErrUnavailable.
type Driver struct {
	client nodev1connect.NodeServiceClient
}

// New returns a Driver for the node API at baseURL. Every call carries a bearer token from tokens.
func New(httpClient connect.HTTPClient, baseURL string, tokens authz.TokenSource, opts ...connect.ClientOption) *Driver {
	opts = append([]connect.ClientOption{connect.WithInterceptors(authz.ClientInterceptor(tokens))}, opts...)
	return &Driver{client: nodev1connect.NewNodeServiceClient(httpClient, baseURL, opts...)}
}

// GetCapacity forwards to the node's GetCapacity RPC.
func (d *Driver) GetCapacity(ctx context.Context, r *nodev1.GetCapacityRequest) (*nodev1.GetCapacityResponse, error) {
	res, err := d.client.GetCapacity(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// RegisterImage forwards to the node's RegisterImage RPC.
func (d *Driver) RegisterImage(ctx context.Context, r *nodev1.RegisterImageRequest) (*nodev1.RegisterImageResponse, error) {
	res, err := d.client.RegisterImage(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// DeleteImage forwards to the node's DeleteImage RPC.
func (d *Driver) DeleteImage(ctx context.Context, r *nodev1.DeleteImageRequest) (*nodev1.DeleteImageResponse, error) {
	res, err := d.client.DeleteImage(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// CreateDisk forwards to the node's CreateDisk RPC.
func (d *Driver) CreateDisk(ctx context.Context, r *nodev1.CreateDiskRequest) (*nodev1.CreateDiskResponse, error) {
	res, err := d.client.CreateDisk(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// ResizeDisk forwards to the node's ResizeDisk RPC.
func (d *Driver) ResizeDisk(ctx context.Context, r *nodev1.ResizeDiskRequest) (*nodev1.ResizeDiskResponse, error) {
	res, err := d.client.ResizeDisk(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// DeleteDisk forwards to the node's DeleteDisk RPC.
func (d *Driver) DeleteDisk(ctx context.Context, r *nodev1.DeleteDiskRequest) (*nodev1.DeleteDiskResponse, error) {
	res, err := d.client.DeleteDisk(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// SnapshotDisk forwards to the node's SnapshotDisk RPC.
func (d *Driver) SnapshotDisk(ctx context.Context, r *nodev1.SnapshotDiskRequest) (*nodev1.SnapshotDiskResponse, error) {
	res, err := d.client.SnapshotDisk(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// StartVM forwards to the node's StartVM RPC.
func (d *Driver) StartVM(ctx context.Context, r *nodev1.StartVMRequest) (*nodev1.StartVMResponse, error) {
	res, err := d.client.StartVM(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// StopVM forwards to the node's StopVM RPC.
func (d *Driver) StopVM(ctx context.Context, r *nodev1.StopVMRequest) (*nodev1.StopVMResponse, error) {
	res, err := d.client.StopVM(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// GetVMRuntime forwards to the node's GetVMRuntime RPC.
func (d *Driver) GetVMRuntime(ctx context.Context, r *nodev1.GetVMRuntimeRequest) (*nodev1.GetVMRuntimeResponse, error) {
	res, err := d.client.GetVMRuntime(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// ForgetVM forwards to the node's ForgetVM RPC.
func (d *Driver) ForgetVM(ctx context.Context, r *nodev1.ForgetVMRequest) (*nodev1.ForgetVMResponse, error) {
	res, err := d.client.ForgetVM(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// StartJob forwards to the node's StartJob RPC.
func (d *Driver) StartJob(ctx context.Context, r *nodev1.StartJobRequest) (*nodev1.StartJobResponse, error) {
	res, err := d.client.StartJob(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// PollJob forwards to the node's PollJob RPC.
func (d *Driver) PollJob(ctx context.Context, r *nodev1.PollJobRequest) (*nodev1.PollJobResponse, error) {
	res, err := d.client.PollJob(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// CancelJob forwards to the node's CancelJob RPC.
func (d *Driver) CancelJob(ctx context.Context, r *nodev1.CancelJobRequest) (*nodev1.CancelJobResponse, error) {
	res, err := d.client.CancelJob(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// PutFile forwards to the node's PutFile RPC.
func (d *Driver) PutFile(ctx context.Context, r *nodev1.PutFileRequest) (*nodev1.PutFileResponse, error) {
	res, err := d.client.PutFile(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// GetFile forwards to the node's GetFile RPC.
func (d *Driver) GetFile(ctx context.Context, r *nodev1.GetFileRequest) (*nodev1.GetFileResponse, error) {
	res, err := d.client.GetFile(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// GetVMMetrics forwards to the node's GetVMMetrics RPC.
func (d *Driver) GetVMMetrics(ctx context.Context, r *nodev1.GetVMMetricsRequest) (*nodev1.GetVMMetricsResponse, error) {
	res, err := d.client.GetVMMetrics(ctx, connect.NewRequest(r))
	return msg(res, err)
}

// msg unwraps a Connect response, translating any error back to an errs sentinel.
func msg[T any](res *connect.Response[T], err error) (*T, error) {
	if err != nil {
		return nil, errs.FromConnect(err)
	}
	return res.Msg, nil
}
