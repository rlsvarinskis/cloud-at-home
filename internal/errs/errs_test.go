package errs

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
)

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		err  error
		code connect.Code
		is   error
	}{
		{NotFound("vm %s", "x"), connect.CodeNotFound, ErrNotFound},
		{Invalid("bad"), connect.CodeInvalidArgument, ErrInvalidArgument},
		{Precondition("busy"), connect.CodeFailedPrecondition, ErrFailedPrecondition},
		{Exists("dup"), connect.CodeAlreadyExists, ErrAlreadyExists},
		{Exhausted("quota"), connect.CodeResourceExhausted, ErrResourceExhausted},
		{Unavailable("down"), connect.CodeUnavailable, ErrUnavailable},
		{fmt.Errorf("wrapped: %w", ErrPermissionDenied), connect.CodePermissionDenied, ErrPermissionDenied},
		{fmt.Errorf("w: %w", ErrConflict), connect.CodeAborted, ErrConflict},
		{context.DeadlineExceeded, connect.CodeDeadlineExceeded, nil},
	}
	for _, c := range cases {
		ce := ToConnect(c.err)
		var got *connect.Error
		if !errors.As(ce, &got) || got.Code() != c.code {
			t.Errorf("%v: code = %v, want %v", c.err, got, c.code)
			continue
		}
		if c.is != nil && !errors.Is(FromConnect(ce), c.is) {
			t.Errorf("%v: FromConnect lost the sentinel", c.err)
		}
	}
}

func TestInternalErrorsAreHidden(t *testing.T) {
	ce := ToConnect(errors.New("pq: password authentication failed for user x"))
	var got *connect.Error
	if !errors.As(ce, &got) || got.Code() != connect.CodeInternal {
		t.Fatalf("want Internal, got %v", ce)
	}
	if got.Message() != "internal error" {
		t.Fatalf("internal details leaked: %q", got.Message())
	}
	if ToConnect(nil) != nil || FromConnect(nil) != nil {
		t.Fatal("nil must stay nil")
	}
}

func TestSanitizeHidesInfrastructureDetail(t *testing.T) {
	raw := Unavailable(`node: Post "http://192.0.2.7:9000/x": dial tcp 192.0.2.7:9000: connect: connection refused`)
	got, hidden := Sanitize("node n1", raw)
	if !hidden || !errors.Is(got, ErrUnavailable) || got.Error() != "unavailable: node n1 is unavailable" {
		t.Fatalf("transport error: %v hidden=%v", got, hidden)
	}
	if got, hidden := Sanitize("x", errors.New("some driver bug at /var/lib/foo")); !hidden || errors.Is(got, ErrNotFound) {
		t.Fatalf("unknown errors must be hidden: %v", got)
	}
	for _, keep := range []error{NotFound("vm x"), Invalid("bad"), Precondition("busy"), Exists("dup"), Exhausted("quota"),
		ErrPermissionDenied, ErrConflict, context.Canceled, context.DeadlineExceeded} {
		if got, hidden := Sanitize("x", keep); hidden || got != keep {
			t.Errorf("%v must pass through unchanged, got %v hidden=%v", keep, got, hidden)
		}
	}
	if got, hidden := Sanitize("x", nil); got != nil || hidden {
		t.Fatal("nil stays nil")
	}
}
