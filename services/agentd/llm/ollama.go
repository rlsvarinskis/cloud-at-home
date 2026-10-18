package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

// defaultNumCtx is the context window requested from Ollama when Ollama.NumCtx is zero.
const defaultNumCtx = 100000

// maxErrorBody bounds how much of a non-200 response body is included in an error.
const maxErrorBody = 512

// defaultHTTP has no overall timeout: generations can legitimately run for a long time, and cancelling
// the request context is what stops one.
var defaultHTTP = &http.Client{}

var _ Client = (*Ollama)(nil)

// Ollama is a Client backed by an Ollama server's HTTP API.
type Ollama struct {
	// BaseURL is the server root, for example "http://127.0.0.1:11434". A trailing slash is ignored.
	BaseURL string
	// NumCtx is the context window sent as options.num_ctx. Zero means 100000.
	NumCtx int
	// HTTP is the client used for requests. Nil means a client with no overall timeout.
	HTTP *http.Client
}

// NewOllama returns an Ollama client for the server at baseURL, using default settings.
func NewOllama(baseURL string) *Ollama {
	return &Ollama{BaseURL: baseURL}
}

// Wire types for Ollama's /api/chat and /api/tags endpoints.

type ollamaWireMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content,omitempty"`
	Thinking  string           `json:"thinking,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"`
}

type ollamaToolCall struct {
	Function ollamaFunction `json:"function"`
}

type ollamaFunction struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type ollamaTool struct {
	Type     string        `json:"type"`
	Function ollamaToolDef `json:"function"`
}

type ollamaToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ollamaOptions struct {
	NumCtx     int `json:"num_ctx"`
	NumPredict int `json:"num_predict,omitempty"`
}

type ollamaChatRequest struct {
	Model    string              `json:"model"`
	Messages []ollamaWireMessage `json:"messages"`
	Tools    []ollamaTool        `json:"tools"`
	Stream   bool                `json:"stream"`
	Options  ollamaOptions       `json:"options"`
}

type ollamaStreamChunk struct {
	Message         ollamaStreamMessage `json:"message"`
	Done            bool                `json:"done"`
	DoneReason      string              `json:"done_reason"`
	PromptEvalCount int64               `json:"prompt_eval_count"`
	EvalCount       int64               `json:"eval_count"`
	Error           *string             `json:"error"`
}

type ollamaStreamMessage struct {
	Content   string           `json:"content"`
	Thinking  string           `json:"thinking"`
	ToolCalls []ollamaToolCall `json:"tool_calls"`
}

type ollamaTags struct {
	Models []struct {
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		Details struct {
			ParameterSize     string `json:"parameter_size"`
			QuantizationLevel string `json:"quantization_level"`
		} `json:"details"`
	} `json:"models"`
}

type ollamaShow struct {
	ModelInfo map[string]any `json:"model_info"`
}

// toolParseRE matches the error Ollama returns when it cannot parse a tool call the model emitted. The raw call
// text is the group. The pattern is the one the Python gateway uses.
var toolParseRE = regexp.MustCompile(`(?s)error parsing tool call: raw='(.*)', err=`)

// toolParseError returns a *ToolParseError when msg is Ollama's tool-call parse error, else nil.
func toolParseError(msg string) *ToolParseError {
	m := toolParseRE.FindStringSubmatch(msg)
	if m == nil {
		return nil
	}
	return &ToolParseError{Raw: m[1], Msg: strings.TrimSpace(msg)}
}

// Chat streams one reply from /api/chat. Content, thinking and tool calls are accumulated across chunks.
// onChunk (may be nil) is called for every chunk carrying content or thinking, and for the final Done chunk.
// If ctx is cancelled the request is aborted, which closes the connection so Ollama stops generating, and
// ctx.Err() is returned. A tool call Ollama cannot parse is returned as a *ToolParseError.
func (o *Ollama) Chat(ctx context.Context, model string, msgs []Message, tools []Tool, opts ChatOptions, onChunk func(Chunk)) (Message, error) {
	numCtx := o.NumCtx
	if numCtx == 0 {
		numCtx = defaultNumCtx
	}
	body, err := json.Marshal(ollamaChatRequest{
		Model:    model,
		Messages: toOllamaMessages(msgs),
		Tools:    toOllamaTools(tools),
		Stream:   true,
		Options:  ollamaOptions{NumCtx: numCtx, NumPredict: max(opts.MaxTokens, 0)},
	})
	if err != nil {
		return Message{}, fmt.Errorf("ollama chat: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint("/api/chat"), bytes.NewReader(body))
	if err != nil {
		return Message{}, fmt.Errorf("ollama chat: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client().Do(req)
	if err != nil {
		return Message{}, transportErr(ctx, "ollama chat", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		text := errorBody(resp)
		if pe := toolParseError(text); pe != nil {
			return Message{}, pe
		}
		return Message{}, statusErr("ollama chat", resp.StatusCode, text)
	}
	return readStream(ctx, resp.Body, onChunk)
}

// ListModels returns the models installed on the server: GET /api/tags for names, sizes and details, then
// POST /api/show for each model's context length. The show calls run concurrently and a failed one only
// leaves ContextLength zero.
func (o *Ollama) ListModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.endpoint("/api/tags"), nil)
	if err != nil {
		return nil, fmt.Errorf("ollama list models: build request: %w", err)
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, transportErr(ctx, "ollama list models", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("ollama list models", resp.StatusCode, errorBody(resp))
	}
	var tags ollamaTags
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, fmt.Errorf("ollama list models: decode response: %w", err)
	}
	out := make([]ModelInfo, len(tags.Models))
	var wg sync.WaitGroup
	for i, m := range tags.Models {
		out[i] = ModelInfo{Name: m.Name, SizeBytes: m.Size, Params: m.Details.ParameterSize,
			Quant: m.Details.QuantizationLevel}
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i].ContextLength = o.contextLength(ctx, m.Name)
		}()
	}
	wg.Wait()
	return out, nil
}

// contextLength asks /api/show for the model's context length: the value of its "<arch>.context_length" key.
// It returns zero on any failure.
func (o *Ollama) contextLength(ctx context.Context, model string) int64 {
	body, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return 0
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint("/api/show"), bytes.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client().Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var show ollamaShow
	if err := json.NewDecoder(resp.Body).Decode(&show); err != nil {
		return 0
	}
	for k, v := range show.ModelInfo {
		if f, ok := v.(float64); ok && strings.HasSuffix(k, ".context_length") {
			return int64(f)
		}
	}
	return 0
}

func (o *Ollama) endpoint(path string) string {
	return strings.TrimRight(o.BaseURL, "/") + path
}

func (o *Ollama) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return defaultHTTP
}

