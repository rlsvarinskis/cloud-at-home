// Package storetest is a conformance suite for implementations of agentd's store.Store. Run executes each
// behaviour as a subtest on a fresh store, so any implementation can be checked against the same contract.
package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/store"
)

// Run executes the conformance suite. newStore must return an empty store; it is called once per subtest.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()
	run := func(name string, fn func(t *testing.T, s store.Store)) {
		t.Run(name, func(t *testing.T) { fn(t, newStore(t)) })
	}

	run("agent/create-and-get", testAgentCreateAndGet)
	run("agent/duplicate-id-and-name", testAgentDuplicates)
	run("agent/idempotency-key", testAgentIdempotencyKey)
	run("agent/update-cas", testAgentUpdateCAS)
	run("agent/update-name-clash", testAgentUpdateNameClash)
	run("agent/list-filters-and-paging", testAgentListFilters)
	run("agent/count-and-list-by-vm", testAgentCountAndListByVM)
	run("agent/delete-cascades", testAgentDeleteCascades)

	run("task/create-and-get", testTaskCreateAndGet)
	run("task/duplicate-id-and-key", testTaskDuplicates)
	run("task/idempotency-key", testTaskIdempotencyKey)
	run("task/update-cas", testTaskUpdateCAS)
	run("task/list-filters-and-paging", testTaskListFilters)
	run("task/progress-fields-round-trip", testTaskProgressRoundTrip)
	run("task/succeeded-unverified-state", testTaskSucceededUnverifiedState)

	run("claim/order-limit-and-fields", testClaimOrderLimitAndFields)
	run("claim/lease-respected-and-takeover", testClaimLeaseAndTakeover)
	run("claim/skips-cancelled-and-final", testClaimSkipsCancelledAndFinal)
	run("lease/extend-only-by-owner", testExtendLease)

	run("steps/append-and-list", testStepsAppendAndList)
	run("steps/paging", testStepsPaging)
	run("steps/exit-code-round-trip", testStepExitCode)
	run("messages/append-and-list", testMessages)

	run("isolation/copies-in-and-out", testIsolation)

	run("tx/commit", testTxCommit)
	run("tx/rollback", testTxRollback)
	run("tx/read-your-writes", testTxReadYourWrites)

	run("concurrency/single-task-claimed-once", testConcurrentClaim)
	run("concurrency/steps-get-dense-seqs", testConcurrentAppendStep)
}

// ---- fixtures and helpers ----

var (
	bg    = context.Background()
	epoch = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	ttl   = time.Minute
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func newAgent(id, project, name, vmID string) *domain.Agent {
	return &domain.Agent{
		ID: id, ProjectID: project, Name: name, VMID: vmID,
		Model: "m", SystemPrompt: "sp", Workdir: "/work",
		State: domain.AgentActive, Labels: map[string]string{"k": "v"},
		CreatedAt: epoch, UpdatedAt: epoch,
	}
}

func newTask(id, agentID, project string, state domain.TaskState) *domain.Task {
	return &domain.Task{
		ID: id, AgentID: agentID, ProjectID: project, Prompt: "p", MaxSteps: 5,
		State: state, CreatedAt: epoch,
	}
}

func newStep(taskID, command string) *domain.Step {
	return &domain.Step{TaskID: taskID, Command: command, Output: "out " + command, At: epoch}
}

func newMessage(taskID, body string) *domain.Message {
	return &domain.Message{TaskID: taskID, JSON: []byte(body), At: epoch}
}

func agentIDs(as []*domain.Agent) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}

func taskIDs(ts []*domain.Task) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func stepSeqs(ss []*domain.Step) []int {
	out := make([]int, 0, len(ss))
	for _, st := range ss {
		out = append(out, st.Seq)
	}
	return out
}

func expectIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func expectSeqs(t *testing.T, got []int, want ...int) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("seqs = %v, want %v", got, want)
	}
}

// seedAgents creates five agents: a01-a03 in p1 on vm1, a04 in p1 on vm2, a05 in p2 on vm1.
func seedAgents(t *testing.T, s store.Store) {
	t.Helper()
	for _, a := range []*domain.Agent{
		newAgent("a01", "p1", "n1", "vm1"),
		newAgent("a02", "p1", "n2", "vm1"),
		newAgent("a03", "p1", "n3", "vm1"),
		newAgent("a04", "p1", "n4", "vm2"),
		newAgent("a05", "p2", "n5", "vm1"),
	} {
		must(t, s.CreateAgent(bg, a))
	}
}

// ---- agents ----

func testAgentCreateAndGet(t *testing.T, s store.Store) {
	a := newAgent("a1", "p1", "alpha", "vm1")
	must(t, s.CreateAgent(bg, a))
	if a.Version != 1 {
		t.Fatalf("Version after create = %d, want 1", a.Version)
	}
	got, err := s.GetAgent(bg, "a1")
	must(t, err)
	if !reflect.DeepEqual(got, a) {
		t.Fatalf("GetAgent = %+v, want %+v", got, a)
	}
	_, err = s.GetAgent(bg, "missing")
	wantErr(t, err, errs.ErrNotFound)
}

