package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAllToolsRegistered(t *testing.T) {
	cs := newTestSession(t, Clients{})
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	want := []string{
		"agent_create", "agent_list", "agent_stop", "agent_destroy", "agent_resize",
		"agent_exec", "agent_exec_poll", "agent_exec_cancel", "agent_exec_list",
		"agent_upload", "agent_download", "agent_fetch", "agent_ssh_pubkey",
		"agent_task", "agent_status", "agent_cancel", "agent_models",
		"agent_metrics", "cluster_capacity", "node_drain",
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("tool %q is not registered", n)
		}
	}
}
