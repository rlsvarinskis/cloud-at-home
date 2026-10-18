package service

import (
	"context"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

// ClusterServer implements vmdv1connect.ClusterServiceHandler: operator actions on the fleet.
type ClusterServer struct{ base }

// SetNodeDraining records the operator's drain decision and returns the node's resulting capacity view.
func (s *ClusterServer) SetNodeDraining(ctx context.Context, req *connect.Request[vmdv1.SetNodeDrainingRequest]) (*connect.Response[vmdv1.NodeCapacity], error) {
	if err := authz.Require(ctx, authz.NodeDrain, ""); err != nil {
		return nil, err
	}
	if req.Msg.NodeId == "" {
		return nil, errs.Invalid("node_id is required")
	}
	if err := s.st.SetNodeDrain(ctx, req.Msg.NodeId, req.Msg.Draining); err != nil {
		return nil, err
	}
	s.cfg.Log.Info("node drain changed", "node", req.Msg.NodeId, "draining", req.Msg.Draining, "by", callerOf(ctx))
	nodes, _, err := ClusterCapacity(ctx, s.st, s.cfg.Scheduler)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if n.NodeId == req.Msg.NodeId {
			return connect.NewResponse(n), nil
		}
	}
	return nil, errs.NotFound("node %s", req.Msg.NodeId)
}

func callerOf(ctx context.Context) string {
	if p, ok := authz.FromContext(ctx); ok {
		return p.Subject
	}
	return "unknown"
}
