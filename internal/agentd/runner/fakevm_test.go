package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	vmdv1 "example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/internal/agentd/vmclient"
	"example.com/homecloud/internal/errs"
)

var _ vmclient.VM = (*fakeVM)(nil)

// jobScript describes how a command behaves in the fake VM.
type jobScript struct {
	stdout, stderr string
	exit           int32
	hang           bool // the job stays running until it is cancelled or the poll's context ends
}

// fakeJob is one job the fake VM has started.
type fakeJob struct {
	id     string
	state  vmdv1.JobState
	exit   *int32
	stdout string
	stderr string
	done   chan struct{} // closed when the job reaches a final state
}

// fakeVM is a hand-written vmclient.VM. Commands are matched by their exact text; any other command exits 0 and
// prints "ran: <command>". StartJob is idempotent by key, as vmd is.
type fakeVM struct {
	mu       sync.Mutex
	vmStates map[string]vmdv1.VMState
	scripts  map[string]jobScript
	jobs     map[string]*fakeJob
	byKey    map[string]*fakeJob
	nextID   int
	starts   []*vmdv1.StartJobRequest // one per new job
	cancels  []string                 // job IDs passed to CancelJob
	failGets int                      // the next failGets GetVM calls return Unavailable
}

func newFakeVM() *fakeVM {
	return &fakeVM{
		vmStates: map[string]vmdv1.VMState{},
		scripts:  map[string]jobScript{},
		jobs:     map[string]*fakeJob{},
		byKey:    map[string]*fakeJob{},
	}
}

func (f *fakeVM) setVM(id string, st vmdv1.VMState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vmStates[id] = st
}

func (f *fakeVM) deleteVM(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vmStates, id)
}

func (f *fakeVM) setScript(cmd string, s jobScript) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[cmd] = s
}

func (f *fakeVM) setFailGets(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failGets = n
}

func (f *fakeVM) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

func (f *fakeVM) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cancels)
}

func (f *fakeVM) startRequests() []*vmdv1.StartJobRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*vmdv1.StartJobRequest(nil), f.starts...)
}

func (f *fakeVM) cancelled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancels...)
}

// output builds the JobOutput for j. The caller holds f.mu.
func (f *fakeVM) output(j *fakeJob) *vmdv1.JobOutput {
	out := &vmdv1.JobOutput{Job: &vmdv1.Job{Id: j.id, State: j.state, ExitCode: j.exit}}
	if finalJob(j.state) {
		out.Stdout = []byte(j.stdout)
		out.Stderr = []byte(j.stderr)
	}
	return out
}

func (f *fakeVM) GetVM(ctx context.Context, id string) (*vmdv1.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGets > 0 {
		f.failGets--
		return nil, errs.Unavailable("vmd is down")
	}
	st, ok := f.vmStates[id]
	if !ok {
		return nil, errs.NotFound("vm %s", id)
	}
	return &vmdv1.VM{Id: id, State: st}, nil
}

func (f *fakeVM) StartJob(ctx context.Context, req *vmdv1.StartJobRequest) (*vmdv1.JobOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if j, ok := f.byKey[req.IdempotencyKey]; ok {
		return f.output(j), nil
	}
	f.nextID++
	s, ok := f.scripts[req.Command]
	if !ok {
		s = jobScript{stdout: "ran: " + req.Command}
	}
	j := &fakeJob{id: fmt.Sprintf("job-%d", f.nextID), done: make(chan struct{}), stdout: s.stdout, stderr: s.stderr}
	if s.hang {
		j.state = vmdv1.JobState_JOB_STATE_RUNNING
	} else {
		code := s.exit
		j.exit = &code
		j.state = vmdv1.JobState_JOB_STATE_SUCCEEDED
		if code != 0 {
			j.state = vmdv1.JobState_JOB_STATE_FAILED
		}
		close(j.done)
	}
	f.jobs[j.id] = j
	f.byKey[req.IdempotencyKey] = j
	f.starts = append(f.starts, req)
	return f.output(j), nil
}

// PollJob returns at once for a finished job. For a running one it waits until the job finishes, the context
// ends, or a short time passes, then reports the job as still running.
func (f *fakeVM) PollJob(ctx context.Context, req *vmdv1.PollJobRequest) (*vmdv1.JobOutput, error) {
	f.mu.Lock()
	j, ok := f.jobs[req.JobId]
	if !ok {
		f.mu.Unlock()
		return nil, errs.NotFound("job %s", req.JobId)
	}
	if finalJob(j.state) {
		out := f.output(j)
		f.mu.Unlock()
		return out, nil
	}
	done := j.done
	f.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
	case <-time.After(5 * time.Millisecond):
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.output(j), nil
}

func (f *fakeVM) CancelJob(ctx context.Context, jobID string) (*vmdv1.JobOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[jobID]
	if !ok {
		return nil, errs.NotFound("job %s", jobID)
	}
	f.cancels = append(f.cancels, jobID)
	if !finalJob(j.state) {
		j.state = vmdv1.JobState_JOB_STATE_CANCELLED
		close(j.done)
	}
	return f.output(j), nil
}

var errNotUsed = errors.New("fakeVM: not used by these tests")

func (f *fakeVM) AddHold(context.Context, string, string) (*vmdv1.VM, error) {
	return nil, errNotUsed
}

func (f *fakeVM) RemoveHold(context.Context, string, string) (*vmdv1.VM, error) {
	return nil, errNotUsed
}

func (f *fakeVM) PutFile(context.Context, string, string, []byte, uint32) error {
	return errNotUsed
}

func (f *fakeVM) GetFile(context.Context, string, string, int64) ([]byte, bool, error) {
	return nil, false, errNotUsed
}
