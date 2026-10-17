package ids

import (
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestNewIsPrefixedUniqueAndSortable(t *testing.T) {
	const n = 2000
	seen := make(map[string]bool, n)
	var list []string
	for i := 0; i < n; i++ {
		id := New("vm")
		if !strings.HasPrefix(id, "vm_") || id != strings.ToLower(id) {
			t.Fatalf("bad id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		list = append(list, id)
	}
	if !sort.StringsAreSorted(list) {
		t.Fatal("ids created in sequence should sort in creation order")
	}
}

func TestNewConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]bool{}
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				id := New("x")
				mu.Lock()
				if seen[id] {
					t.Errorf("duplicate %s", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}
