package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
	llmmock "github.com/rlsvarinskis/cloud-at-home/services/agentd/llm/mock"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/store"
	storemock "github.com/rlsvarinskis/cloud-at-home/services/agentd/store/mock"
	vmclientmock "github.com/rlsvarinskis/cloud-at-home/services/agentd/vmclient/mock"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

var gmRunNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// gmRun holds the gomock collaborators of one runner. tx is the store handed to Tx callbacks, so writes inside
// a transaction are expected separately from the plain store reads.
type gmRun struct {
	st  *storemock.MockStore
	tx  *storemock.MockStore
	llm *llmmock.MockClient
	vm  *vmclientmock.MockVM
}

// gmNewRun builds a runner whose heartbeat and cancel watch never fire during an ordinary test: the lease
// is a minute long (heartbeat every 20 s) and the cancel poll an hour.
func gmNewRun(t *testing.T, mut ...func(*Config)) (*gmRun, *Runner) {
	t.Helper()
	ctrl := gomock.NewController(t)
	g := &gmRun{st: storemock.NewMockStore(ctrl), tx: storemock.NewMockStore(ctrl),
		llm: llmmock.NewMockClient(ctrl), vm: vmclientmock.NewMockVM(ctrl)}
	cfg := Config{Store: g.st, LLM: g.llm, VM: g.vm, Owner: "gm-worker", Workers: 1,
		LeaseTTL: time.Minute, PollInterval: time.Millisecond, CancelPoll: time.Hour,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return gmRunNow }}
	for _, f := range mut {
		f(&cfg)
	}
	return g, New(cfg)
}

// inTx expects one store transaction; its callback runs against g.tx.
func (g *gmRun) inTx() *gomock.Call {
	return g.st.EXPECT().Tx(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, fn func(store.Store) error) error { return fn(g.tx) })
}

// txMessage expects a transaction that reads the task and appends one conversation message.
func (g *gmRun) txMessage(m gomock.Matcher) []any {
	return []any{
		g.inTx(),
		g.tx.EXPECT().GetTask(gomock.Any(), "task_1").Return(gmClaimed(), nil),
		g.tx.EXPECT().AppendMessage(gomock.Any(), m).Return(1, nil),
	}
}

// gmClaimed is the task as ClaimTasks hands it out: running, leased to this worker, no steps yet.
func gmClaimed() *domain.Task {
	return &domain.Task{ID: "task_1", AgentID: "agent_1", ProjectID: "p1", Prompt: "list the home directory",
		MaxSteps: 40, State: domain.TaskRunning, LeaseOwner: "gm-worker", LeaseUntil: gmRunNow.Add(time.Minute),
		CreatedAt: gmRunNow, StartedAt: gmRunNow}
}

func gmAgentRow() *domain.Agent {
	return &domain.Agent{ID: "agent_1", ProjectID: "p1", VMID: "vm_1", Model: "llama3",
		Workdir: "/home/agent/agents/agent_1", State: domain.AgentActive}
}

func gmRunningVM() *vmdv1.VM { return &vmdv1.VM{Id: "vm_1", State: vmdv1.VMState_VM_STATE_RUNNING} }

// gmMsgWhere matches a stored message (JSON-encoded llm.Message) by its decoded content.
func gmMsgWhere(pred func(llm.Message) bool) gomock.Matcher {
	return gomock.Cond(func(m *domain.Message) bool {
		var lm llm.Message
		return json.Unmarshal(m.JSON, &lm) == nil && pred(lm)
	})
}

func gmSystemMsg() gomock.Matcher {
	return gmMsgWhere(func(m llm.Message) bool {
		return m.Role == "system" && strings.Contains(m.Content, "/home/agent/agents/agent_1")
	})
}

func gmUserMsg() gomock.Matcher {
	return gmMsgWhere(func(m llm.Message) bool { return m.Role == "user" && m.Content == "list the home directory" })
}

func gmBashCallMsg() gomock.Matcher {
	return gmMsgWhere(func(m llm.Message) bool {
		return m.Role == "assistant" && len(m.ToolCalls) == 1 && m.ToolCalls[0].Name == "bash" &&
			m.ToolCalls[0].Arguments["command"] == "ls /home/agent"
	})
}

func gmFinalMsg() gomock.Matcher {
	return gmMsgWhere(func(m llm.Message) bool {
		return m.Role == "assistant" && strings.Contains(m.Content, "VERDICT: SUCCESS")
	})
}

