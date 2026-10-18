package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
)

// addTool registers a tool whose handler returns plain text. Errors become tool errors (isError=true with the
// message), not protocol errors, so the model can read them and react.
func addTool[In any](s *Server, name, description string, h func(ctx context.Context, in In) (string, error)) {
	mcp.AddTool(s.mcp, &mcp.Tool{Name: name, Description: description},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			out, err := h(ctx, in)
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: errText(err)}}}, nil, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil, nil
		})
}

// errText renders an error for the model: the downstream message without Connect's "code: " noise leaking
// infrastructure details (vmd already sanitises those).
func errText(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return fmt.Sprintf("%s: %s", strings.ToLower(ce.Code().String()), ce.Message())
	}
	return err.Error()
}

// project returns the named project or the configured default.
func (s *Server) project(in string) string {
	if in != "" {
		return in
	}
	return s.cfg.DefaultProject
}

// waitSeconds turns a caller-supplied wait (seconds) into the wait to use for one downstream call: never negative,
// never more than 30 (the downstream limit) and never more than the server's MaxWait.
func (s *Server) waitSeconds(requested int) int {
	max := int(s.cfg.MaxWait / time.Second)
	if max > 30 {
		max = 30
	}
	if requested < 0 {
		return 0
	}
	if requested > max {
		return max
	}
	return requested
}

// vmByName finds the VM called name in project. It returns a NotFound connect error when there is none.
func (s *Server) vmByName(ctx context.Context, project, name string) (*vmdv1.VM, error) {
	token := ""
	for {
		res, err := s.cfg.Clients.VMs.ListVMs(ctx, connect.NewRequest(&vmdv1.ListVMsRequest{
			ProjectId: s.project(project), PageSize: 200, PageToken: token}))
		if err != nil {
			return nil, err
		}
		for _, v := range res.Msg.GetVms() {
			if v.GetName() == name {
				return v, nil
			}
		}
		token = res.Msg.GetNextPageToken()
		if token == "" {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no machine named %q in project %q", name, s.project(project)))
		}
	}
}

// clip shortens s to at most n bytes for tool output, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n...[%d more bytes]", len(s)-n)
}
