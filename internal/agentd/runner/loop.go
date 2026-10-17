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

	vmdv1 "example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/internal/agentd/domain"
	"example.com/homecloud/internal/agentd/llm"
	"example.com/homecloud/internal/agentd/store"
	"example.com/homecloud/internal/errs"
)

// systemText and nudgeText are ported from the Python gateway's SYSTEM and NUDGE.
const (
	systemText = "You are an autonomous agent with full control of a Linux virtual machine through the bash tool. " +
		"Your home is /home/agent. Work step by step and verify your results by running commands. " +
		"When you are finished, reply with a concise final report (no tool call) that ends with exactly one " +
		"line: 'VERDICT: SUCCESS' if you verified the task's success criteria are met, otherwise " +
		"'VERDICT: FAILED - <reason>'. Never claim success you have not verified."
	nudgeText = "Give your final report now: what you did and whether the task's success criteria are met. " +
		"End with exactly one line: 'VERDICT: SUCCESS' or 'VERDICT: FAILED - <reason>'."
	defaultStepLimit = defaultMaxSteps
)

// verdictRE finds the verdict line of a reply (ported from the Python gateway's VERDICT_RE).
var verdictRE = regexp.MustCompile(`(?im)^\W*VERDICT:\s*(SUCCESS|FAILED)\b[\s:-]*(.*)$`)

// bashTool is the only tool the model is given.
var bashTool = llm.Tool{
	Name:        "bash",
	Description: "Run a bash command in your Linux machine (Debian, sudo, internet). Returns output.",
	Parameters: map[string]any{
		"type":       "object",
		"properties": map[string]any{"command": map[string]any{"type": "string"}},
		"required":   []any{"command"},
	},
}

// outcome says how a worker left a task.
type outcome int

const (
	outFinished  outcome = iota // the task reached a final state, which this worker wrote
	outAbandoned                // stopped without writing: shutdown or a lost lease; the lease expires on its own
	outRequeued                 // released to queued after a transient failure
)

// Why a task context was cancelled. Only causeTaskCancelled (the user cancelled the task) kills the command
// that is running in the VM; on shutdown or a lost lease the command is left alone, because the worker that
// takes the task over re-attaches to the same job (by idempotency key) and sees its real result.
var (
	causeTaskCancelled = errors.New("task cancelled by request")
	causeLeaseLost     = errors.New("task lease lost")
)

// taskRun is the state of one task run shared by the worker and its heartbeat and cancel-watch goroutines.
type taskRun struct {
	r         *Runner
	cancel    context.CancelCauseFunc // cancels the task's context, recording why
	snap      domain.Task             // copy of the task as claimed; read-only, used by goroutine hooks
	lost      atomic.Bool             // the lease was taken by another owner
	cancelReq atomic.Bool             // a cancel was requested in the store
}

// runTask runs one claimed task. It starts the heartbeat and the cancel watch, and stops both before returning.
func (r *Runner) runTask(parent context.Context, t *domain.Task) (outcome, error) {
	ctx, cancel := context.WithCancelCause(parent)
	run := &taskRun{r: r, cancel: cancel, snap: *t}
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
			run.cancel(causeLeaseLost)
			return
		default:
			r.log.Warn("extend lease", "task", run.snap.ID, "err", err)
		}
	}
}

// watchCancel polls the task row and, when a cancel is requested, cancels the run.
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
		cur, err := r.cfg.Store.GetTask(ctx, run.snap.ID)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("poll task", "task", run.snap.ID, "err", err)
			}
			continue
		}
		if cur.CancelRequested {
			run.cancelReq.Store(true)
			run.cancel(causeTaskCancelled)
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
	r := run.r
	if ctx.Err() != nil {
		return doneWith(run.interrupted(t))
	}
	m := *msgs

	if ai := lastAssistant(m); ai >= 0 && len(m[ai].ToolCalls) > 0 {
		if n := toolResults(m, ai); n < len(m[ai].ToolCalls) {
			return run.runCall(ctx, t, agent, msgs, m[ai].ToolCalls[n])
		}
	}
	if last := m[len(m)-1]; last.Role == "assistant" {
		return run.handleText(ctx, t, agent, msgs, last.Content)
	}
	if t.Steps >= maxSteps(t) {
		text := fmt.Sprintf("[stopped after %d steps without a final answer]", maxSteps(t))
		return doneWith(run.conclude(ctx, t, agent, text, lastToolOutput(m)))
	}

	reply, err := r.cfg.LLM.Chat(ctx, agent.Model, m, []llm.Tool{bashTool}, nil)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return doneWith(run.interrupted(t))
		case errors.Is(err, errs.ErrUnavailable):
			return doneWith(r.requeue(ctx, t, err))
		default:
			return doneWith(r.finish(ctx, t, domain.TaskError, "", "model error: "+err.Error()))
		}
	}
	reply.Role = "assistant"
	if err := run.record(ctx, t, msgs, reply); err != nil {
		return doneWith(run.storeErr(ctx, t, err))
	}
	return 0, false, nil
}

