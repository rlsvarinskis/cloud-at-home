package service

import (
	"net/http"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1/vmdv1connect"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/server/obs"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/store"
)

// Deps is everything the vmd API needs.
type Deps struct {
	Store    store.Store
	Nodes    Nodes
	Verifier *authz.Verifier
	Config   Config

	// Registry receives RPC metrics and is served at /metrics (operator token required). Nil disables both.
	Registry *prometheus.Registry
	// Ready are extra readiness checks for /readyz (the store is always checked by the caller if desired).
	Ready []obs.NamedCheck
}

// NewHandler assembles the vmd HTTP surface: the five Connect services (gRPC, gRPC-Web and
// HTTP+JSON on the same paths), /healthz, /readyz and /metrics.
func NewHandler(d Deps) http.Handler {
	cfg := d.Config.withDefaults()
	b := base{st: d.Store, nodes: d.Nodes, cfg: cfg}

	// Order matters: metrics see the final status code, errors are mapped before that, auth runs innermost.
	var ics []connect.Interceptor
	if d.Registry != nil {
		ics = append(ics, obs.NewRPCMetrics(d.Registry, "vmd"))
	}
	ics = append(ics, obs.ErrorInterceptor(cfg.Log), &authz.ServerInterceptor{Verifier: d.Verifier, Log: cfg.Log})
	opts := []connect.HandlerOption{connect.WithInterceptors(ics...), connect.WithRecover(recoverPanic(cfg))}

	mux := http.NewServeMux()
	mux.Handle(vmdv1connect.NewVMServiceHandler(&VMServer{b}, opts...))
	mux.Handle(vmdv1connect.NewDiskServiceHandler(&DiskServer{b}, opts...))
	mux.Handle(vmdv1connect.NewImageServiceHandler(&ImageServer{b}, opts...))
	mux.Handle(vmdv1connect.NewExecServiceHandler(&ExecServer{b}, opts...))
	mux.Handle(vmdv1connect.NewMetricsServiceHandler(&MetricsServer{b}, opts...))
	mux.Handle(vmdv1connect.NewClusterServiceHandler(&ClusterServer{b}, opts...))

	live, ready := obs.HealthHandlers(d.Ready...)
	mux.Handle("/healthz", live)
	mux.Handle("/readyz", ready)
	if d.Registry != nil {
		mux.Handle("/metrics", obs.MetricsHandler(d.Registry, d.Verifier))
	}
	return mux
}
