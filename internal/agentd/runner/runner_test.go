package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	vmdv1 "example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/internal/agentd/domain"
	"example.com/homecloud/internal/agentd/llm"
	"example.com/homecloud/internal/agentd/store"
)

// ---- fixtures ----

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// spyLLM wraps the scripted fake and signals each Chat call as it starts.
type spyLLM struct {
	inner   *llm.Fake
	entered chan struct{}
}

func (s *spyLLM) Chat(ctx context.Context, model string, msgs []llm.Message, tools []llm.Tool, onChunk func(llm.Chunk)) (llm.Message, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	return s.inner.Chat(ctx, model, msgs, tools, onChunk)
}

func (s *spyLLM) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	return s.inner.ListModels(ctx)
}

// hookLog records hook calls.
type hookLog struct {
	mu        sync.Mutex
	steps     int
	finished  []domain.Task
	states    []domain.TaskState
	leaseLost int
}

func (h *hookLog) onStep(*domain.Task) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.steps++
}

func (h *hookLog) onFinished(t *domain.Task, _ time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.finished = append(h.finished, *t)
	h.states = append(h.states, t.State)
}

func (h *hookLog) onLeaseLost(*domain.Task) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leaseLost++
}

func (h *hookLog) counts() (steps, finished, leaseLost int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.steps, len(h.finished), h.leaseLost
}

type fixture struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Memory
	fake  *llm.Fake
	spy   *spyLLM
	vm    *fakeVM
	clock *fakeClock
	hooks *hookLog
	cfg   Config
}

// newFixture returns a store holding active agent "a1" (VM "vm1", working directory /home/agent/work), a
// running fake VM, and a Config with short intervals. The scripted model replies are used in order.
func newFixture(t *testing.T, replies ...llm.FakeReply) *fixture {
	t.Helper()
	fx := &fixture{
		t:     t,
		ctx:   context.Background(),
		st:    store.NewMemory(),
		fake:  &llm.Fake{Script: replies},
		vm:    newFakeVM(),
		clock: &fakeClock{now: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)},
		hooks: &hookLog{},
	}
	fx.spy = &spyLLM{inner: fx.fake, entered: make(chan struct{}, 100)}
	fx.vm.setVM("vm1", vmdv1.VMState_VM_STATE_RUNNING)
	must(t, fx.st.CreateAgent(fx.ctx, &domain.Agent{
		ID: "a1", ProjectID: "p1", Name: "builder", VMID: "vm1", Model: "test-model",
		SystemPrompt: "Be terse.", Workdir: "/home/agent/work", State: domain.AgentActive,
	}))
	fx.cfg = Config{
		Store:        fx.st,
		LLM:          fx.spy,
		VM:           fx.vm,
		Owner:        "w1",
		LeaseTTL:     30 * time.Second,
		PollInterval: 2 * time.Millisecond,
		CancelPoll:   5 * time.Millisecond,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:          fx.clock.Now,
		Hooks: Hooks{
			OnTaskFinished: fx.hooks.onFinished,
			OnStep:         fx.hooks.onStep,
			OnLeaseLost:    fx.hooks.onLeaseLost,
		},
	}
	return fx
}

func (fx *fixture) runner() *Runner { return New(fx.cfg) }

// addTask queues a task for agent a1.
func (fx *fixture) addTask(id, checkCommand string, maxSteps int) {
	fx.t.Helper()
	must(fx.t, fx.st.CreateTask(fx.ctx, &domain.Task{
		ID: id, AgentID: "a1", ProjectID: "p1", Prompt: "Build the thing.",
		CheckCommand: checkCommand, MaxSteps: maxSteps, State: domain.TaskQueued,
	}))
}

func (fx *fixture) task(id string) *domain.Task {
	fx.t.Helper()
	tk, err := fx.st.GetTask(fx.ctx, id)
	must(fx.t, err)
	return tk
}

// mutate changes a task row inside a store transaction.
func (fx *fixture) mutate(id string, fn func(*domain.Task)) {
	fx.t.Helper()
	must(fx.t, fx.st.Tx(fx.ctx, func(s store.Store) error {
		cur, err := s.GetTask(fx.ctx, id)
		if err != nil {
			return err
		}
		fn(cur)
		return s.UpdateTask(fx.ctx, cur)
	}))
}

func (fx *fixture) requestCancel(id string) {
	fx.mutate(id, func(tk *domain.Task) { tk.CancelRequested = true })
}

// storedMessages returns the stored conversation, decoded, with each row's Seq.
func (fx *fixture) storedMessages(id string) ([]llm.Message, []int) {
	fx.t.Helper()
	rows, err := fx.st.ListMessages(fx.ctx, id)
	must(fx.t, err)
	var out []llm.Message
	var seqs []int
	for _, row := range rows {
		var m llm.Message
		must(fx.t, json.Unmarshal(row.JSON, &m))
		out = append(out, m)
		seqs = append(seqs, row.Seq)
	}
	return out, seqs
}

