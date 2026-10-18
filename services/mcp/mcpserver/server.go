// Package mcpserver exposes homecloud (vmd + agentd) to MCP clients such as Claude. It is a thin adapter: every
// tool maps to calls on the public vmd/agentd APIs, made with the caller's own token, so it adds no privileges.
//
// Tools keep the names of the original Python gateway (agent_create, agent_exec, agent_task, ...). An MCP "agent"
// is a VM (owned by vmd) with the same name; agent_task creates or updates the agentd agent that works on it.
package mcpserver

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
)

// Config configures the MCP server.
type Config struct {
	Clients Clients
	// Verifier checks the caller's bearer token (the same tokens vmd and agentd accept).
	Verifier *authz.Verifier
	// DefaultProject is used when a tool call does not name a project. Required.
	DefaultProject string
	// DefaultImage is the image (name or ID) new machines boot from when agent_create names none.
	DefaultImage string
	// MaxWait caps how long any single tool call may block (MCP clients give up around 60 s). Default 40 s.
	MaxWait time.Duration
	Log     *slog.Logger
	Version string
}

// Server holds the tool implementations.
type Server struct {
	cfg Config
	mcp *mcp.Server
}

// New builds the MCP server with all tools registered.
func New(cfg Config) *Server {
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 40 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	s := &Server{cfg: cfg, mcp: mcp.NewServer(&mcp.Implementation{Name: "homecloud", Version: cfg.Version}, nil)}
	s.registerMachineTools()
	s.registerExecTools()
	s.registerFileTools()
	s.registerAgentTools()
	s.registerOpsTools()
	return s
}

// MCP returns the underlying MCP server (for in-memory transports in tests).
func (s *Server) MCP() *mcp.Server { return s.mcp }

// Handler serves MCP over streamable HTTP. Every request needs a valid bearer token.
func (s *Server) Handler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp }, nil)
	return requireBearer(s.cfg.Verifier, h)
}
