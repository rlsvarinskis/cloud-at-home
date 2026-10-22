package mcpserver

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1"
)

// listedSchema is the part of a tool's input schema that these tests pin, as a client receives it.
type listedSchema struct {
	Required   []string `json:"required"`
	Properties map[string]struct {
		Type      any      `json:"type"`
		Minimum   *float64 `json:"minimum"`
		Maximum   *float64 `json:"maximum"`
		MinLength *int     `json:"minLength"`
		Pattern   string   `json:"pattern"`
	} `json:"properties"`
}

// listedAgentSchemas lists the tools through an in-memory session and returns the schemas of the agent tools.
func listedAgentSchemas(t *testing.T) map[string]listedSchema {
	t.Helper()
	cs := newTestSession(t, Clients{})
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]listedSchema{}
	for _, tl := range res.Tools {
		if !strings.HasPrefix(tl.Name, "agent_") {
			continue
		}
		raw, err := json.Marshal(tl.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var sc listedSchema
		if err := json.Unmarshal(raw, &sc); err != nil {
			t.Fatalf("%s: %v", tl.Name, err)
		}
		out[tl.Name] = sc
	}
	return out
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

// TestAgentToolSchemasPinRequiredAndConstraints catches drift in the schemas of the four agent tools.
func TestAgentToolSchemasPinRequiredAndConstraints(t *testing.T) {
	schemas := listedAgentSchemas(t)
	for _, name := range []string{"agent_task", "agent_status", "agent_cancel", "agent_models"} {
		if _, ok := schemas[name]; !ok {
			t.Fatalf("tool %s not listed", name)
		}
	}
	zero, hi := 0.0, float64(int32Max)

	// agent_task: model is required, like name and task.
	task := schemas["agent_task"]
	if got, want := sortedCopy(task.Required), []string{"model", "name", "task"}; !slices.Equal(got, want) {
		t.Fatalf("agent_task required = %v, want %v", got, want)
	}
	if p := task.Properties["model"]; p.MinLength == nil || *p.MinLength != 1 {
		t.Fatalf("agent_task model minLength = %v, want 1", p.MinLength)
	}
	for _, f := range []string{"max_steps", "max_minutes", "max_call_tokens"} {
		p := task.Properties[f]
		if p.Minimum == nil || *p.Minimum != zero || p.Maximum == nil || *p.Maximum != hi {
			t.Fatalf("agent_task %s bounds = [%v, %v], want [0, int32 max]", f, p.Minimum, p.Maximum)
		}
	}

	// agent_status and agent_cancel: only name is required; the cursor and counts are not negative.
	for _, name := range []string{"agent_status", "agent_cancel"} {
		if got := sortedCopy(schemas[name].Required); !slices.Equal(got, []string{"name"}) {
			t.Fatalf("%s required = %v, want [name]", name, got)
		}
	}
	status := schemas["agent_status"]
	for _, f := range []string{"wait_seconds", "since_step", "max_commands"} {
		if p := status.Properties[f]; p.Minimum == nil || *p.Minimum != zero {
			t.Fatalf("agent_status %s minimum = %v, want 0", f, p.Minimum)
		}
	}
	if p := status.Properties["since_step"]; p.Maximum == nil || *p.Maximum != hi {
		t.Fatalf("agent_status since_step maximum = %v, want int32 max", p.Maximum)
	}

	// The machine name rule is the same for every agent tool that takes a name.
	for _, name := range []string{"agent_task", "agent_status", "agent_cancel"} {
		if p := schemas[name].Properties["name"]; p.Pattern != machineNamePattern {
			t.Fatalf("%s name pattern = %q, want %q", name, p.Pattern, machineNamePattern)
		}
	}

	if got := schemas["agent_models"].Required; len(got) != 0 {
		t.Fatalf("agent_models required = %v, want none", got)
	}
}

// TestAgentToolsRejectInvalidArgumentsBeforeTheHandler checks that the SDK refuses a call that breaks the schema,
// and names the property, so the handler is not reached.
func TestAgentToolsRejectInvalidArgumentsBeforeTheHandler(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"agent_task", map[string]any{"name": "box", "task": "go"}, `"model"`},
		{"agent_task", map[string]any{"name": "box", "task": "go", "model": "qwen", "max_minutes": -1}, "max_minutes"},
		{"agent_status", map[string]any{"name": "box", "since_step": -1}, "since_step"},
		{"agent_status", map[string]any{"name": "box", "max_commands": -1}, "max_commands"},
		{"agent_status", map[string]any{"name": "box", "wait_seconds": -1}, "wait_seconds"},
		{"agent_status", map[string]any{"name": "Box"}, "name"},
		{"agent_cancel", map[string]any{"name": "-box"}, "name"},
	}
	for _, tc := range cases {
		t.Run(tc.tool+" "+tc.want, func(t *testing.T) {
			env := tskStatusEnv(tskTask("task_1", agentdv1.TaskState_TASK_STATE_RUNNING))
			cs := newTestSession(t, env.clients())
			out, isErr := callTool(t, cs, tc.tool, tc.args)
			if !isErr || !strings.Contains(out, tc.want) || !strings.Contains(out, "validating") {
				t.Fatalf("got %q (error %v), want a validation error naming %s", out, isErr, tc.want)
			}
			if len(env.tasks.gets) != 0 || len(env.tasks.created) != 0 || len(env.tasks.cancelled) != 0 {
				t.Fatal("the handler ran for an invalid call")
			}
		})
	}
}