func (fx *fixture) steps(id string) []*domain.Step {
	fx.t.Helper()
	rows, err := fx.st.ListSteps(fx.ctx, id, 0, 0)
	must(fx.t, err)
	return rows
}

// waitEntered waits until the model has been called.
func (fx *fixture) waitEntered() {
	fx.t.Helper()
	select {
	case <-fx.spy.entered:
	case <-time.After(5 * time.Second):
		fx.t.Fatal("the model was never called")
	}
}

// runAsync runs RunOnce in a goroutine and returns the channel its error arrives on.
func runAsync(r *Runner, ctx context.Context) <-chan error {
	errc := make(chan error, 1)
	go func() {
		_, err := r.RunOnce(ctx)
		errc <- err
	}()
	return errc
}

func waitErr(t *testing.T, errc <-chan error) {
	t.Helper()
	select {
	case err := <-errc:
		must(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce did not return")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func bash(cmd string) llm.FakeReply {
	return llm.CallTool("bash", map[string]any{"command": cmd})
}

func say(text string) llm.FakeReply { return llm.Say(text) }

// ---- single-task behaviour ----

func TestSimpleSuccess(t *testing.T) {
	fx := newFixture(t, bash("ls"), say("Listed the files.\nVERDICT: SUCCESS"))
	fx.vm.setScript("ls", jobScript{stdout: "a.txt\n"})
	fx.addTask("t1", "", 0)

	ran, err := fx.runner().RunOnce(fx.ctx)
	must(t, err)
	if !ran {
		t.Fatal("RunOnce claimed nothing")
	}

	tk := fx.task("t1")
	if tk.State != domain.TaskSucceeded || tk.Verdict != "SUCCESS" {
		t.Fatalf("state=%s verdict=%q, want succeeded/SUCCESS", tk.State, tk.Verdict)
	}
	if !strings.Contains(tk.Result, "Listed the files.") || tk.Steps != 1 {
		t.Fatalf("result=%q steps=%d", tk.Result, tk.Steps)
	}
	if tk.LeaseOwner != "" || !tk.LeaseUntil.IsZero() || tk.FinishedAt.IsZero() {
		t.Fatalf("lease not cleared or FinishedAt unset: owner=%q until=%v finished=%v", tk.LeaseOwner, tk.LeaseUntil, tk.FinishedAt)
	}

	reqs := fx.vm.startRequests()
	if len(reqs) != 1 {
		t.Fatalf("StartJob calls = %d, want 1", len(reqs))
	}
	want := &vmdv1.StartJobRequest{VmId: "vm1", Command: "ls", Workdir: "/home/agent/work",
		TimeoutSeconds: 600, WaitSeconds: startWait, MaxOutput: 16384, IdempotencyKey: "t1-1"}
	if got := reqs[0]; got.VmId != want.VmId || got.Command != want.Command || got.Workdir != want.Workdir ||
		got.TimeoutSeconds != want.TimeoutSeconds || got.WaitSeconds != want.WaitSeconds ||
		got.MaxOutput != want.MaxOutput || got.IdempotencyKey != want.IdempotencyKey {
		t.Fatalf("StartJob request = %v, want %v", got, want)
	}

	if len(fx.fake.Calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(fx.fake.Calls))
	}
	first := fx.fake.Calls[0]
	if len(first.Tools) != 1 || first.Tools[0].Name != "bash" {
		t.Fatalf("tools = %+v, want one bash tool", first.Tools)
	}
	sys := first.Messages[0].Content
	if first.Messages[0].Role != "system" || !strings.Contains(sys, "Your working directory is /home/agent/work") ||
		!strings.Contains(sys, "Be terse.") || !strings.Contains(sys, "VERDICT: SUCCESS") {
		t.Fatalf("system message = %q", sys)
	}
	if first.Messages[1].Role != "user" || first.Messages[1].Content != "Build the thing." {
		t.Fatalf("user message = %+v", first.Messages[1])
	}

	msgs, seqs := fx.storedMessages("t1")
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "system,user,assistant,tool,assistant" {
		t.Fatalf("stored roles = %s", got)
	}
	for i, s := range seqs {
		if s != i+1 {
			t.Fatalf("message seqs = %v, want dense from 1", seqs)
		}
	}
	if !strings.Contains(msgs[3].Content, "a.txt") || !strings.HasSuffix(msgs[3].Content, "[exit 0]") {
		t.Fatalf("tool message = %q", msgs[3].Content)
	}

	steps := fx.steps("t1")
	if len(steps) != 1 || steps[0].Seq != 1 || steps[0].Command != "ls" || steps[0].Output != "a.txt\n" ||
		steps[0].ExitCode == nil || *steps[0].ExitCode != 0 {
		t.Fatalf("steps = %+v", steps)
	}

	if s, f, _ := fx.hooks.counts(); s != 1 || f != 1 {
		t.Fatalf("hooks: steps=%d finished=%d, want 1 and 1", s, f)
	}
	if fx.hooks.states[0] != domain.TaskSucceeded {
		t.Fatalf("OnTaskFinished state = %s", fx.hooks.states[0])
	}
}

func TestRunOnceWithNothingQueued(t *testing.T) {
	fx := newFixture(t)
	ran, err := fx.runner().RunOnce(fx.ctx)
	must(t, err)
	if ran {
		t.Fatal("RunOnce reported a task on an empty queue")
	}
}

func TestFailedVerdict(t *testing.T) {
	fx := newFixture(t, say("The tests still fail.\nVERDICT: FAILED - tests red"))
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))
	tk := fx.task("t1")
	if tk.State != domain.TaskFailed || tk.Verdict != "FAILED" {
		t.Fatalf("state=%s verdict=%q, want failed/FAILED", tk.State, tk.Verdict)
	}
}

