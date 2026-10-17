package service_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.uber.org/mock/gomock"

	nodev1 "example.com/homecloud/gen/homecloud/node/v1"
	vmdv1 "example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/gen/homecloud/vmd/v1/vmdv1connect"
	"example.com/homecloud/internal/auth"
	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/vmd/domain"
	nodemock "example.com/homecloud/internal/vmd/node/mock"
	"example.com/homecloud/internal/vmd/scheduler"
	"example.com/homecloud/internal/vmd/service"
	"example.com/homecloud/internal/vmd/service/servicemock"
	"example.com/homecloud/internal/vmd/store"
	storemock "example.com/homecloud/internal/vmd/store/mock"
)

// gmSecret is a store/driver failure text that must never reach an API client.
const gmSecret = "pq: password authentication failed for user hc_prod"

var gmNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// gmH is a vmd API served over HTTP with a gomock store, a gomock node manager and a gomock node driver.
type gmH struct {
	t     *testing.T
	ctrl  *gomock.Controller
	st    *storemock.MockStore
	nodes *servicemock.MockNodes
	drv   *nodemock.MockDriver
	srv   *httptest.Server
	iss   *auth.Issuer
}

type gmClients struct {
	VM   vmdv1connect.VMServiceClient
	Disk vmdv1connect.DiskServiceClient
	Exec vmdv1connect.ExecServiceClient
}

func gmNewH(t *testing.T) *gmH {
	t.Helper()
	ctrl := gomock.NewController(t)
	h := &gmH{t: t, ctrl: ctrl,
		st:    storemock.NewMockStore(ctrl),
		nodes: servicemock.NewMockNodes(ctrl),
		drv:   nodemock.NewMockDriver(ctrl)}
	pub, priv, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	h.iss = &auth.Issuer{KeyID: "gm", Key: priv}
	ver := &auth.Verifier{Keys: map[string]auth.TrustedKey{"gm": {Public: pub}}}
	cfg := service.Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h.srv = httptest.NewServer(service.NewHandler(service.Deps{Store: h.st, Nodes: h.nodes, Verifier: ver, Config: cfg}))
	t.Cleanup(h.srv.Close)
	return h
}

// as returns clients authenticated as one user holding role in project.
func (h *gmH) as(role auth.Role, project string) gmClients {
	h.t.Helper()
	tok, err := h.iss.Mint(auth.Principal{Subject: "user:gm-" + string(role), Type: auth.User,
		Bindings: []auth.Binding{{Project: project, Role: role}}}, time.Hour)
	if err != nil {
		h.t.Fatal(err)
	}
	opt := connect.WithInterceptors(auth.ClientInterceptor(auth.StaticToken(tok)))
	hc := h.srv.Client()
	return gmClients{
		VM:   vmdv1connect.NewVMServiceClient(hc, h.srv.URL, opt),
		Disk: vmdv1connect.NewDiskServiceClient(hc, h.srv.URL, opt),
		Exec: vmdv1connect.NewExecServiceClient(hc, h.srv.URL, opt),
	}
}

// gmVM returns a fresh stopped VM in project p1 on node n1, adjusted by mut. Fresh every call because the
// handlers mutate what the store returns.
func gmVM(mut ...func(*domain.VM)) *domain.VM {
	v := &domain.VM{ID: "vm_1", ProjectID: "p1", Name: "web", NodeID: "n1", CPUs: 2, MemoryMB: 2048,
		DiskIDs: []string{"disk_1"}, Desired: domain.PowerStopped, State: domain.VMStopped, Generation: 1,
		CreatedAt: gmNow, UpdatedAt: gmNow}
	for _, f := range mut {
		f(v)
	}
	return v
}

func gmRunning(v *domain.VM) { v.Desired, v.State = domain.PowerRunning, domain.VMRunning }

func gmImage() *domain.Image {
	return &domain.Image{ID: "img_1", Name: "ubuntu", NodeID: "n1", State: domain.ImageReady, CreatedAt: gmNow, UpdatedAt: gmNow}
}

func gmDisk(mut ...func(*domain.Disk)) *domain.Disk {
	d := &domain.Disk{ID: "disk_1", ProjectID: "p1", Name: "data", NodeID: "n1", SizeGB: 10,
		State: domain.DiskReady, CreatedAt: gmNow, UpdatedAt: gmNow}
	for _, f := range mut {
		f(d)
	}
	return d
}

