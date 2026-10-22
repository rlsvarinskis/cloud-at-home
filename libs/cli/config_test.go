package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

const testHome = "/srv/test-home"

// fakeSources serves the given environment and files. A missing file reports fs.ErrNotExist.
func fakeSources(env map[string]string, files map[string]string) Sources {
	return Sources{
		LookupEnv: func(name string) (string, bool) {
			v, ok := env[name]
			return v, ok
		},
		ReadFile: func(name string) ([]byte, error) {
			v, ok := files[name]
			if !ok {
				return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
			}
			return []byte(v), nil
		},
		HomeDir: func() (string, error) { return testHome, nil },
	}
}

var homeTokenPath = filepath.Join(testHome, ".config", "attic", "token")

func TestResolvePrecedence(t *testing.T) {
	files := map[string]string{
		"/run/flag-token":  " flag-tok \n",
		"/run/env-token":   "env-file-tok",
		homeTokenPath:      "\n  home-tok  \n",
		"/run/empty-token": " \n\t",
	}
	tests := []struct {
		name     string
		settings Settings
		env      map[string]string
		wantURL  string
		wantProj string
		wantTok  string
	}{
		{
			name:     "defaults",
			wantURL:  DefaultVMDURL,
			wantProj: DefaultProject,
			wantTok:  "home-tok",
		},
		{
			name:     "environment overrides default and trailing slash goes",
			env:      map[string]string{EnvVMDURL: " https://192.0.2.10:8443/ ", EnvProject: " demo "},
			wantURL:  "https://192.0.2.10:8443",
			wantProj: "demo",
			wantTok:  "home-tok",
		},
		{
			name:     "flag overrides environment",
			settings: Settings{VMDURL: "http://192.0.2.20:9000", Project: "flagp"},
			env:      map[string]string{EnvVMDURL: "http://192.0.2.10:8080", EnvProject: "envp"},
			wantURL:  "http://192.0.2.20:9000",
			wantProj: "flagp",
			wantTok:  "home-tok",
		},
		{
			name:     "token flag beats every other token source",
			settings: Settings{TokenFile: "/run/flag-token"},
			env:      map[string]string{EnvTokenFile: "/run/env-token", EnvToken: "env-tok"},
			wantURL:  DefaultVMDURL,
			wantProj: DefaultProject,
			wantTok:  "flag-tok",
		},
		{
			name:    "ATTIC_TOKEN_FILE beats ATTIC_TOKEN",
			env:     map[string]string{EnvTokenFile: "/run/env-token", EnvToken: "env-tok"},
			wantURL: DefaultVMDURL, wantProj: DefaultProject,
			wantTok: "env-file-tok",
		},
		{
			name:    "ATTIC_TOKEN beats the default file",
			env:     map[string]string{EnvToken: "  env-tok \n"},
			wantURL: DefaultVMDURL, wantProj: DefaultProject,
			wantTok: "env-tok",
		},
		{
			name:    "a blank ATTIC_TOKEN counts as unset",
			env:     map[string]string{EnvToken: "   "},
			wantURL: DefaultVMDURL, wantProj: DefaultProject,
			wantTok: "home-tok",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Resolve(tt.settings, fakeSources(tt.env, files))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if c.VMDURL != tt.wantURL || c.Project != tt.wantProj || c.Token.Reveal() != tt.wantTok {
				t.Fatalf("got url=%q project=%q token=%q; want url=%q project=%q token=%q",
					c.VMDURL, c.Project, c.Token.Reveal(), tt.wantURL, tt.wantProj, tt.wantTok)
			}
		})
	}
}

func TestResolveNoDefaultTokenFile(t *testing.T) {
	c, err := Resolve(Settings{}, fakeSources(nil, nil))
	if err != nil {
		t.Fatalf("a missing default token file must not be an error: %v", err)
	}
	if c.Token != "" {
		t.Fatalf("token = %q, want none", c.Token)
	}
}

func TestResolveTokenErrors(t *testing.T) {
	files := map[string]string{"/run/empty": " \n", "/run/tok": "s3cr3t-value"}
	tests := []struct {
		name     string
		settings Settings
		env      map[string]string
		want     string
	}{
		{"explicit flag file missing", Settings{TokenFile: "/run/missing"}, nil, "read token file"},
		{"env token file missing", Settings{}, map[string]string{EnvTokenFile: "/run/missing"}, "read token file"},
		{"explicit file empty", Settings{TokenFile: "/run/empty"}, nil, "is empty"},
		{"bad vmd URL", Settings{VMDURL: "ftp://192.0.2.10"}, map[string]string{EnvToken: "s3cr3t-value"}, "must be an http or https URL"},
		{"vmd URL without host", Settings{VMDURL: "http://"}, nil, "must be an http or https URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Resolve(tt.settings, fakeSources(tt.env, files))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "s3cr3t-value") {
				t.Fatalf("error leaks the token: %v", err)
			}
		})
	}
}

func TestResolveHomeError(t *testing.T) {
	src := fakeSources(nil, nil)
	src.HomeDir = func() (string, error) { return "", errors.New("no home") }
	c, err := Resolve(Settings{}, src)
	if err != nil || c.Token != "" {
		t.Fatalf("got token %q, err %v; want no token and no error", c.Token, err)
	}
}

func TestSecretIsRedacted(t *testing.T) {
	const tok = Secret("s3cr3t-value")
	c := Config{VMDURL: "http://192.0.2.10", Project: "p", Token: tok}
	for _, s := range []string{
		fmt.Sprint(tok), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c),
		fmt.Sprintf("%q", tok), fmt.Sprintf("%s", tok),
	} {
		if strings.Contains(s, "s3cr3t-value") {
			t.Fatalf("formatted secret leaks: %s", s)
		}
	}
	if tok.Reveal() != "s3cr3t-value" {
		t.Fatalf("Reveal = %q", tok.Reveal())
	}
}
