package service

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	nodev1 "example.com/homecloud/gen/homecloud/node/v1"
	vmdv1 "example.com/homecloud/gen/homecloud/vmd/v1"
	"example.com/homecloud/internal/auth"
	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/vmd/domain"
)

// DiskServer implements vmdv1connect.DiskServiceHandler.
type DiskServer struct{ base }

// pickDiskNode chooses a ready node with room for the disk: the one with the most free space.
func (b *base) pickDiskNode(ctx context.Context, sizeGB int) (string, error) {
	rows, err := b.st.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	best, bestFree := "", int64(-1)
	for _, n := range rows {
		if n.State == domain.NodeReady && n.DiskBytesFree >= int64(sizeGB)<<30 && n.DiskBytesFree > bestFree {
			best, bestFree = n.ID, n.DiskBytesFree
		}
	}
	if best == "" {
		return "", errs.Exhausted("no ready node has %d GB free for a disk", sizeGB)
	}
	return best, nil
}

func (s *DiskServer) CreateDisk(ctx context.Context, req *connect.Request[vmdv1.CreateDiskRequest]) (*connect.Response[vmdv1.Disk], error) {
	m := req.Msg
	if err := validProject(m.ProjectId); err != nil {
		return nil, err
	}
	if err := auth.Require(ctx, auth.DiskCreate, m.ProjectId); err != nil {
		return nil, err
	}
	if err := validName("disk", m.Name); err != nil {
		return nil, err
	}
	size := int(m.SizeGb)
	if size < 1 || size > s.cfg.MaxDiskSizeGB {
		return nil, errs.Invalid("size_gb must be between 1 and %d", s.cfg.MaxDiskSizeGB)
	}
	nodeID := m.NodeId
	if m.ImageId != "" {
		img, err := s.visibleImage(ctx, m.ProjectId, m.ImageId)
		if err != nil {
			return nil, err
		}
		if img.State != domain.ImageReady {
			return nil, errs.Precondition("image %s is %s, not ready", img.ID, img.State)
		}
		if nodeID != "" && nodeID != img.NodeID {
			return nil, errs.Invalid("image %s lives on node %s, not %s", img.ID, img.NodeID, nodeID)
		}
		nodeID = img.NodeID
	} else if nodeID == "" {
		var err error
		if nodeID, err = s.pickDiskNode(ctx, size); err != nil {
			return nil, err
		}
	} else if infos, err := s.nodes.Info(ctx); err != nil {
		return nil, err
	} else if !hasNode(infos, nodeID) {
		return nil, errs.Precondition("node %s is not registered", nodeID)
	}
	if err := s.checkQuota(ctx, m.ProjectId, 0, 0, 0, size); err != nil {
		return nil, err
	}
	now := s.now()
	d := &domain.Disk{ID: s.newID("disk", m.ProjectId), ProjectID: m.ProjectId, Name: m.Name, NodeID: nodeID, SizeGB: size,
		ImageID: m.ImageId, State: domain.DiskCreating, CreatedAt: now, UpdatedAt: now}
	if err := s.st.CreateDisk(ctx, d); err != nil {
		return nil, err
	}
	return connect.NewResponse(diskToProto(d)), nil
}

