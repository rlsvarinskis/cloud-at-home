package service_test

import (
	"context"
	"errors"
	vmdv1 "example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/internal/errs"
	"io"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	agentdv1 "example.com/homecloud/gen/homecloud/agentd/v1"
	"example.com/homecloud/internal/agentd/service"
	"example.com/homecloud/internal/obs"
)

// okCode stands for "the call succeeded" in the authorization matrix.
const okCode connect.Code = 0

func codeOrOK(err error) connect.Code {
	if err == nil {
		return okCode
	}
	return connect.CodeOf(err)
}

// TestAuthorizationMatrix runs every RPC as each kind of caller. The owner (editor of p1) is not in the
// matrix because it is allowed everything the RPC permits.
func TestAuthorizationMatrix(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.editor()
	a := e.createAgent(owner, "p1", "matrix", "vm_p1")
	tk := e.createTask(owner, a.Id, "matrix task")

	rows := []struct {
		name                                        string
		run                                         func(c *clients) error
		viewer, other, service, operator, anonymous connect.Code
	}{
		{"CreateAgent",
			func(c *clients) error {
				_, err := c.Agent.CreateAgent(ctx, connect.NewRequest(&agentdv1.CreateAgentRequest{ProjectId: "p1", Name: "mx", VmId: "vm_p1", Model: "llama3.2"}))
				return err
			},
			connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodeUnauthenticated},
		{"GetAgent",
			func(c *clients) error {
				_, err := c.Agent.GetAgent(ctx, connect.NewRequest(&agentdv1.GetAgentRequest{Id: a.Id}))
				return err
			},
			okCode, connect.CodeNotFound, connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodeUnauthenticated},
		{"ListAgents",
			func(c *clients) error {
				_, err := c.Agent.ListAgents(ctx, connect.NewRequest(&agentdv1.ListAgentsRequest{ProjectId: "p1"}))
				return err
			},
			okCode, connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodeUnauthenticated},
		{"UpdateAgent",
			func(c *clients) error {
				_, err := c.Agent.UpdateAgent(ctx, connect.NewRequest(&agentdv1.UpdateAgentRequest{Id: a.Id, Model: ptr("qwen2.5-coder:7b")}))
				return err
			},
			connect.CodePermissionDenied, connect.CodeNotFound, connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodeUnauthenticated},
		{"DeleteAgent",
			func(c *clients) error {
				_, err := c.Agent.DeleteAgent(ctx, connect.NewRequest(&agentdv1.DeleteAgentRequest{Id: a.Id}))
				return err
			},
			connect.CodePermissionDenied, connect.CodeNotFound, connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodeUnauthenticated},
		{"CreateTask",
			func(c *clients) error {
				_, err := c.Task.CreateTask(ctx, connect.NewRequest(&agentdv1.CreateTaskRequest{AgentId: a.Id, Prompt: "x"}))
				return err
			},
			connect.CodePermissionDenied, connect.CodeNotFound, connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodeUnauthenticated},
		{"GetTask",
			func(c *clients) error {
				_, err := c.Task.GetTask(ctx, connect.NewRequest(&agentdv1.GetTaskRequest{Id: tk.Id}))
				return err
			},
			okCode, connect.CodeNotFound, connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodeUnauthenticated},
		{"ListTasks by agent",
			func(c *clients) error {
				_, err := c.Task.ListTasks(ctx, connect.NewRequest(&agentdv1.ListTasksRequest{AgentId: a.Id}))
				return err
			},
			okCode, connect.CodeNotFound, connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodeUnauthenticated},
		{"ListTasks by project",
			func(c *clients) error {
				_, err := c.Task.ListTasks(ctx, connect.NewRequest(&agentdv1.ListTasksRequest{ProjectId: "p1"}))
				return err
			},
			okCode, connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodeUnauthenticated},
		{"CancelTask",
			func(c *clients) error {
				_, err := c.Task.CancelTask(ctx, connect.NewRequest(&agentdv1.CancelTaskRequest{Id: tk.Id}))
				return err
			},
			connect.CodePermissionDenied, connect.CodeNotFound, connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodeUnauthenticated},
		{"ListSteps",
			func(c *clients) error {
				_, err := c.Task.ListSteps(ctx, connect.NewRequest(&agentdv1.ListStepsRequest{TaskId: tk.Id}))
				return err
			},
			okCode, connect.CodeNotFound, connect.CodePermissionDenied, connect.CodePermissionDenied, connect.CodeUnauthenticated},
		// The service role carries ModelList in internal/auth, so agentd's own identity may list models.
		{"ListModels",
			func(c *clients) error {
				_, err := c.Model.ListModels(ctx, connect.NewRequest(&agentdv1.ListModelsRequest{}))
				return err
			},
			okCode, okCode, okCode, connect.CodePermissionDenied, connect.CodeUnauthenticated},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			check := func(who string, c *clients, want connect.Code) {
				t.Helper()
				if got := codeOrOK(row.run(c)); got != want {
					t.Errorf("%s: code %v, want %v", who, got, want)
				}
			}
			check("viewer", e.viewer(), row.viewer)
			check("other tenant", e.otherTenant(), row.other)
			check("service", e.service(), row.service)
			check("operator", e.operator(), row.operator)
			check("anonymous", e.anonymous(), row.anonymous)
			check("garbage token", e.client("not-a-token"), connect.CodeUnauthenticated)
		})
	}

	// The matrix ran only read-side or refused calls, so the agent and task must be intact.
	if got, err := owner.Agent.GetAgent(ctx, connect.NewRequest(&agentdv1.GetAgentRequest{Id: a.Id})); err != nil || got.Msg.Model != "llama3.2" {
		t.Fatalf("agent changed by refused calls: %v %+v", err, got)
	}
}

