package llm

import (
	"context"
	"errors"
	"sync"
)

var _ Client = (*Fake)(nil)

// FakeReply is one scripted response consumed by a Fake.Chat call.
type FakeReply struct {
	// Message is returned when Err is nil.
	Message Message
	// Chunks are delivered to onChunk, in order, before the reply completes.
	Chunks []Chunk
	// Err, if set, is returned instead of Message (after Chunks are delivered).
	Err error
	// Block, if non-nil, makes Chat wait until it is closed or the context is done.
	Block <-chan struct{}
}

// FakeCall records one Fake.Chat invocation.
type FakeCall struct {
	Model    string
	Messages []Message
	Tools    []Tool
}

// Fake is a scripted Client for tests. Each Chat call consumes the next entry of Script and is recorded
// in Calls. Fake is safe for concurrent use; set Models and Script before sharing it.
type Fake struct {
	mu sync.Mutex
	// Models is returned by ListModels.
	Models []ModelInfo
	// Script holds the replies Chat hands out, first to last.
	Script []FakeReply
	// Calls records every Chat invocation that got past the initial context check, in order.
	Calls []FakeCall
}

// Say returns a scripted reply consisting of a plain assistant message with the given text.
func Say(text string) FakeReply {
	return FakeReply{Message: Message{Role: "assistant", Content: text}}
}

// CallTool returns a scripted reply in which the assistant calls the named tool with args.
func CallTool(name string, args map[string]any) FakeReply {
	return FakeReply{Message: Message{
		Role:      "assistant",
		ToolCalls: []ToolCall{{Name: name, Arguments: args}},
	}}
}

// Chat consumes the next scripted reply. It fails if the script is exhausted, and returns ctx.Err() without
// consuming a reply or recording a call if ctx is already cancelled. A blocked reply returns ctx.Err() if
// the context ends before the block is released.
func (f *Fake) Chat(ctx context.Context, model string, msgs []Message, tools []Tool, onChunk func(Chunk)) (Message, error) {
	if err := ctx.Err(); err != nil {
		return Message{}, err
	}

	f.mu.Lock()
	f.Calls = append(f.Calls, FakeCall{Model: model, Messages: cloneMessages(msgs), Tools: cloneTools(tools)})
	if len(f.Script) == 0 {
		f.mu.Unlock()
		return Message{}, errors.New("fake: script exhausted")
	}
	reply := f.Script[0]
	f.Script = f.Script[1:]
	f.mu.Unlock()

	if reply.Block != nil {
		select {
		case <-reply.Block:
		case <-ctx.Done():
			return Message{}, ctx.Err()
		}
	}
	if onChunk != nil {
		for _, c := range reply.Chunks {
			onChunk(c)
		}
	}
	if reply.Err != nil {
		return Message{}, reply.Err
	}
	return cloneMessage(reply.Message), nil
}

// ListModels returns a copy of Models.
func (f *Fake) ListModels(ctx context.Context) ([]ModelInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ModelInfo(nil), f.Models...), nil
}

func cloneMessages(msgs []Message) []Message {
	if msgs == nil {
		return nil
	}
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		out[i] = cloneMessage(m)
	}
	return out
}

func cloneMessage(m Message) Message {
	if m.ToolCalls != nil {
		calls := make([]ToolCall, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			calls[i] = ToolCall{Name: tc.Name, Arguments: cloneMap(tc.Arguments)}
		}
		m.ToolCalls = calls
	}
	return m
}

func cloneTools(tools []Tool) []Tool {
	if tools == nil {
		return nil
	}
	out := make([]Tool, len(tools))
	for i, t := range tools {
		out[i] = Tool{Name: t.Name, Description: t.Description, Parameters: cloneMap(t.Parameters)}
	}
	return out
}

// cloneMap deep-copies a JSON-like map so recorded calls cannot be changed by later mutation.
func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneMap(t)
	case []any:
		if t == nil {
			return t
		}
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cloneValue(e)
		}
		return out
	default:
		return v
	}
}
