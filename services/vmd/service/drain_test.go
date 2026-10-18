package service_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1/vmdv1connect"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

func (c *clients) cluster(e *env) vmdv1connect.ClusterServiceClient {
	opt := connect.WithInterceptors(authz.ClientInterceptor(authz.StaticToken(c.Token)))
	return vmdv1connect.NewClusterServiceClient(e.srv.Client(), e.srv.URL, opt)
}

func drain(t *testing.T, e *env, who *clients, node string, on bool) (*vmdv1.NodeCapacity, error) {
	t.Helper()
	res, err := who.cluster(e).SetNodeDraining(context.Background(), connect.NewRequest(&vmdv1.SetNodeDrainingRequest{NodeId: node, Draining: on}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func TestDrainIsOperatorOnly(t *testing.T) {
	e := newEnv(t)
	for name, who := range map[string]*clients{
		"viewer": e.viewer(), "editor": e.editor(), "project admin": e.admin(), "global admin": e.root(), "service": e.service(),
		"project-scoped operator": e.as("user:scoped-ops", bind("p1", authz.RoleOperator)),
	} {
		if _, err := drain(t, e, who, "n1", true); code(err) != connect.CodePermissionDenied {
			t.Errorf("%s: want PermissionDenied, got %v", name, err)
		}
	}
	if _, err := drain(t, e, e.withToken(""), "n1", true); code(err) != connect.CodeUnauthenticated {
		t.Errorf("no token: %v", err)
	}
	if _, err := drain(t, e, e.operator(), "ghost", true); code(err) != connect.CodeNotFound {
		t.Errorf("unknown node: %v", err)
	}
	if _, err := drain(t, e, e.operator(), "", true); code(err) != connect.CodeInvalidArgument {
		t.Errorf("empty node id: %v", err)
	}
	n, err := drain(t, e, e.operator(), "n1", true)
	if err != nil || n.State != "draining" || n.CpusAvailable != 0 {
		t.Fatalf("operator drain: %+v %v", n, err)
	}
}

func TestDrainKeepsRunningVMsAndBlocksNewStartsUntilUndrained(t *testing.T) {
	e := newEnv(t)
	running := e.runningVM("keeps-running")
	ed := e.editor()

	if _, err := drain(t, e, e.operator(), "n1", true); err != nil {
		t.Fatal(err)
	}
	e.tick(15) // several resync intervals
	if got := e.getVM(ed, running.Id); got.State != vmdv1.VMState_VM_STATE_RUNNING {
		t.Fatalf("draining must not touch running VMs, state %s", got.State)
	}

	img := e.sharedImage("base-drain")
	blocked := e.createVM(ed, "p1", "blocked", img.Id, true)
	e.tick(10)
	got := e.getVM(ed, blocked.Id)
	if got.State == vmdv1.VMState_VM_STATE_RUNNING || got.State == vmdv1.VMState_VM_STATE_STARTING {
		t.Fatalf("a drained node must not start new VMs, state %s", got.State)
	}
	if !strings.Contains(got.StateReason, "draining") {
		t.Fatalf("the reason should say why: %q", got.StateReason)
	}

	if _, err := drain(t, e, e.operator(), "n1", false); err != nil {
		t.Fatal(err)
	}
	e.until(15, "blocked VM starts after undrain", func() bool {
		return e.getVM(ed, blocked.Id).State == vmdv1.VMState_VM_STATE_RUNNING
	})
}

// The drain decision is the operator's: an outage (the node is marked down) and its recovery must not erase it.
func TestDrainSurvivesNodeOutage(t *testing.T) {
	e := newEnv(t)
	if _, err := drain(t, e, e.operator(), "n1", true); err != nil {
		t.Fatal(err)
	}
	e.node.SetDown(true)
	e.clk.Advance(60e9)
	e.nodes.Poll(context.Background())
	capRes, err := e.operator().Metrics.GetCapacity(context.Background(), connect.NewRequest(&vmdv1.GetCapacityRequest{}))
	if err != nil || capRes.Msg.Nodes[0].State != "down" {
		t.Fatalf("during the outage the node is down: %+v %v", capRes, err)
	}
	e.node.SetDown(false)
	e.nodes.Poll(context.Background())
	capRes, err = e.operator().Metrics.GetCapacity(context.Background(), connect.NewRequest(&vmdv1.GetCapacityRequest{}))
	if err != nil || capRes.Msg.Nodes[0].State != "draining" {
		t.Fatalf("after recovery the node must still be draining: %+v %v", capRes, err)
	}
}

// A VM in ERROR is retried slowly on its own (every 60 s). An explicit StartVM means "try again now".
func TestStartVMOnAnErroredVMRetriesImmediately(t *testing.T) {
	e := newEnv(t)
	img := e.sharedImage("base-retry")
	ed := e.editor()
	e.node.FailNext("StartVM", errs.Invalid("bad disk layout")) // a permanent-looking failure
	vm := e.createVM(ed, "p1", "stubborn", img.Id, true)
	e.until(10, "ERROR", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_ERROR })

	if _, err := ed.VM.StartVM(context.Background(), connect.NewRequest(&vmdv1.StartVMRequest{Id: vm.Id})); err != nil {
		t.Fatal(err)
	}
	// Only a few seconds of clock pass: far less than the 60 s slow retry.
	e.until(5, "running right after StartVM", func() bool { return e.getVM(ed, vm.Id).State == vmdv1.VMState_VM_STATE_RUNNING })
}