func runOnce(fx *fixture) error {
	_, err := fx.runner().RunOnce(fx.ctx)
	return err
}

func TestNoVerdictNudgesOnceThenGivesUp(t *testing.T) {
	fx := newFixture(t, say("I did some things."), say("Still no verdict."))
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))

	tk := fx.task("t1")
	if tk.State != domain.TaskGaveUp || tk.Verdict != "" {
		t.Fatalf("state=%s verdict=%q, want gave_up with no verdict", tk.State, tk.Verdict)
	}
	if len(fx.fake.Calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(fx.fake.Calls))
	}
	second := fx.fake.Calls[1].Messages
	if last := second[len(second)-1]; last.Role != "user" || !strings.Contains(last.Content, "VERDICT: SUCCESS") {
		t.Fatalf("second call did not end with the nudge: %+v", last)
	}
	msgs, _ := fx.storedMessages("t1")
	if len(msgs) != 5 || msgs[3].Role != "user" {
		t.Fatalf("stored conversation = %d messages, nudge missing", len(msgs))
	}
}

func TestModelWithoutToolsAndWithVerdict(t *testing.T) {
	fx := newFixture(t, say("Nothing to do.\nVERDICT: SUCCESS"))
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))
	tk := fx.task("t1")
	if tk.State != domain.TaskSucceeded || tk.Steps != 0 || fx.vm.startCount() != 0 {
		t.Fatalf("state=%s steps=%d starts=%d", tk.State, tk.Steps, fx.vm.startCount())
	}
}

func TestCheckCommandCombinations(t *testing.T) {
	cases := []struct {
		name      string
		verdict   string
		exit      int32
		wantState domain.TaskState
		wantNote  string
	}{
		{"success verdict, check passes", "VERDICT: SUCCESS", 0, domain.TaskSucceeded, ""},
		{"success verdict, check fails", "VERDICT: SUCCESS", 1, domain.TaskFailed, "reported SUCCESS but the check FAILED"},
		{"failed verdict, check passes", "VERDICT: FAILED - nope", 0, domain.TaskSucceeded, "reported FAILED but the check passed"},
		{"failed verdict, check fails", "VERDICT: FAILED - nope", 1, domain.TaskFailed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t, say("Report.\n"+tc.verdict))
			fx.vm.setScript("make check", jobScript{stdout: "checked\n", exit: tc.exit})
			fx.addTask("t1", "make check", 0)
			must(t, runOnce(fx))

			tk := fx.task("t1")
			if tk.State != tc.wantState {
				t.Fatalf("state=%s, want %s", tk.State, tc.wantState)
			}
			if !strings.Contains(tk.Result, fmt.Sprintf("[check_command exit %d]", tc.exit)) {
				t.Fatalf("result lacks check exit line: %q", tk.Result)
			}
			if tc.wantNote != "" && !strings.Contains(tk.Result, tc.wantNote) {
				t.Fatalf("result lacks note %q: %q", tc.wantNote, tk.Result)
			}
			if tc.wantNote == "" && strings.Contains(tk.Result, "[note:") {
				t.Fatalf("unexpected note: %q", tk.Result)
			}
			if fx.vm.startRequests()[0].IdempotencyKey != "t1-check" {
				t.Fatalf("check key = %q", fx.vm.startRequests()[0].IdempotencyKey)
			}
			if tk.Steps != 0 {
				t.Fatalf("check command counted as a step: %d", tk.Steps)
			}
		})
	}
}

func TestStepLimit(t *testing.T) {
	fx := newFixture(t, bash("echo a"), bash("echo b"), bash("echo c"))
	fx.addTask("t1", "", 2)
	must(t, runOnce(fx))

	tk := fx.task("t1")
	if tk.State != domain.TaskGaveUp || tk.Steps != 2 {
		t.Fatalf("state=%s steps=%d, want gave_up after 2 steps", tk.State, tk.Steps)
	}
	if !strings.Contains(tk.Result, "[stopped after 2 steps without a final answer]") {
		t.Fatalf("result = %q", tk.Result)
	}
	if len(fx.fake.Calls) != 2 {
		t.Fatalf("model calls = %d, want 2 (no call after the limit)", len(fx.fake.Calls))
	}
}

