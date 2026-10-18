package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
)

// invoke runs atticctl with the given stdin and arguments and returns the exit code and output.
func invoke(stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

// generateTestKey creates kid in a fresh temp dir and returns the directory.
func generateTestKey(t *testing.T, kid string) string {
	t.Helper()
	dir := t.TempDir()
	if code, _, stderr := invoke("", "key", "generate", "-out", dir, "-id", kid); code != exitOK {
		t.Fatalf("key generate: exit %d, stderr %q", code, stderr)
	}
	return dir
}

// keyBodyLines returns the base64 body lines of a PEM file. Secret material must never appear in
// atticctl's output, so tests use these lines as needles.
func keyBodyLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l != "" && !strings.HasPrefix(l, "-----") && len(l) >= 16 {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("no key body in %s", path)
	}
	return lines
}

func assertNoSecrets(t *testing.T, where, text string, needles []string) {
	t.Helper()
	if strings.Contains(text, "PRIVATE KEY") {
		t.Errorf("%s contains a PEM private key header", where)
	}
	for _, n := range needles {
		if strings.Contains(text, n) {
			t.Errorf("%s contains private key material", where)
		}
	}
}

func TestKeyGenerateMintInspectRoundTrip(t *testing.T) {
	dir := generateTestKey(t, "k1")
	priv := filepath.Join(dir, "k1.key")
	pub := filepath.Join(dir, "k1.pub")
	needles := keyBodyLines(t, priv)

	info, err := os.Stat(priv)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("private key mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(pub); err != nil {
		t.Fatalf("public key missing: %v", err)
	}

	code, mintOut, mintErr := invoke("", "token", "mint", "-key", priv, "-kid", "k1",
		"-sub", "user:robert", "-type", "user", "-bind", "proj-a:editor", "-bind", "*:viewer", "-ttl", "1h")
	if code != exitOK {
		t.Fatalf("mint: exit %d, stderr %q", code, mintErr)
	}
	if strings.Count(mintOut, "\n") != 1 {
		t.Fatalf("mint stdout must be exactly one line, got %q", mintOut)
	}
	token := strings.TrimSuffix(mintOut, "\n")
	if strings.Count(token, ".") != 2 {
		t.Fatalf("mint output is not a JWT: %q", token)
	}
	assertNoSecrets(t, "mint stdout", mintOut, needles)
	assertNoSecrets(t, "mint stderr", mintErr, needles)

	code, inspOut, inspErr := invoke("", "token", "inspect", "-pub", pub, "-kid", "k1", token)
	if code != exitOK {
		t.Fatalf("inspect: exit %d, stderr %q", code, inspErr)
	}
	for _, want := range []string{"subject:  user:robert", "type:     user", "proj-a:editor", "*:viewer", "expires:"} {
		if !strings.Contains(inspOut, want) {
			t.Errorf("inspect output missing %q:\n%s", want, inspOut)
		}
	}
	assertNoSecrets(t, "inspect stdout", inspOut, needles)
	assertNoSecrets(t, "inspect stderr", inspErr, needles)
}

func TestInspectReadsTokenFromStdin(t *testing.T) {
	dir := generateTestKey(t, "k1")
	_, mintOut, _ := invoke("", "token", "mint", "-key", filepath.Join(dir, "k1.key"), "-kid", "k1",
		"-sub", "svc:agentd", "-type", "service", "-bind", "*:service")
	code, out, errOut := invoke("  "+mintOut+"\n", "token", "inspect", "-pub", filepath.Join(dir, "k1.pub"), "-kid", "k1", "-")
	if code != exitOK {
		t.Fatalf("inspect via stdin: exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "subject:  svc:agentd") || !strings.Contains(out, "type:     service") {
		t.Errorf("unexpected inspect output:\n%s", out)
	}
}

func TestKeyGenerateRefusesOverwrite(t *testing.T) {
	dir := generateTestKey(t, "k1")
	priv := filepath.Join(dir, "k1.key")
	before, err := os.ReadFile(priv)
	if err != nil {
		t.Fatal(err)
	}

	code, _, stderr := invoke("", "key", "generate", "-out", dir, "-id", "k1")
	if code != exitFailure {
		t.Fatalf("second generate: exit %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "refusing to overwrite") {
		t.Errorf("stderr = %q, want overwrite refusal", stderr)
	}
	after, err := os.ReadFile(priv)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("existing private key was modified")
	}
}

func TestKeyGenerateRefusesWhenOnlyPublicExists(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "k1.pub"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := invoke("", "key", "generate", "-out", dir, "-id", "k1"); code != exitFailure {
		t.Fatalf("exit %d, want %d", code, exitFailure)
	}
	if _, err := os.Stat(filepath.Join(dir, "k1.key")); !os.IsNotExist(err) {
		t.Errorf("partial private key left behind: %v", err)
	}
}

func TestKeyGenerateRejectsBadKeyID(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"", "../evil", "a/b", ".hidden", "sp ace"} {
		if code, _, _ := invoke("", "key", "generate", "-out", dir, "-id", id); code != exitUsage {
			t.Errorf("id %q: exit %d, want %d", id, code, exitUsage)
		}
	}
}

