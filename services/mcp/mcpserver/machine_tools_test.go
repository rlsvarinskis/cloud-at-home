package mcpserver

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1"
	agentdv1connect "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1/agentdv1connect"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	vmdv1connect "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1/vmdv1connect"
)

// machPage returns one page of items. pageLen <= 0 returns everything in one page.
func machPage[T any](items []T, pageLen int, token string) ([]T, string) {
	if pageLen <= 0 {
		return items, ""
	}
	start := 0
	if token != "" {
		start, _ = strconv.Atoi(token)
	}
	end := min(start+pageLen, len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[start:end], next
}

// machVMs is a fake vmd VM service holding the machines of one project. seq drives the state that GetVM reports,
// one step per call (the last step sticks); with no seq GetVM reports the stored state.
type machVMs struct {
	vmdv1connect.VMServiceClient
	vms       []*vmdv1.VM
	pageLen   int
	seq       []vmdv1.VMState
	gets      int // GetVM calls so far
	failGetAt int // when > 0, GetVM call number failGetAt and later fail with "vmd dropped"
	stopTo    vmdv1.VMState
	errs      map[string]error
	calls     []string
	waits     []int32
	createReq *vmdv1.CreateVMRequest
	stopReq   *vmdv1.StopVMRequest
	deleteReq *vmdv1.DeleteVMRequest
	updateReq *vmdv1.UpdateVMRequest
}

func (f *machVMs) call(name string) error {
	f.calls = append(f.calls, name)
	return f.errs[name]
}

func (f *machVMs) find(id string) (*vmdv1.VM, error) {
	for _, v := range f.vms {
		if v.GetId() == id {
			return v, nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such vm"))
}

func (f *machVMs) called(name string) bool {
	for _, c := range f.calls {
		if c == name {
			return true
		}
	}
	return false
}

func (f *machVMs) ListVMs(_ context.Context, req *connect.Request[vmdv1.ListVMsRequest]) (*connect.Response[vmdv1.ListVMsResponse], error) {
	if err := f.call("ListVMs"); err != nil {
		return nil, err
	}
	page, next := machPage(f.vms, f.pageLen, req.Msg.GetPageToken())
	return connect.NewResponse(&vmdv1.ListVMsResponse{Vms: page, NextPageToken: next}), nil
}

func (f *machVMs) GetVM(_ context.Context, req *connect.Request[vmdv1.GetVMRequest]) (*connect.Response[vmdv1.VM], error) {
	if err := f.call("GetVM"); err != nil {
		return nil, err
	}
	f.waits = append(f.waits, req.Msg.GetWaitSeconds())
	f.gets++
	if f.failGetAt > 0 && f.gets >= f.failGetAt {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("vmd dropped"))
	}
	vm, err := f.find(req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if len(f.seq) > 0 {
		vm.State = f.seq[0]
		if len(f.seq) > 1 {
			f.seq = f.seq[1:]
		}
	}
	return connect.NewResponse(proto.Clone(vm).(*vmdv1.VM)), nil
}

func (f *machVMs) CreateVM(_ context.Context, req *connect.Request[vmdv1.CreateVMRequest]) (*connect.Response[vmdv1.VM], error) {
	if err := f.call("CreateVM"); err != nil {
		return nil, err
	}
	f.createReq = req.Msg
	vm := &vmdv1.VM{Id: "vm_new", ProjectId: req.Msg.GetProjectId(), Name: req.Msg.GetName(),
		Cpus: req.Msg.GetCpus(), MemoryMb: req.Msg.GetMemoryMb(), State: vmdv1.VMState_VM_STATE_PENDING}
	f.vms = append(f.vms, vm)
	return connect.NewResponse(proto.Clone(vm).(*vmdv1.VM)), nil
}

func (f *machVMs) StartVM(_ context.Context, req *connect.Request[vmdv1.StartVMRequest]) (*connect.Response[vmdv1.VM], error) {
	if err := f.call("StartVM"); err != nil {
		return nil, err
	}
	vm, err := f.find(req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	vm.State = vmdv1.VMState_VM_STATE_STARTING
	vm.DesiredState = vmdv1.PowerState_POWER_STATE_RUNNING
	return connect.NewResponse(proto.Clone(vm).(*vmdv1.VM)), nil
}

func (f *machVMs) StopVM(_ context.Context, req *connect.Request[vmdv1.StopVMRequest]) (*connect.Response[vmdv1.VM], error) {
	if err := f.call("StopVM"); err != nil {
		return nil, err
	}
	f.stopReq = req.Msg
	vm, err := f.find(req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	st := f.stopTo
	if st == vmdv1.VMState_VM_STATE_UNSPECIFIED {
		st = vmdv1.VMState_VM_STATE_STOPPING
	}
	vm.State = st
	vm.DesiredState = vmdv1.PowerState_POWER_STATE_STOPPED
	return connect.NewResponse(proto.Clone(vm).(*vmdv1.VM)), nil
}

func (f *machVMs) UpdateVM(_ context.Context, req *connect.Request[vmdv1.UpdateVMRequest]) (*connect.Response[vmdv1.VM], error) {
	if err := f.call("UpdateVM"); err != nil {
		return nil, err
	}
	f.updateReq = req.Msg
	vm, err := f.find(req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if req.Msg.Cpus != nil {
		vm.Cpus = *req.Msg.Cpus
	}
	if req.Msg.MemoryMb != nil {
		vm.MemoryMb = *req.Msg.MemoryMb
	}
	return connect.NewResponse(proto.Clone(vm).(*vmdv1.VM)), nil
}

func (f *machVMs) DeleteVM(_ context.Context, req *connect.Request[vmdv1.DeleteVMRequest]) (*connect.Response[vmdv1.DeleteVMResponse], error) {
	if err := f.call("DeleteVM"); err != nil {
		return nil, err
	}
	f.deleteReq = req.Msg
	return connect.NewResponse(&vmdv1.DeleteVMResponse{}), nil
}

type machImages struct {
	vmdv1connect.ImageServiceClient
	imgs    []*vmdv1.Image
	pageLen int
	err     error
}

func (f *machImages) ListImages(_ context.Context, req *connect.Request[vmdv1.ListImagesRequest]) (*connect.Response[vmdv1.ListImagesResponse], error) {
	if f.err != nil {
		return nil, f.err
	}
	page, next := machPage(f.imgs, f.pageLen, req.Msg.GetPageToken())
	return connect.NewResponse(&vmdv1.ListImagesResponse{Images: page, NextPageToken: next}), nil
}

type machExec struct {
	vmdv1connect.ExecServiceClient
	jobs    []*vmdv1.Job
	pageLen int
	err     error
}

func (f *machExec) ListJobs(_ context.Context, req *connect.Request[vmdv1.ListJobsRequest]) (*connect.Response[vmdv1.ListJobsResponse], error) {
	if f.err != nil {
		return nil, f.err
	}
	var mine []*vmdv1.Job
	for _, j := range f.jobs {
		if j.GetVmId() == req.Msg.GetVmId() {
			mine = append(mine, j)
		}
	}
	page, next := machPage(mine, f.pageLen, req.Msg.GetPageToken())
	return connect.NewResponse(&vmdv1.ListJobsResponse{Jobs: page, NextPageToken: next}), nil
}

type machAgents struct {
	agentdv1connect.AgentServiceClient
	agents   []*agentdv1.Agent
	pageLen  int
	listErr  error
	delErrs  map[string]error
	attempts []string
}

func (f *machAgents) ListAgents(_ context.Context, req *connect.Request[agentdv1.ListAgentsRequest]) (*connect.Response[agentdv1.ListAgentsResponse], error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var mine []*agentdv1.Agent
	for _, a := range f.agents {
		if req.Msg.GetVmId() == "" || a.GetVmId() == req.Msg.GetVmId() {
			mine = append(mine, a)
		}
	}
	page, next := machPage(mine, f.pageLen, req.Msg.GetPageToken())
	return connect.NewResponse(&agentdv1.ListAgentsResponse{Agents: page, NextPageToken: next}), nil
}

func (f *machAgents) DeleteAgent(_ context.Context, req *connect.Request[agentdv1.DeleteAgentRequest]) (*connect.Response[agentdv1.DeleteAgentResponse], error) {
	f.attempts = append(f.attempts, req.Msg.GetId())
	if err := f.delErrs[req.Msg.GetId()]; err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentdv1.DeleteAgentResponse{}), nil
}

// machTasks serves tasks per agent, oldest first (as agentd does). errs makes ListTasks fail for one agent.
type machTasks struct {
	agentdv1connect.TaskServiceClient
	byAgent map[string][]*agentdv1.Task
	pageLen int
	errs    map[string]error
}

func (f *machTasks) ListTasks(_ context.Context, req *connect.Request[agentdv1.ListTasksRequest]) (*connect.Response[agentdv1.ListTasksResponse], error) {
	if err := f.errs[req.Msg.GetAgentId()]; err != nil {
		return nil, err
	}
	page, next := machPage(f.byAgent[req.Msg.GetAgentId()], f.pageLen, req.Msg.GetPageToken())
	return connect.NewResponse(&agentdv1.ListTasksResponse{Tasks: page, NextPageToken: next}), nil
}

func machVM(id, name string, st vmdv1.VMState, cpus int32, memMB int64) *vmdv1.VM {
	return &vmdv1.VM{Id: id, ProjectId: "p1", Name: name, Cpus: cpus, MemoryMb: memMB, State: st, Generation: 3}
}

func machImgs() *machImages {
	return &machImages{imgs: []*vmdv1.Image{
		{Id: "img-1", Name: "ubuntu-24"},
		{Id: "img-2", Name: "debian"},
	}}
}

// machWait gives one tool call a 300 ms wait budget (MaxWait minus the 5 s reserve), so waiting tests stay fast.
func machWait(c *Config) { c.MaxWait = 5300 * time.Millisecond }

func machCall(t *testing.T, c Clients, tool string, args map[string]any, mutate ...func(*Config)) (string, bool) {
	t.Helper()
	return callTool(t, newTestSession(t, c, mutate...), tool, args)
}

func machWantOK(t *testing.T, text string, isErr bool, want string) {
	t.Helper()
	if isErr || text != want {
		t.Fatalf("got (%q, isError=%v), want %q", text, isErr, want)
	}
}

func machWantErr(t *testing.T, text string, isErr bool, want string) {
	t.Helper()
	if !isErr || !strings.Contains(text, want) {
		t.Fatalf("got (%q, isError=%v), want an error containing %q", text, isErr, want)
	}
}

// --- agent_create -----------------------------------------------------------------------------------------

func TestMachCreateValidation(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"uppercase", map[string]any{"name": "Web"}, "is invalid"},
		{"underscore", map[string]any{"name": "web_1"}, "is invalid"},
		{"leading hyphen", map[string]any{"name": "-web"}, "is invalid"},
		{"too long", map[string]any{"name": strings.Repeat("a", 32)}, "is invalid"},
		{"cpus too many", map[string]any{"name": "web", "cpus": 33}, "cpus must be between 1 and 32"},
		{"cpus negative", map[string]any{"name": "web", "cpus": -1}, "cpus must be between 1 and 32"},
		{"memory too much", map[string]any{"name": "web", "memory_gb": 57}, "memory_gb must be between 1 and 56"},
		{"memory negative", map[string]any{"name": "web", "memory_gb": -4}, "memory_gb must be between 1 and 56"},
		{"disk negative", map[string]any{"name": "web", "disk_gb": -1}, "disk_gb must be at least 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vms := &machVMs{}
			text, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create", tc.args)
			machWantErr(t, text, isErr, tc.want)
			if len(vms.calls) != 0 {
				t.Fatalf("validation failure still called vmd: %v", vms.calls)
			}
		})
	}
}

func TestMachCreateNoImage(t *testing.T) {
	text, isErr := machCall(t, Clients{VMs: &machVMs{}, Images: machImgs()}, "agent_create",
		map[string]any{"name": "web"}, func(c *Config) { c.DefaultImage = "" })
	machWantErr(t, text, isErr, "no image given")
}

func TestMachCreateNewDefaults(t *testing.T) {
	vms := &machVMs{seq: []vmdv1.VMState{vmdv1.VMState_VM_STATE_STARTING, vmdv1.VMState_VM_STATE_RUNNING}}
	text, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create", map[string]any{"name": "web"}, machWait)
	machWantOK(t, text, isErr, "machine 'web' is running (1 CPU, 2 GiB)")
	r := vms.createReq
	if r.GetProjectId() != "p1" || r.GetName() != "web" || r.GetCpus() != 1 || r.GetMemoryMb() != 2048 ||
		r.GetDiskSizeGb() != 40 || !r.GetStart() || r.GetImageId() != "img-1" {
		t.Fatalf("CreateVM request = %v", r)
	}
}

func TestMachCreateExplicitSizeAndImageName(t *testing.T) {
	vms := &machVMs{seq: []vmdv1.VMState{vmdv1.VMState_VM_STATE_RUNNING}}
	text, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create",
		map[string]any{"name": "web", "cpus": 4, "memory_gb": 2, "disk_gb": 80, "image": "debian", "project": "p2"})
	machWantOK(t, text, isErr, "machine 'web' is running (4 CPUs, 2 GiB)")
	r := vms.createReq
	if r.GetProjectId() != "p2" || r.GetCpus() != 4 || r.GetMemoryMb() != 2048 || r.GetDiskSizeGb() != 80 ||
		r.GetImageId() != "img-2" {
		t.Fatalf("CreateVM request = %v", r)
	}
}

func TestMachCreateImageByID(t *testing.T) {
	vms := &machVMs{seq: []vmdv1.VMState{vmdv1.VMState_VM_STATE_RUNNING}}
	_, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create",
		map[string]any{"name": "web", "image": "img-2"})
	if isErr || vms.createReq.GetImageId() != "img-2" {
		t.Fatalf("image ID not used: isErr=%v req=%v", isErr, vms.createReq)
	}
}