func testAgentDuplicates(t *testing.T, s store.Store) {
	must(t, s.CreateAgent(bg, newAgent("a1", "p1", "alpha", "vm1")))
	wantErr(t, s.CreateAgent(bg, newAgent("a1", "p1", "other", "vm1")), errs.ErrAlreadyExists)
	wantErr(t, s.CreateAgent(bg, newAgent("a2", "p1", "alpha", "vm1")), errs.ErrAlreadyExists)
	must(t, s.CreateAgent(bg, newAgent("a3", "p2", "alpha", "vm1")))
}

func testAgentIdempotencyKey(t *testing.T, s store.Store) {
	a := newAgent("a1", "p1", "alpha", "vm1")
	a.IdempotencyKey = "k1"
	must(t, s.CreateAgent(bg, a))

	got, err := s.GetAgentByIdempotencyKey(bg, "p1", "k1")
	must(t, err)
	if got.ID != "a1" {
		t.Fatalf("by key returned %s, want a1", got.ID)
	}
	_, err = s.GetAgentByIdempotencyKey(bg, "p2", "k1")
	wantErr(t, err, errs.ErrNotFound)
	_, err = s.GetAgentByIdempotencyKey(bg, "p1", "")
	wantErr(t, err, errs.ErrNotFound)

	dup := newAgent("a2", "p1", "beta", "vm1")
	dup.IdempotencyKey = "k1"
	wantErr(t, s.CreateAgent(bg, dup), errs.ErrAlreadyExists)

	otherProject := newAgent("a3", "p2", "alpha", "vm1")
	otherProject.IdempotencyKey = "k1"
	must(t, s.CreateAgent(bg, otherProject))

	// Empty keys never collide with each other.
	must(t, s.CreateAgent(bg, newAgent("a4", "p1", "gamma", "vm1")))
	must(t, s.CreateAgent(bg, newAgent("a5", "p1", "delta", "vm1")))
}

func testAgentUpdateCAS(t *testing.T, s store.Store) {
	must(t, s.CreateAgent(bg, newAgent("a1", "p1", "alpha", "vm1")))
	first, err := s.GetAgent(bg, "a1")
	must(t, err)
	second, err := s.GetAgent(bg, "a1")
	must(t, err)

	first.Model = "new"
	must(t, s.UpdateAgent(bg, first))
	if first.Version != 2 {
		t.Fatalf("Version after update = %d, want 2", first.Version)
	}
	wantErr(t, s.UpdateAgent(bg, second), errs.ErrConflict)

	stored, err := s.GetAgent(bg, "a1")
	must(t, err)
	if stored.Model != "new" || stored.Version != 2 {
		t.Fatalf("stored = model %q version %d, want model \"new\" version 2", stored.Model, stored.Version)
	}
	missing := newAgent("zz", "p1", "zz", "vm1")
	missing.Version = 1
	wantErr(t, s.UpdateAgent(bg, missing), errs.ErrNotFound)
}

func testAgentUpdateNameClash(t *testing.T, s store.Store) {
	must(t, s.CreateAgent(bg, newAgent("a1", "p1", "alpha", "vm1")))
	b := newAgent("a2", "p1", "beta", "vm1")
	must(t, s.CreateAgent(bg, b))
	b.Name = "alpha"
	wantErr(t, s.UpdateAgent(bg, b), errs.ErrAlreadyExists)
	stored, err := s.GetAgent(bg, "a2")
	must(t, err)
	if stored.Name != "beta" {
		t.Fatalf("name after rejected update = %q, want beta", stored.Name)
	}
}

func testAgentListFilters(t *testing.T, s store.Store) {
	seedAgents(t, s)

	got, err := s.ListAgents(bg, "p1", "", "", 0)
	must(t, err)
	expectIDs(t, agentIDs(got), "a01", "a02", "a03", "a04")

	got, err = s.ListAgents(bg, "p1", "vm1", "", 0)
	must(t, err)
	expectIDs(t, agentIDs(got), "a01", "a02", "a03")

	got, err = s.ListAgents(bg, "p2", "", "", 0)
	must(t, err)
	expectIDs(t, agentIDs(got), "a05")

	page1, err := s.ListAgents(bg, "p1", "vm1", "", 2)
	must(t, err)
	expectIDs(t, agentIDs(page1), "a01", "a02")
	page2, err := s.ListAgents(bg, "p1", "vm1", "a02", 2)
	must(t, err)
	expectIDs(t, agentIDs(page2), "a03")
	page3, err := s.ListAgents(bg, "p1", "vm1", "a03", 2)
	must(t, err)
	expectIDs(t, agentIDs(page3))

	after, err := s.ListAgents(bg, "p1", "", "a01", 0)
	must(t, err)
	expectIDs(t, agentIDs(after), "a02", "a03", "a04")
}