// gmAssertInternal requires a generic Internal error whose text mentions nothing from the store failure.
func gmAssertInternal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if c := connect.CodeOf(err); c != connect.CodeInternal {
		t.Fatalf("code = %v, want %v (err %v)", c, connect.CodeInternal, err)
	}
	if msg := err.Error(); !strings.Contains(msg, "internal error") || strings.Contains(msg, "hc_prod") || strings.Contains(msg, "password") {
		t.Fatalf("client error %q is not the generic internal error", msg)
	}
}

// TestGMStoreFailureIsGenericInternal: an arbitrary (non-sentinel) store failure at the first store
// touch of each RPC surfaces as CodeInternal with "internal error" and never the secret text.
func TestGMStoreFailureIsGenericInternal(t *testing.T) {
	boom := errors.New(gmSecret)
	cases := []struct {
		name    string
		arrange func(h *gmH)
		call    func(c gmClients) error
	}{
		{
			name:    "GetVM",
			arrange: func(h *gmH) { h.st.EXPECT().GetVM(gomock.Any(), "vm_1").Return(nil, boom) },
			call: func(c gmClients) error {
				_, err := c.VM.GetVM(context.Background(), connect.NewRequest(&vmdv1.GetVMRequest{Id: "vm_1"}))
				return err
			},
		},
		{
			name: "CreateVM",
			arrange: func(h *gmH) {
				h.st.EXPECT().GetImage(gomock.Any(), "img_1").Return(gmImage(), nil)
				h.st.EXPECT().Usage(gomock.Any(), "p1").Return(domain.Usage{}, nil)
				h.nodes.EXPECT().Info(gomock.Any()).Return([]scheduler.NodeInfo{{ID: "n1"}}, nil)
				h.st.EXPECT().Tx(gomock.Any(), gomock.Any()).Return(boom)
			},
			call: func(c gmClients) error {
				_, err := c.VM.CreateVM(context.Background(), connect.NewRequest(&vmdv1.CreateVMRequest{
					ProjectId: "p1", Name: "web", Cpus: 2, MemoryMb: 2048, DiskSizeGb: 10,
					Boot: &vmdv1.CreateVMRequest_ImageId{ImageId: "img_1"}}))
				return err
			},
		},
		{
			name:    "ListVMs",
			arrange: func(h *gmH) { h.st.EXPECT().ListVMs(gomock.Any(), gomock.Any()).Return(nil, boom) },
			call: func(c gmClients) error {
				_, err := c.VM.ListVMs(context.Background(), connect.NewRequest(&vmdv1.ListVMsRequest{ProjectId: "p1"}))
				return err
			},
		},
		{
			name: "UpdateVM",
			arrange: func(h *gmH) {
				h.st.EXPECT().GetVM(gomock.Any(), "vm_1").Return(gmVM(), nil)
				h.st.EXPECT().UpdateVM(gomock.Any(), gomock.Any()).Return(boom)
			},
			call: func(c gmClients) error {
				cpus := int32(4)
				_, err := c.VM.UpdateVM(context.Background(), connect.NewRequest(&vmdv1.UpdateVMRequest{Id: "vm_1", Cpus: &cpus}))
				return err
			},
		},
		{
			name: "StartVM",
			arrange: func(h *gmH) {
				h.st.EXPECT().GetVM(gomock.Any(), "vm_1").Return(gmVM(), nil)
				h.st.EXPECT().Usage(gomock.Any(), "p1").Return(domain.Usage{}, nil)
				h.st.EXPECT().UpdateVM(gomock.Any(), gomock.Any()).Return(boom)
			},
			call: func(c gmClients) error {
				_, err := c.VM.StartVM(context.Background(), connect.NewRequest(&vmdv1.StartVMRequest{Id: "vm_1"}))
				return err
			},
		},
		{
			name: "DeleteVM",
			arrange: func(h *gmH) {
				h.st.EXPECT().GetVM(gomock.Any(), "vm_1").Return(gmVM(), nil)
				h.st.EXPECT().UpdateVM(gomock.Any(), gomock.Any()).Return(boom)
			},
			call: func(c gmClients) error {
				_, err := c.VM.DeleteVM(context.Background(), connect.NewRequest(&vmdv1.DeleteVMRequest{Id: "vm_1"}))
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := gmNewH(t)
			tc.arrange(h)
			gmAssertInternal(t, tc.call(h.as(auth.RoleEditor, "p1")))
		})
	}
}