func TestInvalidToolCallsAreRefused(t *testing.T) {
	fx := newFixture(t, llm.FakeReply{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
		{Name: "python", Arguments: map[string]any{"command": "print(1)"}},
		{Name: "bash", Arguments: map[string]any{}},
		{Name: "bash", Arguments: map[string]any{"command": 42}},
	}}}, say("Gave up on tools.\nVERDICT: SUCCESS"))
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))

	if fx.vm.startCount() != 0 {
		t.Fatalf("refused calls reached the VM: %d starts", fx.vm.startCount())
	}
	msgs, _ := fx.storedMessages("t1")
	var errs []string
	for _, m := range msgs {
		if m.Role == "tool" {
			errs = append(errs, m.Content)
		}
	}
	if len(errs) != 3 {
		t.Fatalf("tool results = %d, want 3", len(errs))
	}
	for _, e := range errs {
		if !strings.HasPrefix(e, "error: ") {
			t.Fatalf("tool result %q does not start with 'error: '", e)
		}
	}
	if tk := fx.task("t1"); tk.Steps != 0 || tk.State != domain.TaskSucceeded {
		t.Fatalf("steps=%d state=%s", tk.Steps, tk.State)
	}
}

func TestMultipleToolCallsInOneReply(t *testing.T) {
	fx := newFixture(t, llm.FakeReply{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
		{Name: "bash", Arguments: map[string]any{"command": "echo 1"}},
		{Name: "bash", Arguments: map[string]any{"command": "echo 2"}},
		{Name: "bash", Arguments: map[string]any{"command": "echo 3"}},
	}}}, say("Three done.\nVERDICT: SUCCESS"))
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))

	var keys, cmds []string
	for _, r := range fx.vm.startRequests() {
		keys = append(keys, r.IdempotencyKey)
		cmds = append(cmds, r.Command)
	}
	if strings.Join(keys, ",") != "t1-1,t1-2,t1-3" || strings.Join(cmds, ",") != "echo 1,echo 2,echo 3" {
		t.Fatalf("keys=%v cmds=%v", keys, cmds)
	}
	msgs, seqs := fx.storedMessages("t1")
	if len(msgs) != 7 {
		t.Fatalf("stored messages = %d, want 7", len(msgs))
	}
	for i, want := range []string{"ran: echo 1", "ran: echo 2", "ran: echo 3"} {
		if got := msgs[3+i]; got.Role != "tool" || !strings.HasPrefix(got.Content, want) {
			t.Fatalf("tool result %d = %+v, want prefix %q", i, got, want)
		}
	}
	for i, s := range seqs {
		if s != i+1 {
			t.Fatalf("message seqs not dense: %v", seqs)
		}
	}
	steps := fx.steps("t1")
	if len(steps) != 3 {
		t.Fatalf("steps = %d", len(steps))
	}
	for i, s := range steps {
		if s.Seq != i+1 {
			t.Fatalf("step seqs not dense: %d at %d", s.Seq, i)
		}
	}
	if s, _, _ := fx.hooks.counts(); s != 3 {
		t.Fatalf("OnStep calls = %d, want 3", s)
	}
}

func TestSanitisesOutputAndCommand(t *testing.T) {
	fx := newFixture(t, bash("echo bad\x00"), say("Done\x00.\nVERDICT: SUCCESS\x00"))
	fx.vm.setScript("echo bad", jobScript{stdout: "a\x00b\xff\xfe end\n", stderr: "warn\x00\n"})
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))

	if got := fx.vm.startRequests()[0].Command; got != "echo bad" {
		t.Fatalf("command sent to VM = %q, want NUL removed", got)
	}
	steps := fx.steps("t1")
	if len(steps) != 1 {
		t.Fatalf("steps = %d", len(steps))
	}
	checkClean := func(what, s string) {
		t.Helper()
		if strings.Contains(s, "\x00") || !utf8.ValidString(s) {
			t.Fatalf("%s is not clean: %q", what, s)
		}
	}
	checkClean("step output", steps[0].Output)
	checkClean("step command", steps[0].Command)
	if !strings.Contains(steps[0].Output, "ab� end") {
		t.Fatalf("step output = %q", steps[0].Output)
	}
	toolMsg := fx.fake.Calls[1].Messages[3].Content
	checkClean("tool message sent to model", toolMsg)

	msgs, _ := fx.storedMessages("t1")
	for _, m := range msgs {
		checkClean("stored message "+m.Role, m.Content)
	}
	tk := fx.task("t1")
	checkClean("result", tk.Result)
	if tk.State != domain.TaskSucceeded {
		t.Fatalf("state = %s", tk.State)
	}
}

