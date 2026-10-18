package vmclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1/vmdv1connect"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

// fakeVMd serves both vmd services. It records the last request, can be told to fail every call
// or to block until the client gives up, and answers with canned results otherwise.
type fakeVMd struct {
	vmdv1connect.UnimplementedVMServiceHandler
	vmdv1connect.UnimplementedExecServiceHandler

	mu       sync.Mutex
	err      error
	block    bool
	last     any
	auth     string
	deadline time.Time
	hasDL    bool
}

func (f *fakeVMd) set(err error, block bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err, f.block = err, block
}

// recorded returns the last request message, its Authorization header and the deadline the
// server saw on the request context.
func (f *fakeVMd) recorded() (msg any, authz string, deadline time.Time, hasDeadline bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last, f.auth, f.deadline, f.hasDL
}

// observe records a call and returns the failure configured for it.
func (f *fakeVMd) observe(ctx context.Context, h http.Header, msg any) error {
	f.mu.Lock()
	f.last = msg
	f.auth = h.Get("Authorization")
	f.deadline, f.hasDL = ctx.Deadline()
	err, block := f.err, f.block
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (f *fakeVMd) GetVM(ctx context.Context, req *connect.Request[vmdv1.GetVMRequest]) (*connect.Response[vmdv1.VM], error) {
	if err := f.observe(ctx, req.Header(), req.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&vmdv1.VM{Id: req.Msg.GetId(), Name: "vm-name"}), nil
}

func (f *fakeVMd) AddHold(ctx context.Context, req *connect.Request[vmdv1.HoldRequest]) (*connect.Response[vmdv1.VM], error) {
	if err := f.observe(ctx, req.Header(), req.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&vmdv1.VM{Id: req.Msg.GetId(), Holds: []string{req.Msg.GetHolder()}}), nil
}

func (f *fakeVMd) RemoveHold(ctx context.Context, req *connect.Request[vmdv1.HoldRequest]) (*connect.Response[vmdv1.VM], error) {
	if err := f.observe(ctx, req.Header(), req.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&vmdv1.VM{Id: req.Msg.GetId()}), nil
}

func (f *fakeVMd) StartJob(ctx context.Context, req *connect.Request[vmdv1.StartJobRequest]) (*connect.Response[vmdv1.JobOutput], error) {
	if err := f.observe(ctx, req.Header(), req.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&vmdv1.JobOutput{
		Job:    &vmdv1.Job{Id: "job-1", VmId: req.Msg.GetVmId(), Command: req.Msg.GetCommand(), State: vmdv1.JobState_JOB_STATE_RUNNING},
		Stdout: []byte("hello"),
	}), nil
}

func (f *fakeVMd) PollJob(ctx context.Context, req *connect.Request[vmdv1.PollJobRequest]) (*connect.Response[vmdv1.JobOutput], error) {
	if err := f.observe(ctx, req.Header(), req.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&vmdv1.JobOutput{
		Job:          &vmdv1.Job{Id: req.Msg.GetJobId(), State: vmdv1.JobState_JOB_STATE_SUCCEEDED},
		StdoutOffset: req.Msg.GetStdoutOffset() + 4,
	}), nil
}

func (f *fakeVMd) CancelJob(ctx context.Context, req *connect.Request[vmdv1.CancelJobRequest]) (*connect.Response[vmdv1.JobOutput], error) {
	if err := f.observe(ctx, req.Header(), req.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&vmdv1.JobOutput{
		Job: &vmdv1.Job{Id: req.Msg.GetJobId(), State: vmdv1.JobState_JOB_STATE_CANCELLED},
	}), nil
}

func (f *fakeVMd) PutFile(ctx context.Context, req *connect.Request[vmdv1.PutFileRequest]) (*connect.Response[vmdv1.PutFileResponse], error) {
	if err := f.observe(ctx, req.Header(), req.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&vmdv1.PutFileResponse{}), nil
}

func (f *fakeVMd) GetFile(ctx context.Context, req *connect.Request[vmdv1.GetFileRequest]) (*connect.Response[vmdv1.GetFileResponse], error) {
	if err := f.observe(ctx, req.Header(), req.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&vmdv1.GetFileResponse{Content: []byte("file-body"), Truncated: true}), nil
}

// newServer starts a fake vmd and returns its URL and the fake itself.
func newServer(t *testing.T) (*httptest.Server, *fakeVMd) {
	t.Helper()
	f := &fakeVMd{}
	mux := http.NewServeMux()
	vmPath, vmHandler := vmdv1connect.NewVMServiceHandler(f)
	mux.Handle(vmPath, vmHandler)
	execPath, execHandler := vmdv1connect.NewExecServiceHandler(f)
	mux.Handle(execPath, execHandler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, f
}

func newClient(t *testing.T, srv *httptest.Server) VM {
	t.Helper()
	return New(srv.Client(), srv.URL, authz.StaticToken("tok-123"))
}

// shortenDefault replaces defaultTimeout for the duration of the test.
func shortenDefault(t *testing.T, d time.Duration) {
	t.Helper()
	old := defaultTimeout
	defaultTimeout = d
	t.Cleanup(func() { defaultTimeout = old })
}

func TestVMServiceCallsForwardArgsAndResults(t *testing.T) {
	srv, f := newServer(t)
	v := newClient(t, srv)
	ctx := context.Background()

	vm, err := v.GetVM(ctx, "vm_1")
	if err != nil {
		t.Fatal(err)
	}
	if vm.GetId() != "vm_1" || vm.GetName() != "vm-name" {
		t.Fatalf("GetVM result = %v", vm)
	}
	msg, authz, _, _ := f.recorded()
	if got, ok := msg.(*vmdv1.GetVMRequest); !ok || got.GetId() != "vm_1" {
		t.Fatalf("GetVM sent %v", msg)
	}
	if authz != "Bearer tok-123" {
		t.Fatalf("Authorization = %q", authz)
	}

	vm, err = v.AddHold(ctx, "vm_1", "agentd/a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(vm.GetHolds()) != 1 || vm.GetHolds()[0] != "agentd/a1" {
		t.Fatalf("AddHold result holds = %v", vm.GetHolds())
	}
	msg, _, _, _ = f.recorded()
	if got, ok := msg.(*vmdv1.HoldRequest); !ok || got.GetId() != "vm_1" || got.GetHolder() != "agentd/a1" {
		t.Fatalf("AddHold sent %v", msg)
	}

	vm, err = v.RemoveHold(ctx, "vm_1", "agentd/a1")
	if err != nil {
		t.Fatal(err)
	}
	if vm.GetId() != "vm_1" {
		t.Fatalf("RemoveHold result = %v", vm)
	}
	msg, _, _, _ = f.recorded()
	if got, ok := msg.(*vmdv1.HoldRequest); !ok || got.GetId() != "vm_1" || got.GetHolder() != "agentd/a1" {
		t.Fatalf("RemoveHold sent %v", msg)
	}
}

func TestExecServiceCallsForwardArgsAndResults(t *testing.T) {
	srv, f := newServer(t)
	v := newClient(t, srv)
	ctx := context.Background()

	out, err := v.StartJob(ctx, &vmdv1.StartJobRequest{VmId: "vm_1", Command: "echo hello", WaitSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	if out.GetJob().GetId() != "job-1" || string(out.GetStdout()) != "hello" {
		t.Fatalf("StartJob result = %v", out)
	}
	msg, authz, _, _ := f.recorded()
	if got, ok := msg.(*vmdv1.StartJobRequest); !ok || got.GetVmId() != "vm_1" || got.GetCommand() != "echo hello" || got.GetWaitSeconds() != 5 {
		t.Fatalf("StartJob sent %v", msg)
	}
	if authz != "Bearer tok-123" {
		t.Fatalf("Authorization = %q", authz)
	}

	out, err = v.PollJob(ctx, &vmdv1.PollJobRequest{JobId: "job-1", StdoutOffset: 5, Tail: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.GetJob().GetState() != vmdv1.JobState_JOB_STATE_SUCCEEDED || out.GetStdoutOffset() != 9 {
		t.Fatalf("PollJob result = %v", out)
	}
	msg, _, _, _ = f.recorded()
	if got, ok := msg.(*vmdv1.PollJobRequest); !ok || got.GetJobId() != "job-1" || got.GetStdoutOffset() != 5 || !got.GetTail() {
		t.Fatalf("PollJob sent %v", msg)
	}

	out, err = v.CancelJob(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if out.GetJob().GetState() != vmdv1.JobState_JOB_STATE_CANCELLED {
		t.Fatalf("CancelJob result = %v", out)
	}
	msg, _, _, _ = f.recorded()
	if got, ok := msg.(*vmdv1.CancelJobRequest); !ok || got.GetJobId() != "job-1" {
		t.Fatalf("CancelJob sent %v", msg)
	}

	if err := v.PutFile(ctx, "vm_1", "/etc/motd", []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, _, _, _ = f.recorded()
	if got, ok := msg.(*vmdv1.PutFileRequest); !ok || got.GetVmId() != "vm_1" || got.GetPath() != "/etc/motd" || string(got.GetContent()) != "data" || got.GetMode() != 0o600 {
		t.Fatalf("PutFile sent %v", msg)
	}

	content, truncated, err := v.GetFile(ctx, "vm_1", "/var/log/x", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "file-body" || !truncated {
		t.Fatalf("GetFile result = %q, truncated %v", content, truncated)
	}
	msg, _, _, _ = f.recorded()
	if got, ok := msg.(*vmdv1.GetFileRequest); !ok || got.GetVmId() != "vm_1" || got.GetPath() != "/var/log/x" || got.GetMaxBytes() != 1024 {
		t.Fatalf("GetFile sent %v", msg)
	}
}

func TestVMdErrorsMapToSentinels(t *testing.T) {
	cases := []struct {
		name string
		code connect.Code
		want error
	}{
		{"not found", connect.CodeNotFound, errs.ErrNotFound},
		{"permission denied", connect.CodePermissionDenied, errs.ErrPermissionDenied},
		{"failed precondition", connect.CodeFailedPrecondition, errs.ErrFailedPrecondition},
		{"unavailable", connect.CodeUnavailable, errs.ErrUnavailable},
	}
	srv, f := newServer(t)
	v := newClient(t, srv)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.set(connect.NewError(c.code, errors.New("vmd says no")), false)
			defer f.set(nil, false)

			_, err := v.GetVM(context.Background(), "vm_1")
			if !errors.Is(err, c.want) {
				t.Fatalf("GetVM error = %v, want %v", err, c.want)
			}
			_, err = v.StartJob(context.Background(), &vmdv1.StartJobRequest{VmId: "vm_1"})
			if !errors.Is(err, c.want) {
				t.Fatalf("StartJob error = %v, want %v", err, c.want)
			}
		})
	}
}

func TestTokenFailureIsUnauthenticated(t *testing.T) {
	srv, _ := newServer(t)
	v := New(srv.Client(), srv.URL, func(context.Context) (string, error) {
		return "", errors.New("no key")
	})
	if _, err := v.GetVM(context.Background(), "vm_1"); !errors.Is(err, errs.ErrUnauthenticated) {
		t.Fatalf("GetVM error = %v, want unauthenticated", err)
	}
}

func TestServerClosedIsUnavailable(t *testing.T) {
	srv, _ := newServer(t)
	v := newClient(t, srv)
	srv.Close()

	if _, err := v.GetVM(context.Background(), "vm_1"); !errors.Is(err, errs.ErrUnavailable) {
		t.Fatalf("GetVM error = %v, want unavailable", err)
	}
	if err := v.PutFile(context.Background(), "vm_1", "/x", nil, 0); !errors.Is(err, errs.ErrUnavailable) {
		t.Fatalf("PutFile error = %v, want unavailable", err)
	}
}

func TestDefaultTimeoutBoundsBlockedCall(t *testing.T) {
	shortenDefault(t, 50*time.Millisecond)
	srv, f := newServer(t)
	v := newClient(t, srv)
	f.set(nil, true)

	start := time.Now()
	_, err := v.GetVM(context.Background(), "vm_1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetVM error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("blocked call took %v", elapsed)
	}
	if _, _, _, hasDL := f.recorded(); !hasDL {
		t.Fatal("server saw no deadline on a call without one")
	}
}

func TestDefaultTimeoutOnlyWhenNoEarlierDeadline(t *testing.T) {
	const tolerance = 5 * time.Second
	shortenDefault(t, time.Hour)
	srv, f := newServer(t)
	v := newClient(t, srv)

	// seenDeadline returns how far in the future the server saw the deadline, relative to now.
	seenDeadline := func(t *testing.T, ctx context.Context) time.Duration {
		t.Helper()
		if _, err := v.GetVM(ctx, "vm_1"); err != nil {
			t.Fatal(err)
		}
		_, _, dl, ok := f.recorded()
		if !ok {
			t.Fatal("server saw no deadline")
		}
		return time.Until(dl)
	}
	near := func(got, want time.Duration) bool {
		d := got - want
		return d > -tolerance && d < tolerance
	}

	t.Run("no caller deadline gets default", func(t *testing.T) {
		if got := seenDeadline(t, context.Background()); !near(got, time.Hour) {
			t.Fatalf("deadline in %v, want about 1h", got)
		}
	})
	t.Run("earlier caller deadline is kept", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if got := seenDeadline(t, ctx); !near(got, 10*time.Minute) {
			t.Fatalf("deadline in %v, want about 10m", got)
		}
	})
	t.Run("later caller deadline is capped at default", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		if got := seenDeadline(t, ctx); !near(got, time.Hour) {
			t.Fatalf("deadline in %v, want about 1h", got)
		}
	})
}

func TestCallerDeadlineRespected(t *testing.T) {
	shortenDefault(t, time.Hour)
	srv, f := newServer(t)
	v := newClient(t, srv)
	f.set(nil, true)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := v.GetVM(ctx, "vm_1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetVM error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("call ignored caller deadline; took %v", elapsed)
	}
}