// TestGMStartVMConflictRetries: a persistent compare-and-swap conflict gives up after exactly 8
// GetVM+UpdateVM attempts (Aborted); a conflict that clears after two attempts succeeds.
func TestGMStartVMConflictRetries(t *testing.T) {
	cases := []struct {
		name      string
		conflicts int  // UpdateVM calls that lose the race before the rest succeed
		wantErr   bool // true: every attempt conflicts
		attempts  int
	}{
		{name: "persistent conflict aborts after 8 attempts", conflicts: 8, wantErr: true, attempts: 8},
		{name: "conflict clears after two attempts", conflicts: 2, attempts: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := gmNewH(t)
			h.st.EXPECT().GetVM(gomock.Any(), "vm_1").DoAndReturn(func(context.Context, string) (*domain.VM, error) {
				return gmVM(), nil
			}).Times(tc.attempts)
			h.st.EXPECT().Usage(gomock.Any(), "p1").Return(domain.Usage{}, nil).Times(tc.attempts)
			wantRunning := gomock.Cond(func(v *domain.VM) bool { return v.Desired == domain.PowerRunning && v.ID == "vm_1" })
			h.st.EXPECT().UpdateVM(gomock.Any(), wantRunning).Return(errs.ErrConflict).Times(tc.conflicts)
			if !tc.wantErr {
				h.st.EXPECT().UpdateVM(gomock.Any(), wantRunning).Return(nil).Times(1)
			}

			_, err := h.as(auth.RoleEditor, "p1").VM.StartVM(context.Background(),
				connect.NewRequest(&vmdv1.StartVMRequest{Id: "vm_1"}))
			if tc.wantErr {
				if c := connect.CodeOf(err); c != connect.CodeAborted {
					t.Fatalf("code = %v, want Aborted (err %v)", c, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("StartVM: %v", err)
			}
		})
	}
}

// TestGMCreateVMIdempotencyRace: the insert loses to an earlier request with the same key; the handler
// re-reads by key and answers with that VM instead of an error.
func TestGMCreateVMIdempotencyRace(t *testing.T) {
	h := gmNewH(t)
	existing := gmVM(func(v *domain.VM) { v.ID, v.IdempotencyKey = "vm_existing", "idem-1" })
	tx := storemock.NewMockStore(h.ctrl)
	tx.EXPECT().CreateDisk(gomock.Any(), gomock.Any()).Return(nil)
	tx.EXPECT().CreateVM(gomock.Any(), gomock.Any()).Return(fmt.Errorf("insert vm: %w", errs.ErrAlreadyExists))

	gomock.InOrder(
		h.st.EXPECT().GetVMByIdempotencyKey(gomock.Any(), "p1", "idem-1").Return(nil, errs.ErrNotFound),
		h.st.EXPECT().GetImage(gomock.Any(), "img_1").Return(gmImage(), nil),
		h.st.EXPECT().Usage(gomock.Any(), "p1").Return(domain.Usage{}, nil),
		h.nodes.EXPECT().Info(gomock.Any()).Return([]scheduler.NodeInfo{{ID: "n1"}}, nil),
		h.st.EXPECT().Tx(gomock.Any(), gomock.Any()).DoAndReturn(
			func(_ context.Context, fn func(store.Store) error) error { return fn(tx) }),
		h.st.EXPECT().GetVMByIdempotencyKey(gomock.Any(), "p1", "idem-1").Return(existing, nil),
	)

	resp, err := h.as(auth.RoleEditor, "p1").VM.CreateVM(context.Background(), connect.NewRequest(&vmdv1.CreateVMRequest{
		ProjectId: "p1", Name: "web", Cpus: 2, MemoryMb: 2048, DiskSizeGb: 10, IdempotencyKey: "idem-1",
		Boot: &vmdv1.CreateVMRequest_ImageId{ImageId: "img_1"}}))
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if resp.Msg.Id != "vm_existing" {
		t.Fatalf("answered with %q, want the VM the key already created (vm_existing)", resp.Msg.Id)
	}
}

// TestGMPutFileMode: an unset mode defaults to 0644 and an explicit one is passed unchanged; the driver
// receives exactly VmId, Path, Content and Mode.
func TestGMPutFileMode(t *testing.T) {
	cases := []struct {
		name     string
		mode     uint32
		wantMode uint32
	}{
		{name: "mode 0 defaults to 0644", mode: 0, wantMode: 0o644},
		{name: "explicit 0600 is passed unchanged", mode: 0o600, wantMode: 0o600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := gmNewH(t)
			h.st.EXPECT().GetVM(gomock.Any(), "vm_1").Return(gmVM(gmRunning), nil)
			h.nodes.EXPECT().Driver("n1").Return(h.drv, nil)
			h.drv.EXPECT().PutFile(gomock.Any(), gomock.Cond(func(r *nodev1.PutFileRequest) bool {
				return r.VmId == "vm_1" && r.Path == "/etc/app.conf" && string(r.Content) == "hello" && r.Mode == tc.wantMode
			})).Return(&nodev1.PutFileResponse{}, nil)

			_, err := h.as(auth.RoleEditor, "p1").Exec.PutFile(context.Background(), connect.NewRequest(&vmdv1.PutFileRequest{
				VmId: "vm_1", Path: "/etc/app.conf", Content: []byte("hello"), Mode: tc.mode}))
			if err != nil {
				t.Fatalf("PutFile: %v", err)
			}
		})
	}
}

