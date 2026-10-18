// Package toolcall repairs and normalises the tool calls that language models make. Local models often emit
// calls that are slightly malformed (raw newlines in strings, a missing closing brace, a leaked harmony-format
// call in the text, an argv list instead of a command). These helpers recover the intended call, or report why
// they cannot. They are ported from the Python gateway's lenient tool-call parsing.
package toolcall

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Tool names a call can be normalised to.
const (
	Bash       = "bash"
	ApplyPatch = "apply_patch"
)

// Call is a normalised tool call. Exactly one of Command (Tool == Bash) or Patch (Tool == ApplyPatch) is set.
type Call struct {
	Tool    string
	Command string
	Patch   string
}

// HarmonyCall is one call found in harmony-style text: the (unnormalised) name and the raw JSON object text.
type HarmonyCall struct {
	Name string
	Args string
}

var (
	prefixes     = []string{"functions.", "repo_browser."}
	pythonNames  = map[string]bool{"python": true, "python3": true, "ipython": true, "code_interpreter": true}
	bashNames    = map[string]bool{"bash": true, "shell": true, "sh": true, "run": true, "run_command": true, "execute": true, "exec": true, "container.exec": true, "container.run": true, "terminal": true}
	patchNames   = map[string]bool{"apply_patch": true, "applypatch": true, "patch": true, "edit": true}
	bashKeys     = []string{"command", "cmd", "code", "script", "input", "args", "text", "source"}
	patchKeys    = []string{"patch", "input", "diff", "text", "content"}
	workdirKeys  = []string{"workdir", "cwd", "working_directory"}
	harmonyTo    = regexp.MustCompile(`to=([\w.\-]+)`)
	harmonyGap   = regexp.MustCompile(`^(?:\s|<\|\w+\|>|json)*$`)
	safeShellArg = regexp.MustCompile(`^[\w@%+=:,./-]+$`)
)

// LenientJSON parses the first JSON object or array in text, tolerating junk around it. Trailing text is ignored,
// raw control characters inside strings are escaped, and missing closing brackets at the end are appended. A JSON
// string whose content is itself a JSON object or array is decoded once more. It returns an error when nothing
// can be decoded.
func LenientJSON(text string) (any, error) {
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, `"`) {
		var inner string
		if err := json.Unmarshal([]byte(s), &inner); err == nil {
			if t := strings.TrimSpace(inner); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
				if v, err := decodeFirst(inner); err == nil {
					return v, nil
				}
			}
			return inner, nil
		}
	}
	return decodeFirst(text)
}

// decodeFirst decodes the first JSON value starting at the first '{' or '[' of text, with repair fallbacks.
func decodeFirst(text string) (any, error) {
	start := strings.IndexAny(text, "[{")
	if start < 0 {
		return nil, errors.New("no JSON object or array found")
	}
	v, firstErr := decodeValue(text[start:])
	if firstErr == nil {
		return v, nil
	}
	body := escapeControls(text[start:])
	if v, err := decodeValue(body); err == nil {
		return v, nil
	}
	if v, err := decodeValue(closeOpen(body)); err == nil {
		return v, nil
	}
	return nil, fmt.Errorf("invalid JSON: %v", firstErr)
}

// decodeValue decodes the first JSON value in s, ignoring whatever follows it.
func decodeValue(s string) (any, error) {
	var v any
	if err := json.NewDecoder(strings.NewReader(s)).Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// escapeControls escapes raw control characters inside JSON string literals. Control characters are ASCII, so
// working on bytes is safe for UTF-8 input.
func escapeControls(s string) string {
	var b strings.Builder
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			case c < ' ':
				switch c {
				case '\n':
					b.WriteString(`\n`)
				case '\r':
					b.WriteString(`\r`)
				case '\t':
					b.WriteString(`\t`)
				default:
					fmt.Fprintf(&b, `\u%04x`, c)
				}
				continue
			}
		} else if c == '"' {
			inStr = true
		}
		b.WriteByte(c)
	}
	return b.String()
}

// closeOpen appends the closers still needed by the brackets left open at the end of s. An unterminated string
// is returned unchanged, as it cannot be repaired by closing brackets.
func closeOpen(s string) string {
	var stack []byte
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr:
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{':
			stack = append(stack, '}')
		case c == '[':
			stack = append(stack, ']')
		case (c == '}' || c == ']') && len(stack) > 0:
			stack = stack[:len(stack)-1]
		}
	}
	if inStr || len(stack) == 0 {
		return s
	}
	out := []byte(s)
	for i := len(stack) - 1; i >= 0; i-- {
		out = append(out, stack[i])
	}
	return string(out)
}

// Normalize turns a model's tool call into a canonical bash or apply_patch call. args may be a map (decoded JSON
// arguments), a list (argv, or patch lines), or a string holding JSON; a non-JSON string is the raw main argument.
// Python-flavoured names run the code through a python3 heredoc, and a workdir/cwd key prefixes a cd. Unknown tools
// and missing command or patch text are errors.
func Normalize(name string, args any) (Call, error) {
	tool, alias := canonical(name)
	if tool == "" {
		return Call{}, fmt.Errorf("unknown tool '%s'; available: bash, apply_patch", name)
	}
	var raw *string
	switch a := args.(type) {
	case string:
		parsed, ok := tryParse(a)
		if ok {
			if s, isStr := parsed.(string); isStr {
				raw = &s
			} else if m, isMap := parsed.(map[string]any); isMap {
				args = m
			} else if l, isList := parsed.([]any); isList {
				args = l
			} else {
				args = nil
			}
		} else {
			raw = &a
			args = nil
		}
	}
	if raw == nil {
		switch a := args.(type) {
		case []any:
			if tool == Bash {
				args = map[string]any{"command": a}
			} else {
				args = map[string]any{"patch": a}
			}
		}
	}
	m, _ := args.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	if tool == Bash {
		cmd, err := bashCommand(alias, m, raw)
		if err != nil {
			return Call{}, err
		}
		return Call{Tool: Bash, Command: cmd}, nil
	}
	patch, err := patchText(m, raw)
	if err != nil {
		return Call{}, err
	}
	return Call{Tool: ApplyPatch, Patch: patch}, nil
}

