// Command mcp serves Attic Cloud to MCP clients (such as Claude) over streamable HTTP. Callers present the same
// bearer tokens vmd and agentd accept; the server forwards them, so it can do nothing its caller could not.
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

	"github.com/rlsvarinskis/cloud-at-home/libs/server/daemon"
	"github.com/rlsvarinskis/cloud-at-home/libs/server/obs"
	"github.com/rlsvarinskis/cloud-at-home/services/mcp/mcpserver"
)

var version = "dev" // set with -ldflags "-X main.version=..."

// onListen, when set (tests), is told the address the server listens on.
var onListen func(net.Addr)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", ":8082", "address to listen on")
	vmdURL := fs.String("vmd-url", "", "base URL of the vmd API (required)")
	agentdURL := fs.String("agentd-url", "", "base URL of the agentd API (required)")
	project := fs.String("default-project", "", "project used when a tool call names none (required)")
	image := fs.String("default-image", "", "image name or ID that agent_create boots from when none is given")
	maxWait := fs.Duration("max-wait", 40*time.Second, "longest a single tool call may block (MCP clients give up near 60s)")
	logLevel := fs.String("log-level", "info", "debug | info | warn | error")
	maxTTL := fs.Duration("max-token-ttl", 24*time.Hour, "reject tokens valid for longer than this")
	var trusted daemon.TrustedKeys
	fs.Var(&trusted, "trusted-key", "kid=path[:role,role] public key to trust (repeatable)")
	revoked := fs.String("revoked-tokens", "", "file of revoked token ids (jti), one per line; reloaded when it changes")
	tlsCert := fs.String("tls-cert", "", "TLS certificate (strongly recommended: tokens travel in headers)")
	tlsKey := fs.String("tls-key", "", "TLS key")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *vmdURL == "" || *agentdURL == "" || *project == "" {
		return fmt.Errorf("-vmd-url, -agentd-url and -default-project are required")
	}
	log, err := daemon.NewLogger(*logLevel, stderr)
	if err != nil {
		return err
	}
	ver, err := trusted.Verifier(*maxTTL)
	if err != nil {
		return err
	}
	if err := daemon.WithRevocations(ctx, ver, *revoked, log); err != nil {
		return err
	}
	if *tlsCert == "" {
		log.Warn("no -tls-cert: bearer tokens are sent in clear text; terminate TLS in front of this server")
	}

	hc := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second}}
	srv := mcpserver.New(mcpserver.Config{
		Clients:        mcpserver.NewClients(hc, *vmdURL, *agentdURL),
		Verifier:       ver,
		DefaultProject: *project,
		DefaultImage:   *image,
		MaxWait:        *maxWait,
		Log:            log,
		Version:        version,
	})
	live, _ := obs.HealthHandlers()
	mux := http.NewServeMux()
	mux.Handle("/healthz", live)
	mux.Handle("/mcp", srv.Handler())
	mux.Handle("/mcp/", srv.Handler())

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	if onListen != nil {
		onListen(ln.Addr())
	}
	return daemon.Serve(ctx, log, ln, mux, *tlsCert, *tlsKey, 20*time.Second)
}
