package runner

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
)

// parseErr is the reply Ollama's parse error becomes: no message, only the typed error.
func parseErr(raw, msg string) llm.FakeReply {
	return llm.FakeReply{Err: &llm.ToolParseError{Raw: raw, Msg: msg}}
}

// cutReply is a reply that the backend stopped at the per-reply token cap.
func cutReply(text string) llm.FakeReply {
	r := llm.Say(text)
	r.DoneReason = "length"
	return r
}

// endlessReply streams the same text forever, like a model stuck in a loop.
func endlessReply() llm.FakeReply {
	return llm.FakeReply{Endless: &llm.Chunk{Content: "abcdefghij"}}
}

// userMessages returns the content of the stored user messages, in order.
func userMessages(fx *fixture, id string) []string {
	msgs, _ := fx.storedMessages(id)
	var out []string
	for _, m := range msgs {
		if m.Role == "user" {
			out = append(out, m.Content)
		}
	}
	return out
}

func hasMessage(list []string, sub string) bool {
	for _, m := range list {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// addTaskWith queues a task for agent a1 and lets change adjust it before it is stored.
func (fx *fixture) addTaskWith(id string, change func(*domain.Task)) {
	fx.t.Helper()
	tk := &domain.Task{ID: id, AgentID: "a1", ProjectID: "p1", Prompt: "Build the thing.",
		MaxSteps: 0, State: domain.TaskQueued, CheckEachStep: true, MaxCallTokens: 32768}
	change(tk)
	must(fx.t, fx.st.CreateTask(fx.ctx, tk))
}

// ---- P1: malformed tool calls ----

func TestParseErrorIsRepairedAndTheCallRuns(t *testing.T) {
	fx := newFixture(t,
		parseErr(`{"command": "echo repaired"`, "invalid character"),
		say("Done.\nVERDICT: SUCCESS"),
	)
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))

	steps := fx.steps("t1")
	if len(steps) != 1 || steps[0].Command != "echo repaired" {
		t.Fatalf("steps = %+v, want the repaired command", steps)
	}
	tk := fx.task("t1")
	if len(tk.Calls) != 2 || tk.Calls[0].End != "parse" || tk.Calls[0].Note != "repaired a malformed tool call" {
		t.Fatalf("calls = %+v", tk.Calls)
	}
}

func TestUnparseableCallIsNudgedAndTheTaskContinues(t *testing.T) {
	fx := newFixture(t,
		parseErr("total garbage", "invalid character 't' looking for beginning of value"),
		say("Done.\nVERDICT: SUCCESS"),
	)
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))

	if !hasMessage(userMessages(fx, "t1"), "Your tool call could not be parsed: invalid character") {
		t.Fatalf("no parse nudge in %q", userMessages(fx, "t1"))
	}
	if tk := fx.task("t1"); tk.State != domain.TaskSucceededUnverified || tk.Calls[0].Note == "" {
		t.Fatalf("state=%s calls=%+v", tk.State, tk.Calls)
	}
}

func TestFourUnparseableCallsInARowEndInError(t *testing.T) {
	var replies []llm.FakeReply
	for i := 0; i < 4; i++ {
		replies = append(replies, parseErr(fmt.Sprintf("bad %d", i), "boom"))
	}
	fx := newFixture(t, replies...)
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))

	tk := fx.task("t1")
	if tk.State != domain.TaskError || !strings.Contains(tk.Result, "4 consecutive unusable tool calls") {
		t.Fatalf("state=%s result=%q", tk.State, tk.Result)
	}
	if !strings.Contains(tk.Result, "bad 3") {
		t.Fatalf("result %q does not show the last raw call", tk.Result)
	}
}

func TestGoodCallResetsTheUnparseableCounter(t *testing.T) {
	fx := newFixture(t,
		parseErr("bad 1", "boom"), parseErr("bad 2", "boom"), parseErr("bad 3", "boom"),
		bash("echo ok"),
		parseErr("bad 4", "boom"), parseErr("bad 5", "boom"), parseErr("bad 6", "boom"),
		say("Done.\nVERDICT: SUCCESS"),
	)
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))
	if tk := fx.task("t1"); tk.State != domain.TaskSucceededUnverified {
		t.Fatalf("state=%s result=%q", tk.State, tk.Result)
	}
}

