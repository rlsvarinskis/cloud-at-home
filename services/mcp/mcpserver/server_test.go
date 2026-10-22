package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestInitializeCarriesServerInstructions checks that MCP clients get the instructions on initialize, with the
// numbers taken from the server configuration.
func TestInitializeCarriesServerInstructions(t *testing.T) {
	cs := newTestSession(t, Clients{}, func(c *Config) {
		c.DefaultProject = "proj-x"
		c.MaxWait = 40 * time.Second
	})
	ins := cs.InitializeResult().Instructions
	for _, want := range []string{
		"Attic Cloud", `"proj-x"`, "agent_exec_poll", "agent_task", "agent_status", "since_step", "wait_seconds 0",
		"agent_cancel", "check_command", "done_unverified", "not_found:", "40s",
	} {
		if !strings.Contains(ins, want) {
			t.Errorf("instructions lack %q:\n%s", want, ins)
		}
	}
	if n := strings.Count(ins, "\n") + 1; n < 12 || n > 30 {
		t.Errorf("instructions have %d lines, want about 15 to 25", n)
	}
}

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

// pythonGatewaySchema is the tool argument schema the Python gateway exposed in production: argument names and JSON
// types per tool. Existing MCP clients and prompts were written against it, so every one of these arguments must
// still exist here with the same type. The Go server may add arguments (project, image, ...), never drop or rename these.
var pythonGatewaySchema = map[string]map[string]string{
	"agent_create":      {"name": "string", "cpus": "integer", "memory_gb": "integer"},
	"agent_resize":      {"name": "string", "cpus": "integer", "memory_gb": "integer", "force": "boolean"},
	"agent_list":        {},
	"agent_stop":        {"name": "string"},
	"agent_destroy":     {"name": "string"},
	"agent_exec":        {"name": "string", "command": "string", "timeout": "integer", "wait_seconds": "integer", "max_output": "integer"},
	"agent_exec_poll":   {"name": "string", "job_id": "string", "wait_seconds": "integer", "stdout_offset": "integer", "stderr_offset": "integer", "max_output": "integer"},
	"agent_exec_cancel": {"name": "string", "job_id": "string"},
	"agent_exec_list":   {"name": "string"},
	"agent_upload":      {"name": "string", "path": "string", "content": "string", "base64_encoded": "boolean"},
	"agent_download":    {"name": "string", "path": "string", "encoding": "string", "as_base64": "boolean"},
	"agent_ssh_pubkey":  {"name": "string"},
	"agent_fetch":       {"name": "string", "url": "string", "path": "string", "wait_seconds": "integer"},
	"agent_models":      {},
	"agent_task":        {"name": "string", "task": "string", "model": "string", "check_command": "string", "max_steps": "integer", "max_minutes": "integer", "max_call_tokens": "integer", "check_each_step": "boolean"},
	"agent_status":      {"name": "string", "wait_seconds": "integer", "since_step": "integer", "max_commands": "integer", "brief": "boolean"},
	"agent_cancel":      {"name": "string"},
}

// deliberateRenames lists the only two arguments that differ from the Python gateway, and what they are called here:
// the old names ("base64_encoded", "as_base64") were awkward, and a plain "base64" says the same thing.
var deliberateRenames = map[string]string{
	"agent_upload.base64_encoded": "base64",
	"agent_download.as_base64":    "base64",
}

func TestSchemaCoversTheDeployedPythonGateway(t *testing.T) {
	cs := newTestSession(t, Clients{})
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		tools[tl.Name] = tl
	}
	for name, args := range pythonGatewaySchema {
		tl, ok := tools[name]
		if !ok {
			t.Errorf("tool %q of the deployed schema is missing", name)
			continue
		}
		raw, err := json.Marshal(tl.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var sch struct {
			Properties map[string]struct {
				Type any `json:"type"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &sch); err != nil {
			t.Fatal(err)
		}
		for arg, typ := range args {
			if now, renamed := deliberateRenames[name+"."+arg]; renamed {
				arg = now
			}
			p, ok := sch.Properties[arg]
			if !ok {
				t.Errorf("%s: argument %q of the deployed schema is missing", name, arg)
				continue
			}
			if !schemaTypeIs(p.Type, typ) {
				t.Errorf("%s.%s: type %v, the deployed schema has %s", name, arg, p.Type, typ)
			}
		}
	}
}

// schemaTypeIs reports whether a JSON schema "type" (a string or a list of strings) allows typ.
func schemaTypeIs(got any, typ string) bool {
	switch g := got.(type) {
	case string:
		return g == typ
	case []any:
		for _, v := range g {
			if v == typ {
				return true
			}
		}
	}
	return false
}