// TestGMToolCallRequestAndOrder: a model bash call becomes exactly one StartJob (idempotency key
// "<task>-<seq>", agent workdir, default timeout, short start wait), the assistant turn is persisted before the
// command runs, and the step and tool result are persisted after the command finishes. The job is polled with
// Tail set.
func TestGMToolCallRequestAndOrder(t *testing.T) {
	g, r := gmNewRun(t)
	zero := int32(0)

	seq := []any{
		g.st.EXPECT().ClaimTasks(gomock.Any(), "gm-worker", gomock.Any(), time.Minute, 1).
			Return([]*domain.Task{gmClaimed()}, nil),
		g.st.EXPECT().GetAgent(gomock.Any(), "agent_1").Return(gmAgentRow(), nil),
		g.vm.EXPECT().GetVM(gomock.Any(), "vm_1").Return(gmRunningVM(), nil),
		g.st.EXPECT().ListMessages(gomock.Any(), "task_1").Return(nil, nil),
	}
	seq = append(seq, g.txMessage(gmSystemMsg())...)
	seq = append(seq, g.txMessage(gmUserMsg())...)
	seq = append(seq,
		g.llm.EXPECT().Chat(gomock.Any(), "llama3",
			gomock.Cond(func(m []llm.Message) bool {
				return len(m) == 2 && m[0].Role == "system" && m[1].Content == "list the home directory"
			}),
			gomock.Cond(func(ts []llm.Tool) bool { return len(ts) == 1 && ts[0].Name == "bash" }),
			gomock.Any(),
		).Return(llm.Message{ToolCalls: []llm.ToolCall{{Name: "bash", Arguments: map[string]any{"command": "ls /home/agent"}}}}, nil),
	)
	// The assistant turn is persisted before its command runs.
	seq = append(seq, g.txMessage(gmBashCallMsg())...)
	seq = append(seq,
		g.vm.EXPECT().StartJob(gomock.Any(), gomock.Cond(func(q *vmdv1.StartJobRequest) bool {
			return q.VmId == "vm_1" && q.Command == "ls /home/agent" && q.Workdir == "/home/agent/agents/agent_1" &&
				q.TimeoutSeconds == 600 && // defaultCommandTimeout
				q.WaitSeconds == 2 && q.MaxOutput == 16384 && q.IdempotencyKey == "task_1-1"
		})).Return(&vmdv1.JobOutput{Job: &vmdv1.Job{Id: "job_1", State: vmdv1.JobState_JOB_STATE_RUNNING}}, nil),
		g.vm.EXPECT().PollJob(gomock.Any(), gomock.Cond(func(q *vmdv1.PollJobRequest) bool {
			return q.JobId == "job_1" && q.Tail && q.WaitSeconds == 25 && q.MaxOutput == 16384
		})).Return(&vmdv1.JobOutput{
			Job:    &vmdv1.Job{Id: "job_1", State: vmdv1.JobState_JOB_STATE_SUCCEEDED, ExitCode: &zero},
			Stdout: []byte("a\nb\n"),
		}, nil),
	)
	// After the command: the step and the tool message are persisted together.
	seq = append(seq, g.inTx(),
		g.tx.EXPECT().GetTask(gomock.Any(), "task_1").Return(gmClaimed(), nil),
		g.tx.EXPECT().AppendStep(gomock.Any(), gomock.Cond(func(s *domain.Step) bool {
			return s.TaskID == "task_1" && s.Command == "ls /home/agent" && s.ExitCode != nil && *s.ExitCode == 0
		})).Return(1, nil),
		g.tx.EXPECT().UpdateTask(gomock.Any(), gomock.Cond(func(t *domain.Task) bool { return t.Steps == 1 })).Return(nil),
		g.tx.EXPECT().AppendMessage(gomock.Any(), gomock.Any()).Return(4, nil),
	)
	seq = append(seq, g.llm.EXPECT().Chat(gomock.Any(), "llama3",
		gomock.Cond(func(m []llm.Message) bool { return len(m) == 4 && m[3].Role == "tool" }),
		gomock.Any(), gomock.Any(),
	).Return(llm.Message{Content: "Listed the home directory.\nVERDICT: SUCCESS - a and b exist"}, nil))
	seq = append(seq, g.txMessage(gmFinalMsg())...)
	seq = append(seq, g.inTx(),
		g.tx.EXPECT().GetTask(gomock.Any(), "task_1").Return(gmClaimed(), nil),
		g.tx.EXPECT().UpdateTask(gomock.Any(), gomock.Cond(func(t *domain.Task) bool {
			return t.State == domain.TaskSucceeded && t.Verdict == "SUCCESS" && t.LeaseOwner == "" && !t.FinishedAt.IsZero()
		})).Return(nil),
	)
	gomock.InOrder(seq...)

	claimed, err := r.RunOnce(context.Background())
	if err != nil || !claimed {
		t.Fatalf("RunOnce = (%v, %v), want (true, nil)", claimed, err)
	}
}