func testAgentCountAndListByVM(t *testing.T, s store.Store) {
	seedAgents(t, s)

	n, err := s.CountAgentsOnVM(bg, "vm1")
	must(t, err)
	if n != 4 {
		t.Fatalf("CountAgentsOnVM(vm1) = %d, want 4 (across projects)", n)
	}
	n, err = s.CountAgentsOnVM(bg, "vm2")
	must(t, err)
	if n != 1 {
		t.Fatalf("CountAgentsOnVM(vm2) = %d, want 1", n)
	}
	n, err = s.CountAgentsOnVM(bg, "vm9")
	must(t, err)
	if n != 0 {
		t.Fatalf("CountAgentsOnVM(vm9) = %d, want 0", n)
	}

	onVM, err := s.ListAgentsByVM(bg, "vm1")
	must(t, err)
	expectIDs(t, agentIDs(onVM), "a01", "a02", "a03", "a05")
}

func testAgentDeleteCascades(t *testing.T, s store.Store) {
	must(t, s.CreateAgent(bg, newAgent("a", "p1", "alpha", "vm1")))
	must(t, s.CreateAgent(bg, newAgent("b", "p1", "beta", "vm1")))
	must(t, s.CreateTask(bg, newTask("t1", "a", "p1", domain.TaskQueued)))
	must(t, s.CreateTask(bg, newTask("t2", "a", "p1", domain.TaskQueued)))
	must(t, s.CreateTask(bg, newTask("t3", "b", "p1", domain.TaskQueued)))
	_, err := s.AppendStep(bg, newStep("t1", "ls"))
	must(t, err)
	_, err = s.AppendStep(bg, newStep("t2", "pwd"))
	must(t, err)
	_, err = s.AppendMessage(bg, newMessage("t1", `{"role":"user"}`))
	must(t, err)

	must(t, s.DeleteAgent(bg, "a"))

	_, err = s.GetAgent(bg, "a")
	wantErr(t, err, errs.ErrNotFound)
	_, err = s.GetTask(bg, "t1")
	wantErr(t, err, errs.ErrNotFound)
	_, err = s.GetTask(bg, "t2")
	wantErr(t, err, errs.ErrNotFound)
	_, err = s.ListSteps(bg, "t1", 0, 0)
	wantErr(t, err, errs.ErrNotFound)
	_, err = s.ListMessages(bg, "t1")
	wantErr(t, err, errs.ErrNotFound)
	if _, err := s.GetTask(bg, "t3"); err != nil {
		t.Fatalf("sibling agent's task was removed: %v", err)
	}
	left, err := s.ListTasks(bg, "a", "", "", "", 0)
	must(t, err)
	expectIDs(t, taskIDs(left))

	wantErr(t, s.DeleteAgent(bg, "a"), errs.ErrNotFound)

	// Re-using the IDs must not resurrect old steps or messages.
	must(t, s.CreateTask(bg, newTask("t1", "b", "p1", domain.TaskQueued)))
	steps, err := s.ListSteps(bg, "t1", 0, 0)
	must(t, err)
	if len(steps) != 0 {
		t.Fatalf("re-created task has %d leftover steps", len(steps))
	}
	msgs, err := s.ListMessages(bg, "t1")
	must(t, err)
	if len(msgs) != 0 {
		t.Fatalf("re-created task has %d leftover messages", len(msgs))
	}
	seq, err := s.AppendStep(bg, newStep("t1", "echo"))
	must(t, err)
	if seq != 1 {
		t.Fatalf("first step of re-created task has Seq %d, want 1", seq)
	}
}

// ---- tasks ----

func testTaskCreateAndGet(t *testing.T, s store.Store) {
	tk := newTask("t1", "a1", "p1", domain.TaskQueued)
	must(t, s.CreateTask(bg, tk))
	if tk.Version != 1 {
		t.Fatalf("Version after create = %d, want 1", tk.Version)
	}
	got, err := s.GetTask(bg, "t1")
	must(t, err)
	if !reflect.DeepEqual(got, tk) {
		t.Fatalf("GetTask = %+v, want %+v", got, tk)
	}
	_, err = s.GetTask(bg, "missing")
	wantErr(t, err, errs.ErrNotFound)
}

func testTaskDuplicates(t *testing.T, s store.Store) {
	must(t, s.CreateTask(bg, newTask("t1", "a1", "p1", domain.TaskQueued)))
	wantErr(t, s.CreateTask(bg, newTask("t1", "a1", "p1", domain.TaskQueued)), errs.ErrAlreadyExists)

	dup := newTask("t2", "a1", "p1", domain.TaskQueued)
	dup.IdempotencyKey = "x"
	must(t, s.CreateTask(bg, dup))
	dup2 := newTask("t3", "a1", "p1", domain.TaskQueued)
	dup2.IdempotencyKey = "x"
	wantErr(t, s.CreateTask(bg, dup2), errs.ErrAlreadyExists)
}

func testTaskIdempotencyKey(t *testing.T, s store.Store) {
	tk := newTask("t1", "a1", "p1", domain.TaskQueued)
	tk.IdempotencyKey = "k1"
	must(t, s.CreateTask(bg, tk))

	got, err := s.GetTaskByIdempotencyKey(bg, "p1", "k1")
	must(t, err)
	if got.ID != "t1" {
		t.Fatalf("by key returned %s, want t1", got.ID)
	}
	_, err = s.GetTaskByIdempotencyKey(bg, "p2", "k1")
	wantErr(t, err, errs.ErrNotFound)
	_, err = s.GetTaskByIdempotencyKey(bg, "p1", "")
	wantErr(t, err, errs.ErrNotFound)

	other := newTask("t2", "a1", "p2", domain.TaskQueued)
	other.IdempotencyKey = "k1"
	must(t, s.CreateTask(bg, other))
}

