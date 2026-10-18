// Package applypatch applies OpenAI-style patches (the apply_patch tool that gpt-oss models are trained on) to
// files. The parser and the hunk matcher are pure. Apply works over the small FS interface, so the same code runs
// against an in-memory tree in tests and against files inside a VM in the runner. The patch is atomic: every
// operation is computed first, and files are written or removed only when all of them succeeded.
package applypatch

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// FS is the file system a patch acts on. Paths are slash-separated and already absolute.
type FS interface {
	// Read returns the content of path. exists is false (with a nil error) when there is no such file.
	Read(path string) (data []byte, exists bool, err error)
	// Write creates or replaces path, creating parent directories as needed.
	Write(path string, data []byte) error
	// Delete removes path. A missing file is not an error.
	Delete(path string) error
}

const (
	beginMarker = "*** Begin Patch"
	endMarker   = "*** End Patch"
	addHeader   = "*** Add File: "
	updHeader   = "*** Update File: "
	delHeader   = "*** Delete File: "
	moveHeader  = "*** Move to: "
	eofMarker   = "*** End of File"
)

// Op is one parsed file operation.
type Op struct {
	Kind    string // "add", "delete" or "update"
	Path    string
	Move    string // update only: the new path, or ""
	Content []string
	Hunks   []Hunk
}

// Hunk is one '@@' block of an update: the lines to find (Old), the lines to put in their place (New), an optional
// line to search for first (Ctx), and whether it is anchored at the end of the file.
type Hunk struct {
	Ctx      string
	HasCtx   bool
	Old, New []string
	EOF      bool
}

// Parse turns patch text into operations without touching any file. A missing Begin/End marker is tolerated and
// CRLF line endings are accepted. It returns an error when no file operation is found.
func Parse(patch string) ([]Op, error) {
	lines := strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n")
	start := 0
	for i, l := range lines {
		if strings.TrimSpace(l) == beginMarker {
			start = i + 1
			break
		}
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == endMarker {
			end = i
			break
		}
	}
	body := lines[start:end]
	for len(body) > 0 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
	}
	var ops []Op
	for i := 0; i < len(body); {
		line := body[i]
		i++
		switch {
		case strings.HasPrefix(line, addHeader):
			op := Op{Kind: "add", Path: strings.TrimSpace(line[len(addHeader):])}
			for i < len(body) && !isBoundary(body[i]) {
				if body[i] != "" && body[i][0] != '+' {
					return nil, fmt.Errorf("Add File %s: line must start with '+': %q", op.Path, body[i])
				}
				op.Content = append(op.Content, body[i][min(1, len(body[i])):])
				i++
			}
			ops = append(ops, op)
		case strings.HasPrefix(line, delHeader):
			ops = append(ops, Op{Kind: "delete", Path: strings.TrimSpace(line[len(delHeader):])})
		case strings.HasPrefix(line, updHeader):
			op := Op{Kind: "update", Path: strings.TrimSpace(line[len(updHeader):])}
			if i < len(body) && strings.HasPrefix(body[i], moveHeader) {
				op.Move = strings.TrimSpace(body[i][len(moveHeader):])
				i++
			}
			for i < len(body) && !isBoundary(body[i]) {
				var h Hunk
				if strings.HasPrefix(body[i], "@@") {
					ctx := strings.TrimSpace(body[i][2:])
					ctx = strings.TrimSpace(strings.TrimSuffix(ctx, "@@"))
					h.Ctx, h.HasCtx = ctx, ctx != ""
					i++
				}
				for i < len(body) && !strings.HasPrefix(body[i], "@@") && !isBoundary(body[i]) {
					l := body[i]
					i++
					switch {
					case strings.HasPrefix(l, eofMarker):
						h.EOF = true
					case strings.HasPrefix(l, "-"):
						h.Old = append(h.Old, l[1:])
					case strings.HasPrefix(l, "+"):
						h.New = append(h.New, l[1:])
					case strings.HasPrefix(l, " ") || l == "":
						h.Old = append(h.Old, l[min(1, len(l)):])
						h.New = append(h.New, l[min(1, len(l)):])
					default:
						return nil, fmt.Errorf("Update File %s: bad hunk line %q", op.Path, l)
					}
				}
				op.Hunks = append(op.Hunks, h)
			}
			if len(op.Hunks) == 0 && op.Move == "" {
				return nil, fmt.Errorf("Update File %s has no hunks", op.Path)
			}
			ops = append(ops, op)
		case strings.TrimSpace(line) != "":
			return nil, fmt.Errorf("unexpected line in patch: %q", line)
		}
	}
	if len(ops) == 0 {
		return nil, errors.New("no file operations found in patch (expected an '*** Add/Update/Delete File:' header)")
	}
	return ops, nil
}

func isBoundary(line string) bool {
	return strings.HasPrefix(line, addHeader) || strings.HasPrefix(line, updHeader) ||
		strings.HasPrefix(line, delHeader) || strings.TrimSpace(line) == endMarker
}

// locate finds the index of old in lines at or after start. It tries an exact match, then one that ignores
// trailing whitespace, then one that ignores all leading and trailing whitespace. An end-of-file hunk tries the
// last possible position first. It returns -1 when nothing matches.
func locate(lines, old []string, start int, eof bool) int {
	last := len(lines) - len(old)
	norms := []func(string) string{
		func(s string) string { return s },
		func(s string) string { return strings.TrimRightFunc(s, unicode.IsSpace) },
		func(s string) string { return strings.TrimFunc(s, unicode.IsSpace) },
	}
	for _, norm := range norms {
		var starts []int
		if eof && last >= start {
			starts = append(starts, last)
		}
		for i := start; i <= last; i++ {
			starts = append(starts, i)
		}
		for _, i := range starts {
			if matchAt(lines, old, i, norm) {
				return i
			}
		}
	}
	return -1
}

