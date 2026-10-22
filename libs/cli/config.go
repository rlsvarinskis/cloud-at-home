package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Defaults of the global settings, used when neither a flag nor an environment variable sets them.
const (
	DefaultVMDURL  = "http://127.0.0.1:8080"
	DefaultProject = "default"
)

// Environment variables of the global settings.
const (
	EnvVMDURL    = "ATTIC_VMD_URL"
	EnvProject   = "ATTIC_PROJECT"
	EnvToken     = "ATTIC_TOKEN"
	EnvTokenFile = "ATTIC_TOKEN_FILE"
)

// defaultTokenFile is read when no other source names a token, relative to the home directory.
var defaultTokenFile = filepath.Join(".config", "attic", "token")

// Secret is a credential. It prints as <redacted> under %v, %s, %q and %#v, so a configuration that is logged or
// formatted into an error does not reveal it. Reveal returns the value; use it only for the Authorization header.
type Secret string

// String returns the redacted form.
func (Secret) String() string { return "<redacted>" }

// GoString returns the redacted form.
func (Secret) GoString() string { return `cli.Secret("<redacted>")` }

// Reveal returns the raw token.
func (s Secret) Reveal() string { return string(s) }

// Config is the resolved global configuration.
type Config struct {
	VMDURL  string // base URL of vmd, without a trailing slash
	Project string // project that VM names are resolved in
	Token   Secret // bearer token; empty when none is configured
}

// Settings are the global options as given on the command line. An empty field means the flag was not given.
type Settings struct {
	VMDURL    string
	Project   string
	TokenFile string
}

// Sources are the places Resolve reads from. A nil function means the process's own: the environment, the file
// system and the home directory. Tests set them to control every input.
type Sources struct {
	LookupEnv func(name string) (string, bool)
	ReadFile  func(name string) ([]byte, error)
	HomeDir   func() (string, error)
}

func (s Sources) withDefaults() Sources {
	if s.LookupEnv == nil {
		s.LookupEnv = os.LookupEnv
	}
	if s.ReadFile == nil {
		s.ReadFile = os.ReadFile
	}
	if s.HomeDir == nil {
		s.HomeDir = os.UserHomeDir
	}
	return s
}

// env returns the trimmed value of an environment variable, or "" when it is unset.
func (s Sources) env(name string) string {
	v, _ := s.LookupEnv(name)
	return strings.TrimSpace(v)
}

// Resolve computes the global configuration. For each setting the precedence is: the flag, then the environment
// variable, then the default. The token comes from the first of these that is set: --token-file, ATTIC_TOKEN_FILE,
// ATTIC_TOKEN, and the file ~/.config/attic/token (which may be absent). Surrounding whitespace is trimmed from every
// value. Resolve never puts the token into an error.
func Resolve(s Settings, src Sources) (Config, error) {
	src = src.withDefaults()
	var c Config
	c.VMDURL = firstNonEmpty(strings.TrimSpace(s.VMDURL), src.env(EnvVMDURL), DefaultVMDURL)
	u, err := url.Parse(c.VMDURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Config{}, fmt.Errorf("vmd URL %q must be an http or https URL", c.VMDURL)
	}
	c.VMDURL = strings.TrimRight(c.VMDURL, "/")
	c.Project = firstNonEmpty(strings.TrimSpace(s.Project), src.env(EnvProject), DefaultProject)
	c.Token, err = resolveToken(strings.TrimSpace(s.TokenFile), src)
	if err != nil {
		return Config{}, err
	}
	return c, nil
}

func resolveToken(flagFile string, src Sources) (Secret, error) {
	if flagFile != "" {
		return readToken(flagFile, src, false)
	}
	if f := src.env(EnvTokenFile); f != "" {
		return readToken(f, src, false)
	}
	if t := src.env(EnvToken); t != "" {
		return Secret(t), nil
	}
	home, err := src.HomeDir()
	if err != nil {
		return "", nil // no home directory, so no default token file
	}
	return readToken(filepath.Join(home, defaultTokenFile), src, true)
}

// readToken reads a token file. A missing file is an error unless optional is set. The error names the file but
// never its contents.
func readToken(path string, src Sources, optional bool) (Secret, error) {
	data, err := src.ReadFile(path)
	if err != nil {
		if optional && errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read token file: %w", err)
	}
	t := strings.TrimSpace(string(data))
	if t == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return Secret(t), nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
