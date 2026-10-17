package runner

import (
	"strings"
	"unicode/utf8"

	"example.com/homecloud/internal/agentd/llm"
)

// Size limits for text the runner stores or sends to the model. Sizes are in bytes.
const (
	toolHeadBytes   = 4000  // head kept of a command's combined output shown to the model
	toolTailBytes   = 4000  // tail kept of a command's combined output shown to the model
	stepHeadBytes   = 2000  // head kept of a command's output in the stored step
	stepTailBytes   = 2000  // tail kept of a command's output in the stored step
	checkTailBytes  = 1500  // tail of the check command's output kept in the result
	maxResultBytes  = 32768 // cap for a task's stored result
	maxCommandBytes = 8192  // cap for a command stored in a step (the executed command is never cut)
	truncMarker     = "\n...[truncated]...\n"
)

// Clean makes s safe to store and to send to the model. Invalid UTF-8 becomes U+FFFD, and NUL bytes are
// removed because Postgres TEXT cannot hold them.
func Clean(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return strings.ToValidUTF8(s, "�")
}

// cleanMessage returns m with every string in it passed through Clean.
func cleanMessage(m llm.Message) llm.Message {
	m.Content = Clean(m.Content)
	m.Thinking = Clean(m.Thinking)
	m.ToolName = Clean(m.ToolName)
	if m.ToolCalls != nil {
		calls := make([]llm.ToolCall, len(m.ToolCalls))
		for i, c := range m.ToolCalls {
			calls[i] = llm.ToolCall{Name: Clean(c.Name), Arguments: cleanMap(c.Arguments)}
		}
		m.ToolCalls = calls
	}
	return m
}

func cleanMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[Clean(k)] = cleanValue(v)
	}
	return out
}

func cleanValue(v any) any {
	switch t := v.(type) {
	case string:
		return Clean(t)
	case map[string]any:
		return cleanMap(t)
	case []any:
		if t == nil {
			return t
		}
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cleanValue(e)
		}
		return out
	default:
		return v
	}
}

// headBytes returns at most the first n bytes of s, without splitting a UTF-8 sequence.
func headBytes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// tailBytes returns at most the last n bytes of s, without splitting a UTF-8 sequence.
func tailBytes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// clipMiddle keeps the first head and the last tail bytes of s with a marker between them, when s is longer
// than head+tail.
func clipMiddle(s string, head, tail int) string {
	if len(s) <= head+tail {
		return s
	}
	return headBytes(s, head) + truncMarker + tailBytes(s, tail)
}

// excerpt keeps the first and last stepHeadBytes/stepTailBytes of s, with no marker, when s is longer.
func excerpt(s string) string {
	if len(s) <= stepHeadBytes+stepTailBytes {
		return s
	}
	return headBytes(s, stepHeadBytes) + tailBytes(s, stepTailBytes)
}

// capResult limits a stored result to maxResultBytes, keeping its beginning.
func capResult(s string) string {
	if len(s) <= maxResultBytes {
		return s
	}
	return headBytes(s, maxResultBytes) + "\n...[truncated]"
}

// capCommand limits a command stored in a step. The command actually executed is never cut.
func capCommand(s string) string {
	if len(s) <= maxCommandBytes {
		return s
	}
	return headBytes(s, maxCommandBytes)
}
