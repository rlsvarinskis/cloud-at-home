package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/node/v1"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

const leakyNodeError = `node n1: Post "http://203.0.113.40:9000/homecloud.node.v1.NodeService/PollJob": dial tcp 203.0.113.40:9000: connect: connection refused`

func assertNoInternals(t *testing.T, what, text string) {
	t.Helper()
	for _, leak := range []string{"203.0.113.40", "9000", "dial tcp", "NodeService", "http://"} {
		if strings.Contains(text, leak) {
			t.Errorf("%s leaks %q: %s", what, leak, text)
		}
	}
}

// What a client sees (API errors and VM.state_reason) must say that a node is unavailable, never where it is.
func TestNodeFailuresDoNotLeakAddressesToClients(t *testing.T) {
	e := newEnv(t)
	vm := e.runningVM("leaky")
	ed := e.editor()
	ctx := context.Background()
	e.node.Exec = func(string, *nodev1.JobSpec) (int, string, string, time.Duration) { return 0, "", "", time.Hour } // stays running

	start, err := ed.Exec.StartJob(ctx, connect.NewRequest(&vmdv1.StartJobRequest{VmId: vm.Id, Command: "sleep 1", WaitSeconds: 0}))
	if err != nil {
		t.Fatal(err)
	}

	for name, call := range map[string]func() error{
		"PollJob": func() error {
			e.node.FailNext("PollJob", errs.Unavailable(leakyNodeError))
			_, err := ed.Exec.PollJob(ctx, connect.NewRequest(&vmdv1.PollJobRequest{JobId: start.Msg.Job.Id}))
			return err
		},
		"StartJob": func() error {
			e.node.FailNext("StartJob", errs.Unavailable(leakyNodeError))
			_, err := ed.Exec.StartJob(ctx, connect.NewRequest(&vmdv1.StartJobRequest{VmId: vm.Id, Command: "true"}))
			return err
		},
		"CancelJob": func() error {
			e.node.FailNext("CancelJob", errs.Unavailable(leakyNodeError))
			_, err := ed.Exec.CancelJob(ctx, connect.NewRequest(&vmdv1.CancelJobRequest{JobId: start.Msg.Job.Id}))
			return err
		},
		"PutFile": func() error {
			e.node.FailNext("PutFile", errs.Unavailable(leakyNodeError))
			_, err := ed.Exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{VmId: vm.Id, Path: "/tmp/x", Content: []byte("x")}))
			return err
		},
		"GetFile": func() error {
			e.node.FailNext("GetFile", errs.Unavailable(leakyNodeError))
			_, err := ed.Exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{VmId: vm.Id, Path: "/tmp/x"}))
			return err
		},
		"GetVMMetrics": func() error {
			e.node.FailNext("GetVMMetrics", errs.Unavailable(leakyNodeError))
			_, err := ed.Metrics.GetVMMetrics(ctx, connect.NewRequest(&vmdv1.GetVMMetricsRequest{VmId: vm.Id}))
			return err
		},
	} {
		err := call()
		if code(err) != connect.CodeUnavailable {
			t.Errorf("%s: want Unavailable, got %v", name, err)
			continue
		}
		assertNoInternals(t, name, err.Error())
	}

	// A meaningful node answer still reaches the client.
	e.node.FailNext("PutFile", errs.Precondition("disk quota exceeded in guest"))
	_, err = ed.Exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{VmId: vm.Id, Path: "/tmp/x", Content: []byte("x")}))
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "disk quota exceeded in guest") {
		t.Errorf("a precondition from the node must pass through: %v", err)
	}

	// The reconciler's reason is part of the VM resource clients can read.
	e.node.FailNext("GetVMRuntime", errs.Unavailable(leakyNodeError))
	e.clk.Advance(20e9)
	e.tick(1)
	got := e.getVM(ed, vm.Id)
	if !strings.Contains(got.StateReason, "node unreachable") {
		t.Fatalf("state_reason should say the node is unreachable, got %q", got.StateReason)
	}
	assertNoInternals(t, "state_reason", got.StateReason)
}
