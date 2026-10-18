package nodeserver_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/fake"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/node"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/nodeserver"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/remote"
)

const gib = 1 << 30

// setup starts a node API backed by a fake driver and returns a remote client for it. The client
// presents tok; tests that need a different identity pass their own.
type env struct {
	fake   *fake.Driver
	srv    *httptest.Server
	issuer *authz.Issuer
	verify *authz.Verifier
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pub, priv, err := authz.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	iss := &authz.Issuer{KeyID: "vmd", Key: priv}
	v := &authz.Verifier{Keys: map[string]authz.TrustedKey{"vmd": {Public: pub}}}
	d := fake.New(fake.Config{CPUs: 8, MemoryMB: 16384, DiskBytesTotal: 100 * gib, DiskBytesFree: 90 * gib})
	d.Exec = func(_ string, spec *nodev1.JobSpec) (int, string, string, time.Duration) {
		if spec.GetJobId() == "slow" {
			return 0, "", "", time.Hour
		}
		return 0, "ok\n", "", 0
	}
	path, h := nodeserver.New(d, v)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{fake: d, srv: srv, issuer: iss, verify: v}
}

// mint returns a token for a principal with the given bindings.
func (e *env) mint(t *testing.T, subject string, typ authz.PrincipalType, bindings ...authz.Binding) string {
	t.Helper()
	tok, err := e.issuer.Mint(authz.Principal{Subject: subject, Type: typ, Bindings: bindings}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func nodeToken(t *testing.T, e *env) string {
	return e.mint(t, "svc:vmd", authz.Service, authz.Binding{Project: authz.AnyProject, Role: authz.RoleNode})
}

func (e *env) client(tok string) *remote.Driver {
	return remote.New(e.srv.Client(), e.srv.URL, authz.StaticToken(tok))
}

// must unwraps a call that is expected to succeed. A failure panics, which the testing package
// reports as a failed test with the error and a stack trace.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func wantErr(t *testing.T, err error, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want errors.Is %v", err, want)
	}
}

