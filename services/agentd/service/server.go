package service

import (
	"context"
	"errors"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/agentd/v1/agentdv1connect"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/server/obs"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/llm"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/store"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/vmclient"
)

// Deps is everything the agentd API needs.
type Deps struct {
	Store    store.Store
	VM       vmclient.VM
	LLM      llm.Client
	Verifier *authz.Verifier
	Config   Config

	// Registry receives RPC metrics and is served at /metrics (operator token required). Nil disables both.
	Registry *prometheus.Registry
	// Ready are extra readiness checks for /readyz.
	Ready []obs.NamedCheck
}

// base carries what every service handler needs.
type base struct {
	st  store.Store
	vm  vmclient.VM
	llm llm.Client
	cfg Config
}

func (b *base) now() time.Time { return b.cfg.Now().UTC() }

// NewHandler assembles the agentd HTTP surface: the Agent, Task and Model Connect services (gRPC,
// gRPC-Web and HTTP+JSON on the same paths), /healthz, /readyz and /metrics.
func NewHandler(d Deps) http.Handler {
	cfg := d.Config.withDefaults()
	b := base{st: d.Store, vm: d.VM, llm: d.LLM, cfg: cfg}

	// Order matters: metrics see the final status code, errors are mapped before that, auth runs innermost.
	var ics []connect.Interceptor
	if d.Registry != nil {
		ics = append(ics, obs.NewRPCMetrics(d.Registry, "agentd"))
	}
	ics = append(ics, obs.ErrorInterceptor(cfg.Log), &authz.ServerInterceptor{Verifier: d.Verifier, Log: cfg.Log})
	opts := []connect.HandlerOption{connect.WithInterceptors(ics...), connect.WithRecover(recoverPanic(cfg))}

	mux := http.NewServeMux()
	mux.Handle(agentdv1connect.NewAgentServiceHandler(&AgentServer{b}, opts...))
	mux.Handle(agentdv1connect.NewTaskServiceHandler(&TaskServer{b}, opts...))
	mux.Handle(agentdv1connect.NewModelServiceHandler(&ModelServer{b}, opts...))

	live, ready := obs.HealthHandlers(d.Ready...)
	mux.Handle("/healthz", live)
	mux.Handle("/readyz", ready)
	if d.Registry != nil {
		mux.Handle("/metrics", obs.MetricsHandler(d.Registry, d.Verifier))
	}
	return mux
}

// recoverPanic turns a handler panic into a generic Internal error and logs the details.
func recoverPanic(cfg Config) func(context.Context, connect.Spec, http.Header, any) error {
	return func(_ context.Context, spec connect.Spec, _ http.Header, p any) error {
		cfg.Log.Error("panic in handler", "procedure", spec.Procedure, "panic", p)
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
}

// ---- shared validation and paging helpers ----

var nameRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validName(kind, n string) error {
	if !nameRE.MatchString(n) {
		return errs.Invalid("%s name %q must be 1-63 chars of a-z, 0-9 and '-', starting with a letter", kind, n)
	}
	return nil
}

// validProject requires a concrete project; "*" (every project) is a binding, not a project.
func validProject(p string) error {
	if p == "" || p == authz.AnyProject {
		return errs.Invalid("project_id is required")
	}
	return nil
}

func validSystemPrompt(s string) error {
	if len(s) > maxSystemPromptBytes {
		return errs.Invalid("system_prompt is %d bytes; the limit is %d", len(s), maxSystemPromptBytes)
	}
	return checkNoNUL("system_prompt", s)
}

// checkNoNUL rejects NUL bytes, which the store and the VM cannot represent in text fields.
func checkNoNUL(field, s string) error {
	if strings.IndexByte(s, 0) >= 0 {
		return errs.Invalid("%s must not contain NUL bytes", field)
	}
	return nil
}

// validWorkdir requires an absolute, clean path without NUL bytes.
func validWorkdir(w string) error {
	if err := checkNoNUL("workdir", w); err != nil {
		return err
	}
	if !path.IsAbs(w) || path.Clean(w) != w {
		return errs.Invalid("workdir %q must be an absolute, clean path", w)
	}
	return nil
}

// checkModel reports whether model is installed on the LLM backend. If the backend cannot be reached
// the check is skipped: the model may still be valid, and the task will fail later with a clear error.
func (b *base) checkModel(ctx context.Context, model string) error {
	installed, err := b.llm.ListModels(ctx)
	if err != nil {
		b.cfg.Log.Warn("model validation skipped: list models failed", "err", err)
		return nil
	}
	names := make([]string, 0, len(installed))
	for _, m := range installed {
		if m.Name == model {
			return nil
		}
		names = append(names, m.Name)
	}
	if len(names) == 0 {
		return errs.Invalid("model %q is not installed; no models are installed", model)
	}
	return errs.Invalid("model %q is not installed; installed models: %s", model, strings.Join(names, ", "))
}

func pageSize(n int32) int {
	switch {
	case n <= 0:
		return 50
	case n > 200:
		return 200
	}
	return int(n)
}

// nextToken: lists fetch limit+1 rows; the extra row proves there is another page, and the token is the
// ID of the last row returned (the next page starts after it).
func nextToken[T any](items []T, limit int, id func(T) string) ([]T, string) {
	if len(items) > limit {
		items = items[:limit]
		return items, id(items[len(items)-1])
	}
	return items, ""
}

// poll re-evaluates check every 200 ms until it reports done, the wait expires or ctx ends.
// waitSeconds is clamped to maxWaitSeconds (clients and proxies give up around 60 s).
func poll(ctx context.Context, waitSeconds int32, check func(context.Context) (bool, error)) error {
	wait := min(max(waitSeconds, 0), maxWaitSeconds)
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		done, err := check(ctx)
		if err != nil || done {
			return err
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// errUnchanged tells a mutate loop that nothing needs writing; the loop returns the current row.
var errUnchanged = errors.New("unchanged")

// maxCASAttempts bounds read-modify-write loops that retry on ErrConflict.
const maxCASAttempts = 8

// downstream reports a failure of vmd or the model backend to a client without infrastructure detail
// (see errs.Sanitize); the original is logged.
func (b *base) downstream(what string, err error) error {
	out, hidden := errs.Sanitize(what, err)
	if hidden {
		b.cfg.Log.Warn("downstream call failed", "service", what, "err", err)
	}
	return out
}
