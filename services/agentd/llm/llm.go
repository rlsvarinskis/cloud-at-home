// Package llm abstracts the language-model backend (Ollama today) so the agent loop can be tested
// with a scripted fake and other backends can be added.
package llm

import (
	"context"
	"fmt"
)

type ToolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// Message roles: "system", "user", "assistant", "tool".
type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content,omitempty"`
	Thinking  string     `json:"thinking,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"` // for role "tool": which tool produced Content
}

// Tool describes a function the model may call. Parameters is a JSON-schema object.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ChatOptions tunes one model call.
type ChatOptions struct {
	// MaxTokens caps the tokens generated in this reply (Ollama's num_predict). Zero means no cap.
	MaxTokens int
}

// Chunk is a streamed fragment of the reply, delivered as it is generated. The last chunk of a reply that
// completed has Done set: it carries why generation stopped (DoneReason, "stop" or "length") and the token
// counts that the backend reported. A reply that was aborted never delivers a Done chunk.
type Chunk struct {
	Content  string
	Thinking string

	Done       bool
	DoneReason string
	// PromptTokens and GenTokens are set on the Done chunk only.
	PromptTokens int64
	GenTokens    int64
}

// ToolParseError reports that the backend could not parse a tool call the model emitted. Raw is the text
// the model produced for the call, as far as the backend reports it.
type ToolParseError struct {
	Raw string
	Msg string // the backend's message, for logs and nudges
}

func (e *ToolParseError) Error() string {
	return fmt.Sprintf("tool call could not be parsed: %s (raw: %q)", e.Msg, e.Raw)
}

// ModelInfo describes an installed model. ContextLength is zero when the backend did not report it.
type ModelInfo struct {
	Name          string
	SizeBytes     int64
	Params        string // parameter count as the backend spells it, for example "8.0B"
	Quant         string // quantisation, for example "Q4_K_M"
	ContextLength int64
}

type Client interface {
	// Chat streams one reply. onChunk (may be nil) sees fragments as they arrive, and the final Done chunk
	// of a completed reply. Cancelling ctx aborts the generation immediately (the connection is closed so
	// the backend stops) and returns ctx.Err(). A malformed tool call from the model is reported as a
	// *ToolParseError.
	Chat(ctx context.Context, model string, msgs []Message, tools []Tool, opts ChatOptions, onChunk func(Chunk)) (Message, error)
	ListModels(ctx context.Context) ([]ModelInfo, error)
}