func (s *DiskServer) loadDisk(ctx context.Context, id string, perm auth.Permission) (*domain.Disk, error) {
	d, err := s.st.GetDisk(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := auth.RequireResource(ctx, perm, d.ProjectID, "disk", id); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *DiskServer) GetDisk(ctx context.Context, req *connect.Request[vmdv1.GetDiskRequest]) (*connect.Response[vmdv1.Disk], error) {
	d, err := s.loadDisk(ctx, req.Msg.Id, auth.DiskGet)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(diskToProto(d)), nil
}

func (s *DiskServer) ListDisks(ctx context.Context, req *connect.Request[vmdv1.ListDisksRequest]) (*connect.Response[vmdv1.ListDisksResponse], error) {
	m := req.Msg
	if err := validProject(m.ProjectId); err != nil {
		return nil, err
	}
	if err := auth.Require(ctx, auth.DiskList, m.ProjectId); err != nil {
		return nil, err
	}
	limit := s.pageSize(m.PageSize)
	disks, err := s.st.ListDisks(ctx, m.ProjectId, m.PageToken, limit+1)
	if err != nil {
		return nil, err
	}
	disks, next := nextToken(disks, limit, func(d *domain.Disk) string { return d.ID })
	res := &vmdv1.ListDisksResponse{NextPageToken: next}
	for _, d := range disks {
		res.Disks = append(res.Disks, diskToProto(d))
	}
	return connect.NewResponse(res), nil
}

// detachedOrStopped checks that nothing is using the disk right now.
func (s *DiskServer) detachedOrStopped(ctx context.Context, d *domain.Disk) error {
	if d.AttachedVMID == "" {
		return nil
	}
	vm, err := s.st.GetVM(ctx, d.AttachedVMID)
	if errors.Is(err, errs.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if vm.State != domain.VMStopped || vm.Desired != domain.PowerStopped {
		return errs.Precondition("disk %s is attached to vm %s which is %s; stop it first", d.ID, vm.ID, vm.State)
	}
	return nil
}

func (s *DiskServer) ResizeDisk(ctx context.Context, req *connect.Request[vmdv1.ResizeDiskRequest]) (*connect.Response[vmdv1.Disk], error) {
	m := req.Msg
	d, err := s.loadDisk(ctx, m.Id, auth.DiskUpdate)
	if err != nil {
		return nil, err
	}
	size := int(m.SizeGb)
	switch {
	case d.State != domain.DiskReady:
		return nil, errs.Precondition("disk %s is %s, not ready", d.ID, d.State)
	case size <= d.SizeGB:
		return nil, errs.Invalid("disks can only grow: %d GB is not larger than %d GB", size, d.SizeGB)
	case size > s.cfg.MaxDiskSizeGB:
		return nil, errs.Invalid("size_gb may not exceed %d", s.cfg.MaxDiskSizeGB)
	}
	if err := s.detachedOrStopped(ctx, d); err != nil {
		return nil, err
	}
	if err := s.checkQuota(ctx, d.ProjectID, 0, 0, 0, size-d.SizeGB); err != nil {
		return nil, err
	}
	drv, err := s.nodes.Driver(d.NodeID)
	if err != nil {
		return nil, errs.Unavailable("node %s is not registered", d.NodeID)
	}
	if _, err := drv.ResizeDisk(ctx, &nodev1.ResizeDiskRequest{DiskId: d.ID, SizeGb: int32(size)}); err != nil {
		return nil, s.nodeErr(d.NodeID, err)
	}
	d.SizeGB, d.UpdatedAt = size, s.now()
	if err := s.st.UpdateDisk(ctx, d); err != nil {
		return nil, err
	}
	return connect.NewResponse(diskToProto(d)), nil
}

func (s *DiskServer) DeleteDisk(ctx context.Context, req *connect.Request[vmdv1.DeleteDiskRequest]) (*connect.Response[vmdv1.DeleteDiskResponse], error) {
	d, err := s.loadDisk(ctx, req.Msg.Id, auth.DiskDelete)
	if err != nil {
		return nil, err
	}
	if d.AttachedVMID != "" {
		return nil, errs.Precondition("disk %s is attached to vm %s", d.ID, d.AttachedVMID)
	}
	if d.State == domain.DiskDeleting {
		return connect.NewResponse(&vmdv1.DeleteDiskResponse{}), nil
	}
	d.State, d.UpdatedAt = domain.DiskDeleting, s.now()
	if err := s.st.UpdateDisk(ctx, d); err != nil {
		return nil, err
	}
	return connect.NewResponse(&vmdv1.DeleteDiskResponse{}), nil
}

func (s *DiskServer) SnapshotDisk(ctx context.Context, req *connect.Request[vmdv1.SnapshotDiskRequest]) (*connect.Response[vmdv1.Image], error) {
	m := req.Msg
	d, err := s.loadDisk(ctx, m.Id, auth.DiskSnapshot)
	if err != nil {
		return nil, err
	}
	if err := validName("image", m.ImageName); err != nil {
		return nil, err
	}
	if d.State != domain.DiskReady {
		return nil, errs.Precondition("disk %s is %s, not ready", d.ID, d.State)
	}
	if err := s.detachedOrStopped(ctx, d); err != nil {
		return nil, err
	}
	now := s.now()
	img := &domain.Image{ID: s.newID("img", d.ProjectID), ProjectID: d.ProjectID, Name: m.ImageName, NodeID: d.NodeID,
		State: domain.ImageCreating, SourceDiskID: d.ID, CreatedAt: now, UpdatedAt: now}
	if err := s.st.CreateImage(ctx, img); err != nil {
		return nil, err
	}
	return connect.NewResponse(imageToProto(img)), nil
}
