// Package vmclitest is a fake vmd for tests of the vm command group and of cmd/attic. It serves VMService and
// ExecService over HTTP with the generated Connect handlers, keeps VMs and files in memory and records the calls it
// receives. It is test support only: no production code imports it.
package vmclitest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1/vmdv1connect"
)

// VM states for AddVM, so that a test does not need to import the generated API.
const (
	Pending = vmdv1.VMState_VM_STATE_PENDING
	Running = vmdv1.VMState_VM_STATE_RUNNING
	Stopped = vmdv1.VMState_VM_STATE_STOPPED
)

// MaxFileBytes is the vmd limit for PutFile content and for the bytes GetFile returns.
const MaxFileBytes = 8 << 20

// Put is one PutFile call as the fake received it.
type Put struct {
	VMID string
	Path string
	Mode uint32
	Size int
}

// Server is the fake. Its methods are safe for concurrent use. Configure it with Fail and PageSize before the code
// under test runs.
type Server struct {
	HTTP *httptest.Server

	mu       sync.Mutex
	vms      []*vmdv1.VM
	files    map[string][]byte // key: VM id and path
	puts     []Put
	auth     []string
	fail     map[string]*connect.Error
	pageSize int
	nextID   int
}

// New starts a fake vmd. The caller closes it with Close.
func New() *Server {
	s := &Server{files: map[string][]byte{}, fail: map[string]*connect.Error{}}
	mux := http.NewServeMux()
	path, h := vmdv1connect.NewVMServiceHandler(vmService{s: s})
	mux.Handle(path, h)
	path, h = vmdv1connect.NewExecServiceHandler(execService{s: s})
	mux.Handle(path, h)
	s.HTTP = httptest.NewServer(mux)
	return s
}

// URL is the base URL of the fake.
func (s *Server) URL() string { return s.HTTP.URL }

// Close stops the fake.
func (s *Server) Close() { s.HTTP.Close() }

// Client is an HTTP client for the fake.
func (s *Server) Client() *http.Client { return s.HTTP.Client() }

// AddVM adds a VM to a project and returns its id.
func (s *Server) AddVM(project, name string, state vmdv1.VMState, cpus int32, memoryMB int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("vm-%d", s.nextID)
	s.vms = append(s.vms, &vmdv1.VM{Id: id, ProjectId: project, Name: name, State: state, Cpus: cpus, MemoryMb: memoryMB})
	return id
}

// SetPageSize makes ListVMs return at most n VMs per page. Zero returns every match in one page.
func (s *Server) SetPageSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSize = n
}

// Fail makes every later call to the named procedure ("ListVMs", "PutFile" or "GetFile") return that error.
func (s *Server) Fail(procedure string, code connect.Code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[procedure] = connect.NewError(code, fmt.Errorf("%s", message))
}

// SetFile stores a file in a VM, bypassing the size check, so a test can hold a file larger than the limit.
func (s *Server) SetFile(vmID, path string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[fileKey(vmID, path)] = append([]byte(nil), data...)
}

// File returns the content of a file in a VM.
func (s *Server) File(vmID, path string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.files[fileKey(vmID, path)]
	return append([]byte(nil), data...), ok
}

// Puts returns the PutFile calls received so far.
func (s *Server) Puts() []Put {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Put(nil), s.puts...)
}

// Authorizations returns the Authorization header of every call received so far.
func (s *Server) Authorizations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auth...)
}

func fileKey(vmID, path string) string { return vmID + "\x00" + path }

// begin records the call and returns the configured failure for it, if any. Must not hold s.mu.
func (s *Server) begin(procedure string, h http.Header) *connect.Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth = append(s.auth, h.Get("Authorization"))
	return s.fail[procedure]
}

type vmService struct {
	vmdv1connect.UnimplementedVMServiceHandler
	s *Server
}

func (v vmService) ListVMs(_ context.Context, req *connect.Request[vmdv1.ListVMsRequest]) (*connect.Response[vmdv1.ListVMsResponse], error) {
	if err := v.s.begin("ListVMs", req.Header()); err != nil {
		return nil, err
	}
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	var matches []*vmdv1.VM
	for _, vm := range v.s.vms {
		if vm.ProjectId == req.Msg.GetProjectId() {
			matches = append(matches, vm)
		}
	}
	start := 0
	if tok := req.Msg.GetPageToken(); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 0 || n > len(matches) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("bad page token"))
		}
		start = n
	}
	end := len(matches)
	if v.s.pageSize > 0 && start+v.s.pageSize < end {
		end = start + v.s.pageSize
	}
	res := &vmdv1.ListVMsResponse{Vms: matches[start:end]}
	if end < len(matches) {
		res.NextPageToken = strconv.Itoa(end)
	}
	return connect.NewResponse(res), nil
}

type execService struct {
	vmdv1connect.UnimplementedExecServiceHandler
	s *Server
}

func (e execService) PutFile(_ context.Context, req *connect.Request[vmdv1.PutFileRequest]) (*connect.Response[vmdv1.PutFileResponse], error) {
	if err := e.s.begin("PutFile", req.Header()); err != nil {
		return nil, err
	}
	m := req.Msg
	if !e.s.hasVM(m.GetVmId()) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no such VM"))
	}
	if len(m.GetContent()) > MaxFileBytes {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("content is larger than 8 MiB"))
	}
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	e.s.files[fileKey(m.GetVmId(), m.GetPath())] = append([]byte(nil), m.GetContent()...)
	e.s.puts = append(e.s.puts, Put{VMID: m.GetVmId(), Path: m.GetPath(), Mode: m.GetMode(), Size: len(m.GetContent())})
	return connect.NewResponse(&vmdv1.PutFileResponse{}), nil
}

func (e execService) GetFile(_ context.Context, req *connect.Request[vmdv1.GetFileRequest]) (*connect.Response[vmdv1.GetFileResponse], error) {
	if err := e.s.begin("GetFile", req.Header()); err != nil {
		return nil, err
	}
	m := req.Msg
	if !e.s.hasVM(m.GetVmId()) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no such VM"))
	}
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	data, ok := e.s.files[fileKey(m.GetVmId(), m.GetPath())]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no such file"))
	}
	limit := int64(MaxFileBytes)
	if m.GetMaxBytes() > 0 && m.GetMaxBytes() < limit {
		limit = m.GetMaxBytes()
	}
	res := &vmdv1.GetFileResponse{Content: append([]byte(nil), data...)}
	if int64(len(data)) > limit {
		res.Content = res.Content[:limit]
		res.Truncated = true
	}
	return connect.NewResponse(res), nil
}

func (s *Server) hasVM(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, vm := range s.vms {
		if vm.Id == id {
			return true
		}
	}
	return false
}
