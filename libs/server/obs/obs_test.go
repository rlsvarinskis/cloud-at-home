package obs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1/vmdv1connect"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

// vmStub answers GetVM differently depending on the requested ID.
type vmStub struct {
	vmdv1connect.UnimplementedVMServiceHandler
}

func (vmStub) GetVM(_ context.Context, req *connect.Request[vmdv1.GetVMRequest]) (*connect.Response[vmdv1.VM], error) {
	switch req.Msg.Id {
	case "ok":
		return connect.NewResponse(&vmdv1.VM{Name: "vm-ok"}), nil
	case "denied":
		return nil, errs.ToConnect(fmt.Errorf("%w: no access", errs.ErrPermissionDenied))
	default:
		return nil, errs.ToConnect(errs.NotFound("vm %s", req.Msg.Id))
	}
}

const getVMProc = "/homecloud.vmd.v1.VMService/GetVM"

func TestSplitProcedure(t *testing.T) {
	svc, method := splitProcedure(getVMProc)
	if svc != "homecloud.vmd.v1.VMService" || method != "GetVM" {
		t.Fatalf("got %q %q", svc, method)
	}
	if svc, method := splitProcedure("weird"); svc != "" || method != "weird" {
		t.Fatalf("malformed procedure: got %q %q", svc, method)
	}
}

func TestRPCMetricsOverConnect(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewRPCMetrics(reg, "vmd")
	mux := http.NewServeMux()
	mux.Handle(vmdv1connect.NewVMServiceHandler(vmStub{}, connect.WithInterceptors(m)))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := vmdv1connect.NewVMServiceClient(srv.Client(), srv.URL)

	for _, id := range []string{"ok", "ok", "missing", "denied"} {
		_, _ = client.GetVM(context.Background(), connect.NewRequest(&vmdv1.GetVMRequest{Id: id}))
	}

	counter := func(code string) float64 {
		return testutil.ToFloat64(m.requests.WithLabelValues("homecloud.vmd.v1.VMService", "GetVM", code))
	}
	if got := counter("ok"); got != 2 {
		t.Errorf("ok count = %v, want 2", got)
	}
	if got := counter("not_found"); got != 1 {
		t.Errorf("not_found count = %v, want 1", got)
	}
	if got := counter("permission_denied"); got != 1 {
		t.Errorf("permission_denied count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.inFlight.WithLabelValues("homecloud.vmd.v1.VMService", "GetVM")); got != 0 {
		t.Errorf("in flight after calls = %v, want 0", got)
	}
	if got := histogramCount(t, reg, "homecloud_rpc_duration_seconds"); got != 4 {
		t.Errorf("histogram sample count = %v, want 4", got)
	}

	// the server label is a constant on every series
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), "homecloud_rpc_") {
			continue
		}
		for _, mt := range f.GetMetric() {
			found := false
			for _, lp := range mt.GetLabel() {
				if lp.GetName() == "server" && lp.GetValue() == "vmd" {
					found = true
				}
			}
			if !found {
				t.Errorf("%s series lacks server=vmd: %v", f.GetName(), mt.GetLabel())
			}
		}
	}
}

func TestRPCMetricsClientCallsNotRecorded(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewRPCMetrics(reg, "vmd")
	// Server has no metrics interceptor; the client carries one. Nothing may be recorded.
	mux := http.NewServeMux()
	mux.Handle(vmdv1connect.NewVMServiceHandler(vmStub{}))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := vmdv1connect.NewVMServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(m))
	if _, err := client.GetVM(context.Background(), connect.NewRequest(&vmdv1.GetVMRequest{Id: "ok"})); err != nil {
		t.Fatal(err)
	}
	if n, err := testutil.GatherAndCount(reg, "homecloud_rpc_requests_total"); err != nil || n != 0 {
		t.Fatalf("client call recorded: %d series, %v", n, err)
	}
}

// histogramCount sums the sample counts of every series in the named histogram family.
func histogramCount(t *testing.T, reg prometheus.Gatherer, name string) uint64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, mt := range f.GetMetric() {
			total += mt.GetHistogram().GetSampleCount()
		}
	}
	return total
}

// authFixture mints tokens with a fresh key and verifies them with the matching trusted key.
type authFixture struct {
	iss *authz.Issuer
	v   *authz.Verifier
}

func newAuthFixture(t *testing.T) authFixture {
	t.Helper()
	pub, priv, err := authz.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return authFixture{
		iss: &authz.Issuer{KeyID: "k1", Key: priv},
		v:   &authz.Verifier{Keys: map[string]authz.TrustedKey{"k1": {Public: pub}}},
	}
}

