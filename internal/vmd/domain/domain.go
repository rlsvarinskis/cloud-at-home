// Package domain holds vmd's internal model. It is deliberately separate from the protobuf
// messages: the wire format can evolve independently of what is stored.
package domain

import "time"

type PowerState string

const (
	PowerRunning PowerState = "running"
	PowerStopped PowerState = "stopped"
	PowerDeleted PowerState = "deleted" // being torn down; the row disappears when done
)

type VMState string

const (
	VMPending  VMState = "pending"
	VMStopped  VMState = "stopped"
	VMStarting VMState = "starting"
	VMRunning  VMState = "running"
	VMStopping VMState = "stopping"
	VMError    VMState = "error"
	VMDeleting VMState = "deleting"
)

// VM is a desired-state resource: API calls change Desired (and spec fields), the reconciler
// moves State towards it and records why in StateReason.
type VM struct {
	ID, ProjectID, Name, NodeID string
	CPUs                        int
	MemoryMB                    int64
	DiskIDs                     []string
	Desired                     PowerState
	State                       VMState
	StateReason                 string
	Holds                       []string // sorted, unique
	Labels                      map[string]string
	Generation                  int64 // bumped on spec changes
	Version                     int64 // row version for compare-and-swap; managed by the store
	StopForce                   bool  // skip graceful shutdown on the next stop
	DeleteDisks                 bool  // delete the disks together with the VM
	IdempotencyKey              string

	// Reconciliation bookkeeping. NextReconcile zero means "nothing to do".
	NextReconcile time.Time
	LeaseOwner    string
	LeaseUntil    time.Time
	Failures      int // consecutive failed reconcile attempts against the node (drives retry backoff)
	Crashes       int // unexpected VM exits; decays while the VM stays healthy, so flapping VMs back off

	CreatedAt, UpdatedAt time.Time
}

type DiskState string

const (
	DiskCreating DiskState = "creating"
	DiskReady    DiskState = "ready"
	DiskError    DiskState = "error"
	DiskDeleting DiskState = "deleting"
)

type Disk struct {
	ID, ProjectID, Name, NodeID string
	SizeGB                      int
	ImageID                     string
	State                       DiskState
	StateReason                 string
	AttachedVMID                string
	Version                     int64
	CreatedAt, UpdatedAt        time.Time
}

type ImageState string

const (
	ImageCreating ImageState = "creating"
	ImageReady    ImageState = "ready"
	ImageError    ImageState = "error"
	ImageDeleting ImageState = "deleting"
)

type Image struct {
	ID, ProjectID, Name, NodeID string // ProjectID "" = shared by every project
	State                       ImageState
	StateReason                 string
	SizeBytes                   int64
	SourceDiskID                string
	Path                        string // only for RegisterImage; the node adopts this file
	Version                     int64
	CreatedAt, UpdatedAt        time.Time
}

type JobState string

const (
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
	JobTimedOut  JobState = "timed_out"
	JobLost      JobState = "lost"
)

func (s JobState) Final() bool { return s != JobRunning }

type Job struct {
	ID, VMID, ProjectID, Command, Workdir string
	State                                 JobState
	ExitCode                              *int
	TimeoutSeconds                        int
	KilledReason                          string
	IdempotencyKey                        string
	Version                               int64
	StartedAt, FinishedAt                 time.Time
}

type NodeState string

const (
	NodeReady    NodeState = "ready"
	NodeDraining NodeState = "draining" // keeps running its VMs, takes no new ones
	NodeDown     NodeState = "down"
)

type Node struct {
	ID, Address string
	// State is the node's HEALTH as observed (ready or down). Drain is the operator's decision to take no new
	// VM starts on it; it is kept apart so an outage can never erase it. Use Effective for scheduling.
	State                         NodeState
	Drain                         bool
	CPUs                          int
	MemoryMB                      int64
	DiskBytesTotal, DiskBytesFree int64
	LastHeartbeat                 time.Time
}

// Effective is what scheduling should act on: down beats draining beats ready. (A legacy row whose State is
// "draining" counts as drained.)
func (n *Node) Effective() NodeState {
	switch {
	case n.State == NodeDown:
		return NodeDown
	case n.Drain || n.State == NodeDraining:
		return NodeDraining
	}
	return NodeReady
}

// Usage is what a project currently consumes (VMs that are, or should be, running plus all disks).
type Usage struct {
	VMs      int
	CPUs     int
	MemoryMB int64
	DiskGB   int
}

type VMFilter struct {
	ProjectID string
	Labels    map[string]string
	After     string // keyset pagination: return IDs greater than this
	Limit     int
}
