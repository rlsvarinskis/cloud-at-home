// Command vmd is the VM control plane and/or the per-host node daemon.
//
//	vmd --role=all      control plane + an in-process node on this host (single machine, the default)
//	vmd --role=control  control plane only; hosts are reached over the node API (--node id=URL)
//	vmd --role=node     node daemon only: manages QEMU/KVM on this host for a control plane
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/server/daemon"
	"github.com/rlsvarinskis/cloud-at-home/libs/server/obs"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/pg"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/node"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/guestexec"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/nodeserver"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/qemudriver"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/remote"
	"github.com/rlsvarinskis/cloud-at-home/libs/qemu/sshrunner"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/promcollect"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/scheduler"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/service"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/store"
	pgstore "github.com/rlsvarinskis/cloud-at-home/services/vmd/store/postgres"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/store/shard"
)

var version = "dev" // set with -ldflags "-X main.version=..."

type multi []string

func (m *multi) String() string     { return strings.Join(*m, " ") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// onListen, when set (tests), is told the address the server listens on.
var onListen func(net.Addr)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "vmd:", err)
		os.Exit(1)
	}
}

type options struct {
	role, listen, logLevel, revoked, dataDir, nodeID, sshKey, runAs, tlsCert, tlsKey string
	serviceKey, serviceKeyID, sharedDir                                              string
	maxTTL                                                                           time.Duration
	metrics, kvm, noFirewall                                                         bool
	trusted                                                                          daemon.TrustedKeys
	nodes, allowDNS, dsns                                                            multi
	quotaVMs, quotaVCPUs, quotaDiskGB                                                int
	quotaMemMB                                                                       int64
	cpuOvercommit, memOvercommit                                                     float64
	reserveMemMB                                                                     int64
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	var o options
	fs := flag.NewFlagSet("vmd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.role, "role", "all", "all | control | node")
	fs.StringVar(&o.listen, "listen", ":8080", "address to listen on")
	fs.Var(&o.dsns, "postgres-dsn", "Postgres DSN (control plane; env HOMECLOUD_POSTGRES_DSN). Repeat to shard projects across several databases (the set is part of your data layout: do not change it once data exists). None = in-memory store, development only")
	fs.StringVar(&o.revoked, "revoked-tokens", "", "file of revoked token ids (jti), one per line; reloaded when it changes")
	fs.IntVar(&o.quotaVMs, "quota-vms", 0, "max VMs per project (0 = unlimited)")
	fs.IntVar(&o.quotaVCPUs, "quota-vcpus", 0, "max vCPUs of running VMs per project (0 = unlimited)")
	fs.Int64Var(&o.quotaMemMB, "quota-memory-mb", 0, "max RAM (MB) of running VMs per project (0 = unlimited)")
	fs.IntVar(&o.quotaDiskGB, "quota-disk-gb", 0, "max disk (GB) per project (0 = unlimited)")
	fs.StringVar(&o.logLevel, "log-level", "info", "debug | info | warn | error")
	fs.DurationVar(&o.maxTTL, "max-token-ttl", 24*time.Hour, "reject tokens valid for longer than this")
	fs.Var(&o.trusted, "trusted-key", "kid=path[:role,role] public key to trust (repeatable)")
	fs.BoolVar(&o.metrics, "metrics", true, "serve /metrics (operator token required)")
	fs.StringVar(&o.tlsCert, "tls-cert", "", "TLS certificate (optional)")
	fs.StringVar(&o.tlsKey, "tls-key", "", "TLS key (optional)")
	fs.Var(&o.nodes, "node", "id=URL of a node daemon (control plane, repeatable)")
	fs.StringVar(&o.serviceKey, "service-key", "", "private key this control plane uses to call nodes (role node)")
	fs.StringVar(&o.serviceKeyID, "service-key-id", "vmd", "key id of -service-key; nodes must trust it with -trusted-key")
	fs.StringVar(&o.nodeID, "node-id", hostname(), "id of this host as a node")
	fs.StringVar(&o.dataDir, "data-dir", "/var/lib/homecloud", "node: directory for images, disks and VM runtime state")
	fs.StringVar(&o.sharedDir, "shared-dir", "", "node: folder shared into every guest at /shared (default <data-dir>/shared)")
	fs.StringVar(&o.sshKey, "ssh-key", "", "node: private key used to reach guests (public key is injected via cloud-init)")
	fs.StringVar(&o.runAs, "run-as", "qemu", "node: unprivileged user the VMs run as")
	fs.BoolVar(&o.kvm, "kvm", true, "node: use KVM acceleration")
	fs.BoolVar(&o.noFirewall, "no-firewall", false, "node: do NOT block guests from private networks (unsafe; for development)")
	fs.Var(&o.allowDNS, "allow-dns", "node: resolver IP guests may reach despite the firewall (repeatable)")
	fs.Float64Var(&o.cpuOvercommit, "cpu-overcommit", 1, "vCPUs per host thread the scheduler may hand out")
	fs.Float64Var(&o.memOvercommit, "memory-overcommit", 1, "memory overcommit factor")
	fs.Int64Var(&o.reserveMemMB, "reserve-memory-mb", 2048, "RAM kept back for the host itself")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(o.dsns) == 0 && os.Getenv("HOMECLOUD_POSTGRES_DSN") != "" {
		o.dsns = multi{os.Getenv("HOMECLOUD_POSTGRES_DSN")}
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	log, err := daemon.NewLogger(o.logLevel, stderr)
	if err != nil {
		return err
	}

	switch o.role {
	case "node":
		return runNode(ctx, log, &o)
	case "control", "all":
		return runControl(ctx, log, &o)
	}
	return fmt.Errorf("unknown -role %q", o.role)
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "node-1"
	}
	return strings.ToLower(strings.ReplaceAll(h, ".", "-"))
}

// newDriver builds the real QEMU driver for this host.
func newDriver(ctx context.Context, log *slog.Logger, o *options) (node.Driver, error) {
	if o.sshKey == "" {
		return nil, errors.New("-ssh-key is required for a node")
	}
	signer, err := sshrunner.GenerateKey(o.sshKey) // loads the key if it exists
	if err != nil {
		return nil, fmt.Errorf("ssh key: %w", err)
	}
	if !o.noFirewall {
		// Fail closed: without the firewall a guest could reach the host's private networks.
		if err := qemudriver.LockDownNetwork(ctx, qemudriver.OSCommander(), o.runAs, o.allowDNS...); err != nil {
			return nil, fmt.Errorf("firewall (needs root/CAP_NET_ADMIN; use -no-firewall only for development): %w", err)
		}
		log.Info("guest network lockdown active", "user", o.runAs)
	}
	return qemudriver.New(qemudriver.Config{
		NodeID: o.nodeID, DataDir: o.dataDir, SharedDir: o.sharedDir, SSHKeyPath: o.sshKey,
		KVM: o.kvm, RunAs: o.runAs,
		Keys: keyLine{signer},
		NewRunner: func(port int) guestexec.Runner {
			return sshrunner.New(sshrunner.Config{Addr: fmt.Sprintf("127.0.0.1:%d", port), Signer: signer})
		},
	})
}

func runNode(ctx context.Context, log *slog.Logger, o *options) error {
	ver, err := o.trusted.Verifier(o.maxTTL)
	if err != nil {
		return err
	}
	if err := daemon.WithRevocations(ctx, ver, o.revoked, log); err != nil {
		return err
	}
	drv, err := newDriver(ctx, log, o)
	if err != nil {
		return err
	}
	path, h := nodeserver.New(drv, ver)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	live, ready := obs.HealthHandlers()
	mux.Handle("/healthz", live)
	mux.Handle("/readyz", ready)
	ln, err := netListen(o.listen)
	if err != nil {
		return err
	}
	return daemon.Serve(ctx, log, ln, mux, o.tlsCert, o.tlsKey, 20*time.Second)
}

func runControl(ctx context.Context, log *slog.Logger, o *options) error {
	ver, err := o.trusted.Verifier(o.maxTTL)
	if err != nil {
		return err
	}
	if err := daemon.WithRevocations(ctx, ver, o.revoked, log); err != nil {
		return err
	}
	st, checks, shardHooks, err := openStore(ctx, log, o.dsns)
	if err != nil {
		return err
	}

	nodes := service.NewNodeManager(st, log, nil)
	if o.role == "all" {
		drv, err := newDriver(ctx, log, o)
		if err != nil {
			return err
		}
		nodes.Add(o.nodeID, "local", drv)
	}
	if len(o.nodes) > 0 {
		iss, err := daemon.LoadIssuer(o.serviceKey, o.serviceKeyID)
		if err != nil {
			return fmt.Errorf("-node needs -service-key/-service-key-id: %w", err)
		}
		src := authz.ServiceTokens(iss, authz.Principal{Subject: "svc:vmd", Type: authz.Service,
			Bindings: []authz.Binding{{Project: authz.AnyProject, Role: authz.RoleNode}}}, 5*time.Minute)
		for _, spec := range o.nodes {
			id, url, ok := strings.Cut(spec, "=")
			if !ok || id == "" || url == "" {
				return fmt.Errorf("bad -node %q: want id=URL", spec)
			}
			nodes.Add(id, url, remote.New(newHTTPClient(), url, src))
		}
	}
	if o.role == "control" && len(o.nodes) == 0 {
		return errors.New("a control plane needs at least one -node id=URL")
	}

	cfg := service.Config{
		Scheduler: scheduler.Config{CPUOvercommit: o.cpuOvercommit, MemoryOvercommit: o.memOvercommit, ReserveMemoryMB: o.reserveMemMB},
		Quota:     service.Quota{MaxVMs: o.quotaVMs, MaxCPUs: o.quotaVCPUs, MaxMemoryMB: o.quotaMemMB, MaxDiskGB: o.quotaDiskGB},
		Log:       log,
	}
	if shardHooks != nil {
		cfg.NewID, cfg.ProjectContext = shardHooks.NewID, shardHooks.ProjectContext
	}
	var reg = obs.NewRegistry()
	obs.RegisterBuildInfo(reg, version)
	rec := service.NewReconciler(st, nodes, cfg, fmt.Sprintf("%s-%d", hostname(), os.Getpid()))
	col := promcollect.New(st, nodes, cfg.Scheduler, promcollect.Options{Log: log})
	reg.MustRegister(col)
	rec.Hooks = col.Hooks()

	go nodes.Run(ctx, 5*time.Second)
	go rec.Run(ctx)
	go col.Run(ctx)

	checks = append(checks, obs.NamedCheck{Name: "store", Check: func(c context.Context) error { _, err := st.ListNodes(c); return err }})
	deps := service.Deps{Store: st, Nodes: nodes, Verifier: ver, Config: cfg, Ready: checks}
	if o.metrics {
		deps.Registry = reg
	}
	ln, err := netListen(o.listen)
	if err != nil {
		return err
	}
	return daemon.Serve(ctx, log, ln, service.NewHandler(deps), o.tlsCert, o.tlsKey, 20*time.Second)
}

// keyLine adapts an SSH signer to qemudriver.KeySource (the public key injected into guests).
type keyLine struct{ s ssh.Signer }

func (k keyLine) PublicKeyLine() (string, error) { return sshrunner.PublicKeyLine(k.s), nil }

func newHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second,
		DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}} // no overall timeout: callers pass contexts, and some calls legitimately take a while
}

