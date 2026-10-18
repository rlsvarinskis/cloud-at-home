package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/runner/applypatch"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/runner/toolcall"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/store"
)

// systemText, nudgeText and the other model-facing texts are ported from the Python gateway.
const (
	systemText = "You are an autonomous agent with full control of a Linux virtual machine through the bash tool. " +
		"Your home is /home/agent. Work step by step and verify your results by running commands. " +
		"When you are finished, reply with a concise final report (no tool call) that ends with exactly one " +
		"line: 'VERDICT: SUCCESS' if you verified the task's success criteria are met, otherwise " +
		"'VERDICT: FAILED - <reason>'. Never claim success you have not verified."
	nudgeText = "Give your final report now: what you did and whether the task's success criteria are met. " +
		"End with exactly one line: 'VERDICT: SUCCESS' or 'VERDICT: FAILED - <reason>'."
	cutNudgeFmt = "Your last reply was cut off (%s) and was discarded. Do not repeat yourself and keep your reasoning " +
		"short. If the task is done, reply now with a short final report ending in 'VERDICT: SUCCESS' or " +
		"'VERDICT: FAILED - <reason>'. Otherwise make your next tool call."
	parseNudgeFmt = "Your tool call could not be parsed: %s. Re-emit it as valid JSON, with every newline and quote " +
		"inside string values escaped."
	patchDescription = "Edit files with a patch. Format:\n" +
		"*** Begin Patch\n" +
		"*** Add File: path/to/new.txt     (every following line starts with '+')\n" +
		"*** Update File: path/to/file.py  (optional '*** Move to: new/path' line, then hunks)\n" +
		"*** Delete File: path/to/old.txt\n" +
		"*** End Patch\n" +
		"Hunks start with '@@' (optionally '@@ <code line to search for>'). Hunk lines start with ' ' (context), '-' (remove) or '+' (add).\n" +
		"Give 3 lines of context around each change. Paths are relative to the working directory.\n" +
		"The whole patch is atomic: if any part fails, no file is changed."
)

// Loop guards. See docs/adr/0004-agent-loop-guards.md.
const (
	maxParseFails = 3   // consecutive unusable tool calls tolerated before the task ends in state error
	maxCuts       = 2   // replies cut off (repetition or token limit) tolerated before the task gives up
	repeatNGram   = 24  // a phrase of this many characters ...
	repeatMinAlnu = 8   // ... with at least this many letters or digits ...
	repeatTimes   = 8   // ... that occurs this many times ...
	repeatWindow  = 500 // ... in the last this many characters of the streamed text
	repeatEvery   = 16  // the check runs every this many streamed pieces
	liveAnswerMax = 600 // characters of the partial answer shown while a call streams
	liveReasonMax = 300 // characters of the raw reasoning shown while a call streams
	liveEvery     = time.Second
	stepLimitText = "stopped after %d steps without a final answer"
)

// Reasons recorded on a task that ends early or stops on a guard.
const (
	reasonRepetition = "repetition"
	reasonTokenLimit = "token_limit"
	reasonStepLimit  = "step_limit"
	reasonTimeout    = "timeout"
	reasonCheckPass  = "check passed"
)

// Causes of the task context: why it was cancelled. errRepetition aborts one model call, the others end the task.
var (
	errRepetition  = errors.New("the model's output repeats itself")
	errTaskTimeout = errors.New("the task's time limit was reached")
)

// verdictRE finds the verdict line of a reply (ported from the Python gateway's VERDICT_RE).
var verdictRE = regexp.MustCompile(`(?im)^\W*VERDICT:\s*(SUCCESS|FAILED)\b[\s:-]*(.*)$`)

// bashTool is always offered to the model.
var bashTool = llm.Tool{
	Name:        "bash",
	Description: "Run a bash command in your Linux machine (Debian, sudo, internet). Returns output.",
	Parameters: map[string]any{
		"type":       "object",
		"properties": map[string]any{"command": map[string]any{"type": "string"}},
		"required":   []any{"command"},
	},
}

// applyPatchTool is offered only to gpt-oss models, which are trained on this format.
var applyPatchTool = llm.Tool{
	Name:        "apply_patch",
	Description: patchDescription,
	Parameters: map[string]any{
		"type":       "object",
		"properties": map[string]any{"patch": map[string]any{"type": "string"}},
		"required":   []any{"patch"},
	},
}

// toolsFor returns the tools a model is offered.
func toolsFor(model string) []llm.Tool {
	if strings.HasPrefix(model, "gpt-oss") {
		return []llm.Tool{bashTool, applyPatchTool}
	}
	return []llm.Tool{bashTool}
}

// outcome says how a worker left a task.
type outcome int

const (
	outFinished  outcome = iota // the task reached a final state, which this worker wrote
	outAbandoned                // stopped without writing: shutdown or a lost lease; the lease expires on its own
	outRequeued                 // released to queued after a transient failure
)