func TestHarmonyCallLeakedIntoTheTextRuns(t *testing.T) {
	fx := newFixture(t,
		say(`commentary to=functions.bash json{"command":"echo harmony"}`),
		say("Done.\nVERDICT: SUCCESS"),
	)
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))
	steps := fx.steps("t1")
	if len(steps) != 1 || steps[0].Command != "echo harmony" {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestApplyPatchIsOfferedOnlyToGptOss(t *testing.T) {
	names := func(tools []llm.Tool) []string {
		var out []string
		for _, tl := range tools {
			out = append(out, tl.Name)
		}
		return out
	}
	for model, want := range map[string]string{"gpt-oss:20b": "bash,apply_patch", "llama3:8b": "bash"} {
		fx := newFixture(t, say("VERDICT: SUCCESS"))
		agent, err := fx.st.GetAgent(fx.ctx, "a1")
		must(t, err)
		agent.Model = model
		must(t, fx.st.UpdateAgent(fx.ctx, agent))
		fx.addTask("t1", "", 0)
		must(t, runOnce(fx))
		if got := strings.Join(names(fx.fake.Calls[0].Tools), ","); got != want {
			t.Errorf("model %s: tools = %s, want %s", model, got, want)
		}
	}
}

func TestApplyPatchChangesFilesInTheVM(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: hello.txt\n+hi\n*** End Patch"
	fx := newFixture(t,
		llm.CallTool("apply_patch", map[string]any{"patch": patch}),
		say("Done.\nVERDICT: SUCCESS"),
	)
	agent, err := fx.st.GetAgent(fx.ctx, "a1")
	must(t, err)
	agent.Model = "gpt-oss:20b"
	must(t, fx.st.UpdateAgent(fx.ctx, agent))
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))

	if c, ok := fx.vm.file("/home/agent/work/hello.txt"); !ok || c != "hi\n" {
		t.Fatalf("file in VM = %q, %v", c, ok)
	}
	steps := fx.steps("t1")
	if len(steps) != 1 || !strings.HasPrefix(steps[0].Command, "apply_patch") || steps[0].Output != "Done!\nA hello.txt" {
		t.Fatalf("steps = %+v", steps)
	}
	if steps[0].ExitCode == nil || *steps[0].ExitCode != 0 {
		t.Fatalf("exit code = %v", steps[0].ExitCode)
	}
}

func TestFailingPatchIsReportedAsAnExitStatusNotAnError(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: missing.txt\n@@\n-a\n+b\n*** End Patch"
	fx := newFixture(t,
		llm.CallTool("apply_patch", map[string]any{"patch": patch}),
		say("Done.\nVERDICT: FAILED - could not edit"),
	)
	agent, err := fx.st.GetAgent(fx.ctx, "a1")
	must(t, err)
	agent.Model = "gpt-oss:20b"
	must(t, fx.st.UpdateAgent(fx.ctx, agent))
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))

	steps := fx.steps("t1")
	if len(steps) != 1 || steps[0].ExitCode == nil || *steps[0].ExitCode != 1 ||
		!strings.HasPrefix(steps[0].Output, "error: cannot update missing.txt") {
		t.Fatalf("steps = %+v", steps)
	}
	if tk := fx.task("t1"); tk.State != domain.TaskFailed {
		t.Fatalf("state = %s", tk.State)
	}
}

// ---- P2: loop guards ----

func TestTokenCapIsSentWithEveryCall(t *testing.T) {
	fx := newFixture(t, bash("echo a"), say("Done.\nVERDICT: SUCCESS"))
	fx.addTaskWith("t1", func(tk *domain.Task) { tk.MaxCallTokens = 123 })
	must(t, runOnce(fx))
	for i, c := range fx.fake.Calls {
		if c.Opts.MaxTokens != 123 {
			t.Errorf("call %d: MaxTokens = %d, want 123", i, c.Opts.MaxTokens)
		}
	}
}

func TestLengthCutReplyIsDiscardedAndTheModelIsNudged(t *testing.T) {
	fx := newFixture(t, cutReply("a very long reply that never ends"), say("Done.\nVERDICT: SUCCESS"))
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))

	if !hasMessage(userMessages(fx, "t1"), "Your last reply was cut off (it hit the per-reply token limit)") {
		t.Fatalf("no cut nudge in %q", userMessages(fx, "t1"))
	}
	msgs, _ := fx.storedMessages("t1")
	for _, m := range msgs {
		if m.Content == "a very long reply that never ends" {
			t.Fatal("the cut reply was stored")
		}
	}
	tk := fx.task("t1")
	if tk.State != domain.TaskSucceededUnverified || tk.Calls[0].End != "length" {
		t.Fatalf("state=%s calls=%+v", tk.State, tk.Calls)
	}
}

