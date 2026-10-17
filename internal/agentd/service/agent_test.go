package service_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	agentdv1 "example.com/homecloud/gen/homecloud/agentd/v1"
	vmdv1 "example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/internal/agentd/domain"
	"example.com/homecloud/internal/agentd/service"
	"example.com/homecloud/internal/agentd/store"
	"example.com/homecloud/internal/errs"
)

func ptr[T any](v T) *T { return &v }

func TestAgentCRUD(t *testing.T) {
	e := newEnv(t)
	ed, vw := e.editor(), e.viewer()
	ctx := context.Background()

	created, err := ed.Agent.CreateAgent(ctx, connect.NewRequest(&agentdv1.CreateAgentRequest{
		ProjectId: "p1", Name: "builder", VmId: "vm_p1", Model: "llama3.2",
		SystemPrompt: "be terse", Labels: map[string]string{"team": "infra"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := created.Msg
	if !strings.HasPrefix(a.Id, "agent_") || a.ProjectId != "p1" || a.VmId != "vm_p1" || a.Name != "builder" {
		t.Fatalf("created agent: %+v", a)
	}
	if a.Workdir != "/home/agent/agents/"+a.Id {
		t.Fatalf("default workdir = %q", a.Workdir)
	}
	if a.State != agentdv1.AgentState_AGENT_STATE_ACTIVE || a.CreatedAt == nil || a.UpdatedAt == nil {
		t.Fatalf("state/timestamps: %+v", a)
	}
	if a.Labels["team"] != "infra" || a.SystemPrompt != "be terse" {
		t.Fatalf("labels/system prompt not stored: %+v", a)
	}

	got, err := vw.Agent.GetAgent(ctx, connect.NewRequest(&agentdv1.GetAgentRequest{Id: a.Id}))
	if err != nil || got.Msg.Model != "llama3.2" {
		t.Fatalf("viewer get: %v %+v", err, got)
	}

	upd, err := ed.Agent.UpdateAgent(ctx, connect.NewRequest(&agentdv1.UpdateAgentRequest{
		Id: a.Id, Model: ptr("qwen2.5-coder:7b"), SystemPrompt: ptr("be verbose"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if upd.Msg.Model != "qwen2.5-coder:7b" || upd.Msg.SystemPrompt != "be verbose" || upd.Msg.Workdir != a.Workdir {
		t.Fatalf("update: %+v", upd.Msg)
	}

	if _, err := ed.Agent.DeleteAgent(ctx, connect.NewRequest(&agentdv1.DeleteAgentRequest{Id: a.Id})); err != nil {
		t.Fatal(err)
	}
	_, err = vw.Agent.GetAgent(ctx, connect.NewRequest(&agentdv1.GetAgentRequest{Id: a.Id}))
	if code(err) != connect.CodeNotFound {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestCreateAgentValidation(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()

	base := func() *agentdv1.CreateAgentRequest {
		return &agentdv1.CreateAgentRequest{ProjectId: "p1", Name: "ok-name", VmId: "vm_p1", Model: "llama3.2"}
	}
	cases := []struct {
		name   string
		mutate func(*agentdv1.CreateAgentRequest)
		want   connect.Code
	}{
		{"project missing", func(r *agentdv1.CreateAgentRequest) { r.ProjectId = "" }, connect.CodeInvalidArgument},
		{"project wildcard", func(r *agentdv1.CreateAgentRequest) { r.ProjectId = "*" }, connect.CodeInvalidArgument},
		{"name uppercase", func(r *agentdv1.CreateAgentRequest) { r.Name = "Bad" }, connect.CodeInvalidArgument},
		{"name trailing dash", func(r *agentdv1.CreateAgentRequest) { r.Name = "bad-" }, connect.CodeInvalidArgument},
		{"name starts with digit", func(r *agentdv1.CreateAgentRequest) { r.Name = "1abc" }, connect.CodeInvalidArgument},
		{"name too long", func(r *agentdv1.CreateAgentRequest) { r.Name = "a" + strings.Repeat("b", 63) }, connect.CodeInvalidArgument},
		{"model missing", func(r *agentdv1.CreateAgentRequest) { r.Model = "" }, connect.CodeInvalidArgument},
		{"vm missing", func(r *agentdv1.CreateAgentRequest) { r.VmId = "" }, connect.CodeInvalidArgument},
		{"model not installed", func(r *agentdv1.CreateAgentRequest) { r.Model = "gpt-nope" }, connect.CodeInvalidArgument},
		{"system prompt too big", func(r *agentdv1.CreateAgentRequest) {
			r.SystemPrompt = strings.Repeat("x", 8<<10+1)
		}, connect.CodeInvalidArgument},
		{"system prompt with NUL", func(r *agentdv1.CreateAgentRequest) { r.SystemPrompt = "a\x00b" }, connect.CodeInvalidArgument},
		{"workdir relative", func(r *agentdv1.CreateAgentRequest) { r.Workdir = "home/agent" }, connect.CodeInvalidArgument},
		{"workdir not clean", func(r *agentdv1.CreateAgentRequest) { r.Workdir = "/home/agent/../root" }, connect.CodeInvalidArgument},
		{"workdir trailing slash", func(r *agentdv1.CreateAgentRequest) { r.Workdir = "/home/agent/" }, connect.CodeInvalidArgument},
		{"workdir with NUL", func(r *agentdv1.CreateAgentRequest) { r.Workdir = "/home/\x00agent" }, connect.CodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base()
			tc.mutate(req)
			_, err := ed.Agent.CreateAgent(ctx, connect.NewRequest(req))
			if code(err) != tc.want {
				t.Fatalf("got %v, want %v", code(err), tc.want)
			}
		})
	}

	// Boundary: exactly 8 KiB of system prompt and a clean absolute workdir are accepted.
	req := base()
	req.SystemPrompt = strings.Repeat("x", 8<<10)
	req.Workdir = "/srv/agent"
	if _, err := ed.Agent.CreateAgent(ctx, connect.NewRequest(req)); err != nil {
		t.Fatalf("boundary request rejected: %v", err)
	}
}

func TestCreateAgentModelValidationLists(t *testing.T) {
	e := newEnv(t)
	_, err := e.editor().Agent.CreateAgent(context.Background(), connect.NewRequest(&agentdv1.CreateAgentRequest{
		ProjectId: "p1", Name: "a", VmId: "vm_p1", Model: "gpt-nope",
	}))
	if code(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v", code(err))
	}
	for _, want := range []string{"llama3.2", "qwen2.5-coder:7b"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not list installed model %s", err, want)
		}
	}
}

func TestCreateAgentSkipsModelValidationWhenLLMDown(t *testing.T) {
	e := newEnv(t)
	e.llm.down.Store(true)
	ed := e.editor()
	ctx := context.Background()

	if _, err := ed.Agent.CreateAgent(ctx, connect.NewRequest(&agentdv1.CreateAgentRequest{
		ProjectId: "p1", Name: "offline", VmId: "vm_p1", Model: "anything",
	})); err != nil {
		t.Fatalf("create while LLM down: %v", err)
	}
	_, err := ed.Agent.UpdateAgent(ctx, connect.NewRequest(&agentdv1.UpdateAgentRequest{
		Id: e.listAgentIDs(ed, "p1")[0], Model: ptr("whatever"),
	}))
	if err != nil {
		t.Fatalf("update while LLM down: %v", err)
	}
}

// listAgentIDs returns the IDs of the agents of project, in list order.
func (e *env) listAgentIDs(c *clients, project string) []string {
	e.t.Helper()
	res, err := c.Agent.ListAgents(context.Background(), connect.NewRequest(&agentdv1.ListAgentsRequest{ProjectId: project}))
	if err != nil {
		e.t.Fatal(err)
	}
	var ids []string
	for _, a := range res.Msg.Agents {
		ids = append(ids, a.Id)
	}
	return ids
}

func TestCreateAgentVMChecks(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()
	e.vm.put("vm_deleting", "p1", vmdv1.VMState_VM_STATE_DELETING)

	try := func(vmID string) error {
		_, err := ed.Agent.CreateAgent(ctx, connect.NewRequest(&agentdv1.CreateAgentRequest{
			ProjectId: "p1", Name: "a", VmId: vmID, Model: "llama3.2",
		}))
		return err
	}
	if c := code(try("vm_missing")); c != connect.CodeNotFound {
		t.Fatalf("missing VM: %v", c)
	}
	if c := code(try("vm_p2")); c != connect.CodeNotFound {
		t.Fatalf("VM of another project must look missing, got %v", c)
	}
	if c := code(try("vm_deleting")); c != connect.CodeFailedPrecondition {
		t.Fatalf("VM being deleted: %v", c)
	}
	e.vm.failGet(errs.Unavailable("vmd down"))
	if c := code(try("vm_p1")); c != connect.CodeUnavailable {
		t.Fatalf("vmd unavailable: %v", c)
	}
	e.vm.failGet(nil)
	if err := try("vm_p1"); err != nil {
		t.Fatalf("running VM rejected: %v", err)
	}
}

func TestAgentHoldPlacedAndReleased(t *testing.T) {
	e := newEnv(t)
	a := e.createAgent(e.editor(), "p1", "holder", "vm_p1")
	if got := e.vm.holds("vm_p1"); !slices.Equal(got, []string{"agentd/" + a.Id}) {
		t.Fatalf("holds after create = %v", got)
	}
	// A second agent on the same VM adds its own hold.
	b := e.createAgent(e.editor(), "p1", "second", "vm_p1")
	if got := e.vm.holds("vm_p1"); len(got) != 2 {
		t.Fatalf("holds after second agent = %v", got)
	}
	if _, err := e.editor().Agent.DeleteAgent(context.Background(), connect.NewRequest(&agentdv1.DeleteAgentRequest{Id: a.Id})); err != nil {
		t.Fatal(err)
	}
	if got := e.vm.holds("vm_p1"); !slices.Equal(got, []string{"agentd/" + b.Id}) {
		t.Fatalf("holds after deleting first agent = %v", got)
	}
}

func TestDeleteAgentIgnoresFailedHoldRelease(t *testing.T) {
	e := newEnv(t)
	a := e.createAgent(e.editor(), "p1", "gone", "vm_p1")
	e.vm.failRemoveHold(errs.Unavailable("vmd down"))
	if _, err := e.editor().Agent.DeleteAgent(context.Background(), connect.NewRequest(&agentdv1.DeleteAgentRequest{Id: a.Id})); err != nil {
		t.Fatalf("delete with vmd down should still succeed: %v", err)
	}
	if ids := e.listAgentIDs(e.editor(), "p1"); len(ids) != 0 {
		t.Fatalf("agent still listed: %v", ids)
	}
}

func TestCreateAgentAddHoldFailureRollsBack(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()
	e.vm.failAddHold(errs.Precondition("vm vm_p1 is being deleted"))

	_, err := ed.Agent.CreateAgent(ctx, connect.NewRequest(&agentdv1.CreateAgentRequest{
		ProjectId: "p1", Name: "rolled", VmId: "vm_p1", Model: "llama3.2",
	}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", code(err))
	}
	if ids := e.listAgentIDs(ed, "p1"); len(ids) != 0 {
		t.Fatalf("row survived failed hold: %v", ids)
	}
	// The name is free again once the rollback has happened.
	e.vm.failAddHold(nil)
	e.createAgent(ed, "p1", "rolled", "vm_p1")
}

func TestCreateAgentIdempotency(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()
	req := func(project, name, vm, key string) *agentdv1.CreateAgentRequest {
		return &agentdv1.CreateAgentRequest{ProjectId: project, Name: name, VmId: vm, Model: "llama3.2", IdempotencyKey: key}
	}
	first, err := ed.Agent.CreateAgent(ctx, connect.NewRequest(req("p1", "idem", "vm_p1", "key-1")))
	if err != nil {
		t.Fatal(err)
	}
	// Same key, same project: the existing agent comes back, even with a different name in the body.
	again, err := ed.Agent.CreateAgent(ctx, connect.NewRequest(req("p1", "idem-other-name", "vm_p1", "key-1")))
	if err != nil {
		t.Fatal(err)
	}
	if again.Msg.Id != first.Msg.Id {
		t.Fatalf("idempotent create returned %s, want %s", again.Msg.Id, first.Msg.Id)
	}
	if n := len(e.listAgentIDs(ed, "p1")); n != 1 {
		t.Fatalf("agents after replay = %d", n)
	}
	// Without a key the same name is a conflict.
	if _, err := ed.Agent.CreateAgent(ctx, connect.NewRequest(req("p1", "idem", "vm_p1", ""))); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("duplicate name: %v", code(err))
	}
	// Keys are scoped to the project: another tenant may use the same key for its own agent.
	other, err := e.otherTenant().Agent.CreateAgent(ctx, connect.NewRequest(req("p2", "idem", "vm_p2", "key-1")))
	if err != nil {
		t.Fatal(err)
	}
	if other.Msg.Id == first.Msg.Id {
		t.Fatal("key leaked across projects")
	}
}

func TestListAgentsFilterAndPagination(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()
	e.vm.put("vm_p1b", "p1", vmdv1.VMState_VM_STATE_RUNNING)
	for i := range 5 {
		e.createAgent(ed, "p1", "agent-"+string(rune('a'+i)), "vm_p1")
	}
	e.createAgent(ed, "p1", "elsewhere", "vm_p1b")
	e.createAgent(e.otherTenant(), "p2", "foreign", "vm_p2")

	// Filter by VM.
	res, err := ed.Agent.ListAgents(ctx, connect.NewRequest(&agentdv1.ListAgentsRequest{ProjectId: "p1", VmId: "vm_p1b"}))
	if err != nil || len(res.Msg.Agents) != 1 || res.Msg.Agents[0].Name != "elsewhere" {
		t.Fatalf("vm filter: %v %+v", err, res)
	}

	// Pages of 2 over all six agents: no duplicates, no gaps, and the last page has no token.
	var seen []string
	token := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination does not terminate")
		}
		res, err := ed.Agent.ListAgents(ctx, connect.NewRequest(&agentdv1.ListAgentsRequest{ProjectId: "p1", PageSize: 2, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range res.Msg.Agents {
			seen = append(seen, a.Id)
		}
		token = res.Msg.NextPageToken
		if token == "" {
			break
		}
		if len(res.Msg.Agents) != 2 {
			t.Fatalf("non-final page has %d items", len(res.Msg.Agents))
		}
	}
	if len(seen) != 6 || !slices.IsSorted(seen) {
		t.Fatalf("paged agents = %v", seen)
	}
	if !slices.Equal(seen, e.listAgentIDs(ed, "p1")) {
		t.Fatal("paged order differs from unpaged order")
	}
}

func TestListAgentsRequiresProject(t *testing.T) {
	e := newEnv(t)
	_, err := e.editor().Agent.ListAgents(context.Background(), connect.NewRequest(&agentdv1.ListAgentsRequest{}))
	if code(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v", code(err))
	}
	_, err = e.editor().Agent.ListAgents(context.Background(), connect.NewRequest(&agentdv1.ListAgentsRequest{ProjectId: "p2"}))
	if code(err) != connect.CodePermissionDenied {
		t.Fatalf("listing another tenant's project: %v", code(err))
	}
}

func TestUpdateAgentValidationAndNoOp(t *testing.T) {
	e := newEnv(t)
	ed := e.editor()
	ctx := context.Background()
	a := e.createAgent(ed, "p1", "upd", "vm_p1")

	cases := map[string]*agentdv1.UpdateAgentRequest{
		"empty model":        {Id: a.Id, Model: ptr("")},
		"unknown model":      {Id: a.Id, Model: ptr("gpt-nope")},
		"huge system prompt": {Id: a.Id, SystemPrompt: ptr(strings.Repeat("y", 8<<10+1))},
	}
	for name, req := range cases {
		if _, err := ed.Agent.UpdateAgent(ctx, connect.NewRequest(req)); code(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: code = %v", name, code(err))
		}
	}
	// Setting the values the agent already has changes nothing and keeps its timestamps.
	same, err := ed.Agent.UpdateAgent(ctx, connect.NewRequest(&agentdv1.UpdateAgentRequest{Id: a.Id, Model: ptr("llama3.2")}))
	if err != nil {
		t.Fatal(err)
	}
	if !same.Msg.UpdatedAt.AsTime().Equal(a.UpdatedAt.AsTime()) {
		t.Fatal("no-op update touched updated_at")
	}
}

func TestUpdateAgentNotFound(t *testing.T) {
	e := newEnv(t)
	_, err := e.editor().Agent.UpdateAgent(context.Background(), connect.NewRequest(&agentdv1.UpdateAgentRequest{
		Id: "agent_missing", Model: ptr("llama3.2"),
	}))
	if code(err) != connect.CodeNotFound {
		t.Fatalf("code = %v", code(err))
	}
}

// conflictStore makes the next n UpdateAgent or UpdateTask calls fail with a stale-version conflict, so the
// compare-and-swap retry loops can be exercised.
type conflictStore struct {
	store.Store
	agentConflicts atomic.Int32
}

func (c *conflictStore) UpdateAgent(ctx context.Context, a *domain.Agent) error {
	if c.agentConflicts.Add(-1) >= 0 {
		return fmt.Errorf("agent %s: stale version: %w", a.ID, errs.ErrConflict)
	}
	return c.Store.UpdateAgent(ctx, a)
}

func TestUpdateAgentRetriesOnConflict(t *testing.T) {
	var cs *conflictStore
	e := newEnv(t, func(d *service.Deps) {
		cs = &conflictStore{Store: d.Store}
		d.Store = cs
	})
	ed := e.editor()
	ctx := context.Background()
	a := e.createAgent(ed, "p1", "cas", "vm_p1")

	cs.agentConflicts.Store(3) // three lost races, then the write goes through
	got, err := ed.Agent.UpdateAgent(ctx, connect.NewRequest(&agentdv1.UpdateAgentRequest{Id: a.Id, Model: ptr("qwen2.5-coder:7b")}))
	if err != nil {
		t.Fatalf("update after retries: %v", err)
	}
	if got.Msg.Model != "qwen2.5-coder:7b" {
		t.Fatalf("model = %q", got.Msg.Model)
	}

	cs.agentConflicts.Store(100) // never wins: the caller gets Aborted rather than a silent loss
	_, err = ed.Agent.UpdateAgent(ctx, connect.NewRequest(&agentdv1.UpdateAgentRequest{Id: a.Id, SystemPrompt: ptr("x")}))
	if code(err) != connect.CodeAborted {
		t.Fatalf("persistent conflicts: code = %v", code(err))
	}
	cs.agentConflicts.Store(0)
}
