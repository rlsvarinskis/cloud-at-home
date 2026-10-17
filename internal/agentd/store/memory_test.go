package store_test

import (
	"testing"

	"example.com/homecloud/internal/agentd/store"
	"example.com/homecloud/internal/agentd/store/storetest"
)

func TestMemory(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return store.NewMemory() })
}