func TestThreeCutRepliesGiveUpWithTokenLimit(t *testing.T) {
	fx := newFixture(t, cutReply("x"), cutReply("x"), cutReply("x"))
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))
	tk := fx.task("t1")
	if tk.State != domain.TaskGaveUp || tk.Reason != "token_limit" {
		t.Fatalf("state=%s reason=%q result=%q", tk.State, tk.Reason, tk.Result)
	}
}

func TestRepeatingReplyIsAbortedAndCountedAsPartial(t *testing.T) {
	fx := newFixture(t, endlessReply(), say("Done.\nVERDICT: SUCCESS"))
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))

	if !hasMessage(userMessages(fx, "t1"), "it was repeating itself") {
		t.Fatalf("no repetition nudge in %q", userMessages(fx, "t1"))
	}
	tk := fx.task("t1")
	if tk.State != domain.TaskSucceededUnverified {
		t.Fatalf("state = %s", tk.State)
	}
	// P5: the aborted call is counted: its streamed pieces are generated tokens, and it is marked partial.
	if tk.PartialCalls != 1 || tk.Calls[0].End != "repetition" || !tk.Calls[0].Partial {
		t.Fatalf("partial=%d calls=%+v", tk.PartialCalls, tk.Calls)
	}
	if tk.Calls[0].GenTokens < 25 || tk.CompletionTokens != tk.Calls[0].GenTokens+tk.Calls[1].GenTokens {
		t.Fatalf("gen tokens = %d, completion = %d", tk.Calls[0].GenTokens, tk.CompletionTokens)
	}
}

func TestThreeRepeatingRepliesGiveUpWithRepetition(t *testing.T) {
	fx := newFixture(t, endlessReply(), endlessReply(), endlessReply())
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))
	if tk := fx.task("t1"); tk.State != domain.TaskGaveUp || tk.Reason != "repetition" {
		t.Fatalf("state=%s reason=%q", tk.State, tk.Reason)
	}
}

func TestRepeatingCheckNeedsEightRepeatsOfAPhraseWithLetters(t *testing.T) {
	const phrase = "abcdefghijklmnopqrstuvwx" // 24 characters, 24 letters
	filler := func(n int) string {
		var b strings.Builder
		for i := 0; b.Len() < n; i++ {
			fmt.Fprintf(&b, "%d;", i*7919) // no phrase of 24 characters repeats in this text
		}
		return b.String()[:n]
	}
	if repeating(filler(500)) {
		t.Fatal("distinct text reported as repeating")
	}
	if repeating(filler(332) + strings.Repeat(phrase, 7)) {
		t.Fatal("seven repeats reported as repeating")
	}
	if !repeating(filler(332) + strings.Repeat(phrase, 8)) {
		t.Fatal("eight repeats not reported")
	}
	if repeating(filler(300) + strings.Repeat("!", 200)) {
		t.Fatal("punctuation alone (fewer than 8 letters or digits) reported as repeating")
	}
	if repeating("abc") {
		t.Fatal("a short tail reported as repeating")
	}
}

func TestMaxMinutesCancelsTheTaskAndItsCommand(t *testing.T) {
	fx := newFixture(t, bash("sleep 100"))
	fx.vm.setScript("sleep 100", jobScript{hang: true})
	fx.addTaskWith("t1", func(tk *domain.Task) { tk.MaxMinutes = 1 })
	errc := runAsync(fx.runner(), fx.ctx)

	waitFor(t, "the command to start", func() bool { return fx.vm.startCount() == 1 })
	fx.clock.Advance(2 * time.Minute)
	waitErr(t, errc)

	if got := fx.vm.cancelled(); len(got) != 1 {
		t.Fatalf("CancelJob calls = %v, want one", got)
	}
	if tk := fx.task("t1"); tk.State != domain.TaskCancelled || tk.Reason != "timeout" || tk.Result != "[cancelled: timeout]" {
		t.Fatalf("state=%s reason=%q result=%q", tk.State, tk.Reason, tk.Result)
	}
}

