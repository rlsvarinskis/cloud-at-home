package applypatch

import (
	"errors"
	"path"
	"strings"
	"testing"
)

// memFS is an in-memory FS that records the paths written and deleted.
type memFS struct {
	files   map[string]string
	writes  []string
	deletes []string
	failAt  string // Write or Delete of this path fails
}

func newMemFS(files map[string]string) *memFS {
	if files == nil {
		files = map[string]string{}
	}
	return &memFS{files: files}
}

func (m *memFS) Read(p string) ([]byte, bool, error) {
	s, ok := m.files[p]
	return []byte(s), ok, nil
}

func (m *memFS) Write(p string, data []byte) error {
	if p == m.failAt {
		return errors.New("disk full")
	}
	m.writes = append(m.writes, p)
	m.files[p] = string(data)
	return nil
}

func (m *memFS) Delete(p string) error {
	if p == m.failAt {
		return errors.New("busy")
	}
	m.deletes = append(m.deletes, p)
	delete(m.files, p)
	return nil
}

const root = "/w"

func patch(lines ...string) string {
	return strings.Join(append(append([]string{"*** Begin Patch"}, lines...), "*** End Patch"), "\n")
}

func TestApplyCases(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		patch   string
		want    map[string]string // expected files afterwards (relative to root, as keys)
		summary string            // expected summary, "" to skip
	}{
		{
			name:    "add file",
			patch:   patch("*** Add File: a.txt", "+hello", "+world"),
			want:    map[string]string{"a.txt": "hello\nworld\n"},
			summary: "Done!\nA a.txt",
		},
		{
			name:  "add creates nested paths",
			patch: patch("*** Add File: sub/dir/new.txt", "+hi"),
			want:  map[string]string{"sub/dir/new.txt": "hi\n"},
		},
		{
			name:    "delete file",
			files:   map[string]string{"f.txt": "x\n"},
			patch:   patch("*** Delete File: f.txt"),
			want:    map[string]string{},
			summary: "Done!\nD f.txt",
		},
		{
			name:    "single hunk",
			files:   map[string]string{"f.txt": "a\nb\nc\n"},
			patch:   patch("*** Update File: f.txt", "@@", " a", "-b", "+B", " c"),
			want:    map[string]string{"f.txt": "a\nB\nc\n"},
			summary: "Done!\nM f.txt",
		},
		{
			name:  "multiple hunks",
			files: map[string]string{"f.txt": "a\nb\nc\nd\ne\n"},
			patch: patch("*** Update File: f.txt", "@@", "-a", "+A", "@@", "-d", "+D"),
			want:  map[string]string{"f.txt": "A\nb\nc\nD\ne\n"},
		},
		{
			name:    "move file with edit",
			files:   map[string]string{"old.txt": "a\n"},
			patch:   patch("*** Update File: old.txt", "*** Move to: new/moved.txt", "@@", "-a", "+b"),
			want:    map[string]string{"new/moved.txt": "b\n"},
			summary: "Done!\nM old.txt -> new/moved.txt",
		},
		{
			name:  "move only",
			files: map[string]string{"old.txt": "a\n"},
			patch: patch("*** Update File: old.txt", "*** Move to: new.txt"),
			want:  map[string]string{"new.txt": "a\n"},
		},
		{
			name:  "context header selects location",
			files: map[string]string{"f.py": "def foo():\n    return 1\n\ndef bar():\n    return 1\n"},
			patch: patch("*** Update File: f.py", "@@ def bar():", "-    return 1", "+    return 2"),
			want:  map[string]string{"f.py": "def foo():\n    return 1\n\ndef bar():\n    return 2\n"},
		},
		{
			name:  "trailing whitespace tolerant match",
			files: map[string]string{"f.txt": "x = 1   \ny = 2\n"},
			patch: patch("*** Update File: f.txt", "@@", "-x = 1", "+x = 5"),
			want:  map[string]string{"f.txt": "x = 5\ny = 2\n"},
		},
		{
			name:  "fully whitespace tolerant match",
			files: map[string]string{"f.txt": "    y = 2\n"},
			patch: patch("*** Update File: f.txt", "@@", "-y = 2", "+y = 3"),
			want:  map[string]string{"f.txt": "y = 3\n"},
		},
		{
			name:  "exact match preferred over loose",
			files: map[string]string{"f.txt": "x  \nx\n"},
			patch: patch("*** Update File: f.txt", "@@", "-x", "+y"),
			want:  map[string]string{"f.txt": "x  \ny\n"},
		},
		{
			name:  "missing final newline preserved",
			files: map[string]string{"f.txt": "a\nb"},
			patch: patch("*** Update File: f.txt", "@@", "-b", "+c"),
			want:  map[string]string{"f.txt": "a\nc"},
		},
		{
			name:  "final newline kept when present",
			files: map[string]string{"f.txt": "a\nb\n"},
			patch: patch("*** Update File: f.txt", "@@", "-b", "+c"),
			want:  map[string]string{"f.txt": "a\nc\n"},
		},
		{
			name:  "pure addition appends at end",
			files: map[string]string{"f.txt": "a\nb\n"},
			patch: patch("*** Update File: f.txt", "@@", "+c"),
			want:  map[string]string{"f.txt": "a\nb\nc\n"},
		},
		{
			name:  "pure addition after context line",
			files: map[string]string{"f.py": "def f():\n    pass\nx\n"},
			patch: patch("*** Update File: f.py", "@@ def f():", "+    print(1)"),
			want:  map[string]string{"f.py": "def f():\n    print(1)\n    pass\nx\n"},
		},
		{
			name:  "end of file marker prefers last match",
			files: map[string]string{"f.txt": "a\nb\nb\n"},
			patch: patch("*** Update File: f.txt", "@@", "-b", "+c", "*** End of File"),
			want:  map[string]string{"f.txt": "a\nb\nc\n"},
		},
		{
			name:  "operations see earlier changes in the same patch",
			patch: patch("*** Add File: a.txt", "+one", "*** Update File: a.txt", "@@", "-one", "+two"),
			want:  map[string]string{"a.txt": "two\n"},
		},
		{
			name:  "CRLF patch tolerated",
			patch: "*** Begin Patch\r\n*** Add File: c.txt\r\n+one\r\n*** End Patch\r\n",
			want:  map[string]string{"c.txt": "one\n"},
		},
		{
			name:  "missing begin and end tolerated",
			patch: "*** Add File: a.txt\n+1",
			want:  map[string]string{"a.txt": "1\n"},
		},
		{
			name:  "empty file gets lines",
			files: map[string]string{"e.txt": ""},
			patch: patch("*** Update File: e.txt", "@@", "+first"),
			want:  map[string]string{"e.txt": "first\n"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newMemFS(underRoot(tt.files))
			summary, err := Apply(tt.patch, fs, root)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if tt.summary != "" && summary != tt.summary {
				t.Errorf("summary = %q, want %q", summary, tt.summary)
			}
			got := map[string]string{}
			for p, c := range fs.files {
				rel := strings.TrimPrefix(p, root+"/")
				got[rel] = c
			}
			if !equalMaps(got, tt.want) {
				t.Errorf("files = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyErrors(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		patch string
		msg   string
	}{
		{"add existing", map[string]string{"a.txt": "keep\n"}, patch("*** Add File: a.txt", "+x"), "already exists"},
		{"delete missing", nil, patch("*** Delete File: nope.txt"), "file not found"},
		{"update missing", nil, patch("*** Update File: nope.txt", "@@", "-a", "+b"), "file not found"},
		{"hunk not found names file and line", map[string]string{"f.txt": "one\ntwo\n"},
			patch("*** Update File: f.txt", "@@", "-missing", "+x"), "f.txt"},
		{"context not found", map[string]string{"f.txt": "one\n"},
			patch("*** Update File: f.txt", "@@ nope", "-one", "+x"), "context line 'nope' not found"},
		{"no file header", nil, "hello", "unexpected line"},
		{"bad add line", nil, "*** Add File: a.txt\nnot plus", "must start with '+'"},
		{"update without hunks", map[string]string{"a": "x"}, patch("*** Update File: a"), "has no hunks"},
		{"move onto existing", map[string]string{"a": "x\n", "b": "y\n"},
			patch("*** Update File: a", "*** Move to: b", "@@", "-x", "+z"), "already exists"},
		{"not utf-8", map[string]string{"bin": "\xff\xfe"}, patch("*** Update File: bin", "@@", "+x"), "not UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newMemFS(underRoot(tt.files))
			_, err := Apply(tt.patch, fs, root)
			if err == nil || !strings.Contains(err.Error(), tt.msg) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.msg)
			}
		})
	}
}