// TestDriverSurfaceEndToEnd runs the whole node.Driver surface over HTTP, in dependency order.
func TestDriverSurfaceEndToEnd(t *testing.T) {
	e := newEnv(t)
	var d node.Driver = e.client(nodeToken(t, e)) // the remote client must satisfy node.Driver
	ctx := context.Background()

	// capacity
	c := must(d.GetCapacity(ctx, &nodev1.GetCapacityRequest{})).GetCapacity()
	if c.GetCpusTotal() != 8 || c.GetMemoryMbTotal() != 16384 {
		t.Fatalf("capacity = %v", c)
	}

	// images and disks
	if got := must(d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "img1", Path: "/srv/img1.qcow2"})).GetSizeBytes(); got != gib {
		t.Fatalf("image size = %d", got)
	}
	_, err := d.RegisterImage(ctx, &nodev1.RegisterImageRequest{ImageId: "img1"})
	wantErr(t, err, errs.ErrAlreadyExists)
	must(d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "d1", BackingImageId: "img1", SizeGb: 10}))
	must(d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "d2", SizeGb: 2}))
	_, err = d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "d3", SizeGb: 0})
	wantErr(t, err, errs.ErrInvalidArgument)
	_, err = d.CreateDisk(ctx, &nodev1.CreateDiskRequest{DiskId: "d4", BackingImageId: "missing", SizeGb: 1})
	wantErr(t, err, errs.ErrNotFound)
	must(d.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: "d2", SizeGb: 3}))
	_, err = d.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: "d2", SizeGb: 1})
	wantErr(t, err, errs.ErrFailedPrecondition)
	if got := must(d.SnapshotDisk(ctx, &nodev1.SnapshotDiskRequest{DiskId: "d2", ImageId: "snap1"})).GetSizeBytes(); got != 3*gib {
		t.Fatalf("snapshot size = %d", got)
	}

	// VM lifecycle
	_, err = d.StartVM(ctx, &nodev1.StartVMRequest{VmId: "vm1", Name: "web", Cpus: 2, MemoryMb: 512, DiskIds: []string{"nope"}})
	wantErr(t, err, errs.ErrNotFound)
	rt := must(d.StartVM(ctx, &nodev1.StartVMRequest{VmId: "vm1", Name: "web", Cpus: 2, MemoryMb: 512, DiskIds: []string{"d1"}})).GetRuntime()
	if rt.GetState() != nodev1.RuntimeState_RUNTIME_STATE_RUNNING {
		t.Fatalf("start state = %v", rt.GetState())
	}
	if s := must(d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm1"})).GetRuntime().GetState(); s != nodev1.RuntimeState_RUNTIME_STATE_RUNNING {
		t.Fatalf("runtime = %v", s)
	}
	if s := must(d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "ghost"})).GetRuntime().GetState(); s != nodev1.RuntimeState_RUNTIME_STATE_ABSENT {
		t.Fatalf("unknown vm runtime = %v", s)
	}
	_, err = d.ForgetVM(ctx, &nodev1.ForgetVMRequest{VmId: "vm1"})
	wantErr(t, err, errs.ErrFailedPrecondition)
	_, err = d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: "d1"})
	wantErr(t, err, errs.ErrFailedPrecondition)

	// jobs
	must(d.StartJob(ctx, &nodev1.StartJobRequest{VmId: "vm1", Spec: &nodev1.JobSpec{JobId: "fast", Command: "echo ok"}}))
	poll := must(d.PollJob(ctx, &nodev1.PollJobRequest{VmId: "vm1", JobId: "fast"}))
	if poll.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_EXITED || poll.GetExitCode() != 0 || string(poll.GetStdout()) != "ok\n" {
		t.Fatalf("fast poll = %v", poll)
	}
	must(d.StartJob(ctx, &nodev1.StartJobRequest{VmId: "vm1", Spec: &nodev1.JobSpec{JobId: "slow", Command: "sleep"}}))
	if s := must(d.PollJob(ctx, &nodev1.PollJobRequest{VmId: "vm1", JobId: "slow"})).GetState(); s != nodev1.GuestJobState_GUEST_JOB_STATE_RUNNING {
		t.Fatalf("slow poll state = %v", s)
	}
	must(d.CancelJob(ctx, &nodev1.CancelJobRequest{VmId: "vm1", JobId: "slow"}))
	cancelled := must(d.PollJob(ctx, &nodev1.PollJobRequest{VmId: "vm1", JobId: "slow"}))
	if !cancelled.GetCancelled() || cancelled.GetExitCode() != 130 {
		t.Fatalf("cancelled poll = %v", cancelled)
	}
	if s := must(d.PollJob(ctx, &nodev1.PollJobRequest{VmId: "vm1", JobId: "nope"})).GetState(); s != nodev1.GuestJobState_GUEST_JOB_STATE_MISSING {
		t.Fatalf("unknown job state = %v", s)
	}
	_, err = d.CancelJob(ctx, &nodev1.CancelJobRequest{VmId: "vm1", JobId: "nope"})
	wantErr(t, err, errs.ErrNotFound)

	// files and metrics
	must(d.PutFile(ctx, &nodev1.PutFileRequest{VmId: "vm1", Path: "/etc/motd", Content: []byte("hi")}))
	if got := string(must(d.GetFile(ctx, &nodev1.GetFileRequest{VmId: "vm1", Path: "/etc/motd"})).GetContent()); got != "hi" {
		t.Fatalf("file = %q", got)
	}
	_, err = d.GetFile(ctx, &nodev1.GetFileRequest{VmId: "vm1", Path: "/missing"})
	wantErr(t, err, errs.ErrNotFound)
	m := must(d.GetVMMetrics(ctx, &nodev1.GetVMMetricsRequest{VmId: "vm1", DiskIds: []string{"d1"}}))
	if m.GetCpuCores() != 2 || len(m.GetFilesystems()) != 1 || m.GetFilesystems()[0].GetDiskId() != "d1" {
		t.Fatalf("metrics = %v", m)
	}

	// teardown
	must(d.StopVM(ctx, &nodev1.StopVMRequest{VmId: "vm1"}))
	rt = must(d.GetVMRuntime(ctx, &nodev1.GetVMRuntimeRequest{VmId: "vm1"})).GetRuntime()
	if rt.GetState() != nodev1.RuntimeState_RUNTIME_STATE_EXITED || rt.GetReason() != "stopped" {
		t.Fatalf("stopped runtime = %v", rt)
	}
	must(d.ForgetVM(ctx, &nodev1.ForgetVMRequest{VmId: "vm1"}))
	must(d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: "d1"}))
	_, err = d.DeleteDisk(ctx, &nodev1.DeleteDiskRequest{DiskId: "d1"})
	wantErr(t, err, errs.ErrNotFound)
	must(d.DeleteImage(ctx, &nodev1.DeleteImageRequest{ImageId: "img1"}))
	_, err = d.DeleteImage(ctx, &nodev1.DeleteImageRequest{ImageId: "img1"})
	wantErr(t, err, errs.ErrNotFound)
}