func TestMachCreateImageLookupErrors(t *testing.T) {
	cases := []struct {
		name string
		imgs *machImages
		ref  string
		want string
	}{
		{"not found lists names", machImgs(), "fedora", "available images: debian, ubuntu-24"},
		{"no images", &machImages{}, "fedora", "the project has no images"},
		{"ambiguous name", &machImages{imgs: []*vmdv1.Image{{Id: "a", Name: "x"}, {Id: "b", Name: "x"}}}, "x", "2 images are named"},
		{"list fails", &machImages{err: connect.NewError(connect.CodeInternal, errors.New("db down"))}, "fedora", "db down"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vms := &machVMs{}
			text, isErr := machCall(t, Clients{VMs: vms, Images: tc.imgs}, "agent_create",
				map[string]any{"name": "web", "image": tc.ref})
			machWantErr(t, text, isErr, tc.want)
			if vms.called("CreateVM") {
				t.Fatal("CreateVM called after an image lookup failure")
			}
		})
	}
}

func TestMachCreateImagePaged(t *testing.T) {
	imgs := machImgs()
	imgs.pageLen = 1
	vms := &machVMs{seq: []vmdv1.VMState{vmdv1.VMState_VM_STATE_RUNNING}}
	_, isErr := machCall(t, Clients{VMs: vms, Images: imgs}, "agent_create", map[string]any{"name": "web", "image": "debian"})
	if isErr || vms.createReq.GetImageId() != "img-2" {
		t.Fatalf("image on second page not found: isErr=%v req=%v", isErr, vms.createReq)
	}
}