// Why a task context was cancelled. Only errCancelledByRequest (the user cancelled the task) and errTaskTimeout kill
// the command that is running in the VM; on shutdown or a lost lease the command is left alone, because the worker
// that takes the task over re-attaches to the same job (by idempotency key) and sees its real result.
var (
	errCancelledByRequest = errors.New("task cancelled by request")
	errLeaseTakenOver     = errors.New("task lease lost")
)

// taskRun is the state of one task run shared by the worker and its heartbeat and cancel-watch goroutines.
type taskRun struct {
	r         *Runner
	ctx       context.Context         // the task's context; its cause says why the run stopped
	cancel    context.CancelCauseFunc // cancels the task's context, recording why
	snap      domain.Task             // copy of the task as claimed; read-only, used by goroutine hooks
	deadline  time.Time               // when max_minutes runs out; zero for none
	lost      atomic.Bool             // the lease was taken by another owner
	cancelReq atomic.Bool             // a cancel was requested in the store

	// State of the loop, kept by the worker goroutine only. It is per run, not stored: a task taken over by
	// another worker starts these counters again.
	cuts       int
	parseFails int
	ranBatch   bool      // a command ran since the model's last reply
	lastLive   time.Time // when the live text was last written
}

// runTask runs one claimed task. It starts the heartbeat and the cancel watch, and stops both before returning.
// A panic in the run is recorded as an error on the task rather than taking the worker down.
func (r *Runner) runTask(parent context.Context, t *domain.Task) (oc outcome, err error) {
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("task run panicked", "task", t.ID, "panic", p)
			fctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cancelGrace)
			defer cancel()
			oc, err = r.finish(fctx, t, domain.TaskError, "", fmt.Sprintf("[internal error: %v]", p))
		}
	}()
	ctx, cancel := context.WithCancelCause(parent)
	run := &taskRun{r: r, ctx: ctx, cancel: cancel, snap: *t}
	if t.MaxMinutes > 0 {
		start := t.StartedAt
		if start.IsZero() {
			start = r.now()
		}
		run.deadline = start.Add(time.Duration(t.MaxMinutes) * time.Minute)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		run.heartbeat(ctx)
	}()
	go func() {
		defer wg.Done()
		run.watchCancel(ctx)
	}()
	defer func() {
		cancel(nil)
		wg.Wait()
	}()
	return run.execute(ctx, t)
}

// heartbeat extends the lease every LeaseTTL/3. If the lease is gone, it marks the run lost and cancels it.
func (run *taskRun) heartbeat(ctx context.Context) {
	r := run.r
	tick := time.NewTicker(r.cfg.LeaseTTL / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		err := r.cfg.Store.ExtendLease(ctx, run.snap.ID, r.cfg.Owner, r.now(), r.cfg.LeaseTTL)
		switch {
		case err == nil:
		case errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrNotFound):
			run.lost.Store(true)
			r.log.Warn("lease lost", "task", run.snap.ID, "err", err)
			if r.cfg.Hooks.OnLeaseLost != nil {
				c := run.snap
				r.cfg.Hooks.OnLeaseLost(&c)
			}
			run.cancel(errLeaseTakenOver)
			return
		default:
			r.log.Warn("extend lease", "task", run.snap.ID, "err", err)
		}
	}
}

// watchCancel polls the task row and, when a cancel is requested, cancels the run. It also ends the run when
// max_minutes has run out.
func (run *taskRun) watchCancel(ctx context.Context) {
	r := run.r
	tick := time.NewTicker(r.cfg.CancelPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !run.deadline.IsZero() && !r.now().Before(run.deadline) {
			run.cancel(errTaskTimeout)
			return
		}
		cur, err := r.cfg.Store.GetTask(ctx, run.snap.ID)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("poll task", "task", run.snap.ID, "err", err)
			}
			continue
		}
		if cur.CancelRequested {
			run.cancelReq.Store(true)
			run.cancel(errCancelledByRequest)
			return
		}
	}
}

