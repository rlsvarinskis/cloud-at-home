package mcpserver

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fileAndExecTools are the tools whose descriptions and schemas these tests pin.
var fileAndExecTools = []string{"agent_exec", "agent_exec_poll", "agent_exec_cancel", "agent_exec_list",
	"agent_upload", "agent_download", "agent_fetch", "agent_ssh_pubkey"}

// listedTools lists the tools through an in-memory session whose server has the given MaxWait.
func listedTools(t *testing.T, maxWait time.Duration) map[string]*mcp.Tool {
	t.Helper()
	cs := newTestSession(t, Clients{}, func(c *Config) { c.MaxWait = maxWait })
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		out[tl.Name] = tl
	}
	return out
}

// schemaMap is the listed input schema of a tool as generic JSON.
func schemaMap(t *testing.T, tl *mcp.Tool) map[string]any {
	t.Helper()
	raw, err := json.Marshal(tl.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func execSchemaProp(t *testing.T, m map[string]any, name string) map[string]any {
	t.Helper()
	props, _ := m["properties"].(map[string]any)
	p, ok := props[name].(map[string]any)
	if !ok {
		t.Fatalf("schema has no property %q", name)
	}
	return p
}

func TestExecFileToolSchemasRequiredAndConstraints(t *testing.T) {
	tools := listedTools(t, 7*time.Second)
	for _, name := range fileAndExecTools {
		if tools[name] == nil {
			t.Fatalf("tool %s is not listed", name)
		}
	}
	wantRequired := map[string][]string{
		"agent_exec":        {"command", "name"},
		"agent_exec_poll":   {"job_id", "name"},
		"agent_exec_cancel": {"job_id", "name"},
		"agent_exec_list":   {"name"},
		"agent_upload":      {"content", "name", "path"},
		"agent_download":    {"name", "path"},
		"agent_fetch":       {"name", "path", "url"},
		"agent_ssh_pubkey":  {"name"},
	}
	for tool, want := range wantRequired {
		req, _ := schemaMap(t, tools[tool])["required"].([]any)
		var got []string
		for _, r := range req {
			got = append(got, r.(string))
		}
		slices.Sort(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s required = %v, want %v", tool, got, want)
		}
	}

	// The SDK checks arguments against these, so a change here changes what callers are allowed to send.
	type bound struct {
		tool, prop, key string
		want            any
	}
	bounds := []bound{
		{"agent_exec", "timeout", "minimum", 1.0},
		{"agent_exec", "timeout", "maximum", 604800.0},
		{"agent_exec", "wait_seconds", "minimum", 0.0},
		{"agent_exec", "max_output", "minimum", 1.0},
		{"agent_exec", "max_output", "maximum", 65536.0},
		{"agent_exec", "command", "minLength", 1.0},
		{"agent_exec_poll", "job_id", "pattern", execJobIDPattern},
		{"agent_exec_poll", "wait_seconds", "minimum", 0.0},
		{"agent_exec_poll", "stdout_offset", "minimum", 0.0},
		{"agent_exec_poll", "stderr_offset", "minimum", 0.0},
		{"agent_exec_poll", "max_output", "maximum", 65536.0},
		{"agent_exec_cancel", "job_id", "pattern", execJobIDPattern},
		{"agent_exec_list", "name", "minLength", 1.0},
		{"agent_upload", "path", "pattern", "^/"},
		{"agent_upload", "mode", "pattern", "^[0-7]{1,4}$"},
		{"agent_download", "encoding", "enum", []any{"auto", "utf-8", "base64"}},
		{"agent_download", "max_bytes", "minimum", 1.0},
		{"agent_download", "max_bytes", "maximum", 8388608.0},
		{"agent_fetch", "url", "pattern", `^[Hh][Tt][Tt][Pp][Ss]?://`},
		{"agent_fetch", "wait_seconds", "minimum", 0.0},
	}
	for _, b := range bounds {
		p := execSchemaProp(t, schemaMap(t, tools[b.tool]), b.prop)
		if got := p[b.key]; !reflect.DeepEqual(got, b.want) {
			t.Errorf("%s.%s %s = %v, want %v", b.tool, b.prop, b.key, got, b.want)
		}
	}
}

// TestExecFileToolDescriptionsUseServerMaxWait checks that the waits in the text follow the configured MaxWait
// (7 s here, not the 40 s default), and that the descriptions stay within the length budget.
func TestExecFileToolDescriptionsUseServerMaxWait(t *testing.T) {
	tools := listedTools(t, 7*time.Second)
	for _, name := range []string{"agent_exec", "agent_exec_poll", "agent_fetch"} {
		d := tools[name].Description
		if !strings.Contains(d, "maximum wait of 7 s") || strings.Contains(d, "40 s") {
			t.Errorf("%s description does not state the configured cap of 7 s: %q", name, d)
		}
	}
	if w := execSchemaProp(t, schemaMap(t, tools["agent_exec"]), "wait_seconds")["description"].(string); !strings.Contains(w, "7 s") {
		t.Errorf("agent_exec wait_seconds does not state the cap: %q", w)
	}
	if d := tools["agent_ssh_pubkey"].Description; !strings.Contains(d, "waits up to 7 s") {
		t.Errorf("agent_ssh_pubkey wait is 20 s capped at MaxWait 7 s, description says otherwise: %q", d)
	}
	for _, name := range fileAndExecTools {
		if n := len(tools[name].Description); n > 1200 {
			t.Errorf("%s description is %d bytes, keep it under 1200", name, n)
		}
	}
}

// TestExecJobIDPatternAcceptsVmdIDs checks the job ID pattern against the shapes vmd creates (ids.New and the
// sharded form) and against IDs vmd would never issue.
func TestExecJobIDPatternAcceptsVmdIDs(t *testing.T) {
	re := regexp.MustCompile(execJobIDPattern)
	ok := []string{exeJobID(1), "job_01h" + strings.Repeat("0", 23), "job_01h" + strings.Repeat("0", 23) + "-s0a"}
	for _, id := range ok {
		if !re.MatchString(id) {
			t.Errorf("pattern refuses vmd job ID %q", id)
		}
	}
	for _, id := range []string{"", "job-1", "vm_01h" + strings.Repeat("0", 23), "job_" + strings.Repeat("U", 26)} {
		if re.MatchString(id) {
			t.Errorf("pattern accepts %q", id)
		}
	}
}

// TestExecFileHandlersKeepTheirOwnChecks calls the handlers directly, below the schema, so the checks that the
// schema now also states stay in place as defence in depth.
func TestExecFileHandlersKeepTheirOwnChecks(t *testing.T) {
	s := New(Config{Clients: Clients{VMs: &exeVMs{}, Exec: &exeExec{}}, DefaultProject: "p1", MaxWait: time.Second})
	ctx := context.Background()
	zero, neg, big := 0, -1, 65537
	if _, err := s.execStart(ctx, execArgs{Name: "box", Command: "make", Timeout: &zero}); err == nil || !strings.Contains(err.Error(), "timeout must be between") {
		t.Errorf("execStart timeout 0: %v", err)
	}
	if _, err := s.execStart(ctx, execArgs{Name: "box", Command: "make", WaitSeconds: &neg}); err == nil || !strings.Contains(err.Error(), "wait_seconds must not be negative") {
		t.Errorf("execStart negative wait: %v", err)
	}
	if _, err := s.execPollTool(ctx, execPollArgs{Name: "box", JobID: exeJobID(1), MaxOutput: &big}); err == nil || !strings.Contains(err.Error(), "max_output must be between") {
		t.Errorf("execPollTool max_output: %v", err)
	}
	if _, err := s.execPollTool(ctx, execPollArgs{Name: "box", JobID: exeJobID(1), StdoutOffset: -1}); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Errorf("execPollTool negative offset: %v", err)
	}
	if _, err := s.agentFetch(ctx, filFetchArgs{Name: "box", URL: "ftp://e.test/x", Path: "/p"}); err == nil || !strings.Contains(err.Error(), "http:// or https://") {
		t.Errorf("agentFetch ftp URL: %v", err)
	}
	if _, err := s.agentUpload(ctx, filUploadArgs{Name: "box", Path: "a.txt", Content: "x"}); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Errorf("agentUpload relative path: %v", err)
	}
	if _, err := s.agentDownload(ctx, filDownloadArgs{Name: "box", Path: "/a", Encoding: "latin1"}); err == nil || !strings.Contains(err.Error(), "encoding must be") {
		t.Errorf("agentDownload encoding: %v", err)
	}
}