func TestMachCreateBooting(t *testing.T) {
	vms := &machVMs{seq: []vmdv1.VMState{vmdv1.VMState_VM_STATE_PENDING}}
	text, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create", map[string]any{"name": "web"}, machWait)
	machWantOK(t, text, isErr, "machine 'web' is booting (agent_list shows when it is ready)")
	if len(vms.waits) < 2 {
		t.Fatalf("expected repeated GetVM calls until the budget ran out, got %d", len(vms.waits))
	}
	for _, w := range vms.waits {
		if w < 0 || w > 5 {
			t.Fatalf("GetVM wait_seconds %d outside 0..5 (MaxWait-derived cap)", w)
		}
	}
}

func TestMachCreateBudgetSpentImmediately(t *testing.T) {
	// The default test MaxWait (2 s) leaves no budget after the 5 s reserve: one GetVM with wait 0, then report.
	vms := &machVMs{seq: []vmdv1.VMState{vmdv1.VMState_VM_STATE_PENDING}}
	text, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create", map[string]any{"name": "web"})
	machWantOK(t, text, isErr, "machine 'web' is booting (agent_list shows when it is ready)")
	if len(vms.waits) != 1 || vms.waits[0] != 0 {
		t.Fatalf("waits = %v, want one call with wait 0", vms.waits)
	}
}