func (f authFixture) token(t *testing.T, bindings ...authz.Binding) string {
	t.Helper()
	tok, err := f.iss.Mint(authz.Principal{Subject: "user:test", Type: authz.User, Bindings: bindings}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func serve(h http.Handler, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMetricsHandlerAuthorization(t *testing.T) {
	f := newAuthFixture(t)
	reg := prometheus.NewRegistry()
	NewRPCMetrics(reg, "vmd").requests.WithLabelValues("svc", "m", "ok").Inc()
	h := MetricsHandler(reg, f.v)

	editor := f.token(t, authz.Binding{Project: "p1", Role: authz.RoleEditor})
	operatorP1 := f.token(t, authz.Binding{Project: "p1", Role: authz.RoleOperator})
	operatorAll := f.token(t, authz.Binding{Project: authz.AnyProject, Role: authz.RoleOperator})

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"not bearer", "Basic abc", http.StatusUnauthorized},
		{"garbage token", "Bearer garbage", http.StatusUnauthorized},
		{"editor on p1", "Bearer " + editor, http.StatusForbidden},
		{"operator bound to p1 only", "Bearer " + operatorP1, http.StatusForbidden},
		{"operator on *", "Bearer " + operatorAll, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := serve(h, c.header)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
			if c.want == http.StatusOK && !strings.Contains(rec.Body.String(), "homecloud_rpc_requests_total") {
				t.Fatalf("body lacks rpc metrics:\n%s", rec.Body.String())
			}
			if c.want != http.StatusOK && strings.Contains(rec.Body.String(), "homecloud") {
				t.Fatalf("error body leaks metrics: %q", rec.Body.String())
			}
		})
	}
}

func TestMetricsHandlerNilVerifierServesWithoutAuth(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterBuildInfo(reg, "dev")
	rec := serve(MetricsHandler(reg, nil), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "homecloud_build_info") {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestHealthLiveness(t *testing.T) {
	live, _ := HealthHandlers()
	rec := serve(live, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestHealthReadiness(t *testing.T) {
	t.Run("all pass", func(t *testing.T) {
		_, ready := HealthHandlers(
			NamedCheck{Name: "db", Check: func(context.Context) error { return nil }},
			NamedCheck{Name: "node", Check: func(context.Context) error { return nil }},
		)
		rec := serve(ready, "")
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
			t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
		}
	})

	t.Run("one fails, error text hidden", func(t *testing.T) {
		_, ready := HealthHandlers(
			NamedCheck{Name: "db", Check: func(context.Context) error { return nil }},
			NamedCheck{Name: "node", Check: func(context.Context) error { return errors.New("secret detail") }},
		)
		rec := serve(ready, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d", rec.Code)
		}
		want := `{"failed":["node"],"status":"unavailable"}`
		if strings.TrimSpace(rec.Body.String()) != want {
			t.Fatalf("body %q, want %q", rec.Body.String(), want)
		}
		if strings.Contains(rec.Body.String(), "secret") {
			t.Fatal("error text leaked into the response")
		}
	})

	t.Run("slow check counts as failed", func(t *testing.T) {
		old := checkTimeout
		checkTimeout = 50 * time.Millisecond
		t.Cleanup(func() { checkTimeout = old })

		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		_, ready := HealthHandlers(
			NamedCheck{Name: "fast", Check: func(context.Context) error { return nil }},
			NamedCheck{Name: "slow", Check: func(context.Context) error {
				<-release // ignores its context entirely
				return nil
			}},
		)
		start := time.Now()
		rec := serve(ready, "")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"slow"`) {
			t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), `"fast"`) {
			t.Fatalf("passing check reported failed: %q", rec.Body.String())
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("readiness took %v; checks must run concurrently and time out", elapsed)
		}
	})
}

func TestRegisterBuildInfo(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterBuildInfo(reg, "1.2.3")
	want := fmt.Sprintf(`
# HELP homecloud_build_info Build information. The value is always 1.
# TYPE homecloud_build_info gauge
homecloud_build_info{go_version=%q,version="1.2.3"} 1
`, runtime.Version())
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "homecloud_build_info"); err != nil {
		t.Fatal(err)
	}
}

func TestNewRegistryHasRuntimeCollectors(t *testing.T) {
	reg := NewRegistry()
	n, err := testutil.GatherAndCount(reg, "go_goroutines", "process_cpu_seconds_total")
	if err != nil || n == 0 {
		t.Fatalf("runtime collectors missing: %d, %v", n, err)
	}
}

func TestTwoServersShareRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	vmd := NewRPCMetrics(reg, "vmd")
	agentd := NewRPCMetrics(reg, "agentd")
	vmd.requests.WithLabelValues("s", "m", "ok").Inc()
	agentd.requests.WithLabelValues("s", "m", "ok").Inc()
	if n, err := testutil.GatherAndCount(reg, "homecloud_rpc_requests_total"); err != nil || n != 2 {
		t.Fatalf("got %d series, %v; want 2", n, err)
	}
}