func TestAuthorization(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	all := authz.Binding{Project: authz.AnyProject, Role: authz.RoleAdmin}

	cases := []struct {
		name string
		tok  string
		want error
	}{
		{"no token", "", errs.ErrUnauthenticated},
		{"garbage token", "not.a.token", errs.ErrUnauthenticated},
		{"editor token", e.mint(t, "user:robert", authz.User, authz.Binding{Project: "p1", Role: authz.RoleEditor}), errs.ErrPermissionDenied},
		{"admin token everywhere", e.mint(t, "user:robert", authz.User, all), errs.ErrPermissionDenied},
		{"node role on one project only", e.mint(t, "svc:vmd", authz.Service, authz.Binding{Project: "p1", Role: authz.RoleNode}), errs.ErrPermissionDenied},
		{"service token", e.mint(t, "svc:agentd", authz.Service, authz.Binding{Project: authz.AnyProject, Role: authz.RoleService}), errs.ErrPermissionDenied},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(e.fake.Calls())
			_, err := e.client(c.tok).GetCapacity(ctx, &nodev1.GetCapacityRequest{})
			wantErr(t, err, c.want)
			if n := len(e.fake.Calls()); n != before {
				t.Fatalf("rejected call reached the driver (%d calls)", n-before)
			}
		})
	}
}

// panicky panics in GetCapacity; the other methods are never reached in this test.
type panicky struct{ node.Driver }

func (panicky) GetCapacity(context.Context, *nodev1.GetCapacityRequest) (*nodev1.GetCapacityResponse, error) {
	panic("boom")
}

func TestPanicIsInternalError(t *testing.T) {
	pub, priv, _ := authz.GenerateKey()
	iss := &authz.Issuer{KeyID: "vmd", Key: priv}
	v := &authz.Verifier{Keys: map[string]authz.TrustedKey{"vmd": {Public: pub}}}
	path, h := nodeserver.New(panicky{}, v)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tok, _ := iss.Mint(authz.Principal{Subject: "svc:vmd", Type: authz.Service,
		Bindings: []authz.Binding{{Project: authz.AnyProject, Role: authz.RoleNode}}}, time.Minute)
	d := remote.New(srv.Client(), srv.URL, authz.StaticToken(tok))
	_, err := d.GetCapacity(context.Background(), &nodev1.GetCapacityRequest{})
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("err = %v, want internal", err)
	}
}

func TestFakeDownIsUnavailable(t *testing.T) {
	e := newEnv(t)
	d := e.client(nodeToken(t, e))
	e.fake.SetDown(true)
	_, err := d.GetCapacity(context.Background(), &nodev1.GetCapacityRequest{})
	wantErr(t, err, errs.ErrUnavailable)
	e.fake.SetDown(false)
	must(d.GetCapacity(context.Background(), &nodev1.GetCapacityRequest{}))
}

func TestServerClosedIsUnavailable(t *testing.T) {
	e := newEnv(t)
	d := e.client(nodeToken(t, e))
	e.srv.Close()
	_, err := d.GetCapacity(context.Background(), &nodev1.GetCapacityRequest{})
	wantErr(t, err, errs.ErrUnavailable)
}