func TestMaxMinutesCancelsWhileTheModelIsBlocked(t *testing.T) {
	fx := newFixture(t, llm.FakeReply{Block: make(chan struct{})})
	fx.addTaskWith("t1", func(tk *domain.Task) { tk.MaxMinutes = 1 })
	errc := runAsync(fx.runner(), fx.ctx)
	fx.waitEntered()
	fx.clock.Advance(90 * time.Second)
	waitErr(t, errc)
	if tk := fx.task("t1"); tk.State != domain.TaskCancelled || tk.Reason != "timeout" {
		t.Fatalf("state=%s reason=%q", tk.State, tk.Reason)
	}
}

func TestCheckEachStepEndsTheTaskAfterTheBatch(t *testing.T) {
	fx := newFixture(t, bash("touch x"))
	fx.addTaskWith("t1", func(tk *domain.Task) { tk.CheckCommand = "check-x" })
	must(t, runOnce(fx))

	tk := fx.task("t1")
	if tk.State != domain.TaskSucceeded || tk.VerdictSource != domain.VerdictFromCheck || tk.Reason != "check passed" {
		t.Fatalf("state=%s source=%q reason=%q", tk.State, tk.VerdictSource, tk.Reason)
	}
	if len(fx.fake.Calls) != 1 {
		t.Fatalf("model called %d times; the check should end the task after the first batch", len(fx.fake.Calls))
	}
	if !strings.Contains(tk.Result, "check_command passed after step 1") {
		t.Fatalf("result = %q", tk.Result)
	}
}

func TestCheckEachStepOffWaitsForTheReport(t *testing.T) {
	fx := newFixture(t, bash("touch x"), say("Done.\nVERDICT: SUCCESS"))
	fx.addTaskWith("t1", func(tk *domain.Task) { tk.CheckCommand = "check-x"; tk.CheckEachStep = false })
	must(t, runOnce(fx))

	tk := fx.task("t1")
	if len(fx.fake.Calls) != 2 || tk.State != domain.TaskSucceeded || tk.VerdictSource != domain.VerdictFromCheck {
		t.Fatalf("calls=%d state=%s source=%q", len(fx.fake.Calls), tk.State, tk.VerdictSource)
	}
}

func TestFailingCheckAfterEachBatchKeepsGoing(t *testing.T) {
	fx := newFixture(t, bash("touch x"), say("Still working.\nVERDICT: FAILED - not yet"))
	fx.vm.setScript("check-x", jobScript{exit: 1})
	fx.addTask("t1", "check-x", 0)
	must(t, runOnce(fx))

	tk := fx.task("t1")
	if len(fx.fake.Calls) != 2 || tk.State != domain.TaskFailed || tk.VerdictSource != domain.VerdictFromCheck {
		t.Fatalf("calls=%d state=%s source=%q result=%q", len(fx.fake.Calls), tk.State, tk.VerdictSource, tk.Result)
	}
}

// ---- P6: verdict honesty ----

func TestClaimedSuccessWithoutCheckIsUnverified(t *testing.T) {
	fx := newFixture(t, say("VERDICT: SUCCESS"))
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))
	tk := fx.task("t1")
	if tk.State != domain.TaskSucceededUnverified || tk.VerdictSource != domain.VerdictFromModel || tk.Verdict != "SUCCESS" {
		t.Fatalf("state=%s source=%q verdict=%q", tk.State, tk.VerdictSource, tk.Verdict)
	}
	if !strings.Contains(tk.Result, "the model's own claim") {
		t.Fatalf("result = %q", tk.Result)
	}
}

func TestCheckDecidesASuccessClaim(t *testing.T) {
	fx := newFixture(t, say("VERDICT: SUCCESS"))
	fx.vm.setScript("check-x", jobScript{exit: 3})
	fx.addTask("t1", "check-x", 0)
	must(t, runOnce(fx))
	tk := fx.task("t1")
	if tk.State != domain.TaskFailed || tk.VerdictSource != domain.VerdictFromCheck ||
		!strings.Contains(tk.Result, "reported SUCCESS but the check FAILED") {
		t.Fatalf("state=%s source=%q result=%q", tk.State, tk.VerdictSource, tk.Result)
	}
}

func TestFailedVerdictWithoutCheckIsModelDecided(t *testing.T) {
	fx := newFixture(t, say("VERDICT: FAILED - no access"))
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))
	if tk := fx.task("t1"); tk.State != domain.TaskFailed || tk.VerdictSource != domain.VerdictFromModel {
		t.Fatalf("state=%s source=%q", tk.State, tk.VerdictSource)
	}
}