func netListen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err == nil && onListen != nil {
		onListen(ln.Addr())
	}
	return ln, err
}

// shardRouting are the two hooks the services need when projects are spread over several databases.
type shardRouting struct {
	NewID          func(prefix, project string) string
	ProjectContext func(context.Context, string) context.Context
}

// openStore opens the control plane's database: one DSN is used directly, several are combined into a store that
// shards projects across them, and none gives the in-memory development store.
func openStore(ctx context.Context, log *slog.Logger, dsns []string) (store.Store, []obs.NamedCheck, *shardRouting, error) {
	if len(dsns) == 0 {
		log.Warn("no -postgres-dsn: using the in-memory store; all state is lost on restart")
		return store.NewMemory(), nil, nil, nil
	}
	var stores []store.Store
	var checks []obs.NamedCheck
	for i, dsn := range dsns {
		pool, err := pg.Open(ctx, dsn)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("database %d: %w", i, err)
		}
		if err := pgstore.Migrate(ctx, pool); err != nil {
			return nil, nil, nil, fmt.Errorf("database %d: %w", i, err)
		}
		stores = append(stores, pgstore.New(pool))
		checks = append(checks, obs.NamedCheck{Name: fmt.Sprintf("postgres-%d", i), Check: func(c context.Context) error { return pool.Ping(c) }})
	}
	if len(stores) == 1 {
		return stores[0], checks, nil, nil
	}
	ss, err := shard.New(stores)
	if err != nil {
		return nil, nil, nil, err
	}
	log.Info("sharding projects across databases", "shards", len(stores))
	return ss, checks, &shardRouting{NewID: ss.NewID, ProjectContext: shard.WithProject}, nil
}
