package scheduler

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/vmd/domain"
)

func TestAvail(t *testing.T) {
	tests := []struct {
		name string
		node NodeInfo
		cfg  Config
		want Available
	}{
		{
			name: "no overcommit by default",
			node: NodeInfo{CPUs: 8, MemoryMB: 16384},
			cfg:  Config{},
			want: Available{CPUs: 8, MemoryMB: 16384},
		},
		{
			name: "non-positive overcommit means one",
			node: NodeInfo{CPUs: 8, MemoryMB: 16384},
			cfg:  Config{CPUOvercommit: 0, MemoryOvercommit: -2},
			want: Available{CPUs: 8, MemoryMB: 16384},
		},
		{
			name: "cpu overcommit multiplies threads",
			node: NodeInfo{CPUs: 8},
			cfg:  Config{CPUOvercommit: 2.5},
			want: Available{CPUs: 20},
		},
		{
			name: "cpu overcommit floors fractional vCPUs",
			node: NodeInfo{CPUs: 3},
			cfg:  Config{CPUOvercommit: 1.5},
			want: Available{CPUs: 4},
		},
		{
			name: "allocations are subtracted",
			node: NodeInfo{CPUs: 8, MemoryMB: 16384, AllocatedCPUs: 5, AllocatedMemoryMB: 4096},
			cfg:  Config{CPUOvercommit: 2},
			want: Available{CPUs: 11, MemoryMB: 12288},
		},
		{
			name: "reserve is subtracted before memory overcommit",
			node: NodeInfo{MemoryMB: 16384},
			cfg:  Config{ReserveMemoryMB: 2048, MemoryOvercommit: 1.5},
			want: Available{MemoryMB: 21504},
		},
		{
			name: "reserve without overcommit",
			node: NodeInfo{MemoryMB: 16384},
			cfg:  Config{ReserveMemoryMB: 2048},
			want: Available{MemoryMB: 14336},
		},
		{
			name: "over-allocated cpus clamp to zero",
			node: NodeInfo{CPUs: 4, AllocatedCPUs: 6},
			cfg:  Config{},
			want: Available{},
		},
		{
			name: "over-allocated memory clamps to zero",
			node: NodeInfo{MemoryMB: 4096, AllocatedMemoryMB: 8192},
			cfg:  Config{},
			want: Available{},
		},
		{
			name: "reserve larger than ram clamps to zero",
			node: NodeInfo{MemoryMB: 1024},
			cfg:  Config{ReserveMemoryMB: 4096},
			want: Available{},
		},
		{
			name: "negative node sizes clamp to zero",
			node: NodeInfo{CPUs: -4, MemoryMB: -4096},
			cfg:  Config{},
			want: Available{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Avail(tc.node, tc.cfg); got != tc.want {
				t.Fatalf("Avail() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFits(t *testing.T) {
	base := NodeInfo{
		ID:                "n",
		State:             domain.NodeReady,
		CPUs:              8,
		MemoryMB:          8192,
		DiskBytesFree:     100,
		AllocatedCPUs:     4,
		AllocatedMemoryMB: 0,
	}
	tests := []struct {
		name       string
		mutate     func(*NodeInfo)
		req        Request
		wantOK     bool
		wantReason string
	}{
		{
			name:   "fits exactly at every limit",
			req:    Request{CPUs: 4, MemoryMB: 8192, DiskBytes: 100},
			wantOK: true,
		},
		{
			name:   "zero disk request always fits disk",
			req:    Request{CPUs: 1, MemoryMB: 1024},
			wantOK: true,
		},
		{
			name:       "draining node",
			mutate:     func(n *NodeInfo) { n.State = domain.NodeDraining },
			req:        Request{CPUs: 1, MemoryMB: 1},
			wantReason: "node is draining",
		},
		{
			name:       "down node",
			mutate:     func(n *NodeInfo) { n.State = domain.NodeDown },
			req:        Request{CPUs: 1, MemoryMB: 1},
			wantReason: "node is down",
		},
		{
			name:       "unknown state is not ready",
			mutate:     func(n *NodeInfo) { n.State = "bogus" },
			req:        Request{CPUs: 1, MemoryMB: 1},
			wantReason: `node is not ready (state "bogus")`,
		},
		{
			name:       "state is reported before capacity",
			mutate:     func(n *NodeInfo) { n.State = domain.NodeDraining },
			req:        Request{CPUs: 999, MemoryMB: 999999, DiskBytes: 999999},
			wantReason: "node is draining",
		},
		{
			name:       "not enough cpus",
			req:        Request{CPUs: 8, MemoryMB: 1024},
			wantReason: "not enough CPUs: need 8, 4 available",
		},
		{
			name:       "not enough memory",
			req:        Request{CPUs: 1, MemoryMB: 9000},
			wantReason: "not enough memory: need 9000 MB, 8192 MB available",
		},
		{
			name:       "not enough disk",
			req:        Request{CPUs: 1, MemoryMB: 1024, DiskBytes: 101},
			wantReason: "not enough disk: need 101 bytes, 100 bytes free",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := base
			if tc.mutate != nil {
				tc.mutate(&n)
			}
			ok, reason := Fits(n, tc.req, Config{})
			if ok != tc.wantOK || reason != tc.wantReason {
				t.Fatalf("Fits() = (%v, %q), want (%v, %q)", ok, reason, tc.wantOK, tc.wantReason)
			}
		})
	}
}

func TestFitsUsesOvercommit(t *testing.T) {
	n := NodeInfo{ID: "n", State: domain.NodeReady, CPUs: 2, MemoryMB: 4096}
	req := Request{CPUs: 4, MemoryMB: 1024}
	if ok, _ := Fits(n, req, Config{}); ok {
		t.Fatal("4 vCPUs fit on 2 threads without overcommit")
	}
	if ok, reason := Fits(n, req, Config{CPUOvercommit: 2}); !ok {
		t.Fatalf("4 vCPUs should fit with 2x overcommit, got %q", reason)
	}
}

// pickCase is one table row for Pick. Exactly one of want (the chosen ID) or wantErr is used.
type pickCase struct {
	name    string
	nodes   []NodeInfo
	req     Request
	cfg     Config
	want    string
	wantErr error
	wantMsg []string
}

func runPickCases(t *testing.T, tests []pickCase) {
	t.Helper()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Pick(tc.nodes, tc.req, tc.cfg)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Pick() error = %v, want wrapping %v", err, tc.wantErr)
				}
				for _, m := range tc.wantMsg {
					if !strings.Contains(err.Error(), m) {
						t.Errorf("error %q does not contain %q", err.Error(), m)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Pick() unexpected error: %v", err)
			}
			if got.ID != tc.want {
				t.Fatalf("Pick() chose %q, want %q", got.ID, tc.want)
			}
		})
	}
}

func TestPick(t *testing.T) {
	ready := func(id string, cpus int, mem int64) NodeInfo {
		return NodeInfo{ID: id, State: domain.NodeReady, CPUs: cpus, MemoryMB: mem, DiskBytesFree: 1 << 40}
	}
	small := Request{CPUs: 1, MemoryMB: 512}

	runPickCases(t, []pickCase{
		{
			name:  "only ready nodes are candidates",
			nodes: []NodeInfo{{ID: "a", State: domain.NodeDraining, CPUs: 64, MemoryMB: 1 << 20}, {ID: "b", State: domain.NodeDown, CPUs: 64, MemoryMB: 1 << 20}, ready("c", 2, 1024)},
			req:   small,
			want:  "c",
		},
		{
			name:    "all draining",
			nodes:   []NodeInfo{{ID: "a", State: domain.NodeDraining, CPUs: 8, MemoryMB: 8192}},
			req:     small,
			wantErr: errs.ErrResourceExhausted,
			wantMsg: []string{"no ready nodes (a: node is draining)"},
		},
		{
			name:    "no nodes at all",
			nodes:   nil,
			req:     small,
			wantErr: errs.ErrResourceExhausted,
			wantMsg: []string{"no ready nodes"},
		},
		{
			name:    "no ready nodes",
			nodes:   []NodeInfo{{ID: "a", State: domain.NodeDown}, {ID: "b", State: domain.NodeDraining}},
			req:     small,
			wantErr: errs.ErrResourceExhausted,
			wantMsg: []string{"no ready nodes"},
		},
		{
			name:  "pinned node is used even when another node is better",
			nodes: []NodeInfo{ready("a", 64, 1<<20), ready("b", 4, 4096)},
			req:   Request{CPUs: 1, MemoryMB: 512, NodeID: "b"},
			want:  "b",
		},
		{
			name:    "pinned node is draining",
			nodes:   []NodeInfo{ready("a", 64, 1<<20), {ID: "b", State: domain.NodeDraining, CPUs: 64, MemoryMB: 1 << 20}},
			req:     Request{CPUs: 1, MemoryMB: 512, NodeID: "b"},
			wantErr: errs.ErrResourceExhausted,
			wantMsg: []string{"b: node is draining"},
		},
		{
			name:    "pinned node is down",
			nodes:   []NodeInfo{ready("a", 64, 1<<20), {ID: "b", State: domain.NodeDown, CPUs: 64, MemoryMB: 1 << 20}},
			req:     Request{CPUs: 1, MemoryMB: 512, NodeID: "b"},
			wantErr: errs.ErrResourceExhausted,
			wantMsg: []string{"b: node is down"},
		},
		{
			name:    "pinned node not found",
			nodes:   []NodeInfo{ready("a", 64, 1<<20)},
			req:     Request{CPUs: 1, MemoryMB: 512, NodeID: "zzz"},
			wantErr: errs.ErrResourceExhausted,
			wantMsg: []string{`node "zzz" not found`},
		},
		{
			name:    "pinned node does not fit",
			nodes:   []NodeInfo{ready("a", 64, 1<<20), ready("b", 2, 4096)},
			req:     Request{CPUs: 8, MemoryMB: 512, NodeID: "b"},
			wantErr: errs.ErrResourceExhausted,
			wantMsg: []string{"b: not enough CPUs: need 8, 2 available"},
		},
		{
			name:    "pinned node is also excluded",
			nodes:   []NodeInfo{ready("a", 64, 1<<20), ready("b", 64, 1<<20)},
			req:     Request{CPUs: 1, MemoryMB: 512, NodeID: "b", Exclude: []string{"b"}},
			wantErr: errs.ErrResourceExhausted,
			wantMsg: []string{"b: excluded"},
		},
		{
			name:  "excluded node is skipped",
			nodes: []NodeInfo{ready("a", 64, 1<<20), ready("b", 8, 8192)},
			req:   Request{CPUs: 1, MemoryMB: 512, Exclude: []string{"a"}},
			want:  "b",
		},
		{
			name:    "every node excluded",
			nodes:   []NodeInfo{ready("a", 8, 8192), ready("b", 8, 8192)},
			req:     Request{CPUs: 1, MemoryMB: 512, Exclude: []string{"a", "b"}},
			wantErr: errs.ErrResourceExhausted,
			wantMsg: []string{"a: excluded", "b: excluded"},
		},
		{
			name:  "exclude ids that do not exist are harmless",
			nodes: []NodeInfo{ready("a", 8, 8192)},
			req:   Request{CPUs: 1, MemoryMB: 512, Exclude: []string{"nope"}},
			want:  "a",
		},
		{
			name:    "error lists why each candidate was rejected",
			nodes:   []NodeInfo{ready("a", 2, 8192), ready("b", 64, 1024), {ID: "c", State: domain.NodeDown}},
			req:     Request{CPUs: 4, MemoryMB: 2048},
			wantErr: errs.ErrResourceExhausted,
			wantMsg: []string{
				"a: not enough CPUs: need 4, 2 available",
				"b: not enough memory: need 2048 MB, 1024 MB available",
				"c: node is down",
			},
		},
		{
			name:    "disk rejection is reported",
			nodes:   []NodeInfo{{ID: "a", State: domain.NodeReady, CPUs: 8, MemoryMB: 8192, DiskBytesFree: 10}},
			req:     Request{CPUs: 1, MemoryMB: 512, DiskBytes: 11},
			wantErr: errs.ErrResourceExhausted,
			wantMsg: []string{"a: not enough disk: need 11 bytes, 10 bytes free"},
		},
		{
			name:  "overcommit lets an oversubscribed request land",
			nodes: []NodeInfo{ready("a", 4, 8192)},
			req:   Request{CPUs: 8, MemoryMB: 512},
			cfg:   Config{CPUOvercommit: 2},
			want:  "a",
		},
		{
			name:    "allocations count against the node",
			nodes:   []NodeInfo{{ID: "a", State: domain.NodeReady, CPUs: 8, MemoryMB: 8192, AllocatedMemoryMB: 8000}},
			req:     Request{CPUs: 1, MemoryMB: 512},
			wantErr: errs.ErrResourceExhausted,
			wantMsg: []string{"a: not enough memory: need 512 MB, 192 MB available"},
		},
		{
			name:  "spreads by cpu headroom",
			nodes: []NodeInfo{{ID: "a", State: domain.NodeReady, CPUs: 8, MemoryMB: 16384}, {ID: "b", State: domain.NodeReady, CPUs: 8, MemoryMB: 16384, AllocatedCPUs: 4}},
			req:   Request{CPUs: 2, MemoryMB: 1024},
			want:  "a", // a: 6/8 free after placement, b: 2/8
		},
		{
			name: "score is the minimum of cpu and memory headroom",
			nodes: []NodeInfo{
				// cpu headroom 14/16 = 0.875 but memory headroom 1024/8192 = 0.125
				{ID: "a", State: domain.NodeReady, CPUs: 16, MemoryMB: 8192, AllocatedMemoryMB: 6144},
				// cpu headroom 2/8 = 0.25 and memory headroom 15360/16384 = 0.94
				{ID: "b", State: domain.NodeReady, CPUs: 8, MemoryMB: 16384, AllocatedCPUs: 4},
			},
			req:  Request{CPUs: 2, MemoryMB: 1024},
			want: "b",
		},
		{
			name: "ties break by lowest id regardless of order",
			nodes: []NodeInfo{
				ready("node-c", 8, 8192),
				ready("node-a", 8, 8192),
				ready("node-b", 8, 8192),
			},
			req:  small,
			want: "node-a",
		},
		{
			name:    "invalid cpus",
			nodes:   []NodeInfo{ready("a", 8, 8192)},
			req:     Request{CPUs: 0, MemoryMB: 512},
			wantErr: errs.ErrInvalidArgument,
			wantMsg: []string{"cpus must be at least 1"},
		},
		{
			name:    "invalid memory",
			nodes:   []NodeInfo{ready("a", 8, 8192)},
			req:     Request{CPUs: 1, MemoryMB: 0},
			wantErr: errs.ErrInvalidArgument,
			wantMsg: []string{"memory must be at least 1 MB"},
		},
		{
			name:    "negative disk",
			nodes:   []NodeInfo{ready("a", 8, 8192)},
			req:     Request{CPUs: 1, MemoryMB: 512, DiskBytes: -1},
			wantErr: errs.ErrInvalidArgument,
			wantMsg: []string{"disk bytes must not be negative"},
		},
		{
			name:    "invalid request is reported before missing nodes",
			nodes:   nil,
			req:     Request{CPUs: -1, MemoryMB: 512},
			wantErr: errs.ErrInvalidArgument,
		},
	})
}

func TestPickReturnsNodeUnchanged(t *testing.T) {
	n := NodeInfo{
		ID: "a", State: domain.NodeReady, CPUs: 8, MemoryMB: 8192,
		DiskBytesFree: 42, AllocatedCPUs: 1, AllocatedMemoryMB: 2,
	}
	got, err := Pick([]NodeInfo{n}, Request{CPUs: 1, MemoryMB: 1}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Fatalf("Pick() = %+v, want %+v", got, n)
	}
}

func TestPickIsDeterministicAcrossInputOrder(t *testing.T) {
	nodes := []NodeInfo{
		{ID: "e", State: domain.NodeReady, CPUs: 8, MemoryMB: 8192},
		{ID: "b", State: domain.NodeReady, CPUs: 8, MemoryMB: 8192},
		{ID: "d", State: domain.NodeReady, CPUs: 8, MemoryMB: 8192},
		{ID: "a", State: domain.NodeReady, CPUs: 8, MemoryMB: 8192},
		{ID: "c", State: domain.NodeReady, CPUs: 16, MemoryMB: 8192, AllocatedCPUs: 2},
		{ID: "f", State: domain.NodeDraining, CPUs: 64, MemoryMB: 1 << 20},
	}
	req := Request{CPUs: 2, MemoryMB: 1024}
	// a, b, d and e are identical, and c (16 threads, 2 allocated) scores the same after placement,
	// so five nodes tie and the lowest ID must win regardless of input order.
	const want = "a"

	rng := rand.New(rand.NewPCG(1, 2))
	shuffled := slices.Clone(nodes)
	for i := range 200 {
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got, err := Pick(shuffled, req, Config{})
		if err != nil {
			t.Fatalf("shuffle %d: %v", i, err)
		}
		if got.ID != want {
			t.Fatalf("shuffle %d: picked %q, want %q (order %v)", i, got.ID, want, ids(shuffled))
		}
	}
}

func TestPickTieBreakIsStableWhenShuffled(t *testing.T) {
	nodes := make([]NodeInfo, 0, 10)
	for i := range 10 {
		nodes = append(nodes, NodeInfo{ID: fmt.Sprintf("node-%02d", i), State: domain.NodeReady, CPUs: 8, MemoryMB: 8192})
	}
	rng := rand.New(rand.NewPCG(7, 9))
	for i := range 100 {
		rng.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })
		got, err := Pick(nodes, Request{CPUs: 1, MemoryMB: 1}, Config{})
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != "node-00" {
			t.Fatalf("shuffle %d: picked %q, want node-00", i, got.ID)
		}
	}
}

