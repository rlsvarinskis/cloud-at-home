package toolcall

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestLenientJSON(t *testing.T) {
	tests := []struct {
		name, in string
		want     any
	}{
		{"trailing bracket ignored", `{"cmd":"ls"}]`, map[string]any{"cmd": "ls"}},
		{"trailing text ignored", `{"a":1} trailing`, map[string]any{"a": 1.0}},
		{"junk before json", "Sure! ```json\n{\"cmd\": \"ls\"}\n```", map[string]any{"cmd": "ls"}},
		{"top level array", "[1, 2] x", []any{1.0, 2.0}},
		{"string containing json is parsed once", `"{\"cmd\": \"ls\"}"`, map[string]any{"cmd": "ls"}},
		{"string containing json array", `"[\"a\", \"b\"]"`, []any{"a", "b"}},
		{"plain json string is returned as str", `"hello"`, "hello"},
		{"raw newlines inside string repaired", "{\"cmd\":\"python3 - <<'EOF'\nprint(1)\nEOF\"}", map[string]any{"cmd": "python3 - <<'EOF'\nprint(1)\nEOF"}},
		{"raw tab and carriage return repaired", "{\"a\":\"x\ty\r\nz\"}", map[string]any{"a": "x\ty\r\nz"}},
		{"missing final brace", `{"cmd":"ls"`, map[string]any{"cmd": "ls"}},
		{"missing nested closers", `{"a":{"b":[1,2]`, map[string]any{"a": map[string]any{"b": []any{1.0, 2.0}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LenientJSON(tt.in)
			if err != nil {
				t.Fatalf("LenientJSON(%q): %v", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("LenientJSON(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestLenientJSONStringIsUnwrappedOnlyOnce(t *testing.T) {
	// A string holding a string holding JSON stays a string after one unwrap.
	got, err := LenientJSON(`"\"{\\\"a\\\": 1}\""`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(string); !ok {
		t.Fatalf("got %#v, want a string", got)
	}
}

func TestLenientJSONErrors(t *testing.T) {
	for _, in := range []string{`{"cmd": "ls`, `{"a" 1}`, "total garbage", "{{{", ""} {
		if v, err := LenientJSON(in); err == nil {
			t.Errorf("LenientJSON(%q) = %#v, want an error", in, v)
		}
	}
}

func TestErrorMessageNamesTheJSONProblem(t *testing.T) {
	_, err := LenientJSON(`{"a" 1}`)
	if err == nil || !strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("err = %v, want the JSON error", err)
	}
}

// call is one normalisation case.
type call struct {
	name string
	args any
	tool string
	text string // the command or the patch
}

func runNormalize(t *testing.T, cases []call) {
	t.Helper()
	for _, c := range cases {
		got, err := Normalize(c.name, c.args)
		if err != nil {
			t.Errorf("Normalize(%q, %#v): %v", c.name, c.args, err)
			continue
		}
		want := Call{Tool: c.tool}
		if c.tool == Bash {
			want.Command = c.text
		} else {
			want.Patch = c.text
		}
		if got != want {
			t.Errorf("Normalize(%q, %#v) = %+v, want %+v", c.name, c.args, got, want)
		}
	}
}

func TestNormalizeBashNamesAndPrefixes(t *testing.T) {
	var cases []call
	for _, n := range []string{"bash", "shell", "sh", "run", "run_command", "execute", "exec",
		"container.exec", "container.run", "terminal", "BASH", "functions.bash",
		"repo_browser.shell", "Functions.Exec"} {
		cases = append(cases, call{n, map[string]any{"command": "ls"}, Bash, "ls"})
	}
	runNormalize(t, cases)
}

func TestNormalizeArgumentKeys(t *testing.T) {
	var cases []call
	for _, k := range []string{"command", "cmd", "code", "script", "input", "args", "text", "source"} {
		cases = append(cases, call{"bash", map[string]any{k: "ls"}, Bash, "ls"})
	}
	cases = append(cases, call{"bash", map[string]any{"command": "", "cmd": "ls"}, Bash, "ls"})
	runNormalize(t, cases)
}

func TestNormalizeArgvForms(t *testing.T) {
	runNormalize(t, []call{
		{"container.exec", map[string]any{"cmd": []any{"bash", "-lc", "ls -la"}}, Bash, "ls -la"},
		{"container.exec", map[string]any{"cmd": []any{"sh", "-c", "echo hi"}}, Bash, "echo hi"},
		{"container.exec", map[string]any{"cmd": []any{"bash", "-c", "pwd"}}, Bash, "pwd"},
		{"bash", map[string]any{"cmd": []any{"ls", "-la", "my file"}}, Bash, "ls -la 'my file'"},
		{"bash", []any{"bash", "-lc", "ls"}, Bash, "ls"},
	})
}

func TestNormalizeStringArgs(t *testing.T) {
	runNormalize(t, []call{
		{"shell", `{"cmd": "ls"}`, Bash, "ls"},
		{"bash", "ls -la", Bash, "ls -la"},
		{"bash", "[ -f x ] && echo yes", Bash, "[ -f x ] && echo yes"},
		{"edit", "*** Begin Patch", ApplyPatch, "*** Begin Patch"},
	})
}

func TestNormalizeWorkdir(t *testing.T) {
	runNormalize(t, []call{
		{"bash", map[string]any{"cmd": "ls", "workdir": "/tmp/my dir"}, Bash, "cd '/tmp/my dir' && ls"},
		{"bash", map[string]any{"cmd": "ls", "workdir": ""}, Bash, "ls"},
	})
}

func TestNormalizePythonHeredoc(t *testing.T) {
	for _, n := range []string{"python", "python3", "ipython", "code_interpreter"} {
		got, err := Normalize(n, map[string]any{"code": "print(1+1)"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Tool != Bash {
			t.Fatalf("%s: tool = %s", n, got.Tool)
		}
		if _, err := exec.LookPath("python3"); err == nil {
			out, err := exec.Command("bash", "-c", got.Command).Output()
			if err != nil {
				t.Fatalf("%s: running %q: %v", n, got.Command, err)
			}
			if strings.TrimSpace(string(out)) != "2" {
				t.Fatalf("%s: output %q", n, out)
			}
		}
	}
	got, _ := Normalize("python", "print(1)")
	if got.Command != "python3 - <<'PYEOF'\nprint(1)\nPYEOF" {
		t.Fatalf("heredoc = %q", got.Command)
	}
}

func TestNormalizePythonTerminatorAvoidsCollision(t *testing.T) {
	got, err := Normalize("python3", map[string]any{"code": "print('PYEOF')"})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(got.Command, "\n")
	if lines[0] != "python3 - <<'PYEOF1'" || lines[len(lines)-1] != "PYEOF1" {
		t.Fatalf("heredoc = %q", got.Command)
	}
}

func TestNormalizePythonWorkdir(t *testing.T) {
	got, err := Normalize("python", map[string]any{"code": "import os", "cwd": "/tmp/x y"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Command, "cd '/tmp/x y' && python3 - <<'PYEOF'\n") {
		t.Fatalf("command = %q", got.Command)
	}
}

func TestNormalizePatchAliasesAndKeys(t *testing.T) {
	var cases []call
	for _, n := range []string{"apply_patch", "applypatch", "patch", "edit", "functions.apply_patch"} {
		cases = append(cases, call{n, map[string]any{"patch": "P"}, ApplyPatch, "P"})
	}
	for _, k := range []string{"patch", "input", "diff", "text", "content"} {
		cases = append(cases, call{"edit", map[string]any{k: "P"}, ApplyPatch, "P"})
	}
	cases = append(cases, call{"patch", []any{"*** Begin Patch", "*** End Patch"}, ApplyPatch, "*** Begin Patch\n*** End Patch"})
	runNormalize(t, cases)
}

func TestNormalizeErrors(t *testing.T) {
	for _, c := range []struct {
		name string
		args any
		msg  string
	}{
		{"nope", map[string]any{"command": "ls"}, "unknown tool 'nope'; available: bash, apply_patch"},
		{"browser", map[string]any{"command": "ls"}, "unknown tool"},
		{"bash", map[string]any{"timeout": 5}, "bash needs a 'command' string"},
		{"bash", map[string]any{"command": "   "}, "bash needs a 'command' string"},
		{"edit", map[string]any{"other": "x"}, "apply_patch needs a 'patch' string"},
	} {
		_, err := Normalize(c.name, c.args)
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("Normalize(%q, %#v) error = %v, want %q", c.name, c.args, err, c.msg)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"ls":       "ls",
		"my file":  "'my file'",
		"it's":     `'it'"'"'s'`,
		"":         "''",
		"a/b.c-d_": "a/b.c-d_",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseHarmony(t *testing.T) {
	tests := []struct {
		name, in string
		want     []HarmonyCall
	}{
		{"channel form",
			`<|start|>assistant<|channel|>commentary to=functions.bash <|constrain|>json<|message|>{"command":"ls"}<|call|>`,
			[]HarmonyCall{{"functions.bash", `{"command":"ls"}`}}},
		{"inline json form",
			`commentary to=container.exec json{"cmd":["bash","-lc","ls"]}`,
			[]HarmonyCall{{"container.exec", `{"cmd":["bash","-lc","ls"]}`}}},
		{"braces inside strings",
			`to=functions.bash json{"command":"echo {x}"}<|call|>`,
			[]HarmonyCall{{"functions.bash", `{"command":"echo {x}"}`}}},
		{"unterminated object taken to end",
			`to=functions.bash json{"command":"ls"`,
			[]HarmonyCall{{"functions.bash", `{"command":"ls"`}}},
		{"multiple calls",
			`to=functions.bash json{"command":"a"} x to=container.exec json{"cmd":"b"}`,
			[]HarmonyCall{{"functions.bash", `{"command":"a"}`}, {"container.exec", `{"cmd":"b"}`}}},
		{"no match", "just some text with {braces} and no call", nil},
		{"to= inside a word is not a call", `xto=functions.bash json{"command":"ls"}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseHarmony(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseHarmony = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestHarmonyResultNormalises(t *testing.T) {
	calls := ParseHarmony(`commentary to=container.exec json{"cmd":["bash","-lc","ls"]}`)
	got, err := Normalize(calls[0].Name, calls[0].Args)
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "ls" {
		t.Fatalf("command = %q", got.Command)
	}
}
