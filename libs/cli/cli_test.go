package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1/vmdv1connect"
)

const testToken = "tok-VISIBLE-ONLY-TO-AUTH-HEADER"

// seen records what the test commands observed.
type seen struct {
	cfg  Config
	args []string
}

// testApp builds a hub with one group "vm" whose commands behave as named in the table.
func testApp(s *seen, env map[string]string) *App {
	ok := func(ctx context.Context, e *Env, args []string) error {
		s.cfg = e.Config
		s.args = args
		_, err := fmt.Fprintf(e.Stdout, "ok %s\n", e.Command)
		return err
	}
	return &App{
		Name:    "attic",
		Summary: "test hub",
		Version: "1.2.3",
		Sources: fakeSources(env, nil),
		Groups: []Group{{
			Name:    "vm",
			Summary: "test group",
			Commands: []Command{
				{Name: "list", Summary: "list things", Run: ok},
				{Name: "cp", Summary: "copy", Usage: "[--mode OCTAL] SRC DST", Run: func(ctx context.Context, e *Env, args []string) error {
					fs := e.Flags()
					if err := e.Parse(fs, args); err != nil {
						return err
					}
					if fs.NArg() != 2 {
						return Usagef("need two operands")
					}
					return nil
				}},
				{Name: "fail", Summary: "fails", Run: func(context.Context, *Env, []string) error {
					return errors.New("boom")
				}},
				{Name: "dump", Summary: "prints the config", Run: func(ctx context.Context, e *Env, args []string) error {
					fmt.Fprintf(e.Stdout, "%v %+v\n", e.Config, e.Config)
					_, err := e.ClientOptions()
					return err
				}},
			},
		}},
	}
}

func runApp(t *testing.T, app *App, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := app.Run(context.Background(), args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestDispatchExitCodesAndHelp(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"no arguments prints help to stderr", nil, ExitUsage, "", "Groups:"},
		{"help", []string{"help"}, ExitOK, "Groups:", ""},
		{"-h flag", []string{"-h"}, ExitOK, "Groups:", ""},
		{"version", []string{"version"}, ExitOK, "1.2.3\n", ""},
		{"unknown global flag", []string{"--nope", "vm", "list"}, ExitUsage, "", "run 'attic help'"},
		{"unknown group", []string{"zz", "list"}, ExitUsage, "", `unknown command "zz"`},
		{"group without command", []string{"vm"}, ExitUsage, "", "Commands:"},
		{"group help", []string{"vm", "help"}, ExitOK, "list", ""},
		{"unknown command", []string{"vm", "nope"}, ExitUsage, "", `attic vm: unknown command "nope"`},
		{"command -h prints its usage", []string{"vm", "cp", "-h"}, ExitOK, "usage: attic vm cp [--mode OCTAL] SRC DST", ""},
		{"bad flag is a usage error", []string{"vm", "cp", "--bogus"}, ExitUsage, "", "usage: attic vm cp [--mode OCTAL] SRC DST"},
		{"usage error from a command", []string{"vm", "cp", "only-one"}, ExitUsage, "", "need two operands"},
		{"runtime error", []string{"vm", "fail"}, ExitRuntime, "", "attic vm fail: boom"},
		{"success", []string{"vm", "list"}, ExitOK, "ok attic vm list\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s seen
			app := testApp(&s, map[string]string{EnvToken: testToken})
			code, out, errOut := runApp(t, app, tt.args...)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stdout %q, stderr %q)", code, tt.wantCode, out, errOut)
			}
			if !strings.Contains(out, tt.wantStdout) {
				t.Fatalf("stdout = %q, want it to contain %q", out, tt.wantStdout)
			}
			if !strings.Contains(errOut, tt.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", errOut, tt.wantStderr)
			}
		})
	}
}

func TestVersionDefaultsToDev(t *testing.T) {
	app := &App{Name: "attic", Sources: fakeSources(nil, nil)}
	code, out, _ := runApp(t, app, "version")
	if code != ExitOK || out != "dev\n" {
		t.Fatalf("code %d, out %q; want 0 and dev", code, out)
	}
}

