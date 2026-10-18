package service_test

import (
	"context"
	"math"
	"testing"

	"connectrpc.com/connect"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/service"
)

func TestCreateTaskOptionDefaultsAndValidation(t *testing.T) {
	e := newEnv(t)
	e.vm.put("vm_progress", "p1", vmdv1.VMState_VM_STATE_RUNNING)
	ed := e.editor()
	a := e.createAgent(ed, "p1", "builder", "vm_progress")
	ctx := context.Background()

	// Unset options: check after each batch on, the default token cap, no time limit.
	task := e.createTask(ed, a.GetId(), "do it")
	if !task.GetCheckEachStep() || task.GetMaxCallTokens() != 32768 || task.GetMaxMinutes() != 0 {
		t.Fatalf("defaults: check_each_step=%v max_call_tokens=%d max_minutes=%d",
			task.GetCheckEachStep(), task.GetMaxCallTokens(), task.GetMaxMinutes())
	}

	// Explicit values, including 0 for "no cap" and a time limit.
	off, zero := false, int32(0)
	res, err := ed.Task.CreateTask(ctx, connect.NewRequest(&agentdv1.CreateTaskRequest{
		AgentId: a.GetId(), Prompt: "do it again", CheckEachStep: &off, MaxMinutes: 5, MaxCallTokens: &zero}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetCheckEachStep() || res.Msg.GetMaxCallTokens() != 0 || res.Msg.GetMaxMinutes() != 5 {
		t.Fatalf("explicit: check_each_step=%v max_call_tokens=%d max_minutes=%d",
			res.Msg.GetCheckEachStep(), res.Msg.GetMaxCallTokens(), res.Msg.GetMaxMinutes())
	}

	minus := int32(-1)
	for _, req := range []*agentdv1.CreateTaskRequest{
		{AgentId: a.GetId(), Prompt: "x", MaxMinutes: -1},
		{AgentId: a.GetId(), Prompt: "x", MaxCallTokens: &minus},
	} {
		if _, err := ed.Task.CreateTask(ctx, connect.NewRequest(req)); code(err) != connect.CodeInvalidArgument {
			t.Fatalf("request %v: code %v, want InvalidArgument", req, code(err))
		}
	}
}

func TestListModelsReportsSizeDetailsAndFit(t *testing.T) {
	e := newEnv(t, func(d *service.Deps) { d.Config.VRAMGiB = 16 })
	gib := func(g float64) int64 { return int64(g * (1 << 30)) }
	e.llm.Models = []llm.ModelInfo{
		{Name: "small", SizeBytes: gib(4), Params: "7.6B", Quant: "Q4_K_M", ContextLength: 32768},
		{Name: "edge", SizeBytes: gib(15.5)}, // 16 - 0.5: still fits
		{Name: "over", SizeBytes: gib(15.6)}, // just over the budget
		{Name: "huge", SizeBytes: gib(40)},
	}
	res, err := e.viewer().Model.ListModels(context.Background(), connect.NewRequest(&agentdv1.ListModelsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	fit := map[string]bool{}
	for _, m := range res.Msg.Models {
		fit[m.GetName()] = m.GetFitsVram()
	}
	want := map[string]bool{"small": true, "edge": true, "over": false, "huge": false}
	for name, w := range want {
		if fit[name] != w {
			t.Errorf("%s: fits_vram = %v, want %v", name, fit[name], w)
		}
	}
	small := res.Msg.Models[0]
	if small.GetName() != "small" || math.Abs(small.GetSizeGib()-4) > 1e-9 || small.GetParams() != "7.6B" ||
		small.GetQuant() != "Q4_K_M" || small.GetContextLength() != 32768 {
		t.Fatalf("small = %+v", small)
	}
}

func TestListModelsUsesTheConfiguredBudget(t *testing.T) {
	e := newEnv(t, func(d *service.Deps) { d.Config.VRAMGiB = 8 })
	e.llm.Models = []llm.ModelInfo{
		{Name: "medium", SizeBytes: int64(6 * (1 << 30))}, // 6 GiB fits a budget of 8 GiB less 0.5
		{Name: "big", SizeBytes: int64(8 * (1 << 30))},
	}
	res, err := e.viewer().Model.ListModels(context.Background(), connect.NewRequest(&agentdv1.ListModelsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res.Msg.Models {
		if got, want := m.GetFitsVram(), m.GetName() == "medium"; got != want {
			t.Errorf("%s: fits_vram = %v, want %v", m.GetName(), got, want)
		}
	}
}
