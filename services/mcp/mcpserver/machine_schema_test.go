package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// schemaProp is the part of one property's JSON schema these tests check.
type schemaProp struct {
	Pattern   string   `json:"pattern,omitempty"`
	MinLength *int     `json:"minLength,omitempty"`
	MaxLength *int     `json:"maxLength,omitempty"`
	Minimum   *float64 `json:"minimum,omitempty"`
	Maximum   *float64 `json:"maximum,omitempty"`
}

// listedTool is a tool as a client sees it through tools/list.
type listedTool struct {
	Description string
	Required    []string
	Properties  map[string]schemaProp
}

// listTools starts a server on an in-memory transport and returns its tools by name.
func listTools(t *testing.T, mutate ...func(*Config)) map[string]listedTool {
	t.Helper()
	cs := newTestSession(t, Clients{}, mutate...)
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]listedTool{}
	for _, tl := range res.Tools {
		raw, err := json.Marshal(tl.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Required   []string              `json:"required"`
			Properties map[string]schemaProp `json:"properties"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: schema: %v", tl.Name, err)
		}
		out[tl.Name] = listedTool{Description: tl.Description, Required: doc.Required, Properties: doc.Properties}
	}
	return out
}

func intPtr(v int) *int { return &v }

// bounds is the schema of a numeric property with an inclusive range.
func bounds(lo, hi float64) schemaProp {
	return schemaProp{Minimum: &lo, Maximum: &hi}
}

// machineNameSchema is the schema of a machine name: the MCP rule, which is also what vmd accepts.
var machineNameSchema = schemaProp{Pattern: machineNameRE.String(), MinLength: intPtr(1), MaxLength: intPtr(31)}

func samePtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameProp(a, b schemaProp) bool {
	return a.Pattern == b.Pattern && samePtr(a.MinLength, b.MinLength) && samePtr(a.MaxLength, b.MaxLength) &&
		samePtr(a.Minimum, b.Minimum) && samePtr(a.Maximum, b.Maximum)
}

// TestMachineToolSchemas pins the required arguments and the key constraints of the machine tools, so that schema
// drift (a renamed field, a dropped bound) fails a test rather than silently changing what clients are told.
func TestMachineToolSchemas(t *testing.T) {
	tools := listTools(t)
	cases := []struct {
		tool     string
		required []string
		props    map[string]schemaProp
	}{
		{"agent_create", []string{"name"}, map[string]schemaProp{
			"name": machineNameSchema, "cpus": bounds(0, 32), "memory_gb": bounds(0, 56), "disk_gb": bounds(0, 4096)}},
		{"agent_list", nil, nil},
		{"agent_stop", []string{"name"}, map[string]schemaProp{"name": machineNameSchema}},
		{"agent_destroy", []string{"name"}, map[string]schemaProp{"name": machineNameSchema}},
		{"agent_resize", []string{"name"}, map[string]schemaProp{
			"name": machineNameSchema, "cpus": bounds(0, 32), "memory_gb": bounds(0, 56)}},
		{"agent_metrics", []string{"name"}, map[string]schemaProp{"name": machineNameSchema}},
		{"cluster_capacity", nil, nil},
		{"node_drain", []string{"node_id", "draining"}, map[string]schemaProp{"node_id": {MinLength: intPtr(1)}}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			tl, ok := tools[tc.tool]
			if !ok {
				t.Fatalf("tool %q is not registered", tc.tool)
			}
			want := slices.Clone(tc.required)
			slices.Sort(want)
			got := slices.Clone(tl.Required)
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("required = %v, want %v", got, want)
			}
			for field, wp := range tc.props {
				gp, ok := tl.Properties[field]
				if !ok {
					t.Errorf("no schema property %q", field)
					continue
				}
				if !sameProp(gp, wp) {
					t.Errorf("%s: schema %+v, want %+v", field, gp, wp)
				}
			}
		})
	}
}

// TestMachineToolSchemaRejectsBeforeHandler checks that the SDK refuses out-of-range values with a message naming
// the property, and that the handler is not reached.
func TestMachineToolSchemaRejectsBeforeHandler(t *testing.T) {
	cs := newTestSession(t, Clients{})
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"agent_stop", map[string]any{"name": "Web"}, "name"},
		{"agent_destroy", map[string]any{"name": "web-"}, "name"},
		{"agent_resize", map[string]any{"name": "1web", "cpus": 2}, "name"},
		{"agent_metrics", map[string]any{"name": ""}, "name"},
		{"node_drain", map[string]any{"node_id": "", "draining": true}, "node_id"},
	} {
		t.Run(tc.tool+"/"+tc.want, func(t *testing.T) {
			out, isErr := callTool(t, cs, tc.tool, tc.args)
			if !isErr || !strings.Contains(out, "validating") || !strings.Contains(out, tc.want) {
				t.Errorf("got (%q, isError=%v), want a schema error naming %q", out, isErr, tc.want)
			}
		})
	}
}

// TestMachineToolDescriptionsUseMaxWait checks that the wait in the descriptions follows the server's MaxWait:
// the budget is MaxWait minus waitReserve, so 20 s gives 15 s and the default 40 s gives 35 s.
func TestMachineToolDescriptionsUseMaxWait(t *testing.T) {
	cases := []struct {
		maxWait time.Duration
		want    string
	}{
		{20 * time.Second, "about 15 s"},
		{90 * time.Second, "about 85 s"},
		{40 * time.Second, "about 35 s"},
	}
	for _, tc := range cases {
		tools := listTools(t, func(c *Config) { c.MaxWait = tc.maxWait })
		for _, name := range []string{"agent_create", "agent_resize"} {
			desc := tools[name].Description
			if !strings.Contains(desc, tc.want) {
				t.Errorf("MaxWait %v: %s description does not contain %q", tc.maxWait, name, tc.want)
			}
			if strings.Contains(desc, "25 s") {
				t.Errorf("MaxWait %v: %s still mentions the old 25 s budget", tc.maxWait, name)
			}
		}
	}
}

// TestMachineToolDescriptionLength keeps the descriptions within the size the model reads comfortably.
func TestMachineToolDescriptionLength(t *testing.T) {
	tools := listTools(t)
	for _, name := range []string{"agent_create", "agent_list", "agent_stop", "agent_destroy", "agent_resize",
		"agent_metrics", "cluster_capacity", "node_drain"} {
		if n := len(tools[name].Description); n > 1200 {
			t.Errorf("%s description is %d characters, want at most 1200", name, n)
		}
	}
}

// machineNameVMD is vmd's rule for VM names (services/vmd/service/service.go).
var machineNameVMD = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

func TestMachineNameRule(t *testing.T) {
	valid := []string{"a", "web", "web-1", "a1", strings.Repeat("a", 31), "a" + strings.Repeat("b", 29) + "z"}
	invalid := []string{"", "1web", "-web", "web-", "Web", "web_1", "web.1", "web name", strings.Repeat("a", 32),
		"a" + strings.Repeat("b", 30) + "z"}
	for _, n := range valid {
		if err := checkMachineName(n); err != nil {
			t.Errorf("checkMachineName(%q) = %v, want valid", n, err)
		}
		if !machineNameVMD.MatchString(n) {
			t.Errorf("valid name %q is rejected by vmd", n)
		}
	}
	for _, n := range invalid {
		if err := checkMachineName(n); err == nil {
			t.Errorf("checkMachineName(%q) accepted, want invalid", n)
		}
	}
	// Every name this server accepts must be a name vmd accepts.
	for _, n := range valid {
		if !machineNameVMD.MatchString(n) {
			t.Errorf("MCP accepts %q but vmd does not", n)
		}
	}
}

// TestMachineHandlersKeepTheirChecks calls the handlers directly, which skips the schema check, to show that the
// handler checks remain as defence in depth.
func TestMachineHandlersKeepTheirChecks(t *testing.T) {
	s := New(Config{Clients: Clients{}, DefaultProject: "p1", MaxWait: 2 * time.Second})
	ctx := context.Background()
	check := func(name string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, want)
		}
	}
	_, err := s.agentCreate(ctx, agentCreateArgs{Name: "1web"})
	check("agent_create name", err, "is invalid")
	_, err = s.agentCreate(ctx, agentCreateArgs{Name: "web", CPUs: 33})
	check("agent_create cpus", err, "cpus must be between 1 and 32")
	_, err = s.agentCreate(ctx, agentCreateArgs{Name: "web", DiskGB: 4097})
	check("agent_create disk", err, "disk_gb must be between 1 and 4096")
	_, err = s.agentStop(ctx, agentStopArgs{Name: "Web"})
	check("agent_stop name", err, "is invalid")
	_, err = s.agentDestroy(ctx, agentDestroyArgs{Name: "web-"})
	check("agent_destroy name", err, "is invalid")
	_, err = s.agentResize(ctx, agentResizeArgs{Name: "1web", CPUs: 2})
	check("agent_resize name", err, "is invalid")
	_, err = s.agentResize(ctx, agentResizeArgs{Name: "web", CPUs: 33})
	check("agent_resize cpus", err, "cpus must be between 1 and 32")
	_, err = s.opsAgentMetrics(ctx, opsMetricsArgs{Name: "Web"})
	check("agent_metrics name", err, "is invalid")
}
