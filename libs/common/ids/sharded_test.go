package ids

import (
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestShardedRoundTripAllShards(t *testing.T) {
	for shard := 0; shard < 256; shard++ {
		id := NewSharded("vm", shard)
		if !strings.HasPrefix(id, "vm_") {
			t.Fatalf("shard %d: id %q lost its prefix", shard, id)
		}
		got, ok := ShardOf(id)
		if !ok || got != shard {
			t.Fatalf("ShardOf(%q) = %d, %v; want %d, true", id, got, ok, shard)
		}
	}
}

func TestShardedSuffixFormat(t *testing.T) {
	if id := NewSharded("vm", 10); !strings.HasSuffix(id, "-s0a") {
		t.Fatalf("NewSharded(vm, 10) = %q, want suffix -s0a", id)
	}
	if id := NewSharded("vm", 0); !strings.HasSuffix(id, "-s00") {
		t.Fatalf("NewSharded(vm, 0) = %q, want suffix -s00", id)
	}
	if id := NewSharded("vm", 255); !strings.HasSuffix(id, "-sff") {
		t.Fatalf("NewSharded(vm, 255) = %q, want suffix -sff", id)
	}
}

func TestNewShardedPanicsOutOfRange(t *testing.T) {
	for _, shard := range []int{-1, 256, 1000} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewSharded(vm, %d) did not panic", shard)
				}
			}()
			NewSharded("vm", shard)
		}()
	}
}

func TestShardOfMalformed(t *testing.T) {
	for _, id := range []string{
		"",
		"vm_01hx",        // no suffix
		"vm_x-s0",        // suffix too short
		"-s0a",           // nothing before the suffix
		"vm_x-S0a",       // upper-case marker
		"vm_x-s0A",       // upper-case hex
		"vm_x-sG0",       // not hex
		"vm_x_s0a",       // wrong separator
		"vm_x-x0a",       // wrong marker
		"vm_x-s0a0",      // suffix not at the end
		"vm_01hx0a",      // plain hex tail, no marker
		"vm_01hx-s0a \n", // trailing junk
	} {
		if shard, ok := ShardOf(id); ok {
			t.Errorf("ShardOf(%q) = %d, true; want ok=false", id, shard)
		}
	}
	// A plain New ID has no shard.
	if shard, ok := ShardOf(New("vm")); ok {
		t.Errorf("ShardOf(New(vm)) = %d, true; want ok=false", shard)
	}
}

func TestShardedOrderingPreserved(t *testing.T) {
	// Sequentially created sharded IDs must sort by creation time, whatever shard they carry.
	const n = 1000
	var list []string
	for i := 0; i < n; i++ {
		list = append(list, NewSharded("img", rand.IntN(256)))
	}
	if !sort.StringsAreSorted(list) {
		t.Fatal("sharded IDs created in sequence do not sort in creation order")
	}
}

func TestShardedMatchesIDRegex(t *testing.T) {
	re := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	for _, prefix := range []string{"vm", "img", "disk", "job_long_prefix"} {
		for _, shard := range []int{0, 7, 255} {
			if id := NewSharded(prefix, shard); !re.MatchString(id) {
				t.Errorf("NewSharded(%q, %d) = %q does not match %s", prefix, shard, id, re)
			}
		}
	}
}

func TestShardedUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := NewSharded("vm", i%256)
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
