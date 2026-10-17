// Command agentd serves the agent API and runs the workers that execute agent tasks.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/homecloud/internal/agentd/llm"
	"example.com/homecloud/internal/agentd/metrics"
	"example.com/homecloud/internal/agentd/runner"
	"example.com/homecloud/internal/agentd/service"
	"example.com/homecloud/internal/agentd/store"
	pgstore "example.com/homecloud/internal/agentd/store/postgres"
	"example.com/homecloud/internal/agentd/vmclient"
	"example.com/homecloud/internal/auth"
	"example.com/homecloud/internal/daemon"
	"example.com/homecloud/internal/obs"
	"example.com/homecloud/internal/pg"
)

var version = "dev" // set with -ldflags "-X main.version=..."

// onListen, when set (tests), is told the address the server listens on.
var onListen func(net.Addr)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "agentd:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("agentd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", ":8081", "address to listen on")
	dsn := fs.String("postgres-dsn", os.Getenv("HOMECLOUD_POSTGRES_DSN"), "Postgres DSN; empty = in-memory store, development only")
	logLevel := fs.String("log-level", "info", "debug | info | warn | error")
	maxTTL := fs.Duration("max-token-ttl", 24*time.Hour, "reject tokens valid for longer than this")
	var trusted daemon.TrustedKeys
	fs.Var(&trusted, "trusted-key", "kid=path[:role,role] public key to trust (repeatable)")
	metricsOn := fs.Bool("metrics", true, "serve /metrics (operator token required)")
	tlsCert := fs.String("tls-cert", "", "TLS certificate (optional)")
	tlsKey := fs.String("tls-key", "", "TLS key (optional)")
	vmdURL := fs.String("vmd-url", "", "base URL of the vmd API (required)")
	ollamaURL := fs.String("ollama-url", "http://127.0.0.1:11434", "Ollama base URL")
	serviceKey := fs.String("service-key", "", "private key this service signs its own vmd tokens with (required)")
	serviceKeyID := fs.String("service-key-id", "agentd", "key id of -service-key; vmd must trust it with -trusted-key kid=path:service")
	workers := fs.Int("workers", 2, "tasks run concurrently by this process")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *vmdURL == "" {
		return fmt.Errorf("-vmd-url is required")
	}
	log, err := daemon.NewLogger(*logLevel, stderr)
	if err != nil {
		return err
	}
	ver, err := trusted.Verifier(*maxTTL)
	if err != nil {
		return err
	}
	iss, err := daemon.LoadIssuer(*serviceKey, *serviceKeyID)
	if err != nil {
		return fmt.Errorf("-service-key/-service-key-id: %w", err)
	}

	var st store.Store
	var checks []obs.NamedCheck
	if *dsn == "" {
		log.Warn("no -postgres-dsn: using the in-memory store; all state is lost on restart")
		st = store.NewMemory()
	} else {
		pool, err := pg.Open(ctx, *dsn)
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := pgstore.Migrate(ctx, pool); err != nil {
			return err
		}
		st = pgstore.New(pool)
		checks = append(checks, obs.NamedCheck{Name: "postgres", Check: func(c context.Context) error { return pool.Ping(c) }})
	}

	// agentd reaches vmd as a service principal: short-lived tokens signed with its own key.
	tokens := auth.ServiceTokens(iss, auth.Principal{Subject: "svc:agentd", Type: auth.Service,
		Bindings: []auth.Binding{{Project: auth.AnyProject, Role: auth.RoleService}}}, 5*time.Minute)
	vm := vmclient.New(&http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second}}, *vmdURL, tokens)
	model := llm.NewOllama(*ollamaURL)

	reg := obs.NewRegistry()
	obs.RegisterBuildInfo(reg, version)
	hooks := metrics.New(reg).Hooks()
	host, _ := os.Hostname()
	rn := runner.New(runner.Config{Store: st, LLM: model, VM: vm, Owner: fmt.Sprintf("%s-%d", host, os.Getpid()),
		Workers: *workers, Log: log, Hooks: hooks})
	go rn.Run(ctx)

	deps := service.Deps{Store: st, VM: vm, LLM: model, Verifier: ver, Config: service.Config{Log: log}, Ready: checks}
	if *metricsOn {
		deps.Registry = reg
	}
	ln, err := netListen(*listen)
	if err != nil {
		return err
	}
	return daemon.Serve(ctx, log, ln, service.NewHandler(deps), *tlsCert, *tlsKey, 20*time.Second)
}

func netListen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err == nil && onListen != nil {
		onListen(ln.Addr())
	}
	return ln, err
}