// TestPickPropertyNeverReturnsNonFittingNode checks invariants on random clusters: a returned node
// always passes Fits, respects pinning and exclusion, and has the highest headroom score among the
// eligible nodes; when Pick fails, no eligible node fits.
func TestPickPropertyNeverReturnsNonFittingNode(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 1337))
	states := []domain.NodeState{domain.NodeReady, domain.NodeReady, domain.NodeReady, domain.NodeDraining, domain.NodeDown, "bogus"}
	overcommits := []float64{0, 0.5, 1, 1.5, 2, 3.7}

	const iterations = 3000
	picked, failed := 0, 0
	for iter := range iterations {
		cfg := Config{
			CPUOvercommit:    overcommits[rng.IntN(len(overcommits))],
			MemoryOvercommit: overcommits[rng.IntN(len(overcommits))],
			ReserveMemoryMB:  rng.Int64N(4096),
		}

		nodes := make([]NodeInfo, 1+rng.IntN(8))
		for i := range nodes {
			nodes[i] = NodeInfo{
				ID:                fmt.Sprintf("node-%d", i),
				State:             states[rng.IntN(len(states))],
				CPUs:              rng.IntN(40) - 2,
				MemoryMB:          rng.Int64N(65536),
				DiskBytesFree:     rng.Int64N(1 << 30),
				AllocatedCPUs:     rng.IntN(40),
				AllocatedMemoryMB: rng.Int64N(65536),
			}
		}

		req := Request{
			CPUs:      1 + rng.IntN(8),
			MemoryMB:  1 + rng.Int64N(8192),
			DiskBytes: rng.Int64N(1 << 29),
		}
		if rng.IntN(4) == 0 {
			if rng.IntN(5) == 0 {
				req.NodeID = "missing"
			} else {
				req.NodeID = nodes[rng.IntN(len(nodes))].ID
			}
		}
		for _, n := range nodes {
			if rng.IntN(4) == 0 {
				req.Exclude = append(req.Exclude, n.ID)
			}
		}

		eligible := func(n NodeInfo) bool {
			if req.NodeID != "" && n.ID != req.NodeID {
				return false
			}
			return !slices.Contains(req.Exclude, n.ID)
		}

		got, err := Pick(nodes, req, cfg)
		if err == nil {
			picked++
			if ok, reason := Fits(got, req, cfg); !ok {
				t.Fatalf("iter %d: Pick returned %q which does not fit: %s", iter, got.ID, reason)
			}
			if !eligible(got) {
				t.Fatalf("iter %d: Pick returned ineligible node %q (pin %q, exclude %v)", iter, got.ID, req.NodeID, req.Exclude)
			}
			best := headroom(got, req, cfg)
			for _, n := range nodes {
				if !eligible(n) {
					continue
				}
				if ok, _ := Fits(n, req, cfg); ok && headroom(n, req, cfg) > best {
					t.Fatalf("iter %d: Pick chose %q (score %v) but %q scores %v", iter, got.ID, best, n.ID, headroom(n, req, cfg))
				}
			}
			continue
		}

		failed++
		if !errors.Is(err, errs.ErrResourceExhausted) {
			t.Fatalf("iter %d: error %v does not wrap ErrResourceExhausted", iter, err)
		}
		anyReady, pinExists := false, req.NodeID == ""
		for _, n := range nodes {
			anyReady = anyReady || n.State == domain.NodeReady
			pinExists = pinExists || n.ID == req.NodeID
		}
		for _, n := range nodes {
			if !eligible(n) {
				continue
			}
			if ok, _ := Fits(n, req, cfg); ok {
				t.Fatalf("iter %d: Pick failed but node %q fits (%v)", iter, n.ID, err)
			}
			// Pick lists every rejected candidate, unless it stopped early on a whole-request problem.
			if anyReady && pinExists && !strings.Contains(err.Error(), n.ID+": ") {
				t.Fatalf("iter %d: error %q does not mention candidate %q", iter, err, n.ID)
			}
		}
	}
	if picked == 0 || failed == 0 {
		t.Fatalf("property run was one-sided: %d picks, %d failures", picked, failed)
	}
}

func ids(nodes []NodeInfo) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}
