// Package vmclient is the only way agentd touches virtual machines. It wraps vmd's public
// Connect API behind a narrow interface so agentd code can be tested with a fake.
package vmclient

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"

	"example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/gen/homecloud/vmd/v1/vmdv1connect"
	"example.com/homecloud/internal/auth"
	"example.com/homecloud/internal/errs"
)

// defaultTimeout bounds every call whose context has no deadline earlier than this. It is a
// variable so tests can shorten it.
var defaultTimeout = 60 * time.Second

//go:generate go run go.uber.org/mock/mockgen@latest -destination=mock/mock_vmclient.go -package=mock . VM

// VM is everything agentd needs from vmd. Errors satisfy errors.Is against internal/errs sentinels.
type VM interface {
	GetVM(ctx context.Context, id string) (*vmdv1.VM, error)
	AddHold(ctx context.Context, vmID, holder string) (*vmdv1.VM, error)
	RemoveHold(ctx context.Context, vmID, holder string) (*vmdv1.VM, error)
	StartJob(ctx context.Context, r *vmdv1.StartJobRequest) (*vmdv1.JobOutput, error)
	PollJob(ctx context.Context, r *vmdv1.PollJobRequest) (*vmdv1.JobOutput, error)
	CancelJob(ctx context.Context, jobID string) (*vmdv1.JobOutput, error)
	PutFile(ctx context.Context, vmID, path string, content []byte, mode uint32) error
	GetFile(ctx context.Context, vmID, path string, maxBytes int64) (content []byte, truncated bool, err error)
}

type client struct {
	vms  vmdv1connect.VMServiceClient
	exec vmdv1connect.ExecServiceClient
}

// New returns a VM that calls vmd at baseURL. Every call carries a bearer token from tokens,
// which must not be nil. opts are applied after the auth interceptor.
func New(httpClient connect.HTTPClient, baseURL string, tokens auth.TokenSource, opts ...connect.ClientOption) VM {
	all := append([]connect.ClientOption{connect.WithInterceptors(auth.ClientInterceptor(tokens))}, opts...)
	return &client{
		vms:  vmdv1connect.NewVMServiceClient(httpClient, baseURL, all...),
		exec: vmdv1connect.NewExecServiceClient(httpClient, baseURL, all...),
	}
}

// callContext applies defaultTimeout unless ctx already expires sooner than that.
func callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if d, ok := ctx.Deadline(); ok && time.Until(d) < defaultTimeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultTimeout)
}

// convert maps a Connect error to the internal/errs sentinel vocabulary, keeping the operation
// name for logs. Deadline and cancellation are also wrapped as the context errors, because
// Connect's error does not always unwrap to them (a server-side deadline arrives as a response).
func convert(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	var ctxErr error
	switch connect.CodeOf(err) {
	case connect.CodeDeadlineExceeded:
		ctxErr = context.DeadlineExceeded
	case connect.CodeCanceled:
		ctxErr = context.Canceled
	}
	if ctxErr != nil {
		return fmt.Errorf("vmd %s: %w: %w", op, ctxErr, errs.FromConnect(err))
	}
	return fmt.Errorf("vmd %s: %w", op, errs.FromConnect(err))
}

func (c *client) GetVM(ctx context.Context, id string) (*vmdv1.VM, error) {
	ctx, cancel := callContext(ctx)
	defer cancel()
	res, err := c.vms.GetVM(ctx, connect.NewRequest(&vmdv1.GetVMRequest{Id: id}))
	if err != nil {
		return nil, convert(ctx, "GetVM", err)
	}
	return res.Msg, nil
}

func (c *client) AddHold(ctx context.Context, vmID, holder string) (*vmdv1.VM, error) {
	ctx, cancel := callContext(ctx)
	defer cancel()
	res, err := c.vms.AddHold(ctx, connect.NewRequest(&vmdv1.HoldRequest{Id: vmID, Holder: holder}))
	if err != nil {
		return nil, convert(ctx, "AddHold", err)
	}
	return res.Msg, nil
}

func (c *client) RemoveHold(ctx context.Context, vmID, holder string) (*vmdv1.VM, error) {
	ctx, cancel := callContext(ctx)
	defer cancel()
	res, err := c.vms.RemoveHold(ctx, connect.NewRequest(&vmdv1.HoldRequest{Id: vmID, Holder: holder}))
	if err != nil {
		return nil, convert(ctx, "RemoveHold", err)
	}
	return res.Msg, nil
}

func (c *client) StartJob(ctx context.Context, r *vmdv1.StartJobRequest) (*vmdv1.JobOutput, error) {
	ctx, cancel := callContext(ctx)
	defer cancel()
	res, err := c.exec.StartJob(ctx, connect.NewRequest(r))
	if err != nil {
		return nil, convert(ctx, "StartJob", err)
	}
	return res.Msg, nil
}

func (c *client) PollJob(ctx context.Context, r *vmdv1.PollJobRequest) (*vmdv1.JobOutput, error) {
	ctx, cancel := callContext(ctx)
	defer cancel()
	res, err := c.exec.PollJob(ctx, connect.NewRequest(r))
	if err != nil {
		return nil, convert(ctx, "PollJob", err)
	}
	return res.Msg, nil
}

func (c *client) CancelJob(ctx context.Context, jobID string) (*vmdv1.JobOutput, error) {
	ctx, cancel := callContext(ctx)
	defer cancel()
	res, err := c.exec.CancelJob(ctx, connect.NewRequest(&vmdv1.CancelJobRequest{JobId: jobID}))
	if err != nil {
		return nil, convert(ctx, "CancelJob", err)
	}
	return res.Msg, nil
}

func (c *client) PutFile(ctx context.Context, vmID, path string, content []byte, mode uint32) error {
	ctx, cancel := callContext(ctx)
	defer cancel()
	_, err := c.exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{
		VmId:    vmID,
		Path:    path,
		Content: content,
		Mode:    mode,
	}))
	return convert(ctx, "PutFile", err)
}

func (c *client) GetFile(ctx context.Context, vmID, path string, maxBytes int64) ([]byte, bool, error) {
	ctx, cancel := callContext(ctx)
	defer cancel()
	res, err := c.exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{
		VmId:     vmID,
		Path:     path,
		MaxBytes: maxBytes,
	}))
	if err != nil {
		return nil, false, convert(ctx, "GetFile", err)
	}
	return res.Msg.GetContent(), res.Msg.GetTruncated(), nil
}
