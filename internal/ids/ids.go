// Package ids generates sortable, collision-free resource IDs without any central coordination,
// which is what lets several control-plane replicas (or shards) create resources independently.
package ids

import (
	"crypto/rand"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

var (
	mu      sync.Mutex
	entropy = ulid.Monotonic(rand.Reader, 0)
)

// New returns an ID such as "vm_01hx..." (prefix + lowercase ULID). IDs sort by creation time.
func New(prefix string) string {
	mu.Lock()
	defer mu.Unlock()
	return prefix + "_" + lower(ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String())
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
