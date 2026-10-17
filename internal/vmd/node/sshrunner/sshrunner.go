package sshrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/vmd/node/guestexec"
)

const (
	defaultUser        = "agent"
	defaultDialTimeout = 5 * time.Second
	defaultMaxOutput   = 16 << 20
)

// signalExitCodes maps the signals a guest command is most likely to die from to the
// shell convention 128+signal number. Any other signal maps to 255.
var signalExitCodes = map[string]int{
	"HUP":  129,
	"INT":  130,
	"KILL": 137,
	"TERM": 143,
}

// Config describes where the guest is and how to authenticate to it.
type Config struct {
	// Addr is the host:port to dial, for example 127.0.0.1:2222.
	Addr string
	// User is the login name. Empty means "agent".
	User string
	// Signer is the client key used for public-key authentication. Required.
	Signer ssh.Signer
	// HostKeyCallback verifies the guest's host key. Nil means
	// ssh.InsecureIgnoreHostKey.
	//
	// Ignoring the host key is acceptable because Addr is a loopback-forwarded port of
	// a VM this process has just booted. Only local processes can reach that port, and
	// the guest's host keys are regenerated on every first boot, so there is no stable
	// key to pin against. Callers that reach a guest over a real network must supply a
	// verifying callback.
	HostKeyCallback ssh.HostKeyCallback
	// DialTimeout bounds the TCP connect and the SSH handshake. Zero means 5 seconds.
	DialTimeout time.Duration
	// MaxOutput is the byte limit kept for each of stdout and stderr. Zero means 16 MiB.
	// Output past the limit is read and discarded; the command still succeeds.
	MaxOutput int64
}

// Runner runs commands in a guest, opening a fresh SSH connection for each call.
// It implements guestexec.Runner and is safe for concurrent use.
type Runner struct {
	cfg Config
}

var _ guestexec.Runner = (*Runner)(nil)

// New returns a Runner for c with the documented defaults applied to its zero fields.
func New(c Config) *Runner {
	if c.User == "" {
		c.User = defaultUser
	}
	if c.HostKeyCallback == nil {
		c.HostKeyCallback = ssh.InsecureIgnoreHostKey()
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = defaultDialTimeout
	}
	if c.MaxOutput <= 0 {
		c.MaxOutput = defaultMaxOutput
	}
	return &Runner{cfg: c}
}

// Run executes command in the guest with stdin (which may be nil) as its standard
// input and returns what it wrote to stdout and stderr together with its exit code.
//
// A non-zero exit status is not an error: it is returned as exitCode with a nil err.
// A command killed by a signal reports 128+n for HUP, INT, KILL and TERM, and 255 for
// any other signal. Failing to connect, authenticate or open the session returns an
// error wrapping errs.ErrUnavailable. If ctx is cancelled, the connection is closed,
// which also ends the command in the guest, and the error wraps ctx.Err().
func (r *Runner) Run(ctx context.Context, command string, stdin []byte) (stdout, stderr []byte, exitCode int, err error) {
	if r.cfg.Signer == nil {
		return nil, nil, 0, errs.Invalid("sshrunner: no client signer configured")
	}
	client, err := r.dial(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return nil, nil, 0, failure(ctx, "open session", err)
	}
	defer sess.Close()

	out := &capture{max: r.cfg.MaxOutput}
	errw := &capture{max: r.cfg.MaxOutput}
	// Always supply a stdin reader, even for nil input, so the guest sees EOF rather
	// than a stream that never ends.
	sess.Stdin = bytes.NewReader(stdin)
	sess.Stdout = out
	sess.Stderr = errw
	if err := sess.Start(command); err != nil {
		return nil, nil, 0, failure(ctx, "start command", err)
	}

	// Closing the connection on cancellation unblocks Wait and tears down the session.
	stop := context.AfterFunc(ctx, func() { client.Close() })
	waitErr := sess.Wait()
	if !stop() {
		return nil, nil, 0, fmt.Errorf("sshrunner: run command: %w", ctx.Err())
	}

	if waitErr == nil {
		return out.buf.Bytes(), errw.buf.Bytes(), 0, nil
	}
	var exitErr *ssh.ExitError
	if errors.As(waitErr, &exitErr) {
		code := exitErr.ExitStatus()
		if sig := exitErr.Signal(); sig != "" {
			code = signalExitCode(sig)
		}
		return out.buf.Bytes(), errw.buf.Bytes(), code, nil
	}
	return nil, nil, 0, errs.Unavailable("sshrunner: run command: %v", waitErr)
}

