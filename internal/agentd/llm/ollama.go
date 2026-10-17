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
	"strings"

	"example.com/homecloud/internal/errs"
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
	NumCtx int `json:"num_ctx"`
}

type ollamaChatRequest struct {
	Model    string              `json:"model"`
	Messages []ollamaWireMessage `json:"messages"`
	Tools    []ollamaTool        `json:"tools"`
	Stream   bool                `json:"stream"`
	Options  ollamaOptions       `json:"options"`
}

type ollamaStreamChunk struct {
	Message ollamaStreamMessage `json:"message"`
	Done    bool                `json:"done"`
	Error   *string             `json:"error"`
}

type ollamaStreamMessage struct {
	Content   string           `json:"content"`
	Thinking  string           `json:"thinking"`
	ToolCalls []ollamaToolCall `json:"tool_calls"`
}

type ollamaTags struct {
	Models []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"models"`
}

// Chat streams one reply from /api/chat. Content, thinking and tool calls are accumulated across chunks.
// onChunk (may be nil) is called for every chunk carrying content or thinking. If ctx is cancelled the
// request is aborted, which closes the connection so Ollama stops generating, and ctx.Err() is returned.
func (o *Ollama) Chat(ctx context.Context, model string, msgs []Message, tools []Tool, onChunk func(Chunk)) (Message, error) {
	numCtx := o.NumCtx
	if numCtx == 0 {
		numCtx = defaultNumCtx
	}
	body, err := json.Marshal(ollamaChatRequest{
		Model:    model,
		Messages: toOllamaMessages(msgs),
		Tools:    toOllamaTools(tools),
		Stream:   true,
		Options:  ollamaOptions{NumCtx: numCtx},
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
		return Message{}, statusErr("ollama chat", resp)
	}
	return readStream(ctx, resp.Body, onChunk)
}

// ListModels returns the models installed on the server, from GET /api/tags.
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
		return nil, statusErr("ollama list models", resp)
	}
	var tags ollamaTags
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, fmt.Errorf("ollama list models: decode response: %w", err)
	}
	out := make([]ModelInfo, 0, len(tags.Models))
	for _, m := range tags.Models {
		out = append(out, ModelInfo{Name: m.Name, SizeBytes: m.Size})
	}
	return out, nil
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

// statusErr reports a non-200 response with its status and a trimmed prefix of the body.
func statusErr(op string, resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	text := strings.TrimSpace(string(snippet))
	if text == "" {
		return fmt.Errorf("%s: status %d", op, resp.StatusCode)
	}
	return fmt.Errorf("%s: status %d: %s", op, resp.StatusCode, text)
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