// execute runs the task from its stored state: checks the agent and VM, then drives the conversation.
func (run *taskRun) execute(ctx context.Context, t *domain.Task) (outcome, error) {
	r := run.r
	agent, err := r.cfg.Store.GetAgent(ctx, t.AgentID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) && ctx.Err() == nil {
			return r.finish(ctx, t, domain.TaskError, "", "agent was deleted")
		}
		return run.storeErr(ctx, t, err)
	}
	if agent.State != domain.AgentActive {
		return r.finish(ctx, t, domain.TaskError, "", "agent is "+string(agent.State))
	}
	if t.Attempts > r.cfg.MaxAttempts {
		return r.finish(ctx, t, domain.TaskError, "", fmt.Sprintf("gave up after %d takeovers", r.cfg.MaxAttempts))
	}

	vm, err := r.cfg.VM.GetVM(ctx, agent.VMID)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return run.interrupted(t)
		case errors.Is(err, errs.ErrNotFound):
			agent.State = domain.AgentOrphaned
			if uerr := r.cfg.Store.UpdateAgent(ctx, agent); uerr != nil {
				r.log.Warn("mark agent orphaned", "agent", agent.ID, "err", uerr)
			}
			return r.finish(ctx, t, domain.TaskError, "", "the VM no longer exists")
		case errors.Is(err, errs.ErrUnavailable):
			return r.requeue(ctx, t, err)
		default:
			return outAbandoned, fmt.Errorf("get vm: %w", err)
		}
	}
	if vm.State != vmdv1.VMState_VM_STATE_RUNNING {
		return r.finish(ctx, t, domain.TaskError, "", "vm is "+vmStateName(vm.State))
	}

	stored, err := r.cfg.Store.ListMessages(ctx, t.ID)
	if err != nil {
		return run.storeErr(ctx, t, err)
	}
	msgs := make([]llm.Message, 0, len(stored))
	for _, sm := range stored {
		var m llm.Message
		if err := json.Unmarshal(sm.JSON, &m); err != nil {
			return r.finish(ctx, t, domain.TaskError, "", fmt.Sprintf("stored message %d is corrupt: %v", sm.Seq, err))
		}
		msgs = append(msgs, m)
	}
	if len(msgs) == 0 {
		seed := []llm.Message{
			{Role: "system", Content: systemPrompt(agent)},
			{Role: "user", Content: t.Prompt},
		}
		for _, m := range seed {
			if err := run.record(ctx, t, &msgs, m); err != nil {
				return run.storeErr(ctx, t, err)
			}
		}
	}

	for {
		oc, done, err := run.step(ctx, t, agent, &msgs)
		if done {
			return oc, err
		}
	}
}

// step performs one turn of the loop. It finishes any tool call whose result was not stored (resume), then
// either handles a final text reply or asks the model for the next reply. done is true when the run is over.
func (run *taskRun) step(ctx context.Context, t *domain.Task, agent *domain.Agent, msgs *[]llm.Message) (outcome, bool, error) {
	if ctx.Err() != nil {
		return doneWith(run.interrupted(t))
	}
	m := *msgs

	if ai := lastAssistant(m); ai >= 0 && len(m[ai].ToolCalls) > 0 {
		if n := toolResults(m, ai); n < len(m[ai].ToolCalls) {
			last := n == len(m[ai].ToolCalls)-1
			return run.runCall(ctx, t, agent, msgs, m[ai].ToolCalls[n], last)
		}
	}
	if last := m[len(m)-1]; last.Role == "assistant" {
		return run.handleText(ctx, t, agent, msgs, last.Content)
	}
	if t.Steps >= maxSteps(t) {
		text := fmt.Sprintf("[%s]", fmt.Sprintf(stepLimitText, maxSteps(t)))
		return doneWith(run.conclude(ctx, t, agent, text, lastToolOutput(m), reasonStepLimit))
	}
	return run.modelTurn(ctx, t, agent, msgs)
}