func testTaskUpdateCAS(t *testing.T, s store.Store) {
	must(t, s.CreateTask(bg, newTask("t1", "a1", "p1", domain.TaskQueued)))
	first, err := s.GetTask(bg, "t1")
	must(t, err)
	second, err := s.GetTask(bg, "t1")
	must(t, err)

	first.Result = "done"
	first.State = domain.TaskSucceeded
	must(t, s.UpdateTask(bg, first))
	if first.Version != 2 {
		t.Fatalf("Version after update = %d, want 2", first.Version)
	}
	second.Result = "stale"
	wantErr(t, s.UpdateTask(bg, second), errs.ErrConflict)

	stored, err := s.GetTask(bg, "t1")
	must(t, err)
	if stored.Result != "done" || stored.State != domain.TaskSucceeded {
		t.Fatalf("stored result %q state %q, want done/succeeded", stored.Result, stored.State)
	}
	missing := newTask("zz", "a1", "p1", domain.TaskQueued)
	missing.Version = 1
	wantErr(t, s.UpdateTask(bg, missing), errs.ErrNotFound)
}

func testTaskListFilters(t *testing.T, s store.Store) {
	for _, tk := range []*domain.Task{
		newTask("t01", "A", "p1", domain.TaskQueued),
		newTask("t02", "A", "p1", domain.TaskRunning),
		newTask("t03", "B", "p1", domain.TaskQueued),
		newTask("t04", "C", "p2", domain.TaskQueued),
	} {
		must(t, s.CreateTask(bg, tk))
	}

	got, err := s.ListTasks(bg, "A", "", "", "", 0)
	must(t, err)
	expectIDs(t, taskIDs(got), "t01", "t02")

	got, err = s.ListTasks(bg, "", "p1", "", "", 0)
	must(t, err)
	expectIDs(t, taskIDs(got), "t01", "t02", "t03")

	got, err = s.ListTasks(bg, "", "p1", domain.TaskQueued, "", 0)
	must(t, err)
	expectIDs(t, taskIDs(got), "t01", "t03")

	got, err = s.ListTasks(bg, "A", "p1", domain.TaskRunning, "", 0)
	must(t, err)
	expectIDs(t, taskIDs(got), "t02")

	got, err = s.ListTasks(bg, "", "p1", "", "t01", 1)
	must(t, err)
	expectIDs(t, taskIDs(got), "t02")

	got, err = s.ListTasks(bg, "A", "", "", "t01", 0)
	must(t, err)
	expectIDs(t, taskIDs(got), "t02")

	got, err = s.ListTasks(bg, "", "p1", "", "t03", 0)
	must(t, err)
	expectIDs(t, taskIDs(got))

	_, err = s.ListTasks(bg, "", "", "", "", 0)
	wantErr(t, err, errs.ErrInvalidArgument)
}

// ---- claiming ----

func testTaskProgressRoundTrip(t *testing.T, s store.Store) {
	tk := newTask("t1", "a1", "p1", domain.TaskRunning)
	tk.CheckEachStep = false
	tk.MaxMinutes = 5
	tk.MaxCallTokens = 0
	must(t, s.CreateTask(bg, tk))

	tk.Reason = "repetition"
	tk.VerdictSource = domain.VerdictFromCheck
	tk.PromptTokens, tk.CompletionTokens = 1200, 3400
	tk.ModelCalls, tk.PartialCalls = 3, 1
	tk.FirstTokenSeconds, tk.FirstCommandSeconds = 0.25, 1.5
	tk.LiveAnswer, tk.LiveReasoning = "half an answer", "some thinking"
	tk.Calls = []domain.CallRecord{
		{Index: 1, Seconds: 2.5, PromptTokens: 1000, GenTokens: 40, End: "ok"},
		{Index: 2, Seconds: 1.25, GenTokens: 17, End: "repetition", Partial: true, Note: "repaired a malformed tool call"},
	}
	must(t, s.UpdateTask(bg, tk))
	got, err := s.GetTask(bg, "t1")
	must(t, err)
	if !reflect.DeepEqual(got, tk) {
		t.Fatalf("GetTask = %+v, want %+v", got, tk)
	}

	// Clearing the live fields and the calls is also a plain update.
	tk.LiveAnswer, tk.LiveReasoning, tk.Calls = "", "", nil
	must(t, s.UpdateTask(bg, tk))
	got, err = s.GetTask(bg, "t1")
	must(t, err)
	if got.LiveAnswer != "" || got.LiveReasoning != "" || len(got.Calls) != 0 {
		t.Fatalf("after clearing: %+v", got)
	}
}