// TestGMStartJobOrderAndSpec: the job record is created before the driver starts it, the driver gets the
// exact JobSpec (default timeout 3600), and the first poll follows.
func TestGMStartJobOrderAndSpec(t *testing.T) {
	h := gmNewH(t)
	var jobID string
	gomock.InOrder(
		h.st.EXPECT().GetVM(gomock.Any(), "vm_1").Return(gmVM(gmRunning), nil),
		h.nodes.EXPECT().Driver("n1").Return(h.drv, nil),
		h.st.EXPECT().CreateJob(gomock.Any(), gomock.Cond(func(j *domain.Job) bool {
			return strings.HasPrefix(j.ID, "job_") && j.VMID == "vm_1" && j.ProjectID == "p1" &&
				j.Command == "make test" && j.Workdir == "/srv/app" && j.TimeoutSeconds == 3600 && j.State == domain.JobRunning
		})).DoAndReturn(func(_ context.Context, j *domain.Job) error { jobID = j.ID; return nil }),
		h.drv.EXPECT().StartJob(gomock.Any(), gomock.Cond(func(r *nodev1.StartJobRequest) bool {
			return r.VmId == "vm_1" && r.Spec != nil && r.Spec.JobId == jobID && strings.HasPrefix(jobID, "job_") &&
				r.Spec.Command == "make test" && r.Spec.Workdir == "/srv/app" && r.Spec.TimeoutSeconds == 3600
		})).Return(&nodev1.StartJobResponse{}, nil),
		h.nodes.EXPECT().Driver("n1").Return(h.drv, nil), // the first poll resolves the driver again
		h.drv.EXPECT().PollJob(gomock.Any(), gomock.Any()).Return(
			&nodev1.PollJobResponse{State: nodev1.GuestJobState_GUEST_JOB_STATE_RUNNING}, nil),
	)

	resp, err := h.as(auth.RoleEditor, "p1").Exec.StartJob(context.Background(), connect.NewRequest(&vmdv1.StartJobRequest{
		VmId: "vm_1", Command: "make test", Workdir: "/srv/app"}))
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if got := resp.Msg.GetJob().GetId(); got != jobID {
		t.Fatalf("response job %q, want %q", got, jobID)
	}
}

// TestGMStartJobDriverFailureRecordsFailed: when the driver cannot start the job, the record is marked
// FAILED and its reason does not carry the node address the driver error contained.
func TestGMStartJobDriverFailureRecordsFailed(t *testing.T) {
	h := gmNewH(t)
	driverErr := errors.New("dial tcp 192.0.2.5:7000: connection refused (token gm-secret)")
	gomock.InOrder(
		h.st.EXPECT().GetVM(gomock.Any(), "vm_1").Return(gmVM(gmRunning), nil),
		h.nodes.EXPECT().Driver("n1").Return(h.drv, nil),
		h.st.EXPECT().CreateJob(gomock.Any(), gomock.Any()).Return(nil),
		h.drv.EXPECT().StartJob(gomock.Any(), gomock.Any()).Return(nil, driverErr),
		h.st.EXPECT().UpdateJob(gomock.Any(), gomock.Cond(func(j *domain.Job) bool {
			return j.State == domain.JobFailed && strings.Contains(j.KilledReason, "could not start") &&
				!strings.Contains(j.KilledReason, "192.0.2.5") && !strings.Contains(j.KilledReason, "gm-secret")
		})).Return(nil),
	)

	_, err := h.as(auth.RoleEditor, "p1").Exec.StartJob(context.Background(), connect.NewRequest(&vmdv1.StartJobRequest{
		VmId: "vm_1", Command: "make test", Workdir: "/srv/app"}))
	if c := connect.CodeOf(err); c != connect.CodeUnavailable {
		t.Fatalf("code = %v, want Unavailable (err %v)", c, err)
	}
	if strings.Contains(err.Error(), "192.0.2.5") || strings.Contains(err.Error(), "gm-secret") {
		t.Fatalf("client error leaks driver text: %v", err)
	}
}