// readStream consumes newline-delimited chunks until one reports done. It uses bufio.Reader.ReadBytes
// rather than a Scanner so that lines of any length are accepted.
func readStream(ctx context.Context, body io.Reader, onChunk func(Chunk)) (Message, error) {
	r := bufio.NewReader(body)
	var content, thinking strings.Builder
	var calls []ToolCall
	done := false
	for !done {
		if err := ctx.Err(); err != nil {
			return Message{}, fmt.Errorf("ollama chat: %w", err)
		}
		line, readErr := r.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Message{}, fmt.Errorf("ollama chat: %w", ctxErr)
			}
			return Message{}, fmt.Errorf("ollama chat: read stream: %w", readErr)
		}
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var chunk ollamaStreamChunk
			if err := json.Unmarshal(trimmed, &chunk); err != nil {
				return Message{}, fmt.Errorf("ollama chat: decode stream chunk: %w", err)
			}
			if chunk.Error != nil {
				if pe := toolParseError(*chunk.Error); pe != nil {
					return Message{}, pe
				}
				return Message{}, fmt.Errorf("ollama chat: server error: %s", *chunk.Error)
			}
			m := chunk.Message
			if m.Content != "" || m.Thinking != "" {
				content.WriteString(m.Content)
				thinking.WriteString(m.Thinking)
				if onChunk != nil {
					onChunk(Chunk{Content: m.Content, Thinking: m.Thinking})
				}
			}
			for _, tc := range m.ToolCalls {
				calls = append(calls, ToolCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
			if chunk.Done && onChunk != nil {
				onChunk(Chunk{Done: true, DoneReason: chunk.DoneReason,
					PromptTokens: chunk.PromptEvalCount, GenTokens: chunk.EvalCount})
			}
			done = chunk.Done
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if !done {
		return Message{}, errors.New("ollama chat: stream ended before done")
	}
	return Message{
		Role:      "assistant",
		Content:   content.String(),
		Thinking:  thinking.String(),
		ToolCalls: calls,
	}, nil
}

// transportErr maps a failed request to an error. A cancelled context wins over the transport error;
// any other failure (typically connection refused) is reported as unavailable.
func transportErr(ctx context.Context, op string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", op, ctxErr)
	}
	return errs.Unavailable("%s: %v", op, err)
}

// errorBody returns the trimmed, bounded text of an error response. Ollama sends {"error": "..."}; that
// message is returned when present. It reads the body, so call it once per response.
func errorBody(resp *http.Response) string {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(snippet, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(snippet))
}

// statusErr reports a non-200 response with its status and the trimmed error text.
func statusErr(op string, code int, text string) error {
	text = strings.TrimSpace(text)
	if len(text) > maxErrorBody {
		text = text[:maxErrorBody]
	}
	if text == "" {
		return fmt.Errorf("%s: status %d", op, code)
	}
	return fmt.Errorf("%s: status %d: %s", op, code, text)
}

func toOllamaMessages(msgs []Message) []ollamaWireMessage {
	out := make([]ollamaWireMessage, 0, len(msgs))
	for _, m := range msgs {
		w := ollamaWireMessage{
			Role:     m.Role,
			Content:  m.Content,
			Thinking: m.Thinking,
			ToolName: m.ToolName,
		}
		for _, tc := range m.ToolCalls {
			args := tc.Arguments
			if args == nil {
				args = map[string]any{}
			}
			w.ToolCalls = append(w.ToolCalls, ollamaToolCall{Function: ollamaFunction{Name: tc.Name, Arguments: args}})
		}
		out = append(out, w)
	}
	return out
}

func toOllamaTools(tools []Tool) []ollamaTool {
	out := make([]ollamaTool, 0, len(tools))
	for _, t := range tools {
		params := t.Parameters
		if params == nil {
			params = map[string]any{}
		}
		out = append(out, ollamaTool{
			Type: "function",
			Function: ollamaToolDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out
}
