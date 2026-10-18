package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

// ndjsonServer answers every request with the given lines, one per line of NDJSON.
func ndjsonServer(t *testing.T, lines ...string) *Ollama {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, l := range lines {
			io.WriteString(w, l+"\n")
		}
	}))
	t.Cleanup(srv.Close)
	return NewOllama(srv.URL)
}

// chatOnce runs one Chat with a single user message and no tools.
func chatOnce(o *Ollama, onChunk func(Chunk)) (Message, error) {
	return o.Chat(context.Background(), "gemma4:12b", []Message{{Role: "user", Content: "hi"}}, nil, ChatOptions{}, onChunk)
}

func TestChatStreamsContent(t *testing.T) {
	o := ndjsonServer(t,
		`{"message":{"role":"assistant","content":"Hel"},"done":false}`,
		`{"message":{"role":"assistant","content":"lo"},"done":false}`,
		`{"message":{"role":"assistant","content":"!"},"done":true}`,
	)
	var got []Chunk
	msg, err := chatOnce(o, func(c Chunk) {
		if !c.Done {
			got = append(got, c)
		}
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	want := []Chunk{{Content: "Hel"}, {Content: "lo"}, {Content: "!"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("onChunk got %+v, want %+v", got, want)
	}
	if msg.Role != "assistant" || msg.Content != "Hello!" || msg.Thinking != "" || msg.ToolCalls != nil {
		t.Errorf("message = %+v", msg)
	}
}

func TestChatThinkingAndContent(t *testing.T) {
	o := ndjsonServer(t,
		`{"message":{"role":"assistant","thinking":"Let me"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":false}`,
		`{"message":{"role":"assistant","thinking":" think"},"done":false}`,
		`{"message":{"role":"assistant","content":"Answer"},"done":true}`,
	)
	var got []Chunk
	msg, err := chatOnce(o, func(c Chunk) {
		if !c.Done {
			got = append(got, c)
		}
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	want := []Chunk{{Thinking: "Let me"}, {Thinking: " think"}, {Content: "Answer"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("onChunk got %+v, want %+v (empty chunks must not be delivered)", got, want)
	}
	if msg.Thinking != "Let me think" || msg.Content != "Answer" {
		t.Errorf("message = %+v", msg)
	}
}

func TestChatToolCallInMiddleChunk(t *testing.T) {
	o := ndjsonServer(t,
		`{"message":{"role":"assistant","content":"Checking"},"done":false}`,
		`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"bash","arguments":{"command":"ls"}}}]},"done":false}`,
		`{"message":{"role":"assistant","content":"done"},"done":true}`,
	)
	var got []Chunk
	msg, err := chatOnce(o, func(c Chunk) {
		if !c.Done {
			got = append(got, c)
		}
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d chunks, want 2 (tool-call-only chunk has no content)", len(got))
	}
	if msg.Content != "Checkingdone" {
		t.Errorf("Content = %q", msg.Content)
	}
	want := []ToolCall{{Name: "bash", Arguments: map[string]any{"command": "ls"}}}
	if !reflect.DeepEqual(msg.ToolCalls, want) {
		t.Errorf("ToolCalls = %+v, want %+v", msg.ToolCalls, want)
	}
}

func TestChatMultipleToolCallsKeepOrder(t *testing.T) {
	o := ndjsonServer(t,
		`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"read_file","arguments":{"path":"a.txt"}}}]},"done":false}`,
		`{"message":{"role":"assistant","content":"x"},"done":false}`,
		`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"bash","arguments":{"command":"ls"}}},{"function":{"name":"bash","arguments":{"command":"pwd"}}}]},"done":true}`,
	)
	msg, err := chatOnce(o, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var names, cmds []string
	for _, tc := range msg.ToolCalls {
		names = append(names, tc.Name)
		if c, ok := tc.Arguments["command"].(string); ok {
			cmds = append(cmds, c)
		}
	}
	if !reflect.DeepEqual(names, []string{"read_file", "bash", "bash"}) {
		t.Errorf("tool names = %v", names)
	}
	if !reflect.DeepEqual(cmds, []string{"ls", "pwd"}) {
		t.Errorf("commands = %v", cmds)
	}
	if msg.Content != "x" {
		t.Errorf("Content = %q", msg.Content)
	}
}

func TestChatErrorChunk(t *testing.T) {
	o := ndjsonServer(t,
		`{"message":{"role":"assistant","content":"partial"},"done":false}`,
		`{"error":"model \"nope\" not found"}`,
	)
	_, err := chatOnce(o, nil)
	if err == nil || !strings.Contains(err.Error(), `model "nope" not found`) {
		t.Fatalf("err = %v, want server error text", err)
	}
}

func TestChatNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "  boom happened  \n", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	_, err := chatOnce(NewOllama(srv.URL), nil)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"500", "boom happened"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "  ") || strings.Contains(err.Error(), "\n") {
		t.Errorf("err = %q, body not trimmed", err)
	}
}

func TestChatStreamCutBeforeDone(t *testing.T) {
	o := ndjsonServer(t,
		`{"message":{"role":"assistant","content":"half"},"done":false}`,
	)
	msg, err := chatOnce(o, nil)
	if err == nil {
		t.Fatalf("expected truncated-stream error, got message %+v", msg)
	}
}

func TestChatHugeSingleLineChunk(t *testing.T) {
	big := strings.Repeat("x", 2<<20) // 2 MiB in one NDJSON line, far beyond bufio.Scanner's default
	line, err := json.Marshal(map[string]any{
		"message": map[string]any{"role": "assistant", "content": big},
		"done":    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	o := ndjsonServer(t, string(line))
	msg, err := chatOnce(o, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if msg.Content != big {
		t.Errorf("content length = %d, want %d", len(msg.Content), len(big))
	}
}

func TestChatRequestBody(t *testing.T) {
	bodies := make(chan map[string]any, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		bodies <- body
		io.WriteString(w, `{"message":{"role":"assistant","content":"ok"},"done":true}`+"\n")
	}))
	t.Cleanup(srv.Close)

	msgs := []Message{
		{Role: "user", Content: "list files"},
		{Role: "assistant", ToolCalls: []ToolCall{{Name: "bash", Arguments: map[string]any{"command": "ls"}}}},
		{Role: "tool", Content: "a b", ToolName: "bash"},
	}
	tools := []Tool{{
		Name:        "bash",
		Description: "run a shell command",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []any{"command"},
		},
	}}

	o := NewOllama(srv.URL)
	if _, err := o.Chat(context.Background(), "gemma4:12b", msgs, tools, ChatOptions{}, nil); err != nil {
		t.Fatalf("Chat (default num_ctx): %v", err)
	}
	o.NumCtx = 4096
	if _, err := o.Chat(context.Background(), "gemma4:12b", msgs, tools, ChatOptions{}, nil); err != nil {
		t.Fatalf("Chat (custom num_ctx): %v", err)
	}

	first := <-bodies
	second := <-bodies

	if first["model"] != "gemma4:12b" || first["stream"] != true {
		t.Errorf("model/stream = %v / %v", first["model"], first["stream"])
	}
	if opts := first["options"].(map[string]any); opts["num_ctx"] != float64(100000) {
		t.Errorf("default num_ctx = %v, want 100000", opts["num_ctx"])
	}
	if opts := second["options"].(map[string]any); opts["num_ctx"] != float64(4096) {
		t.Errorf("custom num_ctx = %v, want 4096", opts["num_ctx"])
	}

	wire := first["messages"].([]any)
	if len(wire) != 3 {
		t.Fatalf("got %d messages, want 3", len(wire))
	}
	toolMsg := wire[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["content"] != "a b" || toolMsg["tool_name"] != "bash" {
		t.Errorf("tool message = %v", toolMsg)
	}
	asst := wire[1].(map[string]any)
	fn := asst["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" || fn["arguments"].(map[string]any)["command"] != "ls" {
		t.Errorf("assistant tool call = %v", fn)
	}

	tool := first["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool type = %v", tool["type"])
	}
	def := tool["function"].(map[string]any)
	if def["name"] != "bash" || def["description"] != "run a shell command" {
		t.Errorf("tool function = %v", def)
	}
	if def["parameters"].(map[string]any)["type"] != "object" {
		t.Errorf("tool parameters = %v", def["parameters"])
	}
}

func TestChatCancelledBeforeStart(t *testing.T) {
	o := ndjsonServer(t, `{"message":{"role":"assistant","content":"x"},"done":true}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := o.Chat(ctx, "m", nil, nil, ChatOptions{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestChatCancelMidStream(t *testing.T) {
	disconnected := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, `{"message":{"role":"assistant","content":"first"},"done":false}`+"\n")
		w.(http.Flusher).Flush()
		// Simulate a slow generation that never finishes on its own.
		select {
		case <-r.Context().Done():
			close(disconnected)
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	_, err := NewOllama(srv.URL).Chat(ctx, "m", nil, nil, ChatOptions{}, func(Chunk) { cancel() })
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Errorf("Chat took %v after cancel, want < 1s", elapsed)
	}
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("server never observed the client disconnect")
	}
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/tags":
			io.WriteString(w, `{"models":[`+
				`{"name":"gemma4:12b","size":7516192768,"details":{"parameter_size":"12.0B","quantization_level":"Q4_K_M"}},`+
				`{"name":"tiny","size":1},`+
				`{"name":"broken","size":2}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/show":
			var req struct {
				Model string `json:"model"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			switch req.Model {
			case "gemma4:12b":
				io.WriteString(w, `{"model_info":{"general.architecture":"gemma","gemma.context_length":131072}}`)
			case "tiny":
				io.WriteString(w, `{"model_info":{}}`)
			default:
				http.Error(w, "no such model", http.StatusNotFound)
			}
		default:
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	models, err := NewOllama(srv.URL).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []ModelInfo{
		{Name: "gemma4:12b", SizeBytes: 7516192768, Params: "12.0B", Quant: "Q4_K_M", ContextLength: 131072},
		{Name: "tiny", SizeBytes: 1},
		{Name: "broken", SizeBytes: 2},
	}
	if !reflect.DeepEqual(models, want) {
		t.Errorf("models = %+v, want %+v", models, want)
	}
}

func TestChatSendsNumPredictOnlyWhenCapped(t *testing.T) {
	for _, tc := range []struct {
		max  int
		want string
	}{{0, `"num_predict"`}, {512, `"num_predict":512`}} {
		var body string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			io.WriteString(w, `{"message":{"role":"assistant","content":"x"},"done":true,"done_reason":"stop"}`)
		}))
		o := NewOllama(srv.URL)
		if _, err := o.Chat(context.Background(), "m", nil, nil, ChatOptions{MaxTokens: tc.max}, nil); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if has := strings.Contains(body, tc.want); has != (tc.max > 0) {
			t.Errorf("max %d: body %s, contains %s = %v", tc.max, body, tc.want, has)
		}
	}
}

func TestChatDoneChunkCarriesReasonAndCounts(t *testing.T) {
	o := ndjsonServer(t,
		`{"message":{"role":"assistant","content":"x"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"length","prompt_eval_count":11,"eval_count":22}`,
	)
	var done []Chunk
	if _, err := chatOnce(o, func(c Chunk) {
		if c.Done {
			done = append(done, c)
		}
	}); err != nil {
		t.Fatal(err)
	}
	want := []Chunk{{Done: true, DoneReason: "length", PromptTokens: 11, GenTokens: 22}}
	if !reflect.DeepEqual(done, want) {
		t.Fatalf("done chunks = %+v, want %+v", done, want)
	}
}

func TestToolParseErrorFromStatusAndStream(t *testing.T) {
	// Ollama's HTTP 500 body: {"error": "..."} with the raw call text inside the message.
	status := `{"error":"error parsing tool call: raw='{\"command\": \"a\nb\"}', err=invalid character"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, status)
	}))
	t.Cleanup(srv.Close)
	_, err := chatOnce(NewOllama(srv.URL), nil)
	var pe *ToolParseError
	if !errors.As(err, &pe) {
		t.Fatalf("status err = %v, want a *ToolParseError", err)
	}
	if pe.Raw != "{\"command\": \"a\nb\"}" {
		t.Errorf("Raw = %q", pe.Raw)
	}

	// The same error arriving as an error line inside the stream.
	o := ndjsonServer(t, `{"error":"error parsing tool call: raw='{\"a\": 1', err=boom"}`)
	_, err = chatOnce(o, nil)
	if !errors.As(err, &pe) || pe.Raw != `{"a": 1` {
		t.Fatalf("stream err = %v (raw %v)", err, pe)
	}
}

func TestNon200ErrorTextIsKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"error":"model crashed"}`)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	_, err := chatOnce(NewOllama(srv.URL), nil)
	if err == nil || !strings.Contains(err.Error(), "model crashed") {
		t.Fatalf("err = %v", err)
	}
}

func TestListModelsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "warming up", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	_, err := NewOllama(srv.URL).ListModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want status 503", err)
	}
}

func TestConnectionRefusedIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens on this address any more

	o := NewOllama(url)
	if _, err := chatOnce(o, nil); !errors.Is(err, errs.ErrUnavailable) {
		t.Errorf("Chat err = %v, want errs.ErrUnavailable", err)
	}
	if _, err := o.ListModels(context.Background()); !errors.Is(err, errs.ErrUnavailable) {
		t.Errorf("ListModels err = %v, want errs.ErrUnavailable", err)
	}
}