// ---- P3 and P9: progress counters and live text ----

func TestProgressCountersAreKept(t *testing.T) {
	fx := newFixture(t,
		llm.FakeReply{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{Name: "bash",
			Arguments: map[string]any{"command": "echo hi"}}}}, PromptTokens: 100, GenTokens: 7},
		llm.FakeReply{Message: llm.Message{Role: "assistant", Content: "Done.\nVERDICT: SUCCESS"}, PromptTokens: 50, GenTokens: 3},
	)
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))
	tk := fx.task("t1")
	if tk.ModelCalls != 2 || tk.PromptTokens != 150 || tk.CompletionTokens != 10 || tk.PartialCalls != 0 {
		t.Fatalf("counters: calls=%d prompt=%d completion=%d partial=%d", tk.ModelCalls, tk.PromptTokens, tk.CompletionTokens, tk.PartialCalls)
	}
	if len(tk.Calls) != 2 || tk.Calls[0].Index != 1 || tk.Calls[1].GenTokens != 3 || tk.Calls[1].PromptTokens != 50 {
		t.Fatalf("calls = %+v", tk.Calls)
	}
	if tk.FirstCommandSeconds != 0 { // the fake clock does not move during the run
		t.Fatalf("first command seconds = %v", tk.FirstCommandSeconds)
	}
}

func TestCallRecordsKeepOnlyTheLatest(t *testing.T) {
	var replies []llm.FakeReply
	for i := 0; i < domain.MaxCallRecords+2; i++ {
		replies = append(replies, bash(fmt.Sprintf("echo %d", i)))
	}
	replies = append(replies, say("VERDICT: SUCCESS"))
	fx := newFixture(t, replies...)
	fx.addTaskWith("t1", func(tk *domain.Task) { tk.MaxSteps = 100 })
	must(t, runOnce(fx))

	tk := fx.task("t1")
	last := domain.MaxCallRecords + 3 // model calls: one per command plus the final reply
	if tk.ModelCalls != last || len(tk.Calls) != domain.MaxCallRecords ||
		tk.Calls[0].Index != last-domain.MaxCallRecords+1 || tk.Calls[len(tk.Calls)-1].Index != last {
		t.Fatalf("calls=%d kept=%d first=%d last=%d", tk.ModelCalls, len(tk.Calls), tk.Calls[0].Index, tk.Calls[len(tk.Calls)-1].Index)
	}
}

// pacedLLM streams its first reply chunk by chunk, moving the fake clock forward between chunks so that the
// one-per-second throttle of the live text lets every chunk through. Then it blocks until block is closed.
type pacedLLM struct {
	clock   *fakeClock
	entered chan struct{}
	chunks  []llm.Chunk
	block   <-chan struct{}
	after   *llm.Fake // answers the calls after the first
	calls   int
}

func (p *pacedLLM) Chat(ctx context.Context, model string, msgs []llm.Message, tools []llm.Tool, opts llm.ChatOptions, onChunk func(llm.Chunk)) (llm.Message, error) {
	p.calls++
	if p.calls > 1 {
		return p.after.Chat(ctx, model, msgs, tools, opts, onChunk)
	}
	p.entered <- struct{}{}
	for _, c := range p.chunks {
		p.clock.Advance(2 * time.Second)
		onChunk(c)
	}
	select {
	case <-p.block:
	case <-ctx.Done():
		return llm.Message{}, ctx.Err()
	}
	return llm.Message{}, nil
}

func (p *pacedLLM) ListModels(ctx context.Context) ([]llm.ModelInfo, error) { return nil, nil }