// canonical maps a model-supplied name to Bash, ApplyPatch or "", and also returns the lowercased name without
// its prefixes.
func canonical(name string) (string, string) {
	n := strings.ToLower(strings.TrimSpace(name))
	for again := true; again; {
		again = false
		for _, p := range prefixes {
			if strings.HasPrefix(n, p) {
				n = n[len(p):]
				again = true
				break
			}
		}
	}
	switch {
	case bashNames[n] || pythonNames[n]:
		return Bash, n
	case patchNames[n]:
		return ApplyPatch, n
	}
	return "", n
}

// tryParse parses s as JSON when it looks like JSON (an object, array or string).
func tryParse(s string) (any, bool) {
	t := strings.TrimSpace(s)
	if t == "" || !strings.ContainsAny(t[:1], `{["`) {
		return nil, false
	}
	v, err := LenientJSON(s)
	if err != nil {
		return nil, false
	}
	return v, true
}

// firstValue returns the first present, non-empty value among keys, or nil.
func firstValue(m map[string]any, keys []string) any {
	for _, k := range keys {
		switch v := m[k].(type) {
		case nil:
		case string:
			if v != "" {
				return v
			}
		case []any:
			if len(v) > 0 {
				return v
			}
		default:
			return v
		}
	}
	return nil
}

// commandFromList turns an argv list into a shell command. ["bash","-lc",X] (or sh/-c) becomes X.
func commandFromList(items []any) string {
	strs := make([]string, len(items))
	for i, it := range items {
		strs[i] = fmt.Sprint(it)
	}
	if len(strs) == 3 && (strs[0] == "bash" || strs[0] == "sh") && (strs[1] == "-lc" || strs[1] == "-c") {
		return strs[2]
	}
	quoted := make([]string, len(strs))
	for i, s := range strs {
		quoted[i] = shellQuote(s)
	}
	return strings.Join(quoted, " ")
}

// shellQuote quotes s for a POSIX shell, like Python's shlex.quote.
func shellQuote(s string) string {
	if s != "" && safeShellArg.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// pythonHeredoc runs code with python3 from a bash heredoc whose terminator does not occur in code.
func pythonHeredoc(code string) string {
	code = strings.TrimRight(code, "\n")
	term := "PYEOF"
	for i := 1; strings.Contains(code, term); i++ {
		term = fmt.Sprintf("PYEOF%d", i)
	}
	return fmt.Sprintf("python3 - <<'%s'\n%s\n%s", term, code, term)
}

// bashCommand builds the final command text from the normalised arguments.
func bashCommand(alias string, m map[string]any, raw *string) (string, error) {
	var cmd string
	if raw != nil {
		cmd = *raw
	} else {
		switch v := firstValue(m, bashKeys).(type) {
		case string:
			cmd = v
		case []any:
			cmd = commandFromList(v)
		}
	}
	if strings.TrimSpace(cmd) == "" {
		return "", errors.New("bash needs a 'command' string")
	}
	if pythonNames[alias] {
		cmd = pythonHeredoc(cmd)
	}
	if wd, ok := firstValue(m, workdirKeys).(string); ok && strings.TrimSpace(wd) != "" {
		cmd = "cd " + shellQuote(wd) + " && " + cmd
	}
	return cmd, nil
}

// patchText extracts the patch text from the normalised arguments.
func patchText(m map[string]any, raw *string) (string, error) {
	if raw != nil {
		if strings.TrimSpace(*raw) == "" {
			return "", errors.New("apply_patch needs a 'patch' string")
		}
		return *raw, nil
	}
	switch v := firstValue(m, patchKeys).(type) {
	case string:
		return v, nil
	case []any:
		lines := make([]string, len(v))
		for i, l := range v {
			lines[i] = fmt.Sprint(l)
		}
		return strings.Join(lines, "\n"), nil
	}
	return "", errors.New("apply_patch needs a 'patch' string")
}

// ParseHarmony finds harmony-format tool calls ("to=NAME ... {json}") that leaked into a model's text. It returns
// them in order, with the raw object text, or nil when there are none.
func ParseHarmony(text string) []HarmonyCall {
	var calls []HarmonyCall
	for _, loc := range harmonyTo.FindAllStringSubmatchIndex(text, -1) {
		if loc[0] > 0 && isWordByte(text[loc[0]-1]) {
			continue // Python's (?<!\w): "to=" must not be part of a longer word
		}
		name := text[loc[2]:loc[3]]
		brace := strings.IndexByte(text[loc[1]:], '{')
		if brace < 0 {
			continue
		}
		brace += loc[1]
		if !harmonyGap.MatchString(text[loc[1]:brace]) {
			continue
		}
		calls = append(calls, HarmonyCall{Name: name, Args: text[brace:objectEnd(text, brace)]})
	}
	return calls
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

// objectEnd returns the index just past the '}' that closes the object opened at s[start], or len(s).
func objectEnd(s string, start int) int {
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr:
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(s)
}
