# ADR 3: Command jobs live inside the guest

**Decision.** `ExecService` starts commands as detached jobs whose state (`cmd`, `pgid`, `stdout`, `stderr`,
`exit_code`) is files under `/var/tmp/mcp-jobs/<id>/` in the VM. The control plane keeps only metadata.

**Why.** Output piped through an SSH session hangs when a command leaves a background child holding the pipe,
and any control-plane or node restart would lose the job. File-based jobs are immune to both and can be
polled by offset from any replica.

**Consequences.** Calls never block more than 30 s (clients time out near 60 s). A VM that stops marks its
running jobs `LOST` with the VM's stop reason. Full output is retrievable until the job directory is pruned.