// TestGMLeaseLossWritesNothingFinal: when the heartbeat finds the lease taken over, the run stops with no final
// write, and the running command is not cancelled (the new owner re-attaches to it).
func TestGMLeaseLossWritesNothingFinal(t *testing.T) {
	const ttl = 90 * time.Millisecond // heartbeat every 30 ms
	g, r := gmNewRun(t, func(c *Config) { c.LeaseTTL = ttl })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gomock.InOrder(
		g.st.EXPECT().ClaimTasks(gomock.Any(), "gm-worker", gomock.Any(), ttl, 1).Return([]*domain.Task{gmClaimed()}, nil),
		g.st.EXPECT().GetAgent(gomock.Any(), "agent_1").Return(gmAgentRow(), nil),
		g.vm.EXPECT().GetVM(gomock.Any(), "vm_1").Return(gmRunningVM(), nil),
		g.st.EXPECT().ListMessages(gomock.Any(), "task_1").Return(nil, nil),
	)
	seq := []any{}
	seq = append(seq, g.txMessage(gmSystemMsg())...)
	seq = append(seq, g.txMessage(gmUserMsg())...)
	seq = append(seq, g.llm.EXPECT().Chat(gomock.Any(), "llama3", gomock.Any(), gomock.Any(), gomock.Any()).
		Return(llm.Message{ToolCalls: []llm.ToolCall{{Name: "bash", Arguments: map[string]any{"command": "ls /home/agent"}}}}, nil))
	seq = append(seq, g.txMessage(gmBashCallMsg())...)
	seq = append(seq,
		g.vm.EXPECT().StartJob(gomock.Any(), gomock.Any()).Return(&vmdv1.JobOutput{Job: &vmdv1.Job{Id: "job_1", State: vmdv1.JobState_JOB_STATE_RUNNING}}, nil),
		// The job never finishes: only the lease loss (cancelling ctx) ends this poll.
		g.vm.EXPECT().PollJob(gomock.Any(), gomock.Any()).DoAndReturn(
			func(c context.Context, _ *vmdv1.PollJobRequest) (*vmdv1.JobOutput, error) {
				<-c.Done()
				return nil, c.Err()
			}),
	)
	gomock.InOrder(seq...)
	// The heartbeat sees the lease taken by someone else. Exactly one extend is expected, and no CancelJob:
	// the command keeps running for the new owner.
	g.st.EXPECT().ExtendLease(gomock.Any(), "task_1", "gm-worker", gomock.Any(), ttl).Return(errs.ErrConflict).Times(1)

	claimed, err := r.RunOnce(ctx)
	if err != nil || !claimed {
		t.Fatalf("RunOnce = (%v, %v), want (true, nil)", claimed, err)
	}
	if ctx.Err() != nil {
		t.Fatal("the run was only stopped by the test deadline, not by the lease loss")
	}
}

// TestGMTransientGetVMRequeues: an Unavailable VM is a transient failure: the task goes back to queued with one
// attempt counted, the lease is released, and the call is not an error.
func TestGMTransientGetVMRequeues(t *testing.T) {
	g, r := gmNewRun(t)
	gomock.InOrder(
		g.st.EXPECT().ClaimTasks(gomock.Any(), "gm-worker", gomock.Any(), gomock.Any(), 1).Return([]*domain.Task{gmClaimed()}, nil),
		g.st.EXPECT().GetAgent(gomock.Any(), "agent_1").Return(gmAgentRow(), nil),
		g.vm.EXPECT().GetVM(gomock.Any(), "vm_1").Return(nil, errs.Unavailable("vmd is restarting")),
		g.inTx(),
		g.tx.EXPECT().GetTask(gomock.Any(), "task_1").Return(gmClaimed(), nil),
		g.tx.EXPECT().UpdateTask(gomock.Any(), gomock.Cond(func(t *domain.Task) bool {
			return t.State == domain.TaskQueued && t.Attempts == 1 && t.LeaseOwner == "" && t.FinishedAt.IsZero()
		})).Return(nil),
	)

	claimed, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce returned an error for a transient failure: %v", err)
	}
	if !claimed {
		t.Fatal("RunOnce did not report the claimed task")
	}
}

// TestGMWorkerSurvivesClaimErrors: a failed ClaimTasks is logged, the worker keeps polling, and Run returns
// normally once its context ends.
func TestGMWorkerSurvivesClaimErrors(t *testing.T) {
	ctrl := gomock.NewController(t)
	st := storemock.NewMockStore(ctrl)
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := New(Config{Store: st, LLM: llmmock.NewMockClient(ctrl), VM: vmclientmock.NewMockVM(ctrl), Owner: "gm-worker",
		Workers: 1, PollInterval: time.Millisecond, CancelPoll: time.Hour,
		Log: slog.New(slog.NewTextHandler(&logs, nil)), Now: func() time.Time { return gmRunNow }})

	calls := 0
	st.EXPECT().ClaimTasks(gomock.Any(), "gm-worker", gomock.Any(), gomock.Any(), 1).DoAndReturn(
		func(context.Context, string, time.Time, time.Duration, int) ([]*domain.Task, error) {
			calls++
			switch calls {
			case 1:
				return nil, errors.New("pq: connection refused")
			case 2:
				return nil, nil // idle
			}
			cancel()
			return nil, nil
		}).Times(3)

	r.Run(ctx) // must return: a panic or a worker that exited early would fail here
	if calls != 3 {
		t.Fatalf("ClaimTasks called %d times, want 3 (the worker must keep polling after an error)", calls)
	}
	if !strings.Contains(logs.String(), "claim tasks") {
		t.Fatalf("the claim failure was not logged; log output:\n%s", logs.String())
	}
}
