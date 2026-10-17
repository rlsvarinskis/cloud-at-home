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
	Failures      int // consecutive failed reconcile attempts (drives backoff)

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
	ID, Address                   string
	State                         NodeState
	CPUs                          int
	MemoryMB                      int64
	DiskBytesTotal, DiskBytesFree int64
	LastHeartbeat                 time.Time
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