func TestMachCreateErrorState(t *testing.T) {
	vms := &machVMs{seq: []vmdv1.VMState{vmdv1.VMState_VM_STATE_ERROR}}
	text, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create", map[string]any{"name": "web"})
	machWantErr(t, text, isErr, "machine 'web' is error")
}

func TestMachCreateErrorStateReason(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_ERROR, 8, 16384)}}
	vms.vms[0].StateReason = "no capacity on any node"
	text, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create", map[string]any{"name": "web"})
	machWantErr(t, text, isErr, "machine 'web' is error: no capacity on any node")
}

func TestMachCreateExistingRunning(t *testing.T) {
	for _, st := range []vmdv1.VMState{vmdv1.VMState_VM_STATE_RUNNING, vmdv1.VMState_VM_STATE_STARTING} {
		vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", st, 8, 16384)}}
		text, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create", map[string]any{"name": "web"})
		machWantErr(t, text, isErr, "machine 'web' is already running (agent_resize changes CPUs/memory)")
		if vms.called("StartVM") || vms.called("CreateVM") {
			t.Fatalf("state %v: VM was started or created again: %v", st, vms.calls)
		}
	}
}

func TestMachCreateExistingStopped(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_STOPPED, 8, 16384)},
		seq: []vmdv1.VMState{vmdv1.VMState_VM_STATE_RUNNING}}
	text, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create", map[string]any{"name": "web"})
	machWantOK(t, text, isErr, "machine 'web' is running (8 CPUs, 16 GiB)")
	if !vms.called("StartVM") || vms.called("CreateVM") {
		t.Fatalf("expected StartVM and no CreateVM, got %v", vms.calls)
	}
}

func TestMachCreateExistingStoppedStartFails(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_STOPPED, 8, 16384)},
		errs: map[string]error{"StartVM": connect.NewError(connect.CodeFailedPrecondition, errors.New("being deleted"))}}
	text, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create", map[string]any{"name": "web"})
	machWantErr(t, text, isErr, "failed_precondition: being deleted")
}

func TestMachCreateExistingOtherState(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_PENDING, 8, 16384)}}
	text, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create", map[string]any{"name": "web"})
	machWantErr(t, text, isErr, "machine 'web' is pending")
}

func TestMachCreateCreateFails(t *testing.T) {
	vms := &machVMs{errs: map[string]error{"CreateVM": connect.NewError(connect.CodeResourceExhausted, errors.New("quota"))}}
	text, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create", map[string]any{"name": "web"})
	machWantErr(t, text, isErr, "resource_exhausted: quota")
}

func TestMachCreateWaitFails(t *testing.T) {
	vms := &machVMs{errs: map[string]error{"GetVM": connect.NewError(connect.CodeUnavailable, errors.New("vmd gone"))}}
	text, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create", map[string]any{"name": "web"})
	machWantErr(t, text, isErr, "vmd gone")
}

func TestMachCreateLookupFails(t *testing.T) {
	vms := &machVMs{errs: map[string]error{"ListVMs": connect.NewError(connect.CodeUnavailable, errors.New("no vmd"))}}
	text, isErr := machCall(t, Clients{VMs: vms, Images: machImgs()}, "agent_create", map[string]any{"name": "web"})
	machWantErr(t, text, isErr, "no vmd")
}

// --- agent_list ------------------------------------------------------------------------------------------

func TestMachListEmpty(t *testing.T) {
	text, isErr := machCall(t, Clients{VMs: &machVMs{}}, "agent_list", map[string]any{})
	machWantOK(t, text, isErr, "(no machines)")
}

func TestMachListWithoutAgentd(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{
		machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384),
		func() *vmdv1.VM {
			v := machVM("vm_2", "db", vmdv1.VMState_VM_STATE_ERROR, 4, 2560)
			v.StateReason = "boot failed"
			return v
		}(),
	}}
	text, isErr := machCall(t, Clients{VMs: vms}, "agent_list", map[string]any{"project": "p2"})
	machWantOK(t, text, isErr, "web: running, 8 CPUs / 16 GiB\ndb: error, 4 CPUs / 2.5 GiB, reason: boot failed")
}

func TestMachListWithTasks(t *testing.T) {
	vms := &machVMs{pageLen: 2, vms: []*vmdv1.VM{
		machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384),
		func() *vmdv1.VM {
			v := machVM("vm_2", "db", vmdv1.VMState_VM_STATE_ERROR, 4, 2560)
			v.StateReason = "boot failed"
			return v
		}(),
		machVM("vm_3", "idle", vmdv1.VMState_VM_STATE_STOPPED, 2, 4096),
	}}
	ags := &machAgents{pageLen: 1, agents: []*agentdv1.Agent{
		{Id: "a1", VmId: "vm_1"},
		{Id: "a2", VmId: "vm_1"},
		{Id: "a3", VmId: "vm_2"},
	}}
	tks := &machTasks{pageLen: 1, byAgent: map[string][]*agentdv1.Task{
		"a1": {{Id: "task_01", State: agentdv1.TaskState_TASK_STATE_SUCCEEDED}},
		"a2": {{Id: "task_02", State: agentdv1.TaskState_TASK_STATE_RUNNING}, {Id: "task_05", State: agentdv1.TaskState_TASK_STATE_FAILED}},
		"a3": {},
	}}
	text, isErr := machCall(t, Clients{VMs: vms, Agents: ags, Tasks: tks}, "agent_list", map[string]any{})
	machWantOK(t, text, isErr, strings.Join([]string{
		"web: running, 8 CPUs / 16 GiB, task: failed",
		"db: error, 4 CPUs / 2.5 GiB, reason: boot failed, task: none",
		"idle: stopped, 2 CPUs / 4 GiB, task: none",
	}, "\n"))
}