func testTaskSucceededUnverifiedState(t *testing.T, s store.Store) {
	tk := newTask("t1", "a1", "p1", domain.TaskRunning)
	must(t, s.CreateTask(bg, tk))
	tk.State = domain.TaskSucceededUnverified
	tk.VerdictSource = domain.VerdictFromModel
	must(t, s.UpdateTask(bg, tk))
	got, err := s.GetTask(bg, "t1")
	must(t, err)
	if got.State != domain.TaskSucceededUnverified || !got.State.Succeeded() {
		t.Fatalf("state = %q", got.State)
	}
	if !got.State.Final() {
		t.Fatalf("succeeded_unverified should be final")
	}
}

func testClaimOrderLimitAndFields(t *testing.T, s store.Store) {
	// Created out of ID order: claims must follow ID order, not creation order.
	for _, id := range []string{"t03", "t01", "t02"} {
		must(t, s.CreateTask(bg, newTask(id, "a", "p1", domain.TaskQueued)))
	}
	stale, err := s.GetTask(bg, "t01")
	must(t, err)

	got, err := s.ClaimTasks(bg, "w1", epoch, ttl, 2)
	must(t, err)
	expectIDs(t, taskIDs(got), "t01", "t02")
	for _, c := range got {
		if c.State != domain.TaskRunning || c.LeaseOwner != "w1" ||
			!c.LeaseUntil.Equal(epoch.Add(ttl)) || !c.StartedAt.Equal(epoch) || c.Attempts != 0 {
			t.Fatalf("claimed task %s = %+v", c.ID, c)
		}
	}
	stored, err := s.GetTask(bg, "t01")
	must(t, err)
	if stored.State != domain.TaskRunning || stored.LeaseOwner != "w1" {
		t.Fatalf("stored claim = state %q owner %q", stored.State, stored.LeaseOwner)
	}
	// A claim is a write: a copy read before it is stale afterwards.
	wantErr(t, s.UpdateTask(bg, stale), errs.ErrConflict)

	got, err = s.ClaimTasks(bg, "w2", epoch, ttl, 10)
	must(t, err)
	expectIDs(t, taskIDs(got), "t03")
}

func testClaimLeaseAndTakeover(t *testing.T, s store.Store) {
	must(t, s.CreateTask(bg, newTask("t01", "a", "p1", domain.TaskQueued)))
	got, err := s.ClaimTasks(bg, "w1", epoch, ttl, 10)
	must(t, err)
	expectIDs(t, taskIDs(got), "t01")

	// The lease is held until it expires.
	got, err = s.ClaimTasks(bg, "w2", epoch.Add(ttl-time.Nanosecond), ttl, 10)
	must(t, err)
	expectIDs(t, taskIDs(got))

	// At expiry the task is up for grabs again.
	got, err = s.ClaimTasks(bg, "w2", epoch.Add(ttl), ttl, 10)
	must(t, err)
	expectIDs(t, taskIDs(got), "t01")
	c := got[0]
	if c.LeaseOwner != "w2" || c.Attempts != 1 || c.State != domain.TaskRunning {
		t.Fatalf("takeover = owner %q attempts %d state %q, want w2/1/running", c.LeaseOwner, c.Attempts, c.State)
	}
	if !c.LeaseUntil.Equal(epoch.Add(2 * ttl)) {
		t.Fatalf("LeaseUntil = %v, want %v", c.LeaseUntil, epoch.Add(2*ttl))
	}
	if !c.StartedAt.Equal(epoch) {
		t.Fatalf("StartedAt changed on takeover: %v, want %v", c.StartedAt, epoch)
	}

	got, err = s.ClaimTasks(bg, "w3", epoch.Add(ttl+time.Second), ttl, 10)
	must(t, err)
	expectIDs(t, taskIDs(got))
}

func testClaimSkipsCancelledAndFinal(t *testing.T, s store.Store) {
	cancelled := newTask("t01", "a", "p1", domain.TaskQueued)
	cancelled.CancelRequested = true
	must(t, s.CreateTask(bg, cancelled))
	must(t, s.CreateTask(bg, newTask("t02", "a", "p1", domain.TaskQueued)))
	must(t, s.CreateTask(bg, newTask("t03", "a", "p1", domain.TaskSucceeded)))
	expired := newTask("t04", "a", "p1", domain.TaskRunning)
	expired.CancelRequested = true
	expired.LeaseOwner = "dead"
	expired.LeaseUntil = epoch.Add(-time.Hour)
	must(t, s.CreateTask(bg, expired))

	got, err := s.ClaimTasks(bg, "w1", epoch, ttl, 10)
	must(t, err)
	expectIDs(t, taskIDs(got), "t02")
}