func TestLiveTextIsShownWhileStreamingAndNeverInTheResult(t *testing.T) {
	block := make(chan struct{})
	fx := newFixture(t)
	fx.cfg.LLM = &pacedLLM{clock: fx.clock, entered: fx.spy.entered, block: block,
		chunks: []llm.Chunk{{Thinking: "reasoning here"}, {Content: "answer so far"}},
		after:  &llm.Fake{Script: []llm.FakeReply{say("Done.\nVERDICT: SUCCESS")}}}
	fx.addTask("t1", "", 0)
	errc := runAsync(fx.runner(), fx.ctx)
	fx.waitEntered()
	waitFor(t, "the live answer", func() bool { return fx.task("t1").LiveAnswer == "answer so far" })
	if tk := fx.task("t1"); tk.LiveReasoning != "reasoning here" {
		t.Fatalf("live reasoning = %q", tk.LiveReasoning)
	}
	close(block)
	waitErr(t, errc)

	tk := fx.task("t1")
	if tk.State != domain.TaskSucceededUnverified {
		t.Fatalf("state=%s result=%q", tk.State, tk.Result)
	}
	if tk.LiveAnswer != "" || tk.LiveReasoning != "" {
		t.Fatalf("live text not cleared: %q / %q", tk.LiveAnswer, tk.LiveReasoning)
	}
	if strings.Contains(tk.Result, "reasoning here") || strings.Contains(tk.Result, "answer so far") {
		t.Fatalf("result %q contains live text", tk.Result)
	}
}

// ---- P10: a task never ends with an empty result ----

func TestFinishNeverStoresAnEmptyResult(t *testing.T) {
	fx := newFixture(t)
	fx.addTask("t1", "", 0)
	claimed, err := fx.st.ClaimTasks(fx.ctx, "w1", fx.clock.Now(), time.Minute, 1)
	must(t, err)
	r := fx.runner()

	if _, err := r.finish(fx.ctx, claimed[0], domain.TaskSucceeded, "", "   "); err != nil {
		t.Fatal(err)
	}
	tk := fx.task("t1")
	if tk.State != domain.TaskError || !strings.Contains(tk.Result, "left no result") {
		t.Fatalf("success with no result: state=%s result=%q", tk.State, tk.Result)
	}
}

func TestFinishWithoutResultExplainsTheState(t *testing.T) {
	for _, state := range []domain.TaskState{domain.TaskCancelled, domain.TaskFailed, domain.TaskGaveUp, domain.TaskError} {
		fx := newFixture(t)
		fx.addTask("t1", "", 0)
		claimed, err := fx.st.ClaimTasks(fx.ctx, "w1", fx.clock.Now(), time.Minute, 1)
		must(t, err)
		if _, err := fx.runner().finish(fx.ctx, claimed[0], state, "", ""); err != nil {
			t.Fatal(err)
		}
		tk := fx.task("t1")
		if want := fmt.Sprintf("[no result recorded; state was %s]", state); tk.State != state || tk.Result != want {
			t.Errorf("state %s: got state=%s result=%q", state, tk.State, tk.Result)
		}
	}
}

// panicLLM panics on every call, as a bug in the worker would.
type panicLLM struct{}

func (panicLLM) Chat(context.Context, string, []llm.Message, []llm.Tool, llm.ChatOptions, func(llm.Chunk)) (llm.Message, error) {
	panic("boom")
}

func (panicLLM) ListModels(context.Context) ([]llm.ModelInfo, error) { return nil, nil }

func TestPanicInTheRunEndsTheTaskInError(t *testing.T) {
	fx := newFixture(t)
	fx.cfg.LLM = panicLLM{}
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))
	tk := fx.task("t1")
	if tk.State != domain.TaskError || tk.Result != "[internal error: boom]" || tk.LeaseOwner != "" {
		t.Fatalf("state=%s result=%q lease=%q", tk.State, tk.Result, tk.LeaseOwner)
	}
}

func TestApplyPatchCreatesMissingDirectories(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: new/dir/f.txt\n+x\n*** End Patch"
	fx := newFixture(t,
		llm.CallTool("apply_patch", map[string]any{"patch": patch}),
		say("Done.\nVERDICT: SUCCESS"),
	)
	fx.vm.putFails = map[string]int{"/home/agent/work/new/dir/f.txt": 1}
	agent, err := fx.st.GetAgent(fx.ctx, "a1")
	must(t, err)
	agent.Model = "gpt-oss:20b"
	must(t, fx.st.UpdateAgent(fx.ctx, agent))
	fx.addTask("t1", "", 0)
	must(t, runOnce(fx))

	if _, ok := fx.vm.file("/home/agent/work/new/dir/f.txt"); !ok {
		t.Fatal("file not written after the directory was created")
	}
	var mkdir bool
	for _, st := range fx.vm.startRequests() {
		if strings.HasPrefix(st.Command, "mkdir -p -- ") {
			mkdir = true
		}
	}
	if !mkdir {
		t.Fatal("no mkdir was run for the missing directory")
	}
}
