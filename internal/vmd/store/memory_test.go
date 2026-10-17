package store_test

import (
	"testing"

	"example.com/homecloud/internal/vmd/store"
	"example.com/homecloud/internal/vmd/store/storetest"
)

// TestMemory runs the store conformance suite against the in-memory reference implementation.
func TestMemory(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return store.NewMemory() })
}