func TestOutputIsClipped(t *testing.T) {
	big := strings.Repeat("x", 10000) + "END"
	fx := newFixture(t, bash("big"), say("Big.\nVERDICT: SUCCESS"))
	fx.vm.setScript("big", jobScript{stdout: big})
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))

	toolMsg := fx.fake.Calls[1].Messages[3].Content
	if !strings.Contains(toolMsg, "\n...[truncated]...\n") {
		t.Fatal("tool message has no truncation marker")
	}
	if !strings.HasSuffix(toolMsg, "END\n[exit 0]") {
		t.Fatalf("tool message does not keep the tail: ...%q", toolMsg[len(toolMsg)-30:])
	}
	if len(toolMsg) > 8000+len("\n...[truncated]...\n")+len("\n[exit 0]") {
		t.Fatalf("tool message too long: %d", len(toolMsg))
	}
	if got := fx.steps("t1")[0].Output; len(got) > 4000 || !strings.HasSuffix(got, "END") {
		t.Fatalf("stored output len=%d, want <= 4000 keeping the tail", len(got))
	}
}

func TestCleanAndClipHelpers(t *testing.T) {
	if got := Clean("a\x00b\xffc"); got != "ab�c" {
		t.Fatalf("Clean = %q", got)
	}
	if got := tailBytes("ééé", 3); !utf8.ValidString(got) || got != "é" {
		t.Fatalf("tailBytes on runes = %q", got)
	}
	if got := headBytes("ééé", 3); !utf8.ValidString(got) || got != "é" {
		t.Fatalf("headBytes on runes = %q", got)
	}
}

// ---- cancellation ----

func TestCancelWhileModelIsBlocked(t *testing.T) {
	block := make(chan struct{})
	fx := newFixture(t, llm.FakeReply{Block: block})
	fx.addTask("t1", "", 0)
	errc := runAsync(fx.runner(), fx.ctx)

	fx.waitEntered()
	fx.requestCancel("t1")
	waitErr(t, errc) // RunOnce returns only once the blocked model call returned on its cancelled context

	tk := fx.task("t1")
	if tk.State != domain.TaskCancelled || tk.Result != "[cancelled]" || tk.Verdict != "" {
		t.Fatalf("state=%s result=%q verdict=%q", tk.State, tk.Result, tk.Verdict)
	}
	if _, finished, _ := fx.hooks.counts(); finished != 1 {
		t.Fatalf("OnTaskFinished calls = %d", finished)
	}
}

func TestCancelWhileCommandRuns(t *testing.T) {
	fx := newFixture(t, bash("sleep 100"))
	fx.vm.setScript("sleep 100", jobScript{hang: true})
	fx.addTask("t1", "", 0)
	errc := runAsync(fx.runner(), fx.ctx)

	waitFor(t, "the command to start", func() bool { return fx.vm.startCount() == 1 })
	fx.requestCancel("t1")
	waitErr(t, errc)

	if got := fx.vm.cancelled(); len(got) != 1 || got[0] != "job-1" {
		t.Fatalf("CancelJob calls = %v, want [job-1]", got)
	}
	if tk := fx.task("t1"); tk.State != domain.TaskCancelled || tk.Steps != 0 {
		t.Fatalf("state=%s steps=%d", tk.State, tk.Steps)
	}
}

func TestGracefulShutdownLeavesTaskLeased(t *testing.T) {
	block := make(chan struct{})
	fx := newFixture(t, llm.FakeReply{Block: block})
	fx.addTask("t1", "", 0)
	fx.cfg.Workers = 1
	ctx, stop := context.WithCancel(fx.ctx)
	runDone := make(chan struct{})
	go func() {
		fx.runner().Run(ctx)
		close(runDone)
	}()

	fx.waitEntered()
	stop()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}

	tk := fx.task("t1")
	if tk.State != domain.TaskRunning || tk.LeaseOwner != "w1" || !tk.FinishedAt.IsZero() {
		t.Fatalf("state=%s owner=%q finished=%v: want running and still leased", tk.State, tk.LeaseOwner, tk.FinishedAt)
	}
	if _, finished, _ := fx.hooks.counts(); finished != 0 {
		t.Fatal("OnTaskFinished ran during shutdown")
	}
}

// ---- leases ----

func TestHeartbeatExtendsLease(t *testing.T) {
	block := make(chan struct{})
	fx := newFixture(t, llm.FakeReply{Block: block})
	fx.cfg.LeaseTTL = 60 * time.Millisecond
	fx.addTask("t1", "", 0)
	errc := runAsync(fx.runner(), fx.ctx)

	fx.waitEntered()
	before := fx.task("t1").LeaseUntil
	fx.clock.Advance(time.Minute)
	waitFor(t, "the heartbeat to extend the lease", func() bool {
		return fx.task("t1").LeaseUntil.After(before.Add(30 * time.Second))
	})
	fx.requestCancel("t1")
	waitErr(t, errc)
}

