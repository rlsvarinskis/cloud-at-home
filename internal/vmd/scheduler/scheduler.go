// Package scheduler chooses the node a VM is placed on and does the capacity arithmetic behind
// that choice. Everything here is a pure function over values: the caller gathers node state
// and current allocations from the store and passes them in, so no I/O happens in this package.
package scheduler

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/vmd/domain"
)

// NodeInfo is a node's capacity and what is already allocated on it.
type NodeInfo struct {
	ID    string
	State domain.NodeState
	// CPUs is the number of physical threads on the node.
	CPUs int
	// MemoryMB is the physical RAM on the node.
	MemoryMB int64
	// DiskBytesFree is the free space available for new disks.
	DiskBytesFree int64
	// AllocatedCPUs is the sum of vCPUs of VMs that should be running on this node.
	AllocatedCPUs int
	// AllocatedMemoryMB is the sum of memory of VMs that should be running on this node.
	AllocatedMemoryMB int64
}

// Config controls how much physical capacity may be promised to VMs.
type Config struct {
	// CPUOvercommit is the number of vCPUs allowed per physical thread. Values <= 0 mean 1.0.
	CPUOvercommit float64
	// MemoryOvercommit multiplies usable RAM. Values <= 0 mean 1.0.
	MemoryOvercommit float64
	// ReserveMemoryMB is RAM kept back for the host itself. It is subtracted before overcommit.
	ReserveMemoryMB int64
}

// Request describes the VM to place.
type Request struct {
	CPUs      int
	MemoryMB  int64
	DiskBytes int64
	// NodeID, when non-empty, means the VM must go on exactly this node.
	NodeID string
	// Exclude lists node IDs that must not be used.
	Exclude []string
}

// Available is what can still be allocated on a node.
type Available struct {
	CPUs     int
	MemoryMB int64
}

// overcommit returns f, or 1.0 when f is unset or not a positive number.
func overcommit(f float64) float64 {
	if !(f > 0) {
		return 1
	}
	return f
}

// cpuCapacity is the total vCPUs a node may hold after overcommit.
func cpuCapacity(n NodeInfo, c Config) int {
	v := math.Floor(float64(n.CPUs) * overcommit(c.CPUOvercommit))
	if v < 0 {
		return 0
	}
	return int(v)
}

// memCapacity is the total memory in MB a node may hold after reserve and overcommit.
func memCapacity(n NodeInfo, c Config) int64 {
	base := max(n.MemoryMB-c.ReserveMemoryMB, 0)
	return int64(math.Floor(float64(base) * overcommit(c.MemoryOvercommit)))
}

// Avail returns what can still be allocated on n: floor(CPUs*CPUOvercommit)-AllocatedCPUs and
// floor((MemoryMB-ReserveMemoryMB)*MemoryOvercommit)-AllocatedMemoryMB, never negative.
func Avail(n NodeInfo, c Config) Available {
	return Available{
		CPUs:     max(cpuCapacity(n, c)-n.AllocatedCPUs, 0),
		MemoryMB: max(memCapacity(n, c)-n.AllocatedMemoryMB, 0),
	}
}

// Fits reports whether the request fits on n. When it does not, reason says why.
// Fits looks only at the node: NodeID and Exclude in r are honoured by Pick, not here.
func Fits(n NodeInfo, r Request, c Config) (ok bool, reason string) {
	switch n.State {
	case domain.NodeReady:
	case domain.NodeDraining:
		return false, "node is draining"
	case domain.NodeDown:
		return false, "node is down"
	default:
		return false, fmt.Sprintf("node is not ready (state %q)", n.State)
	}
	a := Avail(n, c)
	if r.CPUs > a.CPUs {
		return false, fmt.Sprintf("not enough CPUs: need %d, %d available", r.CPUs, a.CPUs)
	}
	if r.MemoryMB > a.MemoryMB {
		return false, fmt.Sprintf("not enough memory: need %d MB, %d MB available", r.MemoryMB, a.MemoryMB)
	}
	if r.DiskBytes > n.DiskBytesFree {
		return false, fmt.Sprintf("not enough disk: need %d bytes, %d bytes free", r.DiskBytes, n.DiskBytesFree)
	}
	return true, ""
}

// Pick chooses a node for r from nodes. Only NodeReady nodes are candidates, and if r.NodeID is
// set only that node is considered. Among the nodes that fit, Pick takes the one with the highest
// headroom score after placement (see headroom), breaking ties by the lowest ID. When nothing fits
// the returned error wraps errs.ErrResourceExhausted and lists why each candidate was rejected.
func Pick(nodes []NodeInfo, r Request, c Config) (NodeInfo, error) {
	if err := validate(r); err != nil {
		return NodeInfo{}, err
	}

	anyReady := false
	pinnedFound := r.NodeID == ""
	for _, n := range nodes {
		if n.State == domain.NodeReady {
			anyReady = true
		}
		if n.ID == r.NodeID {
			pinnedFound = true
		}
	}
	if !pinnedFound {
		return NodeInfo{}, errs.Exhausted("node %q not found", r.NodeID)
	}

	excluded := make(map[string]bool, len(r.Exclude))
	for _, id := range r.Exclude {
		excluded[id] = true
	}

	var (
		best      NodeInfo
		bestScore float64
		found     bool
		rejects   []string
	)
	for _, n := range nodes {
		if r.NodeID != "" && n.ID != r.NodeID {
			continue
		}
		if excluded[n.ID] {
			rejects = append(rejects, fmt.Sprintf("%s: excluded", n.ID))
			continue
		}
		if ok, reason := Fits(n, r, c); !ok {
			rejects = append(rejects, fmt.Sprintf("%s: %s", n.ID, reason))
			continue
		}
		s := headroom(n, r, c)
		if !found || s > bestScore || (s == bestScore && n.ID < best.ID) {
			best, bestScore, found = n, s, true
		}
	}
	if found {
		return best, nil
	}
	sort.Strings(rejects)
	if !anyReady {
		if len(rejects) == 0 {
			return NodeInfo{}, errs.Exhausted("no ready nodes")
		}
		return NodeInfo{}, errs.Exhausted("no ready nodes (%s)", strings.Join(rejects, "; "))
	}
	return NodeInfo{}, errs.Exhausted("no node can take the VM (%s)", strings.Join(rejects, "; "))
}

// headroom is the score used to spread load: the smaller of the remaining CPU fraction and the
// remaining memory fraction of the node's overcommitted capacity, measured after placing r.
// A higher score means more room is left. It assumes r fits on n.
func headroom(n NodeInfo, r Request, c Config) float64 {
	a := Avail(n, c)
	return min(
		ratio(a.CPUs-r.CPUs, cpuCapacity(n, c)),
		ratio(float64(a.MemoryMB-r.MemoryMB), float64(memCapacity(n, c))),
	)
}

// ratio divides remaining by total, returning 0 when there is no capacity at all.
func ratio[T int | float64](remaining T, total T) float64 {
	if total <= 0 {
		return 0
	}
	return float64(remaining) / float64(total)
}

// validate rejects requests that can never be placed, whatever the cluster looks like.
func validate(r Request) error {
	switch {
	case r.CPUs < 1:
		return errs.Invalid("cpus must be at least 1, got %d", r.CPUs)
	case r.MemoryMB < 1:
		return errs.Invalid("memory must be at least 1 MB, got %d", r.MemoryMB)
	case r.DiskBytes < 0:
		return errs.Invalid("disk bytes must not be negative, got %d", r.DiskBytes)
	}
	return nil
}
