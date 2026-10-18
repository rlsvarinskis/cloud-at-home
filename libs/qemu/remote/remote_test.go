package remote_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/attic/node/v1/nodev1connect"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/remote"
)

// stub implements only GetCapacity. It records the Authorization header it saw and fails with err.
type stub struct {
	nodev1connect.UnimplementedNodeServiceHandler
	err     error
	gotAuth string
}

func (s *stub) GetCapacity(_ context.Context, req *connect.Request[nodev1.GetCapacityRequest]) (*connect.Response[nodev1.GetCapacityResponse], error) {
	s.gotAuth = req.Header().Get("Authorization")
	if s.err != nil {
		return nil, errs.ToConnect(s.err) // what nodeserver does to driver errors
	}
	return connect.NewResponse(&nodev1.GetCapacityResponse{Capacity: &nodev1.Capacity{CpusTotal: 4}}), nil
}

func TestDriverForwardsTokenAndResponse(t *testing.T) {
	h := &stub{}
	mux := http.NewServeMux()
	mux.Handle(nodev1connect.NewNodeServiceHandler(h))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := remote.New(srv.Client(), srv.URL, authz.StaticToken("tok-1"))
	res, err := d.GetCapacity(context.Background(), &nodev1.GetCapacityRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetCapacity().GetCpusTotal() != 4 {
		t.Fatalf("cpus = %d, want 4", res.GetCapacity().GetCpusTotal())
	}
	if h.gotAuth != "Bearer tok-1" {
		t.Fatalf("Authorization = %q", h.gotAuth)
	}
}

func TestDriverMapsErrorsToSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"not found", errs.NotFound("disk %q", "d1"), errs.ErrNotFound},
		{"precondition", errs.Precondition("vm running"), errs.ErrFailedPrecondition},
		{"invalid", errs.Invalid("size"), errs.ErrInvalidArgument},
		{"denied", errs.ErrPermissionDenied, errs.ErrPermissionDenied},
		{"unavailable", errs.Unavailable("down"), errs.ErrUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &stub{err: c.err}
			mux := http.NewServeMux()
			mux.Handle(nodev1connect.NewNodeServiceHandler(h))
			srv := httptest.NewServer(mux)
			defer srv.Close()

			d := remote.New(srv.Client(), srv.URL, authz.StaticToken("t"))
			_, err := d.GetCapacity(context.Background(), &nodev1.GetCapacityRequest{})
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want errors.Is %v", err, c.want)
			}
		})
	}
}

func TestDriverUnreachableIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens here any more: connection refused

	d := remote.New(http.DefaultClient, url, authz.StaticToken("t"))
	_, err := d.GetCapacity(context.Background(), &nodev1.GetCapacityRequest{})
	if !errors.Is(err, errs.ErrUnavailable) {
		t.Fatalf("err = %v, want errs.ErrUnavailable", err)
	}
}

func TestDriverTokenSourceErrorIsUnauthenticated(t *testing.T) {
	src := func(context.Context) (string, error) { return "", errors.New("no token") }
	d := remote.New(http.DefaultClient, "http://127.0.0.1:1", src)
	_, err := d.GetCapacity(context.Background(), &nodev1.GetCapacityRequest{})
	if !errors.Is(err, errs.ErrUnauthenticated) {
		t.Fatalf("err = %v, want errs.ErrUnauthenticated", err)
	}
}