// TestGMRBACShortCircuits: a caller without vm.start in the VM's project is refused after the read,
// before any write or driver call; a caller from another project sees NotFound.
func TestGMRBACShortCircuits(t *testing.T) {
	cases := []struct {
		name     string
		role     auth.Role
		project  string
		wantCode connect.Code
	}{
		{name: "viewer lacks vm.start", role: auth.RoleViewer, project: "p1", wantCode: connect.CodePermissionDenied},
		{name: "other project is NotFound", role: auth.RoleEditor, project: "p2", wantCode: connect.CodeNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := gmNewH(t)
			// Only the read is expected: any UpdateVM, Usage or driver call fails the test as unexpected.
			h.st.EXPECT().GetVM(gomock.Any(), "vm_1").Return(gmVM(), nil)
			_, err := h.as(tc.role, tc.project).VM.StartVM(context.Background(),
				connect.NewRequest(&vmdv1.StartVMRequest{Id: "vm_1"}))
			if c := connect.CodeOf(err); c != tc.wantCode {
				t.Fatalf("code = %v, want %v (err %v)", c, tc.wantCode, err)
			}
		})
	}
}

// TestGMResizeDiskOrder: GetDisk, quota, node driver, driver.ResizeDisk, then UpdateDisk. When the driver
// fails the disk row is never written.
func TestGMResizeDiskOrder(t *testing.T) {
	cases := []struct {
		name      string
		driverErr error
	}{
		{name: "success persists the new size"},
		{name: "driver failure leaves the row alone", driverErr: errors.New("node 192.0.2.5 exploded: " + gmSecret)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := gmNewH(t)
			seq := []any{
				h.st.EXPECT().GetDisk(gomock.Any(), "disk_1").Return(gmDisk(), nil),
				h.st.EXPECT().Usage(gomock.Any(), "p1").Return(domain.Usage{}, nil),
				h.nodes.EXPECT().Driver("n1").Return(h.drv, nil),
				h.drv.EXPECT().ResizeDisk(gomock.Any(), gomock.Cond(func(r *nodev1.ResizeDiskRequest) bool {
					return r.DiskId == "disk_1" && r.SizeGb == 20
				})).DoAndReturn(func(context.Context, *nodev1.ResizeDiskRequest) (*nodev1.ResizeDiskResponse, error) {
					if tc.driverErr != nil {
						return nil, tc.driverErr
					}
					return &nodev1.ResizeDiskResponse{}, nil
				}),
			}
			if tc.driverErr == nil {
				seq = append(seq, h.st.EXPECT().UpdateDisk(gomock.Any(), gomock.Cond(func(d *domain.Disk) bool {
					return d.ID == "disk_1" && d.SizeGB == 20
				})).Return(nil))
			}
			gomock.InOrder(seq...)

			resp, err := h.as(auth.RoleEditor, "p1").Disk.ResizeDisk(context.Background(),
				connect.NewRequest(&vmdv1.ResizeDiskRequest{Id: "disk_1", SizeGb: 20}))
			if tc.driverErr != nil {
				if err == nil {
					t.Fatal("want an error from the failed driver")
				}
				if strings.Contains(err.Error(), gmSecret) || strings.Contains(err.Error(), "192.0.2.5") {
					t.Fatalf("client error leaks driver text: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResizeDisk: %v", err)
			}
			if resp.Msg.SizeGb != 20 {
				t.Fatalf("size = %d, want 20", resp.Msg.SizeGb)
			}
		})
	}
}