// handleText decides what a text reply without tool calls means: a verdict or a second reply without one ends
// the run; the first reply without one gets the nudge.
func (run *taskRun) handleText(ctx context.Context, t *domain.Task, agent *domain.Agent, msgs *[]llm.Message, text string) (outcome, bool, error) {
	if verdictOf(text) != "" || hasNudge(*msgs) {
		return doneWith(run.conclude(ctx, t, agent, text, lastToolOutput(*msgs)))
	}
	if err := run.record(ctx, t, msgs, llm.Message{Role: "user", Content: nudgeText}); err != nil {
		return doneWith(run.storeErr(ctx, t, err))
	}
	return 0, false, nil
}

// runCall executes one bash tool call from the model, or refuses it, and stores the tool result. For a command
// that runs, the step and the result are stored in one transaction, so a crash leaves neither or both.
func (run *taskRun) runCall(ctx context.Context, t *domain.Task, agent *domain.Agent, msgs *[]llm.Message, call llm.ToolCall) (outcome, bool, error) {
	r := run.r
	cmd, isString := call.Arguments["command"].(string)
	var refusal string
	switch {
	case call.Name != "bash":
		refusal = fmt.Sprintf("error: unknown tool %q; the only tool is bash", call.Name)
	case !isString || strings.TrimSpace(cmd) == "":
		refusal = `error: bash needs a non-empty string argument "command"`
	case t.Steps >= maxSteps(t):
		refusal = "error: the step limit has been reached, so the command was not run"
	}
	if refusal != "" {
		if err := run.record(ctx, t, msgs, llm.Message{Role: "tool", ToolName: call.Name, Content: refusal}); err != nil {
			return doneWith(run.storeErr(ctx, t, err))
		}
		return 0, false, nil
	}

	cmd = Clean(cmd)
	seq := t.Steps + 1
	res, err := r.runCommand(ctx, agent, fmt.Sprintf("%s-%d", t.ID, seq), cmd, r.cfg.CommandTimeout)
	if err != nil {
		return doneWith(run.vmErr(ctx, t, "command", err))
	}

	step := domain.Step{
		TaskID:   t.ID,
		Command:  capCommand(cmd),
		Output:   excerpt(res.combined()),
		ExitCode: res.ExitCode,
		At:       r.now(),
	}
	msg := cleanMessage(llm.Message{Role: "tool", ToolName: "bash", Content: toolContent(res)})
	fresh, err := r.fenced(ctx, t.ID, func(s store.Store, cur *domain.Task) error {
		if cur.Steps != seq-1 {
			return fmt.Errorf("step count changed from %d to %d", seq-1, cur.Steps)
		}
		if _, err := s.AppendStep(ctx, &step); err != nil {
			return err
		}
		cur.Steps = seq
		if err := s.UpdateTask(ctx, cur); err != nil {
			return err
		}
		return r.storeMessage(ctx, s, t.ID, msg)
	})
	if err != nil {
		return doneWith(run.storeErr(ctx, t, err))
	}
	*t = *fresh
	*msgs = append(*msgs, msg)
	if r.cfg.Hooks.OnStep != nil {
		r.cfg.Hooks.OnStep(t)
	}
	return 0, false, nil
}

// conclude decides the final state from the model's verdict, or from the check command when one is set, and
// finishes the task.
func (run *taskRun) conclude(ctx context.Context, t *domain.Task, agent *domain.Agent, text, lastOutput string) (outcome, error) {
	r := run.r
	verdict := verdictOf(text)
	state := domain.TaskGaveUp
	switch verdict {
	case "SUCCESS":
		state = domain.TaskSucceeded
	case "FAILED":
		state = domain.TaskFailed
	}
	result := strings.TrimSpace(text)
	if result == "" {
		result = "[the model gave no final message]\n" + tailBytes(lastOutput, 1500)
	}
	if t.CheckCommand != "" {
		res, err := r.runCommand(ctx, agent, t.ID+"-check", t.CheckCommand, checkTimeout)
		if err != nil {
			return run.vmErr(ctx, t, "check_command", err)
		}
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
			if verdict == "SUCCESS" {
				result += "\n[note: the model reported SUCCESS but the check FAILED]"
			}
		}
	}
	return r.finish(ctx, t, state, verdict, result)
}

// record stores a message and adds it to msgs. The message is persisted before it is used.
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

// interrupted ends a run whose context was cancelled. A lost lease writes nothing. A cancel request finishes
// the task as cancelled. Otherwise the worker is shutting down, and the task is left for another worker.
func (run *taskRun) interrupted(t *domain.Task) (outcome, error) {
	switch {
	case run.lost.Load():
		return outAbandoned, nil
	case run.cancelReq.Load():
		ctx, cancel := context.WithTimeout(context.Background(), cancelGrace)
		defer cancel()
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
		return defaultStepLimit
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