func TestMachListTaskLabelsDone(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)}}
	ags := &machAgents{agents: []*agentdv1.Agent{{Id: "a1", VmId: "vm_1"}}}
	tks := &machTasks{byAgent: map[string][]*agentdv1.Task{"a1": {{Id: "task_01", State: agentdv1.TaskState_TASK_STATE_SUCCEEDED}}}}
	text, isErr := machCall(t, Clients{VMs: vms, Agents: ags, Tasks: tks}, "agent_list", map[string]any{})
	machWantOK(t, text, isErr, "web: running, 8 CPUs / 16 GiB, task: done")
}

func TestMachListAgentsFail(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)}}
	ags := &machAgents{listErr: connect.NewError(connect.CodeUnavailable, errors.New("agentd down"))}
	tks := &machTasks{}
	text, isErr := machCall(t, Clients{VMs: vms, Agents: ags, Tasks: tks}, "agent_list", map[string]any{})
	machWantOK(t, text, isErr, "web: running, 8 CPUs / 16 GiB, task: unknown")
}

func TestMachListTasksFailForOneAgent(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{
		machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384),
		machVM("vm_2", "db", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384),
	}}
	ags := &machAgents{agents: []*agentdv1.Agent{{Id: "a1", VmId: "vm_1"}, {Id: "a2", VmId: "vm_2"}}}
	tks := &machTasks{
		byAgent: map[string][]*agentdv1.Task{"a2": {{Id: "task_01", State: agentdv1.TaskState_TASK_STATE_QUEUED}}},
		errs:    map[string]error{"a1": connect.NewError(connect.CodeUnavailable, errors.New("x"))},
	}
	text, isErr := machCall(t, Clients{VMs: vms, Agents: ags, Tasks: tks}, "agent_list", map[string]any{})
	machWantOK(t, text, isErr, "web: running, 8 CPUs / 16 GiB, task: unknown\ndb: running, 8 CPUs / 16 GiB, task: queued")
}

func TestMachListNoTaskClient(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)}}
	text, isErr := machCall(t, Clients{VMs: vms, Agents: &machAgents{}}, "agent_list", map[string]any{})
	machWantOK(t, text, isErr, "web: running, 8 CPUs / 16 GiB")
}

func TestMachListVMsFails(t *testing.T) {
	vms := &machVMs{errs: map[string]error{"ListVMs": connect.NewError(connect.CodeUnavailable, errors.New("vmd down"))}}
	text, isErr := machCall(t, Clients{VMs: vms}, "agent_list", map[string]any{})
	machWantErr(t, text, isErr, "vmd down")
}

func TestMachListVMPaging(t *testing.T) {
	vms := &machVMs{pageLen: 1, vms: []*vmdv1.VM{
		machVM("vm_1", "a", vmdv1.VMState_VM_STATE_STOPPED, 1, 1024),
		machVM("vm_2", "b", vmdv1.VMState_VM_STATE_STOPPED, 1, 1024),
		machVM("vm_3", "c", vmdv1.VMState_VM_STATE_STOPPED, 1, 1024),
	}}
	text, isErr := machCall(t, Clients{VMs: vms}, "agent_list", map[string]any{})
	machWantOK(t, text, isErr, "a: stopped, 1 CPU / 1 GiB\nb: stopped, 1 CPU / 1 GiB\nc: stopped, 1 CPU / 1 GiB")
}

// --- agent_stop ------------------------------------------------------------------------------------------

func TestMachStop(t *testing.T) {
	cases := []struct {
		name   string
		stopTo vmdv1.VMState
		want   string
	}{
		{"stopping", vmdv1.VMState_VM_STATE_UNSPECIFIED, "machine 'web' is stopping"},
		{"stopped", vmdv1.VMState_VM_STATE_STOPPED, "machine 'web' is stopped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vms := &machVMs{stopTo: tc.stopTo, vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)}}
			text, isErr := machCall(t, Clients{VMs: vms}, "agent_stop", map[string]any{"name": "web"})
			machWantOK(t, text, isErr, tc.want)
			if vms.stopReq.GetId() != "vm_1" || vms.stopReq.GetForce() {
				t.Fatalf("StopVM request = %v, want graceful stop of vm_1", vms.stopReq)
			}
		})
	}
}

func TestMachStopErrorState(t *testing.T) {
	vms := &machVMs{stopTo: vmdv1.VMState_VM_STATE_ERROR, vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)}}
	vms.vms[0].StateReason = "node lost"
	text, isErr := machCall(t, Clients{VMs: vms}, "agent_stop", map[string]any{"name": "web"})
	machWantErr(t, text, isErr, "machine 'web' is error: node lost")
}

func TestMachStopNotFound(t *testing.T) {
	text, isErr := machCall(t, Clients{VMs: &machVMs{}}, "agent_stop", map[string]any{"name": "nope"})
	machWantErr(t, text, isErr, `no machine named "nope"`)
}

