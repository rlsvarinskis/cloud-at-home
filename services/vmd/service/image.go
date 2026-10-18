package service

import (
	"context"
	"path"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
)

// ImageServer implements vmdv1connect.ImageServiceHandler.
type ImageServer struct{ base }

func (s *ImageServer) RegisterImage(ctx context.Context, req *connect.Request[vmdv1.RegisterImageRequest]) (*connect.Response[vmdv1.Image], error) {
	m := req.Msg
	// An empty project means a shared image: that needs a global ("*") binding, which Require enforces.
	if err := authz.Require(ctx, authz.ImageRegister, m.ProjectId); err != nil {
		return nil, err
	}
	if m.ProjectId == authz.AnyProject {
		return nil, errs.Invalid("project_id must be a project or empty (shared)")
	}
	if err := validName("image", m.Name); err != nil {
		return nil, err
	}
	if m.Path == "" || !path.IsAbs(m.Path) || path.Clean(m.Path) != m.Path {
		return nil, errs.Invalid("path must be a clean absolute path on the node")
	}
	infos, err := s.nodes.Info(ctx)
	if err != nil {
		return nil, err
	}
	if !hasNode(infos, m.NodeId) {
		return nil, errs.Precondition("node %s is not registered", m.NodeId)
	}
	now := s.now()
	img := &domain.Image{ID: s.newID("img", m.ProjectId), ProjectID: m.ProjectId, Name: m.Name, NodeID: m.NodeId,
		State: domain.ImageCreating, Path: m.Path, CreatedAt: now, UpdatedAt: now}
	if err := s.st.CreateImage(ctx, img); err != nil {
		return nil, err
	}
	return connect.NewResponse(imageToProto(img)), nil
}

// authorizeImage lets any tenant read shared images and everyone else act on their own project's.
func authorizeImage(ctx context.Context, img *domain.Image, perm authz.Permission) error {
	if img.ProjectID == "" {
		if perm == authz.ImageGet || perm == authz.ImageList {
			return authz.RequireAnyProject(ctx, perm)
		}
		return authz.Require(ctx, perm, "") // changing a shared image needs the global role
	}
	return authz.RequireResource(ctx, perm, img.ProjectID, "image", img.ID)
}

func (s *ImageServer) GetImage(ctx context.Context, req *connect.Request[vmdv1.GetImageRequest]) (*connect.Response[vmdv1.Image], error) {
	img, err := s.st.GetImage(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	if err := authorizeImage(ctx, img, authz.ImageGet); err != nil {
		return nil, err
	}
	return connect.NewResponse(imageToProto(img)), nil
}

func (s *ImageServer) ListImages(ctx context.Context, req *connect.Request[vmdv1.ListImagesRequest]) (*connect.Response[vmdv1.ListImagesResponse], error) {
	m := req.Msg
	if err := validProject(m.ProjectId); err != nil {
		return nil, err
	}
	if err := authz.Require(ctx, authz.ImageList, m.ProjectId); err != nil {
		return nil, err
	}
	limit := s.pageSize(m.PageSize)
	imgs, err := s.st.ListImages(ctx, m.ProjectId, true, m.PageToken, limit+1)
	if err != nil {
		return nil, err
	}
	imgs, next := nextToken(imgs, limit, func(i *domain.Image) string { return i.ID })
	res := &vmdv1.ListImagesResponse{NextPageToken: next}
	for _, i := range imgs {
		res.Images = append(res.Images, imageToProto(i))
	}
	return connect.NewResponse(res), nil
}

func (s *ImageServer) DeleteImage(ctx context.Context, req *connect.Request[vmdv1.DeleteImageRequest]) (*connect.Response[vmdv1.DeleteImageResponse], error) {
	img, err := s.st.GetImage(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	if err := authorizeImage(ctx, img, authz.ImageDelete); err != nil {
		return nil, err
	}
	if img.State == domain.ImageDeleting {
		return connect.NewResponse(&vmdv1.DeleteImageResponse{}), nil
	}
	if n, err := s.st.CountDisksUsingImage(ctx, img.ID); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, errs.Precondition("image %s backs %d disk(s); delete them first", img.ID, n)
	}
	img.State, img.UpdatedAt = domain.ImageDeleting, s.now()
	if err := s.st.UpdateImage(ctx, img); err != nil {
		return nil, err
	}
	return connect.NewResponse(&vmdv1.DeleteImageResponse{}), nil
}