func TestLeaseLossStopsWithoutFinalWrite(t *testing.T) {
	block := make(chan struct{})
	fx := newFixture(t, llm.FakeReply{Block: block})
	fx.cfg.LeaseTTL = 30 * time.Millisecond
	fx.addTask("t1", "", 0)
	errc := runAsync(fx.runner(), fx.ctx)

	fx.waitEntered()
	fx.mutate("t1", func(tk *domain.Task) { tk.LeaseOwner = "thief" })
	waitFor(t, "the lease-lost hook", func() bool {
		_, _, lost := fx.hooks.counts()
		return lost == 1
	})
	waitErr(t, errc)

	tk := fx.task("t1")
	if tk.State != domain.TaskRunning || tk.LeaseOwner != "thief" || !tk.FinishedAt.IsZero() || tk.Result != "" {
		t.Fatalf("lost worker wrote to the task: state=%s owner=%q result=%q", tk.State, tk.LeaseOwner, tk.Result)
	}
	if _, finished, _ := fx.hooks.counts(); finished != 0 {
		t.Fatal("OnTaskFinished ran for a task this worker lost")
	}
}

func TestCrashResumesFromStoredConversation(t *testing.T) {
	fx := newFixture(t, bash("ls"), say("Listed.\nVERDICT: SUCCESS"))
	fx.vm.setScript("ls", jobScript{stdout: "a.txt\n"})
	fx.addTask("t1", "", 0)

	// The first worker stops right after its first command is recorded, as if it had crashed.
	ctx1, stop1 := context.WithCancel(fx.ctx)
	cfg1 := fx.cfg
	cfg1.Owner = "w1"
	cfg1.Hooks = Hooks{OnStep: func(*domain.Task) { stop1() }, OnTaskFinished: fx.hooks.onFinished}
	ran, err := New(cfg1).RunOnce(ctx1)
	must(t, err)
	if !ran {
		t.Fatal("first worker claimed nothing")
	}
	if tk := fx.task("t1"); tk.State != domain.TaskRunning || tk.Steps != 1 || tk.LeaseOwner != "w1" {
		t.Fatalf("after crash: state=%s steps=%d owner=%q", tk.State, tk.Steps, tk.LeaseOwner)
	}

	fx.clock.Advance(time.Hour) // the first worker's lease has expired
	cfg2 := fx.cfg
	cfg2.Owner = "w2"
	ran, err = New(cfg2).RunOnce(fx.ctx)
	must(t, err)
	if !ran {
		t.Fatal("second worker claimed nothing")
	}

	tk := fx.task("t1")
	if tk.State != domain.TaskSucceeded || tk.Attempts != 1 || tk.Steps != 1 {
		t.Fatalf("state=%s attempts=%d steps=%d, want succeeded/1/1", tk.State, tk.Attempts, tk.Steps)
	}
	if n := fx.vm.startCount(); n != 1 {
		t.Fatalf("finished command was run again: %d StartJob calls", n)
	}
	if len(fx.fake.Calls) != 2 {
		t.Fatalf("model calls = %d", len(fx.fake.Calls))
	}
	resumed := fx.fake.Calls[1].Messages
	if got := resumed[len(resumed)-1]; got.Role != "tool" || !strings.Contains(got.Content, "a.txt") {
		t.Fatalf("resumed model did not see the stored tool result: %+v", got)
	}
}

func TestResumeRunsOnlyMissingToolCalls(t *testing.T) {
	fx := newFixture(t, say("Both done.\nVERDICT: SUCCESS"))
	fx.addTask("t1", "", 0)
	ctx := fx.ctx
	seed := func(m llm.Message) {
		raw, err := json.Marshal(m)
		must(t, err)
		_, err = fx.st.AppendMessage(ctx, &domain.Message{TaskID: "t1", JSON: raw})
		must(t, err)
	}
	seed(llm.Message{Role: "system", Content: "sys"})
	seed(llm.Message{Role: "user", Content: "Build the thing."})
	seed(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
		{Name: "bash", Arguments: map[string]any{"command": "echo one"}},
		{Name: "bash", Arguments: map[string]any{"command": "echo two"}},
	}})
	seed(llm.Message{Role: "tool", ToolName: "bash", Content: "one\n[exit 0]"})
	_, err := fx.st.AppendStep(ctx, &domain.Step{TaskID: "t1", Command: "echo one", Output: "one\n", ExitCode: intPtr(0)})
	must(t, err)
	fx.mutate("t1", func(tk *domain.Task) { tk.Steps = 1 })

	must(t, runOnce(fx))

	reqs := fx.vm.startRequests()
	if len(reqs) != 1 || reqs[0].Command != "echo two" || reqs[0].IdempotencyKey != "t1-2" {
		t.Fatalf("StartJob calls = %v, want only echo two as t1-2", reqs)
	}
	if tk := fx.task("t1"); tk.State != domain.TaskSucceeded || tk.Steps != 2 {
		t.Fatalf("state=%s steps=%d", tk.State, tk.Steps)
	}
}

func TestCorruptStoredMessageFailsTask(t *testing.T) {
	fx := newFixture(t)
	fx.addTask("t1", "", 0)
	_, err := fx.st.AppendMessage(fx.ctx, &domain.Message{TaskID: "t1", JSON: []byte("{not json")})
	must(t, err)
	must(t, runOnce(fx))
	if tk := fx.task("t1"); tk.State != domain.TaskError || !strings.Contains(tk.Result, "corrupt") {
		t.Fatalf("state=%s result=%q", tk.State, tk.Result)
	}
}

