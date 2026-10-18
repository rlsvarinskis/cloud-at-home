// Package domain holds agentd's internal model (separate from the protobuf wire types).
package domain

import "time"

type AgentState string

const (
	AgentActive   AgentState = "active"
	AgentOrphaned AgentState = "orphaned" // its VM was force-deleted
	AgentDeleting AgentState = "deleting"
)

// Agent is always tied to exactly one VM (VMID is mandatory); many agents may share a VM.
type Agent struct {
	ID, ProjectID, Name, VMID string
	Model, SystemPrompt       string
	Workdir                   string // directory inside the VM this agent works in
	State                     AgentState
	Labels                    map[string]string
	IdempotencyKey            string
	Version                   int64 // row version for compare-and-swap; managed by the store
	CreatedAt, UpdatedAt      time.Time
}

type TaskState string

const (
	TaskQueued    TaskState = "queued"
	TaskRunning   TaskState = "running"
	TaskSucceeded TaskState = "succeeded"
	TaskFailed    TaskState = "failed"
	TaskGaveUp    TaskState = "gave_up"
	TaskCancelled TaskState = "cancelled"
	TaskError     TaskState = "error"
)

func (s TaskState) Final() bool { return s != TaskQueued && s != TaskRunning }

// Task is one unit of work for an agent. Workers (any agentd replica) claim tasks with a lease and keep
// extending it; if a worker dies the lease expires and another worker resumes from the stored messages.
type Task struct {
	ID, AgentID, ProjectID string
	Prompt, CheckCommand   string
	MaxSteps               int
	State                  TaskState
	Verdict, Result        string
	Steps, Attempts        int
	CancelRequested        bool
	IdempotencyKey         string
	LeaseOwner             string
	LeaseUntil             time.Time
	Version                int64
	CreatedAt              time.Time
	StartedAt, FinishedAt  time.Time
}

// Step is one command the model ran (for display and audit).
type Step struct {
	TaskID   string
	Seq      int // 1-based, dense per task
	Command  string
	Output   string
	ExitCode *int
	At       time.Time
}

// Message is one element of the stored conversation (JSON of llm.Message); replaying them resumes a task.
type Message struct {
	TaskID string
	Seq    int // 1-based, dense per task
	JSON   []byte
	At     time.Time
}
