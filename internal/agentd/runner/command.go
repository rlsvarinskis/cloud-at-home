package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	vmdv1 "example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/internal/agentd/domain"
)

const (
	pollWait     = 25               // seconds a StartJob or PollJob call may wait inside vmd (max 30)
	pollMaxOut   = 16384            // bytes per stream vmd returns per call
	checkTimeout = 5 * time.Minute  // lifetime limit for the task's check_command
	cancelGrace  = 10 * time.Second // how long a CancelJob call may take after the task was stopped
)

// cmdResult is the final state of one VM job.
type cmdResult struct {
	State     vmdv1.JobState
	ExitCode  *int   // set when the command exited
	Stdout    string // tail of stdout (vmd returns the last bytes)
	Stderr    string
	Truncated bool   // vmd omitted earlier output
	Killed    string // vmd's reason when the job was killed
}

// status describes how the job ended, in the form shown to the model: "exit 0", "timed_out: reason" and so on.
func (c cmdResult) status() string {
	if (c.State == vmdv1.JobState_JOB_STATE_SUCCEEDED || c.State == vmdv1.JobState_JOB_STATE_FAILED) && c.ExitCode != nil {
		return fmt.Sprintf("exit %d", *c.ExitCode)
	}
	name := strings.ToLower(strings.TrimPrefix(c.State.String(), "JOB_STATE_"))
	if c.Killed != "" {
		return name + ": " + Clean(c.Killed)
	}
	return name
}

// combined is stdout followed by a [stderr] section when stderr is not empty.
func (c cmdResult) combined() string {
	out := c.Stdout
	if c.Truncated {
		out = "[earlier output omitted]\n" + out
	}
	if c.Stderr != "" {
		out += "\n[stderr]\n" + c.Stderr
	}
	return Clean(out)
}

// passed reports whether the command exited with status 0.
func (c cmdResult) passed() bool {
	return c.State == vmdv1.JobState_JOB_STATE_SUCCEEDED && c.ExitCode != nil && *c.ExitCode == 0
}

// toolContent is what the model sees as the result of a bash call.
func toolContent(c cmdResult) string {
	return clipMiddle(c.combined(), toolHeadBytes, toolTailBytes) + "\n[" + c.status() + "]"
}

// finalJob reports whether a job state is terminal.
func finalJob(s vmdv1.JobState) bool {
	switch s {
	case vmdv1.JobState_JOB_STATE_SUCCEEDED, vmdv1.JobState_JOB_STATE_FAILED,
		vmdv1.JobState_JOB_STATE_CANCELLED, vmdv1.JobState_JOB_STATE_TIMED_OUT,
		vmdv1.JobState_JOB_STATE_LOST:
		return true
	}
	return false
}

// runCommand runs command in the agent's VM and waits for it to finish. key is the idempotency key, so a job
// started before a worker crashed is found again rather than run twice. If ctx ends first, the job is
// cancelled with a fresh context and ctx's error is returned.
func (r *Runner) runCommand(ctx context.Context, agent *domain.Agent, key, command string, timeout time.Duration) (cmdResult, error) {
	secs := int32(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	command = Clean(command)
	out, err := r.cfg.VM.StartJob(ctx, &vmdv1.StartJobRequest{
		VmId:           agent.VMID,
		Command:        command,
		Workdir:        agent.Workdir,
		TimeoutSeconds: secs,
		WaitSeconds:    pollWait,
		MaxOutput:      pollMaxOut,
		IdempotencyKey: key,
	})
	if err != nil {
		return cmdResult{}, err
	}
	if out.GetJob() == nil {
		return cmdResult{}, errors.New("vmd returned no job")
	}
	id := out.GetJob().GetId()
	for !finalJob(out.GetJob().GetState()) {
		if err := ctx.Err(); err != nil {
			r.stopJob(ctx, id)
			return cmdResult{}, err
		}
		next, err := r.cfg.VM.PollJob(ctx, &vmdv1.PollJobRequest{
			JobId:       id,
			WaitSeconds: pollWait,
			Tail:        true,
			MaxOutput:   pollMaxOut,
		})
		if err != nil {
			if ctx.Err() != nil {
				r.stopJob(ctx, id)
			}
			return cmdResult{}, err
		}
		if next.GetJob() == nil {
			return cmdResult{}, errors.New("vmd returned a poll without a job")
		}
		out = next
	}
	return resultOf(out), nil
}

// resultOf converts a final JobOutput into a cmdResult with sanitised text.
func resultOf(out *vmdv1.JobOutput) cmdResult {
	j := out.GetJob()
	res := cmdResult{
		State:     j.GetState(),
		Stdout:    Clean(string(out.GetStdout())),
		Stderr:    Clean(string(out.GetStderr())),
		Truncated: out.GetTruncated(),
		Killed:    j.GetKilledReason(),
	}
	if j.ExitCode != nil {
		code := int(*j.ExitCode)
		res.ExitCode = &code
	}
	return res
}

// stopJob kills the VM command only when the task itself was cancelled (not on shutdown or lease loss).
func (r *Runner) stopJob(taskCtx context.Context, id string) {
	if errors.Is(context.Cause(taskCtx), causeTaskCancelled) {
		r.cancelJob(id)
	}
}

// cancelJob asks vmd to stop a job. It uses its own context so the cancel goes out even when the task's
// context is already done.
func (r *Runner) cancelJob(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), cancelGrace)
	defer cancel()
	if _, err := r.cfg.VM.CancelJob(ctx, id); err != nil {
		r.log.Warn("cancel job", "job", id, "err", err)
	}
}