func TestMachStopDownstreamError(t *testing.T) {
	vms := &machVMs{errs: map[string]error{"StopVM": connect.NewError(connect.CodePermissionDenied, errors.New("not yours"))},
		vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)}}
	text, isErr := machCall(t, Clients{VMs: vms}, "agent_stop", map[string]any{"name": "web"})
	machWantErr(t, text, isErr, "permission_denied: not yours")
}

// --- agent_destroy ---------------------------------------------------------------------------------------

func TestMachDestroy(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_STOPPED, 8, 16384)}}
	ags := &machAgents{
		agents:  []*agentdv1.Agent{{Id: "a1", VmId: "vm_1"}, {Id: "a2", VmId: "vm_1"}, {Id: "a3", VmId: "vm_other"}},
		delErrs: map[string]error{"a2": connect.NewError(connect.CodeNotFound, errors.New("gone"))},
	}
	text, isErr := machCall(t, Clients{VMs: vms, Agents: ags}, "agent_destroy", map[string]any{"name": "web"})
	machWantOK(t, text, isErr, "machine 'web' destroyed")
	if strings.Join(ags.attempts, ",") != "a1,a2" {
		t.Fatalf("deleted agents %v, want a1,a2 (a3 belongs to another VM)", ags.attempts)
	}
	if d := vms.deleteReq; d.GetId() != "vm_1" || d.GetForce() || !d.GetDeleteDisks() {
		t.Fatalf("DeleteVM request = %v", d)
	}
}

func TestMachDestroyForce(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_STOPPED, 8, 16384)}}
	text, isErr := machCall(t, Clients{VMs: vms, Agents: &machAgents{}}, "agent_destroy", map[string]any{"name": "web", "force": true})
	machWantOK(t, text, isErr, "machine 'web' destroyed")
	if !vms.deleteReq.GetForce() {
		t.Fatal("force not passed to DeleteVM")
	}
}

func TestMachDestroyErrors(t *testing.T) {
	vm := func() *vmdv1.VM { return machVM("vm_1", "web", vmdv1.VMState_VM_STATE_STOPPED, 8, 16384) }
	cases := []struct {
		name  string
		vms   *machVMs
		ags   *machAgents
		want  string
		noDel bool
	}{
		{"not found", &machVMs{}, &machAgents{}, "no machine named", true},
		{"list agents fails", &machVMs{vms: []*vmdv1.VM{vm()}},
			&machAgents{listErr: connect.NewError(connect.CodeUnavailable, errors.New("agentd down"))}, "agentd down", true},
		{"delete agent fails", &machVMs{vms: []*vmdv1.VM{vm()}},
			&machAgents{agents: []*agentdv1.Agent{{Id: "a1", VmId: "vm_1"}},
				delErrs: map[string]error{"a1": connect.NewError(connect.CodeInternal, errors.New("disk"))}}, "delete agent a1", true},
		{"delete vm fails", &machVMs{vms: []*vmdv1.VM{vm()}, errs: map[string]error{"DeleteVM": connect.NewError(connect.CodeFailedPrecondition, errors.New("held"))}},
			&machAgents{}, "held", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, isErr := machCall(t, Clients{VMs: tc.vms, Agents: tc.ags}, "agent_destroy", map[string]any{"name": "web"})
			machWantErr(t, text, isErr, tc.want)
			if tc.noDel && tc.vms.called("DeleteVM") {
				t.Fatal("DeleteVM called after a failure")
			}
		})
	}
}

// --- agent_resize ----------------------------------------------------------------------------------------

func TestMachResizeValidation(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"nothing", map[string]any{"name": "web"}, "give cpus, memory_gb or both"},
		{"cpus too many", map[string]any{"name": "web", "cpus": 33}, "cpus must be between 1 and 32"},
		{"cpus negative", map[string]any{"name": "web", "cpus": -2}, "cpus must be between 1 and 32"},
		{"memory too much", map[string]any{"name": "web", "memory_gb": 60}, "memory_gb must be between 1 and 56"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vms := &machVMs{}
			text, isErr := machCall(t, Clients{VMs: vms}, "agent_resize", tc.args)
			machWantErr(t, text, isErr, tc.want)
			if len(vms.calls) != 0 {
				t.Fatalf("validation failure still called vmd: %v", vms.calls)
			}
		})
	}
}

func TestMachResizeStopped(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_STOPPED, 8, 16384)}}
	text, isErr := machCall(t, Clients{VMs: vms, Exec: &machExec{}}, "agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantOK(t, text, isErr, "machine 'web' resized from 8 CPUs / 16 GiB to 4 CPUs / 16 GiB; applied; machine is stopped (agent_create starts it)")
	if vms.called("StopVM") || vms.called("StartVM") {
		t.Fatalf("stopped machine was stopped or started: %v", vms.calls)
	}
	u := vms.updateReq
	if u.Cpus == nil || *u.Cpus != 4 || u.MemoryMb != nil || u.GetExpectedGeneration() != 3 {
		t.Fatalf("UpdateVM request = %v", u)
	}
}

func TestMachResizeMemoryOnly(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_STOPPED, 8, 16384)}}
	text, isErr := machCall(t, Clients{VMs: vms}, "agent_resize", map[string]any{"name": "web", "memory_gb": 32})
	machWantOK(t, text, isErr, "machine 'web' resized from 8 CPUs / 16 GiB to 8 CPUs / 32 GiB; applied; machine is stopped (agent_create starts it)")
	if vms.updateReq.Cpus != nil || vms.updateReq.GetMemoryMb() != 32768 {
		t.Fatalf("UpdateVM request = %v", vms.updateReq)
	}
}

