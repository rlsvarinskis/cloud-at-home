package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
)

func TestHooksFeedTheSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := New(reg).Hooks()

	h.OnStep(&domain.Task{})
	h.OnStep(&domain.Task{})
	h.OnLeaseLost(&domain.Task{})
	h.OnTaskFinished(&domain.Task{State: domain.TaskSucceeded}, 3*time.Second)
	h.OnTaskFinished(&domain.Task{State: domain.TaskSucceeded}, 90*time.Second)
	h.OnTaskFinished(&domain.Task{State: domain.TaskError}, time.Second)

	want := `
# HELP attic_agent_tasks_finished_total Tasks that reached a final state, by state.
# TYPE attic_agent_tasks_finished_total counter
attic_agent_tasks_finished_total{state="cancelled"} 0
attic_agent_tasks_finished_total{state="error"} 1
attic_agent_tasks_finished_total{state="failed"} 0
attic_agent_tasks_finished_total{state="gave_up"} 0
attic_agent_tasks_finished_total{state="succeeded"} 2
# HELP attic_agent_steps_total Commands run by agents inside VMs.
# TYPE attic_agent_steps_total counter
attic_agent_steps_total 2
# HELP attic_agent_lease_lost_total Tasks whose lease was taken over while a worker was running them.
# TYPE attic_agent_lease_lost_total counter
attic_agent_lease_lost_total 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"attic_agent_tasks_finished_total", "attic_agent_steps_total", "attic_agent_lease_lost_total"); err != nil {
		t.Fatal(err)
	}
	if n, err := testutil.GatherAndCount(reg, "attic_agent_task_duration_seconds"); err != nil || n != 1 {
		t.Fatalf("duration histogram: %d series, %v", n, err)
	}
}

func TestLintClean(t *testing.T) {
	reg := prometheus.NewRegistry()
	New(reg)
	problems, err := testutil.GatherAndLint(reg)
	if err != nil || len(problems) != 0 {
		t.Fatalf("lint: %v %v", problems, err)
	}
}
