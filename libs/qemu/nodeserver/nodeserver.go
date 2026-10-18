// Package nodeserver exposes a node.Driver over the node API (Connect). Every RPC requires a token
// with the node role; the work itself is delegated to the Driver.
package nodeserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1/nodev1connect"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/node"
)

// New returns the Connect handler for the node API, backed by d. Requests are authenticated with v
// and must hold node.manage on every project (only the node role does). A panic in d is logged
// and returned as an internal error, so it cannot take the daemon down. extra options are applied
// after the defaults.
func New(d node.Driver, v *authz.Verifier, extra ...connect.HandlerOption) (path string, handler http.Handler) {
	opts := []connect.HandlerOption{
		connect.WithRecover(func(_ context.Context, spec connect.Spec, _ http.Header, p any) error {
			slog.Error("node rpc panicked", "procedure", spec.Procedure, "panic", p)
			return connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}),
		connect.WithInterceptors(&authz.ServerInterceptor{Verifier: v}),
	}
	opts = append(opts, extra...)
	return nodev1connect.NewNodeServiceHandler(&service{d: d}, opts...)
}

// service adapts a node.Driver to nodev1connect.NodeServiceHandler.
type service struct {
	d node.Driver
}

// authorize checks the caller holds node.manage. Errors are converted to Connect codes.
func authorize(ctx context.Context) error {
	return errs.ToConnect(authz.Require(ctx, authz.NodeManage, authz.AnyProject))
}

// call runs one RPC: authorize, invoke the driver, and wrap the response or convert the error.
func call[Req, Res any](ctx context.Context, req *connect.Request[Req], f func(context.Context, *Req) (*Res, error)) (*connect.Response[Res], error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	res, err := f(ctx, req.Msg)
	if err != nil {
		return nil, errs.ToConnect(err)
	}
	return connect.NewResponse(res), nil
}

func (s *service) GetCapacity(ctx context.Context, req *connect.Request[nodev1.GetCapacityRequest]) (*connect.Response[nodev1.GetCapacityResponse], error) {
	return call(ctx, req, s.d.GetCapacity)
}

func (s *service) RegisterImage(ctx context.Context, req *connect.Request[nodev1.RegisterImageRequest]) (*connect.Response[nodev1.RegisterImageResponse], error) {
	return call(ctx, req, s.d.RegisterImage)
}

func (s *service) DeleteImage(ctx context.Context, req *connect.Request[nodev1.DeleteImageRequest]) (*connect.Response[nodev1.DeleteImageResponse], error) {
	return call(ctx, req, s.d.DeleteImage)
}

func (s *service) CreateDisk(ctx context.Context, req *connect.Request[nodev1.CreateDiskRequest]) (*connect.Response[nodev1.CreateDiskResponse], error) {
	return call(ctx, req, s.d.CreateDisk)
}

func (s *service) ResizeDisk(ctx context.Context, req *connect.Request[nodev1.ResizeDiskRequest]) (*connect.Response[nodev1.ResizeDiskResponse], error) {
	return call(ctx, req, s.d.ResizeDisk)
}

func (s *service) DeleteDisk(ctx context.Context, req *connect.Request[nodev1.DeleteDiskRequest]) (*connect.Response[nodev1.DeleteDiskResponse], error) {
	return call(ctx, req, s.d.DeleteDisk)
}

func (s *service) SnapshotDisk(ctx context.Context, req *connect.Request[nodev1.SnapshotDiskRequest]) (*connect.Response[nodev1.SnapshotDiskResponse], error) {
	return call(ctx, req, s.d.SnapshotDisk)
}

func (s *service) StartVM(ctx context.Context, req *connect.Request[nodev1.StartVMRequest]) (*connect.Response[nodev1.StartVMResponse], error) {
	return call(ctx, req, s.d.StartVM)
}

func (s *service) StopVM(ctx context.Context, req *connect.Request[nodev1.StopVMRequest]) (*connect.Response[nodev1.StopVMResponse], error) {
	return call(ctx, req, s.d.StopVM)
}

func (s *service) GetVMRuntime(ctx context.Context, req *connect.Request[nodev1.GetVMRuntimeRequest]) (*connect.Response[nodev1.GetVMRuntimeResponse], error) {
	return call(ctx, req, s.d.GetVMRuntime)
}

func (s *service) ForgetVM(ctx context.Context, req *connect.Request[nodev1.ForgetVMRequest]) (*connect.Response[nodev1.ForgetVMResponse], error) {
	return call(ctx, req, s.d.ForgetVM)
}

func (s *service) StartJob(ctx context.Context, req *connect.Request[nodev1.StartJobRequest]) (*connect.Response[nodev1.StartJobResponse], error) {
	return call(ctx, req, s.d.StartJob)
}

func (s *service) PollJob(ctx context.Context, req *connect.Request[nodev1.PollJobRequest]) (*connect.Response[nodev1.PollJobResponse], error) {
	return call(ctx, req, s.d.PollJob)
}

func (s *service) CancelJob(ctx context.Context, req *connect.Request[nodev1.CancelJobRequest]) (*connect.Response[nodev1.CancelJobResponse], error) {
	return call(ctx, req, s.d.CancelJob)
}

func (s *service) PutFile(ctx context.Context, req *connect.Request[nodev1.PutFileRequest]) (*connect.Response[nodev1.PutFileResponse], error) {
	return call(ctx, req, s.d.PutFile)
}

func (s *service) GetFile(ctx context.Context, req *connect.Request[nodev1.GetFileRequest]) (*connect.Response[nodev1.GetFileResponse], error) {
	return call(ctx, req, s.d.GetFile)
}

func (s *service) GetVMMetrics(ctx context.Context, req *connect.Request[nodev1.GetVMMetricsRequest]) (*connect.Response[nodev1.GetVMMetricsResponse], error) {
	return call(ctx, req, s.d.GetVMMetrics)
}
