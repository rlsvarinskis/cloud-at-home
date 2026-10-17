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
