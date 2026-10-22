// Package mcpserver exposes Attic Cloud (vmd + agentd) to MCP clients such as Claude. It is a thin adapter: every
// tool maps to calls on the public vmd/agentd APIs, made with the caller's own token, so it adds no privileges.
//
// Tools keep the names of the original Python gateway (agent_create, agent_exec, agent_task, ...). An MCP "agent"
// is a VM (owned by vmd) with the same name; agent_task creates or updates the agentd agent that works on it.
package mcpserver

import (
	"fmt"
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
	s := &Server{cfg: cfg, mcp: mcp.NewServer(&mcp.Implementation{Name: "attic", Version: cfg.Version},
		&mcp.ServerOptions{Instructions: serverInstructions(cfg)})}
	s.registerMachineTools()
	s.registerExecTools()
	s.registerFileTools()
	s.registerAgentTools()
	s.registerOpsTools()
	return s
}

// serverInstructions is sent to MCP clients on initialize. The numbers come from the configuration.
func serverInstructions(cfg Config) string {
	return fmt.Sprintf(`Attic Cloud: machines (VMs) and an LLM agent on each machine.
A machine is a VM named by the name argument, in a project. When no project is given the default project %q is used.
Machine tools: agent_create (creates a machine, or starts a stopped one), agent_list, agent_stop, agent_destroy (permanent), agent_resize.
Commands: agent_exec runs a bash command in a machine and waits up to wait_seconds (default 45). The command keeps running
after the call returns. If the reply has done=false, call agent_exec_poll with its job_id until done is true. agent_exec_cancel
stops a job; agent_exec_list finds job IDs.
Files: agent_upload, agent_download, agent_fetch, agent_ssh_pubkey (see each tool). Operations: agent_metrics, cluster_capacity, node_drain.
LLM agent: agent_models lists the installed models. agent_task gives a machine's agent a task; model is required and the machine
must be running. It returns at once with a task ID and does not wait for the task.
Follow a task with agent_status. Pass wait_seconds to wait for a state change. For a cheap poll pass wait_seconds 0 and
since_step set to the header step of the previous reply. brief=true returns the header only.
states: running, done, done_unverified, failed, gave_up, cancelled, error. done means check_command exited 0. Without
check_command a model SUCCESS is only done_unverified, so give a check_command when success must be verified.
agent_cancel stops every unfinished task of the machine's agents. Up to two tasks of one agent can run at the same time.
Waits are capped by the server's max wait (%s here). No call blocks longer.
Errors are tool errors whose text starts with a lower-case code, such as not_found: or failed_precondition:. A missing or
invalid argument fails with a validation error that names the argument.
Most results are plain text. agent_exec, agent_exec_poll, agent_exec_cancel and agent_models return JSON. Each tool
description says what its reply holds.`, cfg.DefaultProject, cfg.MaxWait)
}

// MCP returns the underlying MCP server (for in-memory transports in tests).
func (s *Server) MCP() *mcp.Server { return s.mcp }

// Handler serves MCP over streamable HTTP. Every request needs a valid bearer token.
func (s *Server) Handler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp }, nil)
	return requireBearer(s.cfg.Verifier, h)
}