// modelTurn asks the model for its next reply and handles every way the reply can go wrong: an abort by a guard,
// an unparseable tool call, a reply cut off by the token cap, or a backend error. A usable reply is stored.
func (run *taskRun) modelTurn(ctx context.Context, t *domain.Task, agent *domain.Agent, msgs *[]llm.Message) (outcome, bool, error) {
	r := run.r
	callCtx, cancelCall := context.WithCancelCause(ctx)
	defer cancelCall(nil)
	st := &streamState{}
	start := r.now()
	onChunk := func(c llm.Chunk) {
		if c.Done {
			st.done, st.doneReason = true, c.DoneReason
			st.promptTokens, st.genTokens = c.PromptTokens, c.GenTokens
			return
		}
		piece := c.Content + c.Thinking
		if piece == "" {
			return
		}
		now := r.now()
		if st.first.IsZero() {
			st.first = now
			if t.FirstTokenSeconds == 0 && !t.StartedAt.IsZero() {
				t.FirstTokenSeconds = seconds(now.Sub(t.StartedAt))
			}
		}
		st.streamed++
		st.tail = lastRunes(st.tail+piece, repeatWindow)
		st.answer = lastRunes(st.answer+c.Content, liveAnswerMax)
		st.reasoning = lastRunes(st.reasoning+c.Thinking, liveReasonMax)
		if st.streamed%repeatEvery == 0 && repeating(st.tail) {
			st.repetition = true
			cancelCall(errRepetition)
			return
		}
		run.setLive(ctx, t, st.answer, st.reasoning)
	}

	reply, err := r.cfg.LLM.Chat(callCtx, agent.Model, *msgs, toolsFor(agent.Model),
		llm.ChatOptions{MaxTokens: t.MaxCallTokens}, onChunk)
	rec := domain.CallRecord{Index: t.ModelCalls + 1, Seconds: seconds(r.now().Sub(start)), End: "ok"}

	// Decide what the reply is, before the call is recorded.
	var pe *llm.ToolParseError
	switch {
	case st.repetition:
		rec.End = reasonRepetition
	case err != nil && ctx.Err() != nil:
		rec.End = "cancel"
		if run.deadlineHit() {
			rec.End = "timeout"
		}
	case err != nil && errors.As(err, &pe):
		rec.End = "parse"
		args, ok := repairCall(pe.Raw)
		if ok {
			rec.Note = "repaired a malformed tool call"
			reply = llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{Name: inferTool(args), Arguments: args}}}
			err = nil
		} else {
			rec.Note = "unparseable tool call: " + truncateRunes(pe.Msg, 120)
		}
	case err != nil:
		rec.End = "error"
	case st.doneReason == "length":
		rec.End = "length"
	}
	run.recordCall(ctx, t, st, &rec)

	switch {
	case st.repetition:
		return run.cut(ctx, t, agent, msgs, reasonRepetition)
	case err != nil && ctx.Err() != nil:
		return doneWith(run.interrupted(t))
	case err != nil && rec.End == "parse":
		return run.unparseable(ctx, t, msgs, pe)
	case err != nil && errors.Is(err, errs.ErrUnavailable):
		return doneWith(r.requeue(ctx, t, err))
	case err != nil:
		return doneWith(r.finish(ctx, t, domain.TaskError, "", "model error: "+err.Error()))
	case st.doneReason == "length":
		return run.cut(ctx, t, agent, msgs, reasonTokenLimit)
	}

	reply.Role = "assistant"
	if len(reply.ToolCalls) == 0 {
		for _, hc := range toolcall.ParseHarmony(reply.Content) {
			reply.ToolCalls = append(reply.ToolCalls, llm.ToolCall{Name: hc.Name, Arguments: harmonyArgs(hc.Args)})
		}
	}
	run.ranBatch = false
	if err := run.record(ctx, t, msgs, reply); err != nil {
		return doneWith(run.storeErr(ctx, t, err))
	}
	return 0, false, nil
}

// streamState is what one model call has produced so far.
type streamState struct {
	tail, answer, reasoning string
	streamed                int // pieces of text streamed
	first                   time.Time
	done                    bool // the final chunk arrived: the call completed
	doneReason              string
	promptTokens, genTokens int64
	repetition              bool // the call was aborted because its output repeats itself
}

// recordCall stores the record of a finished model call on the task: its counters, the call list, and the cleared
// live text. An aborted call counts its streamed pieces as generated tokens and is marked partial.
func (run *taskRun) recordCall(ctx context.Context, t *domain.Task, st *streamState, rec *domain.CallRecord) {
	t.ModelCalls++
	switch {
	case st.done:
		rec.PromptTokens, rec.GenTokens = st.promptTokens, st.genTokens
		t.PromptTokens += st.promptTokens
		t.CompletionTokens += st.genTokens
	case st.streamed > 0:
		rec.Partial = true
		rec.GenTokens = int64(st.streamed)
		t.CompletionTokens += int64(st.streamed)
		t.PartialCalls++
	}
	t.Calls = append(t.Calls, *rec)
	if over := len(t.Calls) - domain.MaxCallRecords; over > 0 {
		t.Calls = append([]domain.CallRecord(nil), t.Calls[over:]...)
	}
	t.LiveAnswer, t.LiveReasoning = "", ""
	run.persistProgress(ctx, t)
}

// cut discards a reply that was aborted or cut off and asks the model to wrap up or make a shorter call. After
// maxCuts such replies the task gives up.
func (run *taskRun) cut(ctx context.Context, t *domain.Task, agent *domain.Agent, msgs *[]llm.Message, why string) (outcome, bool, error) {
	run.cuts++
	if run.cuts > maxCuts {
		text := fmt.Sprintf("[stopped: the model's replies were cut off %d times (%s)]", run.cuts, why)
		return doneWith(run.conclude(ctx, t, agent, text, lastToolOutput(*msgs), why))
	}
	desc := "it hit the per-reply token limit"
	if why == reasonRepetition {
		desc = "it was repeating itself"
	}
	if err := run.record(ctx, t, msgs, llm.Message{Role: "user", Content: fmt.Sprintf(cutNudgeFmt, desc)}); err != nil {
		return doneWith(run.storeErr(ctx, t, err))
	}
	return 0, false, nil
}

