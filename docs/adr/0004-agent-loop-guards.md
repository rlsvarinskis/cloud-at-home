# ADR 4: Guards on the agent loop, and honest verdicts

**Decision.** agentd's loop stops on its own when a model misbehaves, and never reports a success it did not verify.
Concretely: a malformed tool call is repaired or sent back for a retry (it does not end the run unless four come in a
row); each reply has a token cap; a streamed reply that repeats itself is aborted and discarded; two discarded replies
end the task as `gave_up`; a task may carry a time limit that kills its running command; with a `check_command`, the check
can end the task after the batch of commands that passed it; a claimed success without a check is
`succeeded_unverified`, not `succeeded`; and every model call is recorded with its tokens, timing and how it ended, so
progress can be polled cheaply. Live text is shown separately from the result.

**Why.** Local models fail in predictable ways. They emit tool calls that do not parse, they loop on the same phrase
until the token budget is gone, and they say "done" when nothing was checked. Before these guards a single bad reply
could end a task as an error, or leave it running with nothing to show, and the caller could not tell a verified
success from a claim. The Python gateway had these guards and was the reference for the thresholds, so the Go service
keeps the same behaviour. A `succeeded_unverified` state (rather than changing the meaning of `succeeded`) keeps the
existing states' meanings and lets clients decide how much to trust a success without a check.

**Consequences.**
- Tasks that used to end `succeeded` without a check now end `succeeded_unverified`. Clients that treated every
  `succeeded` as verified should check `verdict_source` or the new state.
- A reply that streams repetitively, or that is cut by the token cap, costs a model call and is not stored. Its
  streamed tokens are counted (one per chunk) and it is marked `partial`, so token counts are an estimate for those calls.
- `check_each_step` makes a passing check end a task after its first batch of commands. A caller who wants the model's
  own final report must set it to false.
- The counters that drive the guards (consecutive unusable calls, cut replies) are kept per worker run. A task taken
  over by another worker starts them again. That can only make a task more patient, never less safe, because the
  step limit and the time limit still apply.
- Thresholds and windows are constants in `runner/loop.go`, not flags. They are tuned for the models that exist today;
  a change must come with a test of the new value.
- The apply_patch tool is offered only to `gpt-oss` models, because the other models do not use the format.
