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
	// TaskSucceededUnverified is a success the model claimed on a task with no check_command, so nothing
	// verified it.
	TaskSucceededUnverified TaskState = "succeeded_unverified"
	TaskFailed              TaskState = "failed"
	TaskGaveUp              TaskState = "gave_up"
	TaskCancelled           TaskState = "cancelled"
	TaskError               TaskState = "error"
)

func (s TaskState) Final() bool { return s != TaskQueued && s != TaskRunning }

// Succeeded reports whether the state is a success, verified or not.
func (s TaskState) Succeeded() bool { return s == TaskSucceeded || s == TaskSucceededUnverified }

// MaxCallRecords is how many model-call records a task keeps (the most recent ones).
const MaxCallRecords = 50

// Verdict sources: who decided the state of a finished task.
const (
	VerdictFromCheck = "check"
	VerdictFromModel = "model"
)

// CallRecord describes one model call of a task, for progress reports.
type CallRecord struct {
	Index        int     `json:"index"`
	Seconds      float64 `json:"seconds"`
	PromptTokens int64   `json:"prompt_tokens"`
	GenTokens    int64   `json:"gen_tokens"`
	End          string  `json:"end"`
	Partial      bool    `json:"partial,omitempty"`
	Note         string  `json:"note,omitempty"`
}

// Task is one unit of work for an agent. Workers (any agentd replica) claim tasks with a lease and keep
// extending it; if a worker dies the lease expires and another worker resumes from the stored messages.
type Task struct {
	ID, AgentID, ProjectID string
	Prompt, CheckCommand   string
	MaxSteps               int
	CheckEachStep          bool // run CheckCommand after every batch of commands
	MaxMinutes             int  // 0 = no time limit
	MaxCallTokens          int  // per-reply token cap; 0 = no cap
	State                  TaskState
	Verdict, Result        string
	Reason                 string // why the task ended early or stopped, "" for an ordinary report
	VerdictSource          string // VerdictFromCheck, VerdictFromModel or ""
	Steps, Attempts        int
	CancelRequested        bool
	IdempotencyKey         string
	LeaseOwner             string
	LeaseUntil             time.Time
	Version                int64
	CreatedAt              time.Time
	StartedAt, FinishedAt  time.Time

	// Progress counters, kept by the worker that runs the task.
	PromptTokens, CompletionTokens int64
	ModelCalls, PartialCalls       int
	FirstTokenSeconds              float64
	FirstCommandSeconds            float64
	// LiveAnswer and LiveReasoning hold the streamed text of a call in progress; they are cleared when it ends.
	LiveAnswer, LiveReasoning string
	// Calls holds the most recent MaxCallRecords model calls, oldest first.
	Calls []CallRecord
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