func matchAt(lines, old []string, at int, norm func(string) string) bool {
	for k, o := range old {
		if norm(lines[at+k]) != norm(o) {
			return false
		}
	}
	return true
}

// applyHunks applies the hunks of one update to the text of a file and returns the new text.
func applyHunks(text string, hunks []Hunk, path string) (string, error) {
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	trailingNL := strings.HasSuffix(text, "\n") || text == ""
	pos := 0
	for _, h := range hunks {
		ctxAt := -1
		if h.HasCtx {
			for i := pos; i < len(lines); i++ {
				if strings.TrimSpace(lines[i]) == h.Ctx {
					ctxAt = i
					break
				}
			}
			if ctxAt < 0 {
				return "", fmt.Errorf("context line '%s' not found in %s", h.Ctx, path)
			}
			pos = ctxAt
		}
		if len(h.Old) == 0 {
			at := len(lines)
			if ctxAt >= 0 {
				at = ctxAt + 1
			}
			lines = splice(lines, at, 0, h.New)
			pos = at + len(h.New)
			continue
		}
		at := locate(lines, h.Old, pos, h.EOF)
		if at < 0 {
			return "", fmt.Errorf("could not apply hunk to %s: old line '%s' not found at or after line %d",
				path, h.Old[0], pos+1)
		}
		lines = splice(lines, at, len(h.Old), h.New)
		pos = at + len(h.New)
	}
	out := strings.Join(lines, "\n")
	if len(lines) > 0 && trailingNL {
		out += "\n"
	}
	return out, nil
}

// splice replaces n lines at index at with repl.
func splice(lines []string, at, n int, repl []string) []string {
	out := make([]string, 0, len(lines)-n+len(repl))
	out = append(out, lines[:at]...)
	out = append(out, repl...)
	return append(out, lines[at+n:]...)
}

// Apply applies a patch to fs. Relative paths resolve against root; absolute paths are used as they are. Every
// operation is computed before anything is written, so a patch that fails leaves fs untouched. The summary has
// one line per operation: "A path", "M path" (or "M old -> new" for a move) and "D path", after a "Done!" line.
func Apply(patch string, fs FS, root string) (string, error) {
	ops, err := Parse(patch)
	if err != nil {
		return "", err
	}
	st := &staging{fs: fs, pending: map[string]*string{}}
	target := func(p string) string {
		if path.IsAbs(p) {
			return path.Clean(p)
		}
		return path.Join(root, p)
	}
	summary := []string{"Done!"}
	for _, op := range ops {
		p := target(op.Path)
		switch op.Kind {
		case "add":
			if cur, err := st.current(p); err != nil {
				return "", err
			} else if cur != nil {
				return "", fmt.Errorf("cannot add %s: file already exists", op.Path)
			}
			content := joinLines(op.Content)
			st.set(p, &content)
			summary = append(summary, "A "+op.Path)
		case "delete":
			if cur, err := st.current(p); err != nil {
				return "", err
			} else if cur == nil {
				return "", fmt.Errorf("cannot delete %s: file not found", op.Path)
			}
			st.set(p, nil)
			summary = append(summary, "D "+op.Path)
		case "update":
			cur, err := st.current(p)
			if err != nil {
				return "", err
			}
			if cur == nil {
				return "", fmt.Errorf("cannot update %s: file not found", op.Path)
			}
			text, err := applyHunks(*cur, op.Hunks, op.Path)
			if err != nil {
				return "", err
			}
			if op.Move == "" {
				st.set(p, &text)
				summary = append(summary, "M "+op.Path)
				continue
			}
			dest := target(op.Move)
			if dest != p {
				if d, err := st.current(dest); err != nil {
					return "", err
				} else if d != nil {
					return "", fmt.Errorf("cannot move %s: %s already exists", op.Path, op.Move)
				}
			}
			st.set(p, nil)
			st.set(dest, &text)
			summary = append(summary, "M "+op.Path+" -> "+op.Move)
		}
	}
	if err := st.commit(); err != nil {
		return "", err
	}
	return strings.Join(summary, "\n"), nil
}

// joinLines joins added lines, each followed by a newline.
func joinLines(lines []string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// staging holds the pending state of every path a patch touches, in the order they were first touched.
type staging struct {
	fs      FS
	order   []string
	pending map[string]*string // nil value: the path will be removed
}

func (s *staging) set(p string, content *string) {
	if _, ok := s.pending[p]; !ok {
		s.order = append(s.order, p)
	}
	s.pending[p] = content
}

// current returns the content a path will have once the patch is applied: the pending content if the patch
// touched it, else the file on disk. nil means there is no file. Files must be UTF-8 text.
func (s *staging) current(p string) (*string, error) {
	if c, ok := s.pending[p]; ok {
		return c, nil
	}
	data, exists, err := s.fs.Read(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	if !exists {
		return nil, nil
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%s is not UTF-8 text", p)
	}
	text := string(data)
	return &text, nil
}

// commit writes and removes the staged paths.
func (s *staging) commit() error {
	for _, p := range s.order {
		c := s.pending[p]
		var err error
		if c == nil {
			err = s.fs.Delete(p)
		} else {
			err = s.fs.Write(p, []byte(*c))
		}
		if err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
	}
	return nil
}