func TestMintBadBindingSyntax(t *testing.T) {
	dir := generateTestKey(t, "k1")
	priv := filepath.Join(dir, "k1.key")
	for _, b := range []string{"nocolon", ":editor", "proj:", "proj:editor:extra", ""} {
		code, out, stderr := invoke("", "token", "mint", "-key", priv, "-kid", "k1",
			"-sub", "user:robert", "-type", "user", "-bind", b)
		if code != exitUsage {
			t.Errorf("binding %q: exit %d, want %d", b, code, exitUsage)
		}
		if out != "" {
			t.Errorf("binding %q: stdout should be empty, got %q", b, out)
		}
		if stderr == "" {
			t.Errorf("binding %q: no error message", b)
		}
	}
}

func TestMintUnknownRole(t *testing.T) {
	dir := generateTestKey(t, "k1")
	code, out, stderr := invoke("", "token", "mint", "-key", filepath.Join(dir, "k1.key"), "-kid", "k1",
		"-sub", "user:robert", "-type", "user", "-bind", "proj:superuser")
	if code != exitUsage {
		t.Fatalf("exit %d, want %d", code, exitUsage)
	}
	if out != "" || !strings.Contains(stderr, "superuser") {
		t.Errorf("stdout %q, stderr %q", out, stderr)
	}
}

func TestMintRequiredFlags(t *testing.T) {
	dir := generateTestKey(t, "k1")
	priv := filepath.Join(dir, "k1.key")
	base := []string{"token", "mint", "-key", priv, "-kid", "k1", "-sub", "user:robert", "-type", "user", "-bind", "proj:viewer"}
	without := func(flagName string) []string {
		var out []string
		for i := 0; i < len(base); i++ {
			if base[i] == flagName {
				i++
				continue
			}
			out = append(out, base[i])
		}
		return out
	}
	cases := map[string][]string{
		"no bindings": without("-bind"),
		"no subject":  without("-sub"),
		"no type":     without("-type"),
		"no key":      without("-key"),
		"no kid":      without("-kid"),
		"bad type":    append(without("-type"), "-type", "admin"),
		"empty sub":   append(without("-sub"), "-sub", "   "),
	}
	for name, args := range cases {
		if code, _, _ := invoke("", args...); code != exitUsage {
			t.Errorf("%s: exit %d, want %d", name, code, exitUsage)
		}
	}
}

func TestMintTTLLimits(t *testing.T) {
	dir := generateTestKey(t, "k1")
	priv := filepath.Join(dir, "k1.key")
	mint := func(extra ...string) int {
		args := append([]string{"token", "mint", "-key", priv, "-kid", "k1",
			"-sub", "user:robert", "-type", "user", "-bind", "proj:viewer"}, extra...)
		code, _, _ := invoke("", args...)
		return code
	}
	bad := map[string][]string{
		"zero":             {"-ttl", "0"},
		"negative":         {"-ttl", "-1h"},
		"over default max": {"-ttl", "721h"},
		"over custom max":  {"-ttl", "2h", "-max-ttl", "1h"},
		"zero max":         {"-ttl", "1h", "-max-ttl", "0"},
	}
	for name, extra := range bad {
		if code := mint(extra...); code != exitUsage {
			t.Errorf("%s: exit %d, want %d", name, code, exitUsage)
		}
	}
	good := map[string][]string{
		"default":      {},
		"exactly 30d":  {"-ttl", "720h"},
		"lifted limit": {"-ttl", "721h", "-max-ttl", "1000h"},
	}
	for name, extra := range good {
		if code := mint(extra...); code != exitOK {
			t.Errorf("%s: exit %d, want %d", name, code, exitOK)
		}
	}
}

func TestMintMissingKeyFile(t *testing.T) {
	code, _, _ := invoke("", "token", "mint", "-key", filepath.Join(t.TempDir(), "nope.key"), "-kid", "k1",
		"-sub", "user:robert", "-type", "user", "-bind", "proj:viewer")
	if code != exitFailure {
		t.Errorf("exit %d, want %d", code, exitFailure)
	}
}