func TestGlobalSettingsReachTheCommand(t *testing.T) {
	var s seen
	app := testApp(&s, map[string]string{EnvVMDURL: "http://192.0.2.99:1", EnvToken: testToken})
	code, _, errOut := runApp(t, app, "-p", "demo", "--vmd-url", "http://192.0.2.10:8080/", "vm", "list", "extra")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if s.cfg.Project != "demo" || s.cfg.VMDURL != "http://192.0.2.10:8080" || s.cfg.Token.Reveal() != testToken {
		t.Fatalf("config = %+v", s.cfg)
	}
	if len(s.args) != 1 || s.args[0] != "extra" {
		t.Fatalf("command args = %q", s.args)
	}
}

func TestBadConfigIsRuntimeErrorWithoutToken(t *testing.T) {
	var s seen
	app := testApp(&s, map[string]string{EnvToken: testToken})
	code, out, errOut := runApp(t, app, "--vmd-url", "ftp://192.0.2.10", "vm", "list")
	if code != ExitRuntime {
		t.Fatalf("exit %d, want %d", code, ExitRuntime)
	}
	if !strings.Contains(errOut, "must be an http or https URL") || strings.Contains(out+errOut, testToken) {
		t.Fatalf("stdout %q stderr %q", out, errOut)
	}
}

func TestTokenIsNeverPrinted(t *testing.T) {
	var s seen
	app := testApp(&s, map[string]string{EnvToken: testToken})
	for _, args := range [][]string{
		{"vm", "dump"},
		{"vm", "fail"},
		{"--token-file", "/no/such/file", "vm", "list"},
		{"--bogus", "vm", "list"},
	} {
		_, out, errOut := runApp(t, app, args...)
		if strings.Contains(out, testToken) || strings.Contains(errOut, testToken) {
			t.Fatalf("args %q printed the token: stdout %q stderr %q", args, out, errOut)
		}
	}
	_, out, _ := runApp(t, app, "vm", "dump")
	if !strings.Contains(out, "<redacted>") {
		t.Fatalf("dump output %q should show the redacted form", out)
	}
}

func TestClientOptionsNeedToken(t *testing.T) {
	if _, err := (&Env{}).ClientOptions(); err == nil {
		t.Fatal("ClientOptions without a token must fail")
	}
	opts, err := (&Env{Config: Config{Token: testToken}}).ClientOptions()
	if err != nil || len(opts) != 1 {
		t.Fatalf("ClientOptions with a token: %d options, err %v", len(opts), err)
	}
}

// The token travels only as the Authorization header, and only to the configured host: the default client does not
// follow redirects.
func TestBearerHeaderAndNoRedirects(t *testing.T) {
	var gotAuth string
	var elsewhereHit bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhereHit = true }))
	defer elsewhere.Close()
	vmd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusTemporaryRedirect)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"code":"not_found","message":"no such VM"}`)
	}))
	defer vmd.Close()

	env := &Env{Config: Config{VMDURL: vmd.URL, Token: testToken}, HTTP: defaultHTTPClient()}
	opts, err := env.ClientOptions()
	if err != nil {
		t.Fatal(err)
	}
	client := vmdv1connect.NewVMServiceClient(env.HTTP, vmd.URL, opts...)
	_, err = client.ListVMs(context.Background(), connect.NewRequest(&vmdv1.ListVMsRequest{ProjectId: "demo"}))
	if gotAuth != "Bearer "+testToken {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if got := RPCError(err).Error(); got != "not_found: no such VM" {
		t.Fatalf("RPCError = %q", got)
	}

	req, _ := http.NewRequest(http.MethodGet, vmd.URL+"/redirect", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := env.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || elsewhereHit {
		t.Fatalf("status %d, redirect followed = %v; want the 307 returned as is", resp.StatusCode, elsewhereHit)
	}
}

func TestRPCErrorPassesOtherErrors(t *testing.T) {
	plain := errors.New("dial failed")
	if RPCError(plain) != plain {
		t.Fatal("non-Connect errors must pass unchanged")
	}
	if got := RPCError(connect.NewError(connect.CodeNotFound, errors.New("gone"))).Error(); got != "not_found: gone" {
		t.Fatalf("RPCError = %q", got)
	}
}