// unparseable handles a tool call that neither Ollama nor the repair could read: the model is told why and asked to
// re-emit it. After maxParseFails such calls in a row the task ends in state error.
func (run *taskRun) unparseable(ctx context.Context, t *domain.Task, msgs *[]llm.Message, pe *llm.ToolParseError) (outcome, bool, error) {
	run.parseFails++
	if run.parseFails > maxParseFails {
		return doneWith(run.r.finish(ctx, t, domain.TaskError, "",
			fmt.Sprintf("[%d consecutive unusable tool calls; last raw call: %s]", run.parseFails, truncateRunes(pe.Raw, 500))))
	}
	if err := run.record(ctx, t, msgs, llm.Message{Role: "assistant", Content: "(malformed tool call)"}); err != nil {
		return doneWith(run.storeErr(ctx, t, err))
	}
	if err := run.record(ctx, t, msgs, llm.Message{Role: "user", Content: fmt.Sprintf(parseNudgeFmt, truncateRunes(pe.Msg, 300))}); err != nil {
		return doneWith(run.storeErr(ctx, t, err))
	}
	return 0, false, nil
}

// repairCall recovers the arguments of a call Ollama could not parse, from its raw text. It succeeds when the raw
// text holds a JSON object.
func repairCall(raw string) (map[string]any, bool) {
	v, err := toolcall.LenientJSON(raw)
	if err != nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

// inferTool names a call whose name was lost, from the keys of its arguments.
func inferTool(args map[string]any) string {
	for _, k := range []string{"patch", "diff"} {
		if _, ok := args[k]; ok {
			return "apply_patch"
		}
	}
	return "bash"
}

// harmonyArgs turns the raw argument text of a harmony-format call into arguments. A JSON object is used as is;
// anything else is passed under "input", which the normaliser reads as the main argument.
func harmonyArgs(raw string) map[string]any {
	v, err := toolcall.LenientJSON(raw)
	if err == nil {
		if m, ok := v.(map[string]any); ok {
			return m
		}
		return map[string]any{"input": v}
	}
	return map[string]any{"input": raw}
}

// handleText decides what a text reply without tool calls means: a verdict or a second reply without one ends
// the run; the first reply without one gets the nudge.
func (run *taskRun) handleText(ctx context.Context, t *domain.Task, agent *domain.Agent, msgs *[]llm.Message, text string) (outcome, bool, error) {
	if verdictOf(text) != "" || hasNudge(*msgs) {
		return doneWith(run.conclude(ctx, t, agent, text, lastToolOutput(*msgs), ""))
	}
	if err := run.record(ctx, t, msgs, llm.Message{Role: "user", Content: nudgeText}); err != nil {
		return doneWith(run.storeErr(ctx, t, err))
	}
	return 0, false, nil
}

// runCall executes one tool call from the model, or refuses it, and stores the tool result. last says whether it is
// the last call of its reply: then, when check_each_step is on and a command ran, the check runs.
func (run *taskRun) runCall(ctx context.Context, t *domain.Task, agent *domain.Agent, msgs *[]llm.Message, call llm.ToolCall, last bool) (outcome, bool, error) {
	r := run.r
	norm, err := toolcall.Normalize(call.Name, call.Arguments)
	if err != nil {
		run.parseFails++
		out := fmt.Sprintf("error: %v. %s", err, fmt.Sprintf(parseNudgeFmt, "see above"))
		if run.parseFails > maxParseFails {
			return doneWith(r.finish(ctx, t, domain.TaskError, "",
				fmt.Sprintf("[%d consecutive unusable tool calls; last: %v]", run.parseFails, err)))
		}
		if err := run.record(ctx, t, msgs, llm.Message{Role: "tool", ToolName: call.Name, Content: out}); err != nil {
			return doneWith(run.storeErr(ctx, t, err))
		}
		return 0, false, nil
	}
	if t.Steps >= maxSteps(t) {
		refusal := "error: the step limit has been reached, so the command was not run"
		if err := run.record(ctx, t, msgs, llm.Message{Role: "tool", ToolName: call.Name, Content: refusal}); err != nil {
			return doneWith(run.storeErr(ctx, t, err))
		}
		return 0, false, nil
	}
	run.parseFails = 0
	seq := t.Steps + 1
	var (
		step domain.Step
		msg  llm.Message
	)
	switch norm.Tool {
	case toolcall.ApplyPatch:
		out, code, err := run.runPatch(ctx, t, agent, seq, norm.Patch)
		if err != nil {
			return doneWith(run.vmErr(ctx, t, "apply_patch", err))
		}
		step = domain.Step{TaskID: t.ID, Command: capCommand("apply_patch " + firstLine(norm.Patch)),
			Output: excerpt(out), ExitCode: &code, At: r.now()}
		msg = cleanMessage(llm.Message{Role: "tool", ToolName: "apply_patch", Content: out + fmt.Sprintf("\n[exit %d]", code)})
	default:
		cmd := Clean(norm.Command)
		res, err := r.runCommand(ctx, agent, fmt.Sprintf("%s-%d", t.ID, seq), cmd, r.cfg.CommandTimeout)
		if err != nil {
			return doneWith(run.vmErr(ctx, t, "command", err))
		}
		step = domain.Step{TaskID: t.ID, Command: capCommand(cmd), Output: excerpt(res.combined()),
			ExitCode: res.ExitCode, At: r.now()}
		msg = cleanMessage(llm.Message{Role: "tool", ToolName: "bash", Content: toolContent(res)})
	}
	if err := run.storeStep(ctx, t, seq, step, msg); err != nil {
		return doneWith(run.storeErr(ctx, t, err))
	}
	*msgs = append(*msgs, msg)
	run.ranBatch = true
	if r.cfg.Hooks.OnStep != nil {
		r.cfg.Hooks.OnStep(t)
	}
	if last && run.ranBatch && t.CheckCommand != "" && t.CheckEachStep {
		return run.checkAfterBatch(ctx, t, agent, seq)
	}
	return 0, false, nil
}

// storeStep writes a command's step and its tool message in one transaction, and records when the first command
// ran. The fenced write makes sure this worker still owns the task.
func (run *taskRun) storeStep(ctx context.Context, t *domain.Task, seq int, step domain.Step, msg llm.Message) error {
	r := run.r
	first := 0.0
	if t.FirstCommandSeconds == 0 && !t.StartedAt.IsZero() {
		first = seconds(r.now().Sub(t.StartedAt))
	}
	fresh, err := r.fenced(ctx, t.ID, func(s store.Store, cur *domain.Task) error {
		if cur.Steps != seq-1 {
			return fmt.Errorf("step count changed from %d to %d", seq-1, cur.Steps)
		}
		if _, err := s.AppendStep(ctx, &step); err != nil {
			return err
		}
		cur.Steps = seq
		if cur.FirstCommandSeconds == 0 && first > 0 {
			cur.FirstCommandSeconds = first
		}
		if err := s.UpdateTask(ctx, cur); err != nil {
			return err
		}
		return r.storeMessage(ctx, s, t.ID, msg)
	})
	if err != nil {
		return err
	}
	*t = *fresh
	return nil
}

// checkAfterBatch runs check_command after a batch of commands. When it exits 0 the task succeeds at once, decided
// by the check, without waiting for a final report.
func (run *taskRun) checkAfterBatch(ctx context.Context, t *domain.Task, agent *domain.Agent, seq int) (outcome, bool, error) {
	res, err := run.r.runCommand(ctx, agent, fmt.Sprintf("%s-check-%d", t.ID, seq), t.CheckCommand, checkTimeout)
	if err != nil {
		return doneWith(run.vmErr(ctx, t, "check_command", err))
	}
	if !res.passed() {
		return 0, false, nil
	}
	t.Reason, t.VerdictSource = reasonCheckPass, domain.VerdictFromCheck
	result := fmt.Sprintf("[check_command passed after step %d; the loop ended without waiting for a final report]", seq)
	if out := tailBytes(strings.TrimSpace(res.Stdout+res.Stderr), checkTailBytes); out != "" {
		result += "\n" + out
	}
	return doneWith(run.r.finish(ctx, t, domain.TaskSucceeded, "", result))
}

// runPatch applies an apply_patch call inside the VM, relative to the agent's working directory. The result text is
// the summary, or the error that stopped the patch (exit 1). A VM failure is returned as an error.
func (run *taskRun) runPatch(ctx context.Context, t *domain.Task, agent *domain.Agent, seq int, patch string) (string, int, error) {
	fs := &vmFS{r: run.r, ctx: ctx, agent: agent, key: fmt.Sprintf("%s-patch-%d", t.ID, seq)}
	summary, err := applypatch.Apply(patch, fs, agent.Workdir)
	if fs.err != nil {
		return "", 0, fs.err
	}
	if err != nil {
		return "error: " + err.Error(), 1, nil
	}
	return summary, 0, nil
}

// conclude decides the final state from the model's verdict, or from the check command when one is set, and
// finishes the task. reason says why the loop ended without a proper final report ("" for none).
func (run *taskRun) conclude(ctx context.Context, t *domain.Task, agent *domain.Agent, text, lastOutput, reason string) (outcome, error) {
	r := run.r
	verdict := verdictOf(text)
	state := domain.TaskGaveUp
	switch verdict {
	case "SUCCESS":
		state = domain.TaskSucceededUnverified
	case "FAILED":
		state = domain.TaskFailed
	}
	if reason != "" {
		t.Reason = reason
		if t.CheckCommand == "" {
			state = domain.TaskGaveUp
		}
	}
	result := strings.TrimSpace(text)
	if result == "" {
		result = "[the model gave no final message]\n" + tailBytes(lastOutput, 1500)
	}
	t.VerdictSource = domain.VerdictFromModel
	if t.CheckCommand != "" {
		res, err := r.runCommand(ctx, agent, t.ID+"-check", t.CheckCommand, checkTimeout)
		if err != nil {
			return run.vmErr(ctx, t, "check_command", err)
		}
		t.VerdictSource = domain.VerdictFromCheck
		result += fmt.Sprintf("\n\n[check_command %s]", res.status())
		if out := tailBytes(strings.TrimSpace(res.Stdout+res.Stderr), checkTailBytes); out != "" {
			result += "\n" + out
		}
		if res.passed() {
			state = domain.TaskSucceeded
			if verdict == "FAILED" {
				result += "\n[note: the model reported FAILED but the check passed]"
			}
		} else {
			state = domain.TaskFailed
			if reason != "" {
				state = domain.TaskGaveUp
			}
			if verdict == "SUCCESS" {
				result += "\n[note: the model reported SUCCESS but the check FAILED]"
			}
		}
	} else if state == domain.TaskSucceededUnverified {
		result += "\n[note: no check_command was given, so this success is the model's own claim]"
	}
	return r.finish(ctx, t, state, verdict, result)
}

// record stores a message and adds it to msgs. The message is persisted before it is used. An assistant message
// starts a new batch of calls.
func (run *taskRun) record(ctx context.Context, t *domain.Task, msgs *[]llm.Message, m llm.Message) error {
	m = cleanMessage(m)
	if _, err := run.r.fenced(ctx, t.ID, func(s store.Store, _ *domain.Task) error {
		return run.r.storeMessage(ctx, s, t.ID, m)
	}); err != nil {
		return err
	}
	*msgs = append(*msgs, m)
	return nil
}

// storeMessage writes one conversation message inside a store transaction.
func (r *Runner) storeMessage(ctx context.Context, s store.Store, taskID string, m llm.Message) error {
	raw, err := json.Marshal(cleanMessage(m))
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	_, err = s.AppendMessage(ctx, &domain.Message{TaskID: taskID, JSON: raw, At: r.now()})
	return err
}

// setLive shows the streamed text of the call in progress, at most once per liveEvery. The live text is never part
// of the result. A failed write is ignored: the heartbeat notices a lost lease.
func (run *taskRun) setLive(ctx context.Context, t *domain.Task, answer, reasoning string) {
	r := run.r
	now := r.now()
	if !run.lastLive.IsZero() && now.Sub(run.lastLive) < liveEvery {
		return
	}
	run.lastLive = now
	if _, err := r.fenced(ctx, t.ID, func(s store.Store, cur *domain.Task) error {
		cur.LiveAnswer, cur.LiveReasoning = answer, reasoning
		return s.UpdateTask(ctx, cur)
	}); err != nil {
		r.log.Debug("live text not written", "task", t.ID, "err", err)
		return
	}
	t.LiveAnswer, t.LiveReasoning = answer, reasoning
}

// persistProgress writes the progress fields of t to the store. It runs even when the task's context has ended,
// so the record of a stopped model call is kept.
func (run *taskRun) persistProgress(ctx context.Context, t *domain.Task) {
	r := run.r
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelGrace)
	defer cancel()
	fresh, err := r.fenced(pctx, t.ID, func(s store.Store, cur *domain.Task) error {
		copyProgress(cur, t)
		return s.UpdateTask(pctx, cur)
	})
	if err != nil {
		if !errors.Is(err, errLeaseLost) {
			r.log.Warn("progress not written", "task", t.ID, "err", err)
		}
		return
	}
	*t = *fresh
}

