// Command hcctl is the homecloud admin CLI. It generates Ed25519 signing keys, mints signed
// tokens, and inspects tokens against a public key.
//
// Secrets are never written to stdout or stderr. The only stdout output that carries sensitive
// data is the JWT printed by "token mint", which is printed alone so it can be captured in a shell
// variable. Exit codes: 0 success, 1 runtime failure (including an invalid token), 2 usage error.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// defaultMaxTTL is the default ceiling for "token mint -ttl". Override with -max-ttl.
const defaultMaxTTL = 30 * 24 * time.Hour

// keyIDPattern limits key IDs to characters that are safe in file names and JWT headers.
var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// knownRoles lists the roles hcctl can grant (every role defined in libs/authz).
var knownRoles = authz.Roles()

const usageText = `usage:
  hcctl key generate -out DIR -id KEYID
  hcctl token mint -key FILE -kid KEYID -sub SUBJECT -type user|service -bind project:role [-bind ...] [-ttl 24h] [-max-ttl 720h]
  hcctl token inspect -pub FILE -kid KEYID (TOKEN | -)
  hcctl roles [-v]        list roles (-v: with the permissions each grants)

Flags must come before positional arguments.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run executes one hcctl invocation and returns the process exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	switch args[0] {
	case "key":
		return runKey(args[1:], stdout, stderr)
	case "token":
		return runToken(args[1:], stdin, stdout, stderr)
	case "roles":
		return runRoles(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usageText)
		return exitOK
	}
	fmt.Fprintf(stderr, "hcctl: unknown command %q\n", args[0])
	fmt.Fprint(stderr, usageText)
	return exitUsage
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fl := flag.NewFlagSet("hcctl "+name, flag.ContinueOnError)
	fl.SetOutput(stderr)
	return fl
}

// parseFlags parses args. It returns the exit code to use and false when the caller should stop.
func parseFlags(fl *flag.FlagSet, args []string) (int, bool) {
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}
		return exitUsage, false
	}
	return exitOK, true
}

func usageErr(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "hcctl: "+format+"\n", a...)
	return exitUsage
}

func failErr(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "hcctl: %v\n", err)
	return exitFailure
}

// ---- key generate ---------------------------------------------------------------------------------

func runKey(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "generate" {
		return usageErr(stderr, "key: want subcommand \"generate\"")
	}
	fl := newFlagSet("key generate", stderr)
	out := fl.String("out", "", "directory to write KEYID.key and KEYID.pub into")
	id := fl.String("id", "", "key id: letters, digits, '.', '_' or '-', starting with a letter or digit")
	if code, ok := parseFlags(fl, args[1:]); !ok {
		return code
	}
	if fl.NArg() != 0 {
		return usageErr(stderr, "key generate: unexpected argument %q", fl.Arg(0))
	}
	if *out == "" {
		return usageErr(stderr, "key generate: -out is required")
	}
	if !keyIDPattern.MatchString(*id) {
		return usageErr(stderr, "key generate: invalid -id %q", *id)
	}
	return generateKey(*out, *id, stdout, stderr)
}

func generateKey(dir, id string, stdout, stderr io.Writer) int {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return failErr(stderr, err)
	}
	privPath := filepath.Join(dir, id+".key")
	pubPath := filepath.Join(dir, id+".pub")

	// Reserve both files with O_EXCL so an existing key is never overwritten, even by a race.
	var reserved []string
	cleanup := func() {
		for _, p := range reserved {
			_ = os.Remove(p)
		}
	}
	for _, rp := range []struct {
		path string
		perm fs.FileMode
	}{{privPath, 0o600}, {pubPath, 0o644}} {
		f, err := os.OpenFile(rp.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, rp.perm)
		if err != nil {
			cleanup()
			if errors.Is(err, fs.ErrExist) {
				return failErr(stderr, fmt.Errorf("refusing to overwrite %s", rp.path))
			}
			return failErr(stderr, err)
		}
		reserved = append(reserved, rp.path)
		if err := f.Close(); err != nil {
			cleanup()
			return failErr(stderr, err)
		}
	}

	pub, priv, err := authz.GenerateKey()
	if err != nil {
		cleanup()
		return failErr(stderr, err)
	}
	if err := authz.SavePrivateKey(privPath, priv); err != nil {
		cleanup()
		return failErr(stderr, err)
	}
	if err := authz.SavePublicKey(pubPath, pub); err != nil {
		cleanup()
		return failErr(stderr, err)
	}
	fmt.Fprintf(stdout, "wrote %s\nwrote %s\n", privPath, pubPath)
	return exitOK
}

// ---- token ----------------------------------------------------------------------------------------

func runToken(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErr(stderr, "token: want subcommand \"mint\" or \"inspect\"")
	}
	switch args[0] {
	case "mint":
		return runMint(args[1:], stdout, stderr)
	case "inspect":
		return runInspect(args[1:], stdin, stdout, stderr)
	}
	return usageErr(stderr, "token: unknown subcommand %q", args[0])
}

// bindingList collects repeated -bind flags, validating each one as it is set.
type bindingList []authz.Binding

func (b *bindingList) String() string {
	if b == nil {
		return ""
	}
	parts := make([]string, 0, len(*b))
	for _, x := range *b {
		parts = append(parts, x.Project+":"+string(x.Role))
	}
	return strings.Join(parts, ",")
}

func (b *bindingList) Set(s string) error {
	bind, err := parseBinding(s)
	if err != nil {
		return err
	}
	*b = append(*b, bind)
	return nil
}

// parseBinding parses "project:role". The project may be "*" for every project.
func parseBinding(s string) (authz.Binding, error) {
	project, role, ok := strings.Cut(s, ":")
	if !ok || project == "" || role == "" {
		return authz.Binding{}, fmt.Errorf("binding %q: want project:role", s)
	}
	r := authz.Role(role)
	if !authz.KnownRole(r) {
		return authz.Binding{}, fmt.Errorf("binding %q: unknown role %q (see hcctl roles)", s, role)
	}
	return authz.Binding{Project: project, Role: r}, nil
}

func runMint(args []string, stdout, stderr io.Writer) int {
	fl := newFlagSet("token mint", stderr)
	keyPath := fl.String("key", "", "path to the PRIVATE signing key (PKCS#8 PEM)")
	kid := fl.String("kid", "", "key id to put in the token header and issuer claim")
	sub := fl.String("sub", "", "subject, for example user:robert")
	typ := fl.String("type", "", "principal type: user or service")
	var binds bindingList
	fl.Var(&binds, "bind", "project:role grant; repeatable; project * means every project")
	ttl := fl.Duration("ttl", 24*time.Hour, "token lifetime, greater than zero")
	maxTTL := fl.Duration("max-ttl", defaultMaxTTL, "largest allowed -ttl")
	if code, ok := parseFlags(fl, args); !ok {
		return code
	}
	if fl.NArg() != 0 {
		return usageErr(stderr, "token mint: unexpected argument %q", fl.Arg(0))
	}
	if *keyPath == "" {
		return usageErr(stderr, "token mint: -key is required")
	}
	if !keyIDPattern.MatchString(*kid) {
		return usageErr(stderr, "token mint: invalid -kid %q", *kid)
	}
	subject := strings.TrimSpace(*sub)
	if subject == "" {
		return usageErr(stderr, "token mint: -sub is required")
	}
	var ptype authz.PrincipalType
	switch *typ {
	case string(authz.User):
		ptype = authz.User
	case string(authz.Service):
		ptype = authz.Service
	default:
		return usageErr(stderr, "token mint: -type must be user or service, got %q", *typ)
	}
	if len(binds) == 0 {
		return usageErr(stderr, "token mint: at least one -bind project:role is required")
	}
	if *maxTTL <= 0 {
		return usageErr(stderr, "token mint: -max-ttl must be positive")
	}
	if *ttl <= 0 {
		return usageErr(stderr, "token mint: -ttl must be positive")
	}
	if *ttl > *maxTTL {
		return usageErr(stderr, "token mint: -ttl %s exceeds -max-ttl %s", *ttl, *maxTTL)
	}

	priv, err := authz.LoadPrivateKey(*keyPath)
	if err != nil {
		return failErr(stderr, fmt.Errorf("load signing key: %w", err))
	}
	issuer := &authz.Issuer{KeyID: *kid, Key: priv}
	token, err := issuer.Mint(authz.Principal{Subject: subject, Type: ptype, Bindings: binds}, *ttl)
	if err != nil {
		return failErr(stderr, fmt.Errorf("mint: %w", err))
	}
	fmt.Fprintln(stdout, token)
	return exitOK
}

func runInspect(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fl := newFlagSet("token inspect", stderr)
	pubPath := fl.String("pub", "", "path to the PUBLIC key (PKIX PEM)")
	kid := fl.String("kid", "", "key id the token must be signed with")
	if code, ok := parseFlags(fl, args); !ok {
		return code
	}
	if *pubPath == "" {
		return usageErr(stderr, "token inspect: -pub is required")
	}
	if !keyIDPattern.MatchString(*kid) {
		return usageErr(stderr, "token inspect: invalid -kid %q", *kid)
	}
	if fl.NArg() != 1 {
		return usageErr(stderr, "token inspect: want exactly one TOKEN argument, or - to read stdin")
	}

	token := fl.Arg(0)
	if token == "-" {
		raw, err := io.ReadAll(stdin)
		if err != nil {
			return failErr(stderr, fmt.Errorf("read stdin: %w", err))
		}
		token = strings.TrimSpace(string(raw))
	}
	if token == "" {
		return failErr(stderr, errors.New("no token given"))
	}

	pub, err := authz.LoadPublicKey(*pubPath)
	if err != nil {
		return failErr(stderr, fmt.Errorf("load public key: %w", err))
	}
	verifier := &authz.Verifier{Keys: map[string]authz.TrustedKey{*kid: {Public: pub}}}
	p, err := verifier.Verify(token)
	if err != nil {
		fmt.Fprintf(stderr, "hcctl: invalid token: %v\n", err)
		return exitFailure
	}

	// The signature is already verified, so reading the expiry from the claims is safe.
	rc := &jwt.RegisteredClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, rc); err != nil {
		return failErr(stderr, fmt.Errorf("read claims: %w", err))
	}
	expires := "never"
	if rc.ExpiresAt != nil {
		expires = rc.ExpiresAt.UTC().Format(time.RFC3339)
	}

	fmt.Fprintf(stdout, "subject:  %s\n", p.Subject)
	fmt.Fprintf(stdout, "type:     %s\n", p.Type)
	fmt.Fprintf(stdout, "token-id: %s\n", p.TokenID)
	fmt.Fprintf(stdout, "key-id:   %s\n", *kid)
	fmt.Fprintf(stdout, "expires:  %s\n", expires)
	if len(p.Bindings) == 0 {
		fmt.Fprintln(stdout, "bindings: (none)")
		return exitOK
	}
	fmt.Fprintln(stdout, "bindings:")
	for _, b := range p.Bindings {
		fmt.Fprintf(stdout, "  %s:%s\n", b.Project, b.Role)
	}
	return exitOK
}

// ---- roles ----------------------------------------------------------------------------------------

func runRoles(args []string, stdout, stderr io.Writer) int {
	verbose := false
	switch {
	case len(args) == 1 && (args[0] == "-v" || args[0] == "--verbose"):
		verbose = true
	case len(args) != 0:
		return usageErr(stderr, "usage: hcctl roles [-v]")
	}
	for _, r := range knownRoles {
		fmt.Fprintln(stdout, r)
		if verbose {
			for _, p := range r.Permissions() {
				fmt.Fprintln(stdout, "    "+string(p))
			}
		}
	}
	return exitOK
}
