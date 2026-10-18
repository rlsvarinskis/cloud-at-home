package store_test

import (
	"testing"

	"github.com/rlsvarinskis/cloud-at-home/services/agentd/store"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/store/storetest"
)

func TestMemory(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return store.NewMemory() })
}