// copyProgress copies the progress fields that the worker owns from src to dst. The live text is cleared.
func copyProgress(dst, src *domain.Task) {
	dst.Reason = src.Reason
	dst.VerdictSource = src.VerdictSource
	dst.PromptTokens = src.PromptTokens
	dst.CompletionTokens = src.CompletionTokens
	dst.ModelCalls = src.ModelCalls
	dst.PartialCalls = src.PartialCalls
	dst.FirstTokenSeconds = src.FirstTokenSeconds
	dst.FirstCommandSeconds = src.FirstCommandSeconds
	dst.LiveAnswer, dst.LiveReasoning = "", ""
	dst.Calls = src.Calls
}

// deadlineHit reports whether the task's time limit has passed.
func (run *taskRun) deadlineHit() bool {
	return errors.Is(context.Cause(run.ctx), errTaskTimeout)
}

// interrupted ends a run whose context was cancelled. A lost lease writes nothing. A cancel request finishes
// the task as cancelled; so does an expired time limit, with reason timeout. Otherwise the worker is shutting
// down, and the task is left for another worker.
func (run *taskRun) interrupted(t *domain.Task) (outcome, error) {
	switch {
	case run.lost.Load():
		return outAbandoned, nil
	case run.cancelReq.Load(), run.deadlineHit():
		ctx, cancel := context.WithTimeout(context.Background(), cancelGrace)
		defer cancel()
		if !run.cancelReq.Load() {
			t.Reason = reasonTimeout
			return run.r.finish(ctx, t, domain.TaskCancelled, "", "[cancelled: timeout]")
		}
		return run.r.finish(ctx, t, domain.TaskCancelled, "", "[cancelled]")
	default:
		return outAbandoned, nil
	}
}

