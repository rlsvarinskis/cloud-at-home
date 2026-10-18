package service

import (
	"context"

	"connectrpc.com/connect"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
)

// ModelServer implements agentdv1connect.ModelServiceHandler.
type ModelServer struct{ base }

// ListModels lists the models installed on the LLM backend. Any tenant that may list models can see
// them: they are shared across projects.
func (s *ModelServer) ListModels(ctx context.Context, _ *connect.Request[agentdv1.ListModelsRequest]) (*connect.Response[agentdv1.ListModelsResponse], error) {
	if err := authz.RequireAnyProject(ctx, authz.ModelList); err != nil {
		return nil, err
	}
	installed, err := s.llm.ListModels(ctx)
	if err != nil {
		return nil, s.downstream("model backend", err)
	}
	res := &agentdv1.ListModelsResponse{}
	for _, m := range installed {
		res.Models = append(res.Models, modelToProto(m))
	}
	return connect.NewResponse(res), nil
}