func testExtendLease(t *testing.T, s store.Store) {
	must(t, s.CreateTask(bg, newTask("t01", "a", "p1", domain.TaskQueued)))
	_, err := s.ClaimTasks(bg, "w1", epoch, ttl, 10)
	must(t, err)

	wantErr(t, s.ExtendLease(bg, "t01", "w2", epoch.Add(time.Second), 5*ttl), errs.ErrConflict)
	wantErr(t, s.ExtendLease(bg, "t01", "", epoch, ttl), errs.ErrInvalidArgument)
	wantErr(t, s.ExtendLease(bg, "nope", "w1", epoch, ttl), errs.ErrNotFound)

	extendAt := epoch.Add(10 * time.Second)
	must(t, s.ExtendLease(bg, "t01", "w1", extendAt, 5*ttl))
	got, err := s.GetTask(bg, "t01")
	must(t, err)
	if !got.LeaseUntil.Equal(extendAt.Add(5 * ttl)) {
		t.Fatalf("LeaseUntil = %v, want %v", got.LeaseUntil, extendAt.Add(5*ttl))
	}
	if got.LeaseOwner != "w1" || got.State != domain.TaskRunning || got.Attempts != 0 {
		t.Fatalf("ExtendLease changed other fields: owner %q state %q attempts %d", got.LeaseOwner, got.State, got.Attempts)
	}

	// Once w2 has taken the task over, w1 has lost the lease.
	_, err = s.ClaimTasks(bg, "w2", epoch.Add(time.Hour), ttl, 10)
	must(t, err)
	wantErr(t, s.ExtendLease(bg, "t01", "w1", epoch.Add(time.Hour), ttl), errs.ErrConflict)
}

// ---- steps and messages ----

func testStepsAppendAndList(t *testing.T, s store.Store) {
	must(t, s.CreateTask(bg, newTask("t01", "a", "p1", domain.TaskRunning)))
	must(t, s.CreateTask(bg, newTask("t02", "a", "p1", domain.TaskRunning)))

	for i, cmd := range []string{"ls", "pwd", "go test"} {
		st := newStep("t01", cmd)
		seq, err := s.AppendStep(bg, st)
		must(t, err)
		if seq != i+1 || st.Seq != seq {
			t.Fatalf("AppendStep(%q) = %d (step Seq %d), want %d", cmd, seq, st.Seq, i+1)
		}
	}
	got, err := s.ListSteps(bg, "t01", 0, 0)
	must(t, err)
	expectSeqs(t, stepSeqs(got), 1, 2, 3)
	if got[2].Command != "go test" || got[2].TaskID != "t01" || got[2].Output != "out go test" {
		t.Fatalf("step 3 = %+v", got[2])
	}

	seq, err := s.AppendStep(bg, newStep("t02", "first"))
	must(t, err)
	if seq != 1 {
		t.Fatalf("first step of t02 has Seq %d, want 1 (seqs are per task)", seq)
	}

	_, err = s.AppendStep(bg, newStep("nope", "x"))
	wantErr(t, err, errs.ErrNotFound)
	_, err = s.ListSteps(bg, "nope", 0, 0)
	wantErr(t, err, errs.ErrNotFound)
}

func testStepsPaging(t *testing.T, s store.Store) {
	must(t, s.CreateTask(bg, newTask("t01", "a", "p1", domain.TaskRunning)))
	for i := range 5 {
		_, err := s.AppendStep(bg, newStep("t01", fmt.Sprint("cmd", i+1)))
		must(t, err)
	}
	got, err := s.ListSteps(bg, "t01", 0, 2)
	must(t, err)
	expectSeqs(t, stepSeqs(got), 1, 2)
	got, err = s.ListSteps(bg, "t01", 2, 2)
	must(t, err)
	expectSeqs(t, stepSeqs(got), 3, 4)
	got, err = s.ListSteps(bg, "t01", 4, 0)
	must(t, err)
	expectSeqs(t, stepSeqs(got), 5)
	got, err = s.ListSteps(bg, "t01", 5, 0)
	must(t, err)
	expectSeqs(t, stepSeqs(got))
}

func testStepExitCode(t *testing.T, s store.Store) {
	must(t, s.CreateTask(bg, newTask("t01", "a", "p1", domain.TaskRunning)))
	code := 7
	withCode := newStep("t01", "make")
	withCode.ExitCode = &code
	_, err := s.AppendStep(bg, withCode)
	must(t, err)
	code = 99 // the stored step must not see this
	_, err = s.AppendStep(bg, newStep("t01", "echo"))
	must(t, err)

	got, err := s.ListSteps(bg, "t01", 0, 0)
	must(t, err)
	if len(got) != 2 {
		t.Fatalf("got %d steps, want 2", len(got))
	}
	if got[0].ExitCode == nil || *got[0].ExitCode != 7 {
		t.Fatalf("exit code = %v, want 7", got[0].ExitCode)
	}
	if got[1].ExitCode != nil {
		t.Fatalf("exit code for step without one = %d, want nil", *got[1].ExitCode)
	}
	if !got[0].At.Equal(epoch) {
		t.Fatalf("At = %v, want %v", got[0].At, epoch)
	}
}