func TestFailureIsAtomic(t *testing.T) {
	fs := newMemFS(underRoot(map[string]string{"keep.txt": "one\ntwo\n", "gone.txt": "bye\n"}))
	_, err := Apply(patch(
		"*** Add File: new.txt", "+hi",
		"*** Delete File: gone.txt",
		"*** Update File: keep.txt", "@@", "-missing", "+x",
	), fs, root)
	if err == nil {
		t.Fatal("want an error")
	}
	if len(fs.writes) != 0 || len(fs.deletes) != 0 {
		t.Fatalf("writes %v deletes %v; want none", fs.writes, fs.deletes)
	}
	if fs.files["/w/gone.txt"] != "bye\n" || fs.files["/w/keep.txt"] != "one\ntwo\n" {
		t.Fatalf("files changed: %q", fs.files)
	}
}

func TestWriteFailureIsReported(t *testing.T) {
	fs := newMemFS(nil)
	fs.failAt = "/w/b.txt"
	_, err := Apply(patch("*** Add File: a.txt", "+1", "*** Add File: b.txt", "+2"), fs, root)
	if err == nil || !strings.Contains(err.Error(), "write /w/b.txt") {
		t.Fatalf("err = %v", err)
	}
}

func TestAbsolutePathsAndRootBase(t *testing.T) {
	fs := newMemFS(nil)
	if _, err := Apply("*** Add File: /abs/x.txt\n+y", fs, "/elsewhere"); err != nil {
		t.Fatal(err)
	}
	if fs.files["/abs/x.txt"] != "y\n" {
		t.Fatalf("files = %q", fs.files)
	}
	fs = newMemFS(nil)
	if _, err := Apply("*** Add File: ../escape.txt\n+x", fs, "/w/root"); err != nil {
		t.Fatal(err)
	}
	if fs.files[path.Clean("/w/root/../escape.txt")] != "x\n" {
		t.Fatalf("files = %q", fs.files)
	}
}

func TestParseOperations(t *testing.T) {
	ops, err := Parse(patch("*** Add File: a", "+1", "*** Update File: b", "*** Move to: c", "@@ ctx @@", "-x", "+y", "*** End of File", "*** Delete File: d"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 || ops[0].Kind != "add" || ops[1].Move != "c" || !ops[1].Hunks[0].EOF ||
		ops[1].Hunks[0].Ctx != "ctx" || ops[2].Kind != "delete" {
		t.Fatalf("ops = %+v", ops)
	}
}

// underRoot copies m with every key placed under root, as the patch's relative paths resolve there.
func underRoot(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[root+"/"+k] = v
	}
	return out
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