func TestInspectInvalidTokens(t *testing.T) {
	dir := generateTestKey(t, "k1")
	pub := filepath.Join(dir, "k1.pub")
	_, mintOut, _ := invoke("", "token", "mint", "-key", filepath.Join(dir, "k1.key"), "-kid", "k1",
		"-sub", "user:robert", "-type", "user", "-bind", "proj:viewer")
	token := strings.TrimSpace(mintOut)

	// Change a character in the middle of the signature. The final base64 character carries only
	// padding bits for a 64-byte signature, so changing it can leave the signature unchanged.
	mid := len(token) - 20
	flipped := "A"
	if token[mid] == 'A' {
		flipped = "B"
	}
	tampered := token[:mid] + flipped + token[mid+1:]

	cases := map[string]struct {
		token string
		kid   string
	}{
		"garbage":      {"not-a-token", "k1"},
		"tampered":     {tampered, "k1"},
		"wrong key id": {token, "other"},
	}
	for name, c := range cases {
		code, out, stderr := invoke("", "token", "inspect", "-pub", pub, "-kid", c.kid, c.token)
		if code != exitFailure {
			t.Errorf("%s: exit %d, want %d", name, code, exitFailure)
		}
		if out != "" {
			t.Errorf("%s: stdout should be empty, got %q", name, out)
		}
		if !strings.Contains(stderr, "invalid token") {
			t.Errorf("%s: stderr = %q", name, stderr)
		}
	}

	if code, _, _ := invoke("", "token", "inspect", "-pub", pub, "-kid", "k1", "-"); code != exitFailure {
		t.Errorf("empty stdin: exit %d, want %d", code, exitFailure)
	}
}

func TestInspectTokenFromOtherKeyRejected(t *testing.T) {
	dirA := generateTestKey(t, "k1")
	dirB := generateTestKey(t, "k1")
	_, mintOut, _ := invoke("", "token", "mint", "-key", filepath.Join(dirA, "k1.key"), "-kid", "k1",
		"-sub", "user:robert", "-type", "user", "-bind", "proj:viewer")
	code, _, _ := invoke("", "token", "inspect", "-pub", filepath.Join(dirB, "k1.pub"), "-kid", "k1", strings.TrimSpace(mintOut))
	if code != exitFailure {
		t.Errorf("exit %d, want %d", code, exitFailure)
	}
}

func TestInspectUsage(t *testing.T) {
	dir := generateTestKey(t, "k1")
	pub := filepath.Join(dir, "k1.pub")
	if code, _, _ := invoke("", "token", "inspect", "-kid", "k1", "x"); code != exitUsage {
		t.Errorf("missing -pub: exit %d", code)
	}
	if code, _, _ := invoke("", "token", "inspect", "-pub", pub, "-kid", "k1"); code != exitUsage {
		t.Errorf("missing token argument: exit %d", code)
	}
	if code, _, _ := invoke("", "token", "inspect", "-pub", pub, "-kid", "k1", "a", "b"); code != exitUsage {
		t.Errorf("two token arguments: exit %d", code)
	}
}

func TestRolesListsKnownRoles(t *testing.T) {
	code, out, stderr := invoke("", "roles")
	if code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	lines := strings.Fields(out)
	if len(lines) != len(knownRoles) {
		t.Fatalf("got %d roles, want %d: %q", len(lines), len(knownRoles), out)
	}
	for i, r := range knownRoles {
		if lines[i] != string(r) {
			t.Errorf("line %d = %q, want %q", i, lines[i], r)
		}
		if !authz.KnownRole(r) {
			t.Errorf("knownRoles entry %q is not an auth role", r)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"bogus"},
		{"key"},
		{"key", "rotate"},
		{"token"},
		{"token", "revoke"},
		{"roles", "extra"},
		{"key", "generate", "-id", "k1"},
	}
	for _, args := range cases {
		code, out, _ := invoke("", args...)
		if code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
		if out != "" {
			t.Errorf("%v: unexpected stdout %q", args, out)
		}
	}
}

func TestBindingListSetAndString(t *testing.T) {
	var b bindingList
	if err := b.Set("*:admin"); err != nil {
		t.Fatal(err)
	}
	if err := b.Set("p:viewer"); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "*:admin,p:viewer" {
		t.Errorf("String() = %q", got)
	}
	if err := b.Set("p:nope"); err == nil {
		t.Error("unknown role accepted")
	}
	if len(b) != 2 {
		t.Errorf("len = %d, want 2", len(b))
	}
}

func TestRolesVerboseListsPermissions(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"roles", "-v"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"viewer\n", "    vm.get\n", "operator\n", "    capacity.get\n", "service\n", "    vm.hold\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
