// Package llm abstracts the language-model backend (Ollama today) so the agent loop can be tested
// with a scripted fake and other backends can be added.
package llm

import "context"

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

// Chunk is a streamed fragment of the reply, delivered as it is generated.
type Chunk struct {
	Content  string
	Thinking string
}

type ModelInfo struct {
	Name      string
	SizeBytes int64
}

type Client interface {
	// Chat streams one reply. onChunk (may be nil) sees fragments as they arrive. Cancelling ctx aborts the
	// generation immediately (the connection is closed so the backend stops) and returns ctx.Err().
	Chat(ctx context.Context, model string, msgs []Message, tools []Tool, onChunk func(Chunk)) (Message, error)
	ListModels(ctx context.Context) ([]ModelInfo, error)
}