func TestListModels(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res, err := e.viewer().Model.ListModels(ctx, connect.NewRequest(&agentdv1.ListModelsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Msg.Models) != 2 || res.Msg.Models[0].Name != "llama3.2" || res.Msg.Models[0].SizeBytes != 2_000_000_000 {
		t.Fatalf("models: %+v", res.Msg.Models)
	}

	e.llm.down.Store(true)
	_, err = e.viewer().Model.ListModels(ctx, connect.NewRequest(&agentdv1.ListModelsRequest{}))
	if code(err) != connect.CodeUnavailable {
		t.Fatalf("LLM down: code %v", code(err))
	}
	// The client learns the backend is down, not where it lives.
	for _, leak := range []string{"198.51.100.7", "11434", "dial tcp", "api/tags"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks infrastructure detail %q: %v", leak, err)
		}
	}
}

// vmd being unreachable must not expose its address either; answers that mean something to the caller still pass.
func TestVMDownstreamErrorsDoNotLeakInternals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.vm.put("vm_1", "p1", vmdv1.VMState_VM_STATE_RUNNING)
	e.vm.getErr = errs.Unavailable(`vmd GetVM: Post "http://vmd.internal:8080/x": dial tcp 198.51.100.3:8080: i/o timeout`)
	_, err := e.editor().Agent.CreateAgent(ctx, connect.NewRequest(&agentdv1.CreateAgentRequest{
		ProjectId: "p1", Name: "a1", VmId: "vm_1", Model: "llama3.2"}))
	if code(err) != connect.CodeUnavailable {
		t.Fatalf("code %v: %v", code(err), err)
	}
	for _, leak := range []string{"vmd.internal", "198.51.100.3", "8080", "dial tcp"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
	e.vm.getErr = nil
	if _, err := e.editor().Agent.CreateAgent(ctx, connect.NewRequest(&agentdv1.CreateAgentRequest{
		ProjectId: "p1", Name: "a1", VmId: "vm_missing", Model: "llama3.2"})); code(err) != connect.CodeNotFound {
		t.Fatalf("a missing VM is still NotFound, got %v", err)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	e := newEnv(t, func(d *service.Deps) {
		d.Ready = []obs.NamedCheck{
			{Name: "store", Check: func(context.Context) error { return nil }},
			{Name: "vmd", Check: func(context.Context) error { return errors.New("connection refused") }},
		}
	})
	get := func(path string) (int, string) {
		t.Helper()
		res, err := e.srv.Client().Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}

	if status, body := get("/healthz"); status != http.StatusOK || body != "ok" {
		t.Fatalf("healthz: %d %q", status, body)
	}
	status, body := get("/readyz")
	if status != http.StatusServiceUnavailable || !strings.Contains(body, `"vmd"`) || strings.Contains(body, "connection refused") {
		t.Fatalf("readyz with a failing dependency: %d %s", status, body)
	}
	if strings.Contains(body, `"store"`) {
		t.Fatalf("passing check listed as failed: %s", body)
	}
}

func TestReadyzOKWithoutChecks(t *testing.T) {
	e := newEnv(t)
	res, err := e.srv.Client().Get(e.srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok"`) {
		t.Fatalf("readyz: %d %s", res.StatusCode, body)
	}
}

func TestMetricsEndpointAuthAndCounters(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ed := e.editor()
	e.createAgent(ed, "p1", "metered", "vm_p1")
	if _, err := e.viewer().Agent.GetAgent(ctx, connect.NewRequest(&agentdv1.GetAgentRequest{Id: "agent_missing"})); code(err) != connect.CodeNotFound {
		t.Fatalf("setup: %v", code(err))
	}

	scrape := func(token string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/metrics", nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := e.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}

	if status, _ := scrape(""); status != http.StatusUnauthorized {
		t.Fatalf("no token: %d", status)
	}
	if status, _ := scrape("garbage"); status != http.StatusUnauthorized {
		t.Fatalf("invalid token: %d", status)
	}
	if status, _ := scrape(e.viewer().Token); status != http.StatusForbidden {
		t.Fatalf("project viewer: %d", status)
	}
	status, body := scrape(e.operator().Token)
	if status != http.StatusOK {
		t.Fatalf("operator: %d", status)
	}
	for _, want := range []string{
		`homecloud_rpc_requests_total{code="ok",method="CreateAgent",server="agentd",service="homecloud.agentd.v1.AgentService"} 1`,
		`code="not_found",method="GetAgent"`,
		`homecloud_rpc_duration_seconds`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}