func testMessages(t *testing.T, s store.Store) {
	must(t, s.CreateTask(bg, newTask("t01", "a", "p1", domain.TaskRunning)))
	must(t, s.CreateTask(bg, newTask("t02", "a", "p1", domain.TaskRunning)))
	bodies := []string{`{"role":"user"}`, `{"role":"assistant"}`, `{"role":"tool"}`}
	for i, body := range bodies {
		m := newMessage("t01", body)
		seq, err := s.AppendMessage(bg, m)
		must(t, err)
		if seq != i+1 || m.Seq != seq {
			t.Fatalf("AppendMessage = %d (message Seq %d), want %d", seq, m.Seq, i+1)
		}
	}

	got, err := s.ListMessages(bg, "t01")
	must(t, err)
	if len(got) != len(bodies) {
		t.Fatalf("got %d messages, want %d", len(got), len(bodies))
	}
	for i, m := range got {
		if m.Seq != i+1 || m.TaskID != "t01" || !bytes.Equal(m.JSON, []byte(bodies[i])) || !m.At.Equal(epoch) {
			t.Fatalf("message %d = %+v", i, m)
		}
	}

	none, err := s.ListMessages(bg, "t02")
	must(t, err)
	if len(none) != 0 {
		t.Fatalf("t02 has %d messages, want 0", len(none))
	}
	seq, err := s.AppendMessage(bg, newMessage("t02", "{}"))
	must(t, err)
	if seq != 1 {
		t.Fatalf("first message of t02 has Seq %d, want 1", seq)
	}

	_, err = s.AppendMessage(bg, newMessage("nope", "{}"))
	wantErr(t, err, errs.ErrNotFound)
	_, err = s.ListMessages(bg, "nope")
	wantErr(t, err, errs.ErrNotFound)
}

// ---- isolation ----

func testIsolation(t *testing.T, s store.Store) {
	a := newAgent("a1", "p1", "alpha", "vm1")
	must(t, s.CreateAgent(bg, a))
	a.Labels["k"] = "changed-after-create"
	a.Model = "changed-after-create"

	got, err := s.GetAgent(bg, "a1")
	must(t, err)
	if got.Labels["k"] != "v" || got.Model != "m" {
		t.Fatalf("store saw caller mutation: labels %v model %q", got.Labels, got.Model)
	}
	got.Labels["k"] = "mutated"
	got.Model = "mutated"
	again, err := s.GetAgent(bg, "a1")
	must(t, err)
	if again.Labels["k"] != "v" || again.Model != "m" {
		t.Fatalf("store saw mutation of returned agent: labels %v model %q", again.Labels, again.Model)
	}
	listed, err := s.ListAgents(bg, "p1", "", "", 0)
	must(t, err)
	listed[0].Labels["k"] = "mutated"
	again, err = s.GetAgent(bg, "a1")
	must(t, err)
	if again.Labels["k"] != "v" {
		t.Fatalf("store saw mutation of listed agent: labels %v", again.Labels)
	}

	code := 3
	st := newStep("t1", "make")
	must(t, s.CreateTask(bg, newTask("t1", "a1", "p1", domain.TaskRunning)))
	st.ExitCode = &code
	_, err = s.AppendStep(bg, st)
	must(t, err)
	code = 4
	steps, err := s.ListSteps(bg, "t1", 0, 0)
	must(t, err)
	if *steps[0].ExitCode != 3 {
		t.Fatalf("exit code after caller mutation = %d, want 3", *steps[0].ExitCode)
	}
	*steps[0].ExitCode = 5
	steps, err = s.ListSteps(bg, "t1", 0, 0)
	must(t, err)
	if *steps[0].ExitCode != 3 {
		t.Fatalf("exit code after returned mutation = %d, want 3", *steps[0].ExitCode)
	}

	m := newMessage("t1", `{"a":1}`)
	_, err = s.AppendMessage(bg, m)
	must(t, err)
	m.JSON[1] = 'X'
	msgs, err := s.ListMessages(bg, "t1")
	must(t, err)
	msgs[0].JSON[1] = 'Y'
	msgs, err = s.ListMessages(bg, "t1")
	must(t, err)
	if string(msgs[0].JSON) != `{"a":1}` {
		t.Fatalf("message bytes = %s, want {\"a\":1}", msgs[0].JSON)
	}

	must(t, s.CreateTask(bg, newTask("t2", "a1", "p1", domain.TaskQueued)))
	claimed, err := s.ClaimTasks(bg, "w1", epoch, ttl, 10)
	must(t, err)
	claimed[0].Prompt = "mutated"
	claimed[0].State = domain.TaskQueued
	stored, err := s.GetTask(bg, "t2")
	must(t, err)
	if stored.Prompt != "p" || stored.State != domain.TaskRunning {
		t.Fatalf("store saw mutation of claimed task: prompt %q state %q", stored.Prompt, stored.State)
	}
}

// ---- transactions ----

func testTxCommit(t *testing.T, s store.Store) {
	must(t, s.CreateAgent(bg, newAgent("a1", "p1", "alpha", "vm1")))
	err := s.Tx(bg, func(tx store.Store) error {
		if err := tx.CreateAgent(bg, newAgent("a2", "p1", "beta", "vm1")); err != nil {
			return err
		}
		return tx.CreateTask(bg, newTask("t1", "a2", "p1", domain.TaskQueued))
	})
	must(t, err)
	_, err = s.GetAgent(bg, "a2")
	must(t, err)
	_, err = s.GetTask(bg, "t1")
	must(t, err)
}

