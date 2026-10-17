// Package metrics exports what the agent runner is doing as Prometheus series.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"example.com/homecloud/internal/agentd/domain"
	"example.com/homecloud/internal/agentd/runner"
)

// Runner counts task outcomes, commands run and lease problems. Pass Hooks() to runner.Config.
type Runner struct {
	finished  *prometheus.CounterVec
	duration  prometheus.Histogram
	steps     prometheus.Counter
	leaseLost prometheus.Counter
}

// New registers the runner series on reg.
func New(reg prometheus.Registerer) *Runner {
	m := &Runner{
		finished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "homecloud_agent_tasks_finished_total", Help: "Tasks that reached a final state, by state.",
		}, []string{"state"}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "homecloud_agent_task_duration_seconds", Help: "Wall time from start to final state.",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
		}),
		steps: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "homecloud_agent_steps_total", Help: "Commands run by agents inside VMs.",
		}),
		leaseLost: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "homecloud_agent_lease_lost_total", Help: "Tasks whose lease was taken over while a worker was running them.",
		}),
	}
	reg.MustRegister(m.finished, m.duration, m.steps, m.leaseLost)
	// Make every state visible from the first scrape.
	for _, s := range []domain.TaskState{domain.TaskSucceeded, domain.TaskFailed, domain.TaskGaveUp, domain.TaskCancelled, domain.TaskError} {
		m.finished.WithLabelValues(string(s))
	}
	return m
}

// Hooks returns the runner hooks that feed these series.
func (m *Runner) Hooks() runner.Hooks {
	return runner.Hooks{
		OnTaskFinished: func(t *domain.Task, d time.Duration) {
			m.finished.WithLabelValues(string(t.State)).Inc()
			m.duration.Observe(d.Seconds())
		},
		OnStep:      func(*domain.Task) { m.steps.Inc() },
		OnLeaseLost: func(*domain.Task) { m.leaseLost.Inc() },
	}
}
