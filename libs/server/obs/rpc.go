// Package obs is the observability layer shared by the vmd and agentd servers: RPC metrics, the
// protected Prometheus endpoint, health endpoints and build information.
//
// Servers create their own *prometheus.Registry (see NewRegistry) rather than using the global
// default, so that only the collectors they explicitly register are exposed.
package obs

import (
	"context"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
)

// rpcBuckets are the latency buckets (seconds) for RPC durations: 5 ms up to 30 s.
var rpcBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// RPCMetrics records server-side Connect RPC traffic. It is a connect.Interceptor: add it to a
// handler with connect.WithInterceptors. Client-side calls are not recorded.
type RPCMetrics struct {
	requests *prometheus.CounterVec   // attic_rpc_requests_total{service,method,code}
	duration *prometheus.HistogramVec // attic_rpc_duration_seconds{service,method}
	inFlight *prometheus.GaugeVec     // attic_rpc_in_flight{service,method}
}

var _ connect.Interceptor = (*RPCMetrics)(nil)

// NewRPCMetrics creates the RPC metrics and registers them with reg. Every series carries a
// constant label server=subsystem (for example "vmd" or "agentd"), so two servers can share one
// registry. It panics if the metrics are already registered with reg with the same subsystem.
func NewRPCMetrics(reg prometheus.Registerer, subsystem string) *RPCMetrics {
	constLabels := prometheus.Labels{"server": subsystem}
	m := &RPCMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "attic_rpc_requests_total",
			Help:        "Completed server-side RPCs, by service, method and Connect status code.",
			ConstLabels: constLabels,
		}, []string{"service", "method", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "attic_rpc_duration_seconds",
			Help:        "Server-side RPC duration in seconds. Streaming RPCs are timed for the whole stream.",
			Buckets:     rpcBuckets,
			ConstLabels: constLabels,
		}, []string{"service", "method"}),
		inFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name:        "attic_rpc_in_flight",
			Help:        "Server-side RPCs currently being handled.",
			ConstLabels: constLabels,
		}, []string{"service", "method"}),
	}
	reg.MustRegister(m.requests, m.duration, m.inFlight)
	return m
}

// WrapUnary records a unary server call.
func (m *RPCMetrics) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		svc, method := splitProcedure(req.Spec().Procedure)
		done := m.start(svc, method)
		res, err := next(ctx, req)
		done(err)
		return res, err
	}
}

// WrapStreamingClient passes client-side streams through unrecorded: these metrics describe the
// server only.
func (m *RPCMetrics) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler records a streaming server call for the lifetime of the stream.
func (m *RPCMetrics) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if conn.Spec().IsClient {
			return next(ctx, conn)
		}
		svc, method := splitProcedure(conn.Spec().Procedure)
		done := m.start(svc, method)
		err := next(ctx, conn)
		done(err)
		return err
	}
}

// start marks a call as in flight and returns the function that completes it with its result.
func (m *RPCMetrics) start(service, method string) func(err error) {
	inFlight := m.inFlight.WithLabelValues(service, method)
	inFlight.Inc()
	began := time.Now()
	return func(err error) {
		inFlight.Dec()
		m.duration.WithLabelValues(service, method).Observe(time.Since(began).Seconds())
		m.requests.WithLabelValues(service, method, codeString(err)).Inc()
	}
}

// codeString is the Connect code of err, or "ok" when err is nil.
func codeString(err error) string {
	if err == nil {
		return "ok"
	}
	return connect.CodeOf(err).String()
}

// splitProcedure turns "/package.Service/Method" into ("package.Service", "Method"). A procedure
// without that shape is reported whole as the method, with an empty service.
func splitProcedure(procedure string) (service, method string) {
	trimmed := strings.TrimPrefix(procedure, "/")
	i := strings.LastIndex(trimmed, "/")
	if i < 0 {
		return "", procedure
	}
	return trimmed[:i], trimmed[i+1:]
}