// storeErr turns a store error into an outcome: a stop request wins, a lost lease abandons quietly, and any
// other error is returned to the caller.
func (run *taskRun) storeErr(ctx context.Context, t *domain.Task, err error) (outcome, error) {
	switch {
	case ctx.Err() != nil:
		return run.interrupted(t)
	case errors.Is(err, errLeaseLost):
		return outAbandoned, nil
	default:
		return outAbandoned, fmt.Errorf("task %s: %w", t.ID, err)
	}
}

// vmErr turns a failed VM call into an outcome: transient failures requeue the task, and other failures end it
// with state error.
func (run *taskRun) vmErr(ctx context.Context, t *domain.Task, what string, err error) (outcome, error) {
	switch {
	case ctx.Err() != nil:
		return run.interrupted(t)
	case errors.Is(err, errs.ErrUnavailable):
		return run.r.requeue(ctx, t, err)
	default:
		return run.r.finish(ctx, t, domain.TaskError, "", fmt.Sprintf("%s failed: %v", what, err))
	}
}

// doneWith adapts the (outcome, error) pair of a finishing call to step's results.
func doneWith(oc outcome, err error) (outcome, bool, error) { return oc, true, err }

// maxSteps is the number of commands the task may run in total.
func maxSteps(t *domain.Task) int {
	if t.MaxSteps <= 0 {
		return defaultMaxSteps
	}
	return t.MaxSteps
}

