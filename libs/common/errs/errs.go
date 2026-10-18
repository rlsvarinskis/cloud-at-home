// Package errs is the shared error vocabulary. Services return these (wrapped with context);
// ToConnect maps them to RPC status codes at the edge, so business code never imports transport types.
package errs

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
)

var (
	ErrNotFound           = errors.New("not found")
	ErrAlreadyExists      = errors.New("already exists")
	ErrInvalidArgument    = errors.New("invalid argument")
	ErrFailedPrecondition = errors.New("failed precondition")
	ErrPermissionDenied   = errors.New("permission denied")
	ErrUnauthenticated    = errors.New("unauthenticated")
	ErrResourceExhausted  = errors.New("resource exhausted")
	ErrUnavailable        = errors.New("unavailable")
	ErrConflict           = errors.New("conflict") // optimistic concurrency failure
)

func NotFound(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, prepend(ErrNotFound, a)...)
}
func Invalid(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, prepend(ErrInvalidArgument, a)...)
}
func Precondition(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, prepend(ErrFailedPrecondition, a)...)
}
func Exists(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, prepend(ErrAlreadyExists, a)...)
}
func Exhausted(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, prepend(ErrResourceExhausted, a)...)
}
func Unavailable(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, prepend(ErrUnavailable, a)...)
}

func prepend(first error, rest []any) []any { return append([]any{first}, rest...) }

// ToConnect converts any error to a *connect.Error with a suitable code.
// Unknown errors become Internal and hide their text from the client (it is logged by the caller).
func ToConnect(err error) error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce
	}
	code := connect.CodeInternal
	switch {
	case errors.Is(err, ErrNotFound):
		code = connect.CodeNotFound
	case errors.Is(err, ErrAlreadyExists):
		code = connect.CodeAlreadyExists
	case errors.Is(err, ErrInvalidArgument):
		code = connect.CodeInvalidArgument
	case errors.Is(err, ErrFailedPrecondition):
		code = connect.CodeFailedPrecondition
	case errors.Is(err, ErrPermissionDenied):
		code = connect.CodePermissionDenied
	case errors.Is(err, ErrUnauthenticated):
		code = connect.CodeUnauthenticated
	case errors.Is(err, ErrResourceExhausted):
		code = connect.CodeResourceExhausted
	case errors.Is(err, ErrUnavailable):
		code = connect.CodeUnavailable
	case errors.Is(err, ErrConflict):
		code = connect.CodeAborted
	case errors.Is(err, context.DeadlineExceeded):
		code = connect.CodeDeadlineExceeded
	case errors.Is(err, context.Canceled):
		code = connect.CodeCanceled
	}
	if code == connect.CodeInternal {
		return connect.NewError(code, errors.New("internal error"))
	}
	return connect.NewError(code, err)
}

// FromConnect is the inverse, for clients calling another service: it keeps the sentinel
// so callers can use errors.Is(err, errs.ErrNotFound) regardless of transport.
func FromConnect(err error) error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return err
	}
	var sentinel error
	switch ce.Code() {
	case connect.CodeNotFound:
		sentinel = ErrNotFound
	case connect.CodeAlreadyExists:
		sentinel = ErrAlreadyExists
	case connect.CodeInvalidArgument:
		sentinel = ErrInvalidArgument
	case connect.CodeFailedPrecondition:
		sentinel = ErrFailedPrecondition
	case connect.CodePermissionDenied:
		sentinel = ErrPermissionDenied
	case connect.CodeUnauthenticated:
		sentinel = ErrUnauthenticated
	case connect.CodeResourceExhausted:
		sentinel = ErrResourceExhausted
	case connect.CodeUnavailable:
		sentinel = ErrUnavailable
	case connect.CodeAborted:
		sentinel = ErrConflict
	default:
		return err
	}
	return fmt.Errorf("%w: %s", sentinel, ce.Message())
}

// Sanitize prepares an error from a downstream system (a node, another service, the model backend) for an API
// client. Answers that mean something to the caller (not found, invalid, precondition, permission, quota,
// conflict) pass through. Everything else (transport failures, unknown errors) would expose hostnames, ports and
// internals, so it is replaced by a generic Unavailable and reported as hidden so the caller can log the original.
func Sanitize(what string, err error) (clientErr error, hidden bool) {
	switch {
	case err == nil:
		return nil, false
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, ErrNotFound), errors.Is(err, ErrInvalidArgument), errors.Is(err, ErrFailedPrecondition),
		errors.Is(err, ErrAlreadyExists), errors.Is(err, ErrPermissionDenied), errors.Is(err, ErrResourceExhausted),
		errors.Is(err, ErrConflict), errors.Is(err, ErrUnauthenticated):
		return err, false
	}
	return Unavailable("%s is unavailable", what), true
}
