package obs

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
)

// MetricsHandler serves the Prometheus exposition of gatherer. Callers must present
// "Authorization: Bearer <jwt>" that verifies with v and whose principal holds authz.CapacityGet
// on the global scope (a "*" binding). A missing or invalid token gets 401; a valid token without
// the permission gets 403. Neither response says why.
//
// If v is nil the handler serves without authentication. This is for local development only and
// must never be exposed on a network.
func MetricsHandler(gatherer prometheus.Gatherer, v *authz.Verifier) http.Handler {
	metrics := promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{})
	if v == nil {
		return metrics
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		p, err := v.Verify(tok)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !p.Can(authz.CapacityGet, "") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		metrics.ServeHTTP(w, r)
	})
}

// Check reports whether a dependency is healthy. A nil error means healthy.
type Check func(ctx context.Context) error

// NamedCheck is a readiness check identified by name in the readiness response.
type NamedCheck struct {
	Name  string
	Check Check
}

// checkTimeout bounds each readiness check. It is a variable so tests can lower it.
var checkTimeout = 2 * time.Second

// HealthHandlers returns the liveness and readiness handlers. Liveness always answers 200 "ok".
// Readiness runs every check concurrently, each with a 2 s timeout, and answers 200
// {"status":"ok"} when all pass or 503 {"status":"unavailable","failed":["name",...]} listing the
// failed checks in the order given. Error text is deliberately not returned to the caller.
func HealthHandlers(ready ...NamedCheck) (live, readyH http.Handler) {
	live = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	readyH = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		passed := make([]bool, len(ready))
		var wg sync.WaitGroup
		for i, nc := range ready {
			wg.Add(1)
			go func() {
				defer wg.Done()
				passed[i] = runCheck(r.Context(), nc.Check)
			}()
		}
		wg.Wait()

		var failed []string
		for i, ok := range passed {
			if !ok {
				failed = append(failed, ready[i].Name)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if len(failed) == 0 {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "unavailable", "failed": failed})
	})
	return live, readyH
}

// runCheck runs c under checkTimeout and reports whether it passed. A check that ignores its
// context and runs past the timeout counts as failed; its goroutine finishes on its own later.
func runCheck(parent context.Context, c Check) bool {
	ctx, cancel := context.WithTimeout(parent, checkTimeout)
	defer cancel()
	done := make(chan error, 1) // buffered so a late result never blocks the goroutine
	go func() { done <- c(ctx) }()
	select {
	case err := <-done:
		return err == nil
	case <-ctx.Done():
		return false
	}
}

// RegisterBuildInfo registers attic_build_info{version,go_version} with value 1, identifying
// the running build. It panics if already registered with reg.
func RegisterBuildInfo(reg prometheus.Registerer, version string) {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "attic_build_info",
		Help: "Build information. The value is always 1.",
	}, []string{"version", "go_version"})
	g.WithLabelValues(version, runtime.Version()).Set(1)
	reg.MustRegister(g)
}

// NewRegistry returns a fresh registry with the Go runtime and process collectors registered.
// Servers use it instead of the global default registry.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}