func testTxRollback(t *testing.T, s store.Store) {
	must(t, s.CreateAgent(bg, newAgent("a1", "p1", "alpha", "vm1")))
	must(t, s.CreateTask(bg, newTask("t1", "a1", "p1", domain.TaskRunning)))
	boom := errors.New("boom")

	err := s.Tx(bg, func(tx store.Store) error {
		if err := tx.CreateAgent(bg, newAgent("a2", "p1", "beta", "vm1")); err != nil {
			return err
		}
		a, err := tx.GetAgent(bg, "a1")
		if err != nil {
			return err
		}
		a.Model = "changed"
		if err := tx.UpdateAgent(bg, a); err != nil {
			return err
		}
		if _, err := tx.AppendStep(bg, newStep("t1", "ls")); err != nil {
			return err
		}
		return boom
	})
	wantErr(t, err, boom)

	_, err = s.GetAgent(bg, "a2")
	wantErr(t, err, errs.ErrNotFound)
	a, err := s.GetAgent(bg, "a1")
	must(t, err)
	if a.Model != "m" || a.Version != 1 {
		t.Fatalf("agent after rollback = model %q version %d, want m/1", a.Model, a.Version)
	}
	steps, err := s.ListSteps(bg, "t1", 0, 0)
	must(t, err)
	if len(steps) != 0 {
		t.Fatalf("rolled-back step is visible")
	}
	seq, err := s.AppendStep(bg, newStep("t1", "pwd"))
	must(t, err)
	if seq != 1 {
		t.Fatalf("Seq after rollback = %d, want 1 (no gap)", seq)
	}
}

func testTxReadYourWrites(t *testing.T, s store.Store) {
	err := s.Tx(bg, func(tx store.Store) error {
		a := newAgent("a1", "p1", "alpha", "vm1")
		if err := tx.CreateAgent(bg, a); err != nil {
			return err
		}
		got, err := tx.GetAgent(bg, "a1")
		if err != nil {
			return err
		}
		if got.Version != 1 {
			return fmt.Errorf("version inside tx = %d, want 1", got.Version)
		}
		got.Model = "x"
		if err := tx.UpdateAgent(bg, got); err != nil {
			return err
		}
		if err := tx.UpdateAgent(bg, a); !errors.Is(err, errs.ErrConflict) {
			return fmt.Errorf("stale update inside tx = %v, want ErrConflict", err)
		}
		if err := tx.CreateTask(bg, newTask("t1", "a1", "p1", domain.TaskQueued)); err != nil {
			return err
		}
		if _, err := tx.ClaimTasks(bg, "w1", epoch, ttl, 10); err != nil {
			return err
		}
		seq, err := tx.AppendStep(bg, newStep("t1", "ls"))
		if err != nil {
			return err
		}
		if seq != 1 {
			return fmt.Errorf("seq inside tx = %d, want 1", seq)
		}
		return nil
	})
	must(t, err)

	got, err := s.GetAgent(bg, "a1")
	must(t, err)
	if got.Model != "x" || got.Version != 2 {
		t.Fatalf("after commit = model %q version %d, want x/2", got.Model, got.Version)
	}
	task, err := s.GetTask(bg, "t1")
	must(t, err)
	if task.State != domain.TaskRunning || task.LeaseOwner != "w1" {
		t.Fatalf("claimed task after commit = state %q owner %q", task.State, task.LeaseOwner)
	}
	steps, err := s.ListSteps(bg, "t1", 0, 0)
	must(t, err)
	expectSeqs(t, stepSeqs(steps), 1)
}

// ---- concurrency ----

func testConcurrentClaim(t *testing.T, s store.Store) {
	must(t, s.CreateTask(bg, newTask("t01", "a", "p1", domain.TaskQueued)))
	const workers = 16
	start := make(chan struct{})
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := fmt.Sprintf("w%02d", i)
			<-start
			got, err := s.ClaimTasks(bg, owner, epoch, ttl, 1)
			if err != nil {
				t.Errorf("ClaimTasks(%s): %v", owner, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, c := range got {
				winners = append(winners, c.LeaseOwner)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("%d workers claimed the task, want exactly 1", len(winners))
	}
	stored, err := s.GetTask(bg, "t01")
	must(t, err)
	if stored.LeaseOwner != winners[0] {
		t.Fatalf("lease owner = %q, winner = %q", stored.LeaseOwner, winners[0])
	}
}

func testConcurrentAppendStep(t *testing.T, s store.Store) {
	must(t, s.CreateTask(bg, newTask("t01", "a", "p1", domain.TaskRunning)))
	const writers = 16
	start := make(chan struct{})
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seqs []int
	)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			seq, err := s.AppendStep(bg, newStep("t01", "cmd"))
			if err != nil {
				t.Errorf("AppendStep: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			seqs = append(seqs, seq)
		}()
	}
	close(start)
	wg.Wait()

	slices.Sort(seqs)
	want := make([]int, writers)
	for i := range want {
		want[i] = i + 1
	}
	if !slices.Equal(seqs, want) {
		t.Fatalf("returned seqs = %v, want 1..%d", seqs, writers)
	}
	steps, err := s.ListSteps(bg, "t01", 0, 0)
	must(t, err)
	expectSeqs(t, stepSeqs(steps), want...)
}
