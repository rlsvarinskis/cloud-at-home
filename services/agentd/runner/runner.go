// Package runner runs agents' tasks. Workers claim queued tasks from the store under a lease, drive the
// model through the bash tool inside the agent's VM, and record every message and command so that another
// worker can resume a task whose owner died.
//
// Every write to a task's conversation or state goes through the store in a transaction that first checks
// the worker still holds the lease. A worker that has lost its lease therefore never writes a final state.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/store"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/vmclient"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

// Defaults applied by New to zero Config fields.
const (
	defaultWorkers        = 2
	defaultLeaseTTL       = 30 * time.Second
	defaultPollInterval   = 500 * time.Millisecond
	defaultCancelPoll     = time.Second
	defaultCommandTimeout = 10 * time.Minute
	defaultMaxAttempts    = 3
	defaultMaxSteps       = 40
)

// Config holds everything a Runner needs. Store, LLM and VM are required.
type Config struct {
	Store store.Store
	LLM   llm.Client
	VM    vmclient.VM

	// Owner is this worker's unique identity, for example hostname and pid. It is the lease owner.
	Owner string
	// Workers is the number of tasks run concurrently (default 2).
	Workers int
	// LeaseTTL is how long a claim lasts without a heartbeat (default 30s). The heartbeat runs every LeaseTTL/3.
	LeaseTTL time.Duration
	// PollInterval is the idle sleep between claim attempts (default 500ms).
	PollInterval time.Duration
	// CancelPoll is how often a running task's row is checked for a cancel request (default 1s).
	CancelPoll time.Duration
	// CommandTimeout limits the lifetime of one command the model runs (default 10m).
	CommandTimeout time.Duration
	// MaxAttempts is how many takeovers or retries a task gets before it is failed with state error (default 3).
	MaxAttempts int

	Log *slog.Logger     // default slog.Default()
	Now func() time.Time // default time.Now

	Hooks Hooks
}

// Hooks are optional callbacks. They run on the worker's goroutine (OnLeaseLost on the heartbeat goroutine)
// and must not block for long.
type Hooks struct {
	// OnTaskFinished runs once after a task's final state is written, with the task as stored and the time
	// since it started.
	OnTaskFinished func(t *domain.Task, d time.Duration)
	// OnStep runs after each command is recorded.
	OnStep func(t *domain.Task)
	// OnLeaseLost runs when a heartbeat finds the lease taken by another owner. The worker then stops the task
	// without writing anything.
	OnLeaseLost func(t *domain.Task)
}

// Runner claims and runs tasks. Create one with New.
type Runner struct {
	cfg Config
	log *slog.Logger
	now func() time.Time
}

// New returns a Runner with defaults filled in for zero fields.
func New(c Config) *Runner {
	if c.Workers <= 0 {
		c.Workers = defaultWorkers
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = defaultLeaseTTL
	}
	if c.PollInterval <= 0 {
		c.PollInterval = defaultPollInterval
	}
	if c.CancelPoll <= 0 {
		c.CancelPoll = defaultCancelPoll
	}
	if c.CommandTimeout <= 0 {
		c.CommandTimeout = defaultCommandTimeout
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultMaxAttempts
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return &Runner{cfg: c, log: c.Log, now: c.Now}
}

// Run starts Workers loops and blocks until ctx ends and all of them have returned. A task in flight when ctx
// ends keeps its lease and is left unfinished for another worker to resume after the lease expires.
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < r.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.work(ctx)
		}()
	}
	wg.Wait()
}

// RunOnce claims at most one task and runs it to completion (or until ctx ends). It reports whether a task
// was claimed.
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	claimed, _, err := r.claimAndRun(ctx)
	return claimed, err
}

func (r *Runner) work(ctx context.Context) {
	for ctx.Err() == nil {
		claimed, out, err := r.claimAndRun(ctx)
		if err != nil && ctx.Err() == nil {
			r.log.Error("run task", "err", err)
		}
		if err != nil || !claimed || out == outRequeued {
			sleep(ctx, r.cfg.PollInterval)
		}
	}
}

// claimAndRun leases one queued or expired task and runs it.
func (r *Runner) claimAndRun(ctx context.Context) (bool, outcome, error) {
	tasks, err := r.cfg.Store.ClaimTasks(ctx, r.cfg.Owner, r.now(), r.cfg.LeaseTTL, 1)
	if err != nil {
		return false, outAbandoned, fmt.Errorf("claim tasks: %w", err)
	}
	if len(tasks) == 0 {
		return false, outAbandoned, nil
	}
	out, err := r.runTask(ctx, tasks[0])
	return true, out, err
}

