package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"

	"connectrpc.com/connect"
)

// Env is what a command runs with: its standard streams, the resolved global configuration and the HTTP client.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer // data output; a command writes nothing else here
	Stderr io.Writer // messages and confirmations
	Config Config
	HTTP   *http.Client

	// Command is the full command path, for example "attic vm cp". Set by the dispatcher.
	Command string
}

// Flags returns a new flag set for a command. The flag package prints nothing itself; Parse turns its errors into
// UsageErrors and -h into ErrHelp, and the dispatcher prints both.
func (e *Env) Flags() *flag.FlagSet {
	fs := flag.NewFlagSet(e.Command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// Parse parses a command's arguments with fs. It returns ErrHelp for -h and -help, and a UsageError for any other
// parse error.
func (e *Env) Parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ErrHelp
		}
		return Usagef("%v", err)
	}
	return nil
}

// ClientOptions returns the Connect options for calls to the configured vmd. Every call sends the token as
// "Authorization: Bearer". It fails when no token is configured, so no command sends an anonymous request by accident.
// Only clients built with these options carry the token.
func (e *Env) ClientOptions() ([]connect.ClientOption, error) {
	if e.Config.Token == "" {
		return nil, errors.New("no API token configured: set ATTIC_TOKEN_FILE or ATTIC_TOKEN, pass --token-file, " +
			"or write the token to ~/.config/attic/token")
	}
	return []connect.ClientOption{connect.WithInterceptors(bearer{header: "Bearer " + e.Config.Token.Reveal()})}, nil
}

// RPCError rewrites an error from a Connect call as "code: message", the form the CLI shows, for example
// "not_found: VM not found". Other errors pass unchanged.
func RPCError(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return fmt.Errorf("%s: %s", ce.Code(), ce.Message())
	}
	return err
}

// bearer is a client interceptor that sets the Authorization header of every call it wraps.
type bearer struct {
	header string
}

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", b.header)
		return next(ctx, req)
	}
}

func (b bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", b.header)
		return conn
	}
}

func (b bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