// ---- agent and VM checks ----

func TestMissingAgentFailsTask(t *testing.T) {
	fx := newFixture(t)
	must(t, fx.st.CreateTask(fx.ctx, &domain.Task{ID: "t1", AgentID: "ghost", ProjectID: "p1", Prompt: "x", State: domain.TaskQueued}))
	must(t, runOnce(fx))
	if tk := fx.task("t1"); tk.State != domain.TaskError || tk.Result != "agent was deleted" {
		t.Fatalf("state=%s result=%q", tk.State, tk.Result)
	}
}

func TestInactiveAgentFailsTask(t *testing.T) {
	fx := newFixture(t)
	a, err := fx.st.GetAgent(fx.ctx, "a1")
	must(t, err)
	a.State = domain.AgentOrphaned
	must(t, fx.st.UpdateAgent(fx.ctx, a))
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))
	if tk := fx.task("t1"); tk.State != domain.TaskError || tk.Result != "agent is orphaned" {
		t.Fatalf("state=%s result=%q", tk.State, tk.Result)
	}
}

func TestDeletedVMOrphansAgent(t *testing.T) {
	fx := newFixture(t)
	fx.vm.deleteVM("vm1")
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))

	if tk := fx.task("t1"); tk.State != domain.TaskError || tk.Result != "the VM no longer exists" {
		t.Fatalf("state=%s result=%q", tk.State, tk.Result)
	}
	a, err := fx.st.GetAgent(fx.ctx, "a1")
	must(t, err)
	if a.State != domain.AgentOrphaned {
		t.Fatalf("agent state = %s, want orphaned", a.State)
	}
}

func TestStoppedVMFailsTaskWithoutStarting(t *testing.T) {
	fx := newFixture(t)
	fx.vm.setVM("vm1", vmdv1.VMState_VM_STATE_STOPPED)
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))
	if tk := fx.task("t1"); tk.State != domain.TaskError || tk.Result != "vm is stopped" {
		t.Fatalf("state=%s result=%q", tk.State, tk.Result)
	}
	if fx.vm.startCount() != 0 {
		t.Fatal("a command started on a stopped VM")
	}
}

func TestVMUnavailableRequeuesThenSucceeds(t *testing.T) {
	fx := newFixture(t, say("Fine.\nVERDICT: SUCCESS"))
	fx.vm.setFailGets(1)
	fx.addTask("t1", "", 0)
	r := fx.runner()

	must(t, runOnceWith(r, fx))
	if tk := fx.task("t1"); tk.State != domain.TaskQueued || tk.Attempts != 1 || tk.LeaseOwner != "" {
		t.Fatalf("after unavailable: state=%s attempts=%d owner=%q", tk.State, tk.Attempts, tk.LeaseOwner)
	}
	must(t, runOnceWith(r, fx))
	if tk := fx.task("t1"); tk.State != domain.TaskSucceeded {
		t.Fatalf("state after retry = %s", tk.State)
	}
}

func runOnceWith(r *Runner, fx *fixture) error {
	_, err := r.RunOnce(fx.ctx)
	return err
}

func TestMaxAttemptsExhaustedFailsTask(t *testing.T) {
	fx := newFixture(t)
	fx.cfg.MaxAttempts = 2
	fx.vm.setFailGets(100)
	fx.addTask("t1", "", 0)
	r := fx.runner()
	for i := 0; i < 3; i++ {
		must(t, runOnceWith(r, fx))
	}
	tk := fx.task("t1")
	if tk.State != domain.TaskError || !strings.Contains(tk.Result, "gave up after 2 attempts") {
		t.Fatalf("state=%s result=%q", tk.State, tk.Result)
	}
}

func TestTakeoverLimitFailsTask(t *testing.T) {
	fx := newFixture(t)
	fx.addTask("t1", "", 0)
	fx.mutate("t1", func(tk *domain.Task) { tk.Attempts = 4 })
	must(t, runOnce(fx))
	if tk := fx.task("t1"); tk.State != domain.TaskError || tk.Result != "gave up after 3 takeovers" {
		t.Fatalf("state=%s result=%q", tk.State, tk.Result)
	}
}

// ---- concurrency ----

func TestConcurrentWorkersFinishEachTaskOnce(t *testing.T) {
	const n = 12
	var replies []llm.FakeReply
	for i := 0; i < n; i++ {
		replies = append(replies, say("Done.\nVERDICT: SUCCESS"))
	}
	fx := newFixture(t, replies...)
	for i := 0; i < n; i++ {
		fx.addTask(fmt.Sprintf("t%02d", i), "", 0)
	}
	fx.cfg.Workers = 4
	ctx, stop := context.WithCancel(fx.ctx)
	runDone := make(chan struct{})
	go func() {
		fx.runner().Run(ctx)
		close(runDone)
	}()

	waitFor(t, "all tasks to finish", func() bool {
		done, err := fx.st.ListTasks(fx.ctx, "", "p1", domain.TaskSucceeded, "", 0)
		must(t, err)
		return len(done) == n
	})
	stop()
	<-runDone

	seen := map[string]int{}
	fx.hooks.mu.Lock()
	for _, tk := range fx.hooks.finished {
		seen[tk.ID]++
	}
	fx.hooks.mu.Unlock()
	if len(seen) != n {
		t.Fatalf("finished hooks for %d tasks, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("task %s finished %d times", id, c)
		}
	}
}