// errLeaseLost means this worker no longer holds the task's lease, or the task is already final.
var errLeaseLost = errors.New("runner: lease lost or task already final")

// fenced runs fn in a store transaction, but only while this worker holds the task's lease and the task is
// not final. Optimistic conflicts are retried a few times. It returns the task as written by fn.
func (r *Runner) fenced(ctx context.Context, taskID string, fn func(s store.Store, cur *domain.Task) error) (*domain.Task, error) {
	for attempt := 0; ; attempt++ {
		var out *domain.Task
		err := r.cfg.Store.Tx(ctx, func(s store.Store) error {
			cur, err := s.GetTask(ctx, taskID)
			if err != nil {
				return err
			}
			if cur.State.Final() || cur.LeaseOwner != r.cfg.Owner {
				return errLeaseLost
			}
			if err := fn(s, cur); err != nil {
				return err
			}
			out = cur
			return nil
		})
		if errors.Is(err, errs.ErrConflict) && attempt < 4 {
			continue
		}
		return out, err
	}
}

// finish writes the task's final state and clears its lease. If the task is no longer leased to this worker
// or is already final, nothing is written and the outcome is outAbandoned.
func (r *Runner) finish(ctx context.Context, t *domain.Task, state domain.TaskState, verdict, result string) (outcome, error) {
	now := r.now()
	result = capResult(Clean(result))
	fresh, err := r.fenced(ctx, t.ID, func(s store.Store, cur *domain.Task) error {
		cur.State = state
		cur.Verdict = verdict
		cur.Result = result
		cur.FinishedAt = now
		cur.LeaseOwner = ""
		cur.LeaseUntil = time.Time{}
		return s.UpdateTask(ctx, cur)
	})
	if errors.Is(err, errLeaseLost) {
		r.log.Info("task not finished: lease lost or already final", "task", t.ID)
		return outAbandoned, nil
	}
	if err != nil {
		return outAbandoned, fmt.Errorf("finish task %s: %w", t.ID, err)
	}
	*t = *fresh
	var d time.Duration
	if !fresh.StartedAt.IsZero() {
		d = now.Sub(fresh.StartedAt)
	}
	if r.cfg.Hooks.OnTaskFinished != nil {
		r.cfg.Hooks.OnTaskFinished(fresh, d)
	}
	r.log.Info("task finished", "task", t.ID, "state", state)
	return outFinished, nil
}

// requeue releases the lease after a transient failure so the task is retried later. Each requeue counts as an
// attempt; once attempts exceed MaxAttempts the task is failed with state error instead.
func (r *Runner) requeue(ctx context.Context, t *domain.Task, cause error) (outcome, error) {
	now := r.now()
	gaveUp := false
	fresh, err := r.fenced(ctx, t.ID, func(s store.Store, cur *domain.Task) error {
		cur.Attempts++
		if cur.Attempts > r.cfg.MaxAttempts {
			gaveUp = true
			cur.State = domain.TaskError
			cur.Result = capResult(Clean(fmt.Sprintf("gave up after %d attempts: %v", r.cfg.MaxAttempts, cause)))
			cur.FinishedAt = now
		} else {
			cur.State = domain.TaskQueued
		}
		cur.LeaseOwner = ""
		cur.LeaseUntil = time.Time{}
		return s.UpdateTask(ctx, cur)
	})
	if errors.Is(err, errLeaseLost) {
		return outAbandoned, nil
	}
	if err != nil {
		return outAbandoned, fmt.Errorf("requeue task %s: %w", t.ID, err)
	}
	*t = *fresh
	if !gaveUp {
		r.log.Warn("task requeued after transient failure", "task", t.ID, "attempts", fresh.Attempts, "err", cause)
		return outRequeued, nil
	}
	if r.cfg.Hooks.OnTaskFinished != nil {
		var d time.Duration
		if !fresh.StartedAt.IsZero() {
			d = now.Sub(fresh.StartedAt)
		}
		r.cfg.Hooks.OnTaskFinished(fresh, d)
	}
	return outFinished, nil
}

// sleep waits for d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