// lastAssistant returns the index of the last assistant message, or -1.
func lastAssistant(m []llm.Message) int {
	for i := len(m) - 1; i >= 0; i-- {
		if m[i].Role == "assistant" {
			return i
		}
	}
	return -1
}

// toolResults counts the tool messages stored after message index ai.
func toolResults(m []llm.Message, ai int) int {
	n := 0
	for _, x := range m[ai+1:] {
		if x.Role == "tool" {
			n++
		}
	}
	return n
}

// lastToolOutput returns the content of the last tool message, or "".
func lastToolOutput(m []llm.Message) string {
	for i := len(m) - 1; i >= 0; i-- {
		if m[i].Role == "tool" {
			return m[i].Content
		}
	}
	return ""
}

// hasNudge reports whether the nudge has already been sent in this conversation.
func hasNudge(m []llm.Message) bool {
	for _, x := range m {
		if x.Role == "user" && x.Content == nudgeText {
			return true
		}
	}
	return false
}

// verdictOf returns SUCCESS or FAILED from the last verdict line of text, or "".
func verdictOf(text string) string {
	found := verdictRE.FindAllStringSubmatch(text, -1)
	if len(found) == 0 {
		return ""
	}
	return strings.ToUpper(found[len(found)-1][1])
}

// repeating reports whether the tail of the model's output is one phrase over and over: a phrase of repeatNGram
// characters with at least repeatMinAlnu letters or digits that occurs at least repeatTimes times in the tail.
// It is checked on every 4th starting position, as the Python gateway does.
func repeating(tail string) bool {
	r := []rune(tail)
	if len(r) < repeatWindow/2 {
		return false
	}
	for i := 0; i+repeatNGram <= len(r); i += 4 {
		gram := string(r[i : i+repeatNGram])
		alnum := 0
		for _, c := range gram {
			if unicode.IsLetter(c) || unicode.IsDigit(c) {
				alnum++
			}
		}
		if alnum >= repeatMinAlnu && strings.Count(tail, gram) >= repeatTimes {
			return true
		}
	}
	return false
}

// lastRunes returns the last n characters of s, on a character boundary.
func lastRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[len(r)-n:])
}

// truncateRunes returns s cut to at most n characters.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}

// firstLine returns the first line of s.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// seconds converts a duration to fractional seconds, rounded to milliseconds.
func seconds(d time.Duration) float64 {
	return float64(d.Round(time.Millisecond)) / float64(time.Second)
}

// vmStateName is a VM state as the model-facing messages spell it: "stopped", "error" and so on.
func vmStateName(s vmdv1.VMState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "VM_STATE_"))
}

// systemPrompt is the system message for an agent: the ported SYSTEM text, the working-directory rule and the
// agent's own system prompt when it has one.
func systemPrompt(agent *domain.Agent) string {
	s := systemText + fmt.Sprintf("\n\nYour working directory is %s; other agents may share this machine, stay inside your directory and do not touch other agents' files.", agent.Workdir)
	if agent.SystemPrompt != "" {
		s += "\n\n" + agent.SystemPrompt
	}
	return Clean(s)
}