// ---- helpers ----

func intPtr(i int) *int { return &i }

// Shutting down, or losing the lease, must NOT kill the command the model started: the worker that takes the
// task over re-attaches to the same job (idempotency key) and sees its real result. Only a user cancel kills it.
func TestShutdownDoesNotKillTheRunningCommand(t *testing.T) {
	fx := newFixture(t, bash("sleep 100"))
	fx.vm.setScript("sleep 100", jobScript{hang: true})
	fx.addTask("t1", "", 0)
	fx.cfg.Workers = 1
	ctx, stop := context.WithCancel(fx.ctx)
	runDone := make(chan struct{})
	go func() {
		fx.runner().Run(ctx)
		close(runDone)
	}()

	waitFor(t, "the command to start", func() bool { return fx.vm.startCount() == 1 })
	stop()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
	if got := fx.vm.cancelled(); len(got) != 0 {
		t.Fatalf("shutdown cancelled VM jobs %v; the next worker must be able to re-attach to them", got)
	}
	if tk := fx.task("t1"); tk.State != domain.TaskRunning || tk.LeaseOwner != "w1" {
		t.Fatalf("task should stay leased for takeover: state=%s owner=%q", tk.State, tk.LeaseOwner)
	}
}

func TestLeaseLossDoesNotKillTheRunningCommand(t *testing.T) {
	fx := newFixture(t, bash("sleep 100"))
	fx.vm.setScript("sleep 100", jobScript{hang: true})
	fx.cfg.LeaseTTL = 30 * time.Millisecond
	fx.addTask("t1", "", 0)
	errc := runAsync(fx.runner(), fx.ctx)

	waitFor(t, "the command to start", func() bool { return fx.vm.startCount() == 1 })
	fx.mutate("t1", func(tk *domain.Task) { tk.LeaseOwner = "thief" })
	waitFor(t, "the lease-lost hook", func() bool {
		_, _, lost := fx.hooks.counts()
		return lost == 1
	})
	waitErr(t, errc)
	if got := fx.vm.cancelled(); len(got) != 0 {
		t.Fatalf("lease loss cancelled VM jobs %v; the new owner now depends on them", got)
	}
}

// slowStartVM makes the StartJob RPC itself take a while, as vmd does when it waits for a quick command.
type slowStartVM struct {
	*fakeVM
	entered chan struct{} // closed when StartJob has been called
	release chan struct{} // StartJob returns once this is closed
	once    sync.Once
	sawDone atomic.Bool // the context StartJob received was cancelled
}

func (s *slowStartVM) StartJob(ctx context.Context, r *vmdv1.StartJobRequest) (*vmdv1.JobOutput, error) {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
		s.sawDone.Store(true)
		return nil, ctx.Err()
	}
	return s.fakeVM.StartJob(ctx, r)
}

// A cancel that arrives while the start request is in flight must not leave the command running unsupervised:
// the request is not abortable, so its job id is learned and the job is cancelled.
func TestCancelWhileStartRequestIsInFlightStillCancelsTheJob(t *testing.T) {
	fx := newFixture(t, bash("sleep 100"))
	fx.vm.setScript("sleep 100", jobScript{hang: true})
	slow := &slowStartVM{fakeVM: fx.vm, entered: make(chan struct{}), release: make(chan struct{})}
	fx.cfg.VM = slow
	fx.addTask("t1", "", 0)
	errc := runAsync(fx.runner(), fx.ctx)

	<-slow.entered
	fx.requestCancel("t1")
	time.Sleep(100 * time.Millisecond) // let the cancel watcher fire while StartJob is still blocked
	close(slow.release)
	waitErr(t, errc)

	if slow.sawDone.Load() {
		t.Fatal("the StartJob request was aborted by the cancel; its job id would have been lost")
	}
	if got := fx.vm.cancelled(); len(got) != 1 || got[0] != "job-1" {
		t.Fatalf("CancelJob calls = %v, want [job-1]", got)
	}
	if tk := fx.task("t1"); tk.State != domain.TaskCancelled {
		t.Fatalf("task state = %s, want cancelled", tk.State)
	}
}

// A task that is already being cancelled must not start new commands at all.
func TestNoCommandIsStartedForACancelledTask(t *testing.T) {
	fx := newFixture(t, bash("echo should-not-run"))
	fx.addTask("t1", "", 0)
	fx.requestCancel("t1")
	errc := runAsync(fx.runner(), fx.ctx)
	waitErr(t, errc)
	if n := fx.vm.startCount(); n != 0 {
		t.Fatalf("StartJob was called %d times for a task cancelled before it ran", n)
	}
}