func TestMachResizeRunning(t *testing.T) {
	for _, st := range []vmdv1.VMState{vmdv1.VMState_VM_STATE_RUNNING, vmdv1.VMState_VM_STATE_STARTING} {
		vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", st, 8, 16384)},
			seq: []vmdv1.VMState{vmdv1.VMState_VM_STATE_STOPPED, vmdv1.VMState_VM_STATE_RUNNING}}
		text, isErr := machCall(t, Clients{VMs: vms, Exec: &machExec{}}, "agent_resize",
			map[string]any{"name": "web", "cpus": 4, "memory_gb": 8})
		machWantOK(t, text, isErr, "machine 'web' resized from 8 CPUs / 16 GiB to 4 CPUs / 8 GiB; running again")
		if vms.stopReq.GetForce() {
			t.Fatal("resize must stop the machine gracefully")
		}
		if vms.updateReq.GetExpectedGeneration() != 3 {
			t.Fatalf("expected generation %d, want 3", vms.updateReq.GetExpectedGeneration())
		}
		if strings.Join(vms.calls, ",") != "ListVMs,StopVM,GetVM,UpdateVM,StartVM,GetVM" {
			t.Fatalf("call order = %v", vms.calls)
		}
	}
}

func TestMachResizeRunningJobsRefused(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)}}
	ex := &machExec{jobs: []*vmdv1.Job{
		{Id: "job_1", VmId: "vm_1", State: vmdv1.JobState_JOB_STATE_RUNNING},
		{Id: "job_2", VmId: "vm_1", State: vmdv1.JobState_JOB_STATE_SUCCEEDED},
		{Id: "job_3", VmId: "vm_2", State: vmdv1.JobState_JOB_STATE_RUNNING},
	}}
	text, isErr := machCall(t, Clients{VMs: vms, Exec: ex}, "agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantErr(t, text, isErr, "is running 1 job(s) (job_1)")
	if vms.called("StopVM") || vms.called("UpdateVM") {
		t.Fatalf("machine touched despite running jobs: %v", vms.calls)
	}
}

func TestMachResizeRunningJobsPaged(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)}}
	ex := &machExec{pageLen: 1, jobs: []*vmdv1.Job{
		{Id: "job_1", VmId: "vm_1", State: vmdv1.JobState_JOB_STATE_SUCCEEDED},
		{Id: "job_2", VmId: "vm_1", State: vmdv1.JobState_JOB_STATE_RUNNING},
	}}
	text, isErr := machCall(t, Clients{VMs: vms, Exec: ex}, "agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantErr(t, text, isErr, "job_2")
}

func TestMachResizeRunningJobsListFails(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)}}
	text, isErr := machCall(t, Clients{VMs: vms, Exec: &machExec{err: connect.NewError(connect.CodeInternal, errors.New("exec down"))}},
		"agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantErr(t, text, isErr, "exec down")
}

func TestMachResizeForceKillsJobs(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)},
		seq: []vmdv1.VMState{vmdv1.VMState_VM_STATE_STOPPED, vmdv1.VMState_VM_STATE_RUNNING}}
	ex := &machExec{jobs: []*vmdv1.Job{{Id: "job_1", VmId: "vm_1", State: vmdv1.JobState_JOB_STATE_RUNNING}}}
	text, isErr := machCall(t, Clients{VMs: vms, Exec: ex}, "agent_resize", map[string]any{"name": "web", "cpus": 4, "force": true})
	machWantOK(t, text, isErr, "machine 'web' resized from 8 CPUs / 16 GiB to 4 CPUs / 16 GiB; running again")
}

func TestMachResizeStopTimesOut(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)}}
	text, isErr := machCall(t, Clients{VMs: vms, Exec: &machExec{}}, "agent_resize",
		map[string]any{"name": "web", "cpus": 4}, machWait)
	machWantErr(t, text, isErr, "still stopping after the wait; resize not applied")
	if vms.called("UpdateVM") || vms.called("StartVM") {
		t.Fatalf("resize applied despite the VM still stopping: %v", vms.calls)
	}
}

func TestMachResizeStopEndsInError(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)},
		seq: []vmdv1.VMState{vmdv1.VMState_VM_STATE_ERROR}}
	vms.vms[0].StateReason = "guest hung"
	text, isErr := machCall(t, Clients{VMs: vms, Exec: &machExec{}}, "agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantErr(t, text, isErr, "machine 'web' is error: guest hung")
}

func TestMachResizeStopCallFails(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)},
		errs: map[string]error{"StopVM": connect.NewError(connect.CodeUnavailable, errors.New("vmd busy"))}}
	text, isErr := machCall(t, Clients{VMs: vms, Exec: &machExec{}}, "agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantErr(t, text, isErr, "vmd busy")
}

func TestMachResizeNotRunnableState(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_PENDING, 8, 16384)}}
	text, isErr := machCall(t, Clients{VMs: vms}, "agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantErr(t, text, isErr, "machine 'web' is pending; agent_resize needs the machine running or stopped")
	if vms.called("UpdateVM") {
		t.Fatal("UpdateVM called for a pending machine")
	}
}

