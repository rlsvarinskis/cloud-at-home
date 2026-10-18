package e2e_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
)

// Scenario 8: both servers expose their metrics behind the operator role.
func TestMetrics(t *testing.T) {
	w := newWorld(t, backendMemory)
	ctx := context.Background()
	vm := w.runningVM("metered")
	ed := w.editor()
	const model = "scripted-metrics"
	w.hub.script(model, llm.CallTool("bash", map[string]any{"command": "echo m"}), llm.Say("Done.\nVERDICT: SUCCESS"))
	agent := w.createAgent(ed, "p1", "metrics-agent", vm.Id, model)
	task := w.createTask(ed, agent.Id, "measure", "")
	w.runTasks()
	if got := w.getTask(ed, task.Id); got.State != agentdv1.TaskState_TASK_STATE_SUCCEEDED_UNVERIFIED {
		t.Fatalf("setup task state %v", got.State)
	}
	_, err := ed.Task.ListSteps(ctx, connect.NewRequest(&agentdv1.ListStepsRequest{TaskId: task.Id}))
	must(t, err)

	op := w.operator()
	vmdStatus, vmdBody := scrape(t, w.vmdSrv.URL, op.Token)
	if vmdStatus != http.StatusOK {
		t.Fatalf("vmd /metrics with operator token: status %d", vmdStatus)
	}
	for _, want := range [][]string{
		{"attic_rpc_requests_total{", `server="vmd"`, `method="CreateVM"`},
		{"attic_rpc_requests_total{", `server="vmd"`, `method="StartJob"`},
		{"attic_rpc_duration_seconds_count{", `server="vmd"`, `method="CreateVM"`},
		{"attic_cluster_vms"},
		{"attic_cluster_cpu_threads"},
	} {
		if !hasSeries(vmdBody, want...) {
			t.Errorf("vmd /metrics has no series containing %q", want)
		}
	}

	agStatus, agBody := scrape(t, w.agentSrv.URL, op.Token)
	if agStatus != http.StatusOK {
		t.Fatalf("agentd /metrics with operator token: status %d", agStatus)
	}
	for _, want := range [][]string{
		{"attic_rpc_requests_total{", `server="agentd"`, `method="CreateAgent"`},
		{"attic_rpc_requests_total{", `server="agentd"`, `method="CreateTask"`},
		{"attic_rpc_requests_total{", `server="agentd"`, `method="ListSteps"`},
	} {
		if !hasSeries(agBody, want...) {
			t.Errorf("agentd /metrics has no series containing %q", want)
		}
	}

	for _, base := range []string{w.vmdSrv.URL, w.agentSrv.URL} {
		if st, _ := scrape(t, base, ""); st != http.StatusUnauthorized {
			t.Errorf("%s /metrics without a token: status %d, want 401", base, st)
		}
		if st, _ := scrape(t, base, ed.Token); st != http.StatusForbidden {
			t.Errorf("%s /metrics with an editor token: status %d, want 403", base, st)
		}
	}
}

// scrape fetches /metrics with an optional bearer token.
func scrape(t *testing.T, base, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/metrics", nil)
	must(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	must(t, err)
	return resp.StatusCode, string(body)
}

// hasSeries reports whether some exposition line is the series named parts[0] (with or without labels) and
// contains every other part.
func hasSeries(body string, parts ...string) bool {
	name := strings.TrimSuffix(parts[0], "{")
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+" ") && !strings.HasPrefix(line, name+"{") {
			continue
		}
		ok := true
		for _, p := range parts[1:] {
			if !strings.Contains(line, p) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