// dial opens and authenticates an SSH connection to the guest. The dial timeout covers
// the TCP connect and the handshake, and cancelling ctx aborts either one.
func (r *Runner) dial(ctx context.Context) (*ssh.Client, error) {
	d := net.Dialer{Timeout: r.cfg.DialTimeout}
	conn, err := d.DialContext(ctx, "tcp", r.cfg.Addr)
	if err != nil {
		return nil, failure(ctx, "dial "+r.cfg.Addr, err)
	}

	stop := context.AfterFunc(ctx, func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(r.cfg.DialTimeout))
	cc := &ssh.ClientConfig{
		User:            r.cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(r.cfg.Signer)},
		HostKeyCallback: r.cfg.HostKeyCallback,
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, r.cfg.Addr, cc)
	if !stop() {
		if err == nil {
			c.Close()
		}
		conn.Close()
		return nil, fmt.Errorf("sshrunner: handshake with %s: %w", r.cfg.Addr, ctx.Err())
	}
	if err != nil {
		conn.Close()
		return nil, failure(ctx, "handshake with "+r.cfg.Addr, err)
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

// failure converts a transport error into the error Run returns. When ctx is done it
// wraps ctx.Err(); otherwise it wraps errs.ErrUnavailable.
func failure(ctx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("sshrunner: %s: %w", op, ctx.Err())
	}
	return errs.Unavailable("sshrunner: %s: %v", op, err)
}

// signalExitCode returns the exit code reported for a command killed by sig.
func signalExitCode(sig string) int {
	if code, ok := signalExitCodes[sig]; ok {
		return code
	}
	return 255
}

// capture is an io.Writer that keeps at most max bytes and discards the rest. It always
// reports the full length as written so the SSH channel never fails on excess output.
type capture struct {
	buf bytes.Buffer
	max int64
}

// Write implements io.Writer.
func (c *capture) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.max - int64(c.buf.Len()); room > 0 {
		if int64(len(p)) > room {
			p = p[:room]
		}
		c.buf.Write(p)
	}
	return n, nil
}

// LoadSigner reads an Ed25519 private key in PEM form from pemPath. Both the OpenSSH
// format and PKCS#8 are accepted. Encrypted keys are not supported.
func LoadSigner(pemPath string) (ssh.Signer, error) {
	data, err := os.ReadFile(pemPath)
	if err != nil {
		return nil, fmt.Errorf("sshrunner: read key: %w", err)
	}
	raw, err := ssh.ParseRawPrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("sshrunner: parse key %s: %w", pemPath, err)
	}
	s, err := ssh.NewSignerFromKey(raw)
	if err != nil {
		return nil, fmt.Errorf("sshrunner: load key %s: %w", pemPath, err)
	}
	if t := s.PublicKey().Type(); t != ssh.KeyAlgoED25519 {
		return nil, fmt.Errorf("sshrunner: key %s is %s, want %s", pemPath, t, ssh.KeyAlgoED25519)
	}
	return s, nil
}

// PublicKeyLine returns the public half of s as one authorized_keys line, such as
// "ssh-ed25519 AAAA...", with no trailing newline.
func PublicKeyLine(s ssh.Signer) string {
	return strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(s.PublicKey())), "\n")
}

// GenerateKey returns the Ed25519 key stored at pemPath. If no file exists there, it
// creates a new key: the private key is written in OpenSSH PEM form with mode 0600,
// and its public half, as an authorized_keys line, goes to pemPath+".pub". An existing
// file is loaded and never overwritten.
func GenerateKey(pemPath string) (ssh.Signer, error) {
	if _, err := os.Stat(pemPath); err == nil {
		return LoadSigner(pemPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("sshrunner: stat key: %w", err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sshrunner: generate key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, fmt.Errorf("sshrunner: encode key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("sshrunner: wrap key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(pemPath), 0o700); err != nil {
		return nil, fmt.Errorf("sshrunner: create key dir: %w", err)
	}

	// O_EXCL keeps a concurrent creator from being overwritten; if it won the race,
	// load its key instead.
	f, err := os.OpenFile(pemPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return LoadSigner(pemPath)
	}
	if err != nil {
		return nil, fmt.Errorf("sshrunner: create key: %w", err)
	}
	if _, err := f.Write(pem.EncodeToMemory(block)); err != nil {
		f.Close()
		os.Remove(pemPath)
		return nil, fmt.Errorf("sshrunner: write key: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(pemPath)
		return nil, fmt.Errorf("sshrunner: write key: %w", err)
	}
	if err := os.WriteFile(pemPath+".pub", []byte(PublicKeyLine(signer)+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("sshrunner: write public key: %w", err)
	}
	return signer, nil
}