func TestMachResizeUpdateFailsWhileRunning(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)},
		seq:  []vmdv1.VMState{vmdv1.VMState_VM_STATE_STOPPED},
		errs: map[string]error{"UpdateVM": connect.NewError(connect.CodeAborted, errors.New("generation changed"))}}
	text, isErr := machCall(t, Clients{VMs: vms, Exec: &machExec{}}, "agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantErr(t, text, isErr, "left stopped (agent_create starts it)")
	if vms.called("StartVM") {
		t.Fatal("machine started after a failed update")
	}
}

func TestMachResizeUpdateFailsWhileStopped(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_STOPPED, 8, 16384)},
		errs: map[string]error{"UpdateVM": connect.NewError(connect.CodeFailedPrecondition, errors.New("quota exceeded"))}}
	text, isErr := machCall(t, Clients{VMs: vms}, "agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantErr(t, text, isErr, "failed_precondition: quota exceeded")
}

func TestMachResizeStartFails(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)},
		seq:  []vmdv1.VMState{vmdv1.VMState_VM_STATE_STOPPED},
		errs: map[string]error{"StartVM": connect.NewError(connect.CodeResourceExhausted, errors.New("no capacity"))}}
	text, isErr := machCall(t, Clients{VMs: vms, Exec: &machExec{}}, "agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantErr(t, text, isErr, "could not be started again and is left stopped")
	machWantErr(t, text, isErr, "no capacity")
}

func TestMachResizeRestartEndsInError(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)},
		seq: []vmdv1.VMState{vmdv1.VMState_VM_STATE_STOPPED, vmdv1.VMState_VM_STATE_ERROR}}
	vms.vms[0].StateReason = "disk missing"
	text, isErr := machCall(t, Clients{VMs: vms, Exec: &machExec{}}, "agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantErr(t, text, isErr, "but it failed to start")
	machWantErr(t, text, isErr, "disk missing")
}

func TestMachResizeRestartStillBooting(t *testing.T) {
	vms := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)},
		seq: []vmdv1.VMState{vmdv1.VMState_VM_STATE_STOPPED, vmdv1.VMState_VM_STATE_STARTING}}
	text, isErr := machCall(t, Clients{VMs: vms, Exec: &machExec{}}, "agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantOK(t, text, isErr, "machine 'web' resized from 8 CPUs / 16 GiB to 4 CPUs / 16 GiB; booting again (agent_list shows when it is ready)")
}

func TestMachResizeWaitFails(t *testing.T) {
	// Call 1 is the stop wait, call 2 the start wait.
	stopWait := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)}, failGetAt: 1}
	text, isErr := machCall(t, Clients{VMs: stopWait, Exec: &machExec{}}, "agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantErr(t, text, isErr, "vmd dropped")
	if stopWait.called("UpdateVM") {
		t.Fatal("UpdateVM called after the stop wait failed")
	}

	startWait := &machVMs{vms: []*vmdv1.VM{machVM("vm_1", "web", vmdv1.VMState_VM_STATE_RUNNING, 8, 16384)},
		seq: []vmdv1.VMState{vmdv1.VMState_VM_STATE_STOPPED}, failGetAt: 2}
	text, isErr = machCall(t, Clients{VMs: startWait, Exec: &machExec{}}, "agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantErr(t, text, isErr, "vmd dropped")
}

func TestMachResizeNotFound(t *testing.T) {
	text, isErr := machCall(t, Clients{VMs: &machVMs{}}, "agent_resize", map[string]any{"name": "web", "cpus": 4})
	machWantErr(t, text, isErr, `no machine named "web"`)
}

// --- helpers ---------------------------------------------------------------------------------------------

func TestMachStateLabels(t *testing.T) {
	want := map[vmdv1.VMState]string{
		vmdv1.VMState_VM_STATE_PENDING:  "pending",
		vmdv1.VMState_VM_STATE_STOPPED:  "stopped",
		vmdv1.VMState_VM_STATE_STARTING: "starting",
		vmdv1.VMState_VM_STATE_RUNNING:  "running",
		vmdv1.VMState_VM_STATE_STOPPING: "stopping",
		vmdv1.VMState_VM_STATE_ERROR:    "error",
		vmdv1.VMState_VM_STATE_DELETING: "deleting",
	}
	for st, w := range want {
		if got := vmStateLabel(st); got != w {
			t.Errorf("vmStateLabel(%v) = %q, want %q", st, got, w)
		}
	}
	tasks := map[agentdv1.TaskState]string{
		agentdv1.TaskState_TASK_STATE_QUEUED:      "queued",
		agentdv1.TaskState_TASK_STATE_RUNNING:     "running",
		agentdv1.TaskState_TASK_STATE_SUCCEEDED:   "done",
		agentdv1.TaskState_TASK_STATE_FAILED:      "failed",
		agentdv1.TaskState_TASK_STATE_GAVE_UP:     "gave_up",
		agentdv1.TaskState_TASK_STATE_CANCELLED:   "cancelled",
		agentdv1.TaskState_TASK_STATE_ERROR:       "error",
		agentdv1.TaskState_TASK_STATE_UNSPECIFIED: "unknown",
	}
	for st, w := range tasks {
		if got := taskStateLabel(st); got != w {
			t.Errorf("taskStateLabel(%v) = %q, want %q", st, got, w)
		}
	}
}

func TestMachGibText(t *testing.T) {
	cases := map[int64]string{16384: "16", 2560: "2.5", 1536: "1.5", 512: "0.5", 0: "0"}
	for mb, want := range cases {
		if got := gibText(mb); got != want {
			t.Errorf("gibText(%d) = %q, want %q", mb, got, want)
		}
	}
}
