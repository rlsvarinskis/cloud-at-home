package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/authz"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
)

// VMServer implements vmdv1connect.VMServiceHandler.
type VMServer struct{ base }

func (s *VMServer) CreateVM(ctx context.Context, req *connect.Request[vmdv1.CreateVMRequest]) (*connect.Response[vmdv1.VM], error) {
	m := req.Msg
	if err := validProject(m.ProjectId); err != nil {
		return nil, err
	}
	if err := authz.Require(ctx, authz.VMCreate, m.ProjectId); err != nil {
		return nil, err
	}
	if err := validName("vm", m.Name); err != nil {
		return nil, err
	}
	if err := s.checkVMSpec(int(m.Cpus), m.MemoryMb); err != nil {
		return nil, err
	}
	if m.IdempotencyKey != "" {
		if v, err := s.st.GetVMByIdempotencyKey(ctx, m.ProjectId, m.IdempotencyKey); err == nil {
			return connect.NewResponse(vmToProto(v)), nil
		}
	}

	var image *domain.Image
	var existing *domain.Disk
	var nodeID string
	switch b := m.Boot.(type) {
	case *vmdv1.CreateVMRequest_ImageId:
		img, err := s.visibleImage(ctx, m.ProjectId, b.ImageId)
		if err != nil {
			return nil, err
		}
		if img.State != domain.ImageReady {
			return nil, errs.Precondition("image %s is %s, not ready", img.ID, img.State)
		}
		image, nodeID = img, img.NodeID
	case *vmdv1.CreateVMRequest_DiskId:
		d, err := s.st.GetDisk(ctx, b.DiskId)
		if err != nil {
			return nil, err
		}
		if d.ProjectID != m.ProjectId {
			return nil, errs.NotFound("disk %s", b.DiskId)
		}
		if d.AttachedVMID != "" {
			return nil, errs.Precondition("disk %s is already attached to %s", d.ID, d.AttachedVMID)
		}
		if d.State == domain.DiskError || d.State == domain.DiskDeleting {
			return nil, errs.Precondition("disk %s is %s", d.ID, d.State)
		}
		existing, nodeID = d, d.NodeID
	default:
		return nil, errs.Invalid("one of image_id or disk_id is required")
	}
	diskGB := int(m.DiskSizeGb)
	if image != nil {
		if diskGB == 0 {
			diskGB = s.cfg.DefaultDiskGB
		}
		if diskGB < 1 || diskGB > s.cfg.MaxDiskSizeGB {
			return nil, errs.Invalid("disk_size_gb must be between 1 and %d", s.cfg.MaxDiskSizeGB)
		}
	}
	addCPUs, addMem, addDisk := 0, int64(0), 0
	if m.Start {
		addCPUs, addMem = int(m.Cpus), m.MemoryMb
	}
	if image != nil {
		addDisk = diskGB
	}
	if err := s.checkQuota(ctx, m.ProjectId, 1, addCPUs, addMem, addDisk); err != nil {
		return nil, err
	}
	if infos, err := s.nodes.Info(ctx); err != nil {
		return nil, err
	} else if !hasNode(infos, nodeID) {
		return nil, errs.Precondition("node %s is not registered", nodeID)
	}

	now := s.now()
	vm := &domain.VM{
		ID: s.newID("vm", m.ProjectId), ProjectID: m.ProjectId, Name: m.Name, NodeID: nodeID,
		CPUs: int(m.Cpus), MemoryMB: m.MemoryMb, Desired: domain.PowerStopped, State: domain.VMPending,
		Labels: m.Labels, Generation: 1, IdempotencyKey: m.IdempotencyKey, NextReconcile: now, CreatedAt: now, UpdatedAt: now,
	}
	if m.Start {
		vm.Desired = domain.PowerRunning
	}
	err := s.st.Tx(s.txCtx(ctx, m.ProjectId), func(tx storeTx) error {
		if image != nil {
			d := &domain.Disk{ID: s.newID("disk", m.ProjectId), ProjectID: m.ProjectId, Name: vm.ID + "-boot", NodeID: nodeID,
				SizeGB: diskGB, ImageID: image.ID, State: domain.DiskCreating, AttachedVMID: vm.ID, CreatedAt: now, UpdatedAt: now}
			if err := tx.CreateDisk(ctx, d); err != nil {
				return err
			}
			vm.DiskIDs = []string{d.ID}
		} else {
			d, err := tx.GetDisk(ctx, existing.ID)
			if err != nil {
				return err
			}
			if d.AttachedVMID != "" {
				return errs.Precondition("disk %s is already attached to %s", d.ID, d.AttachedVMID)
			}
			d.AttachedVMID, d.UpdatedAt = vm.ID, now
			if err := tx.UpdateDisk(ctx, d); err != nil {
				return err
			}
			vm.DiskIDs = []string{d.ID}
		}
		return tx.CreateVM(ctx, vm)
	})
	if err != nil {
		if m.IdempotencyKey != "" && errors.Is(err, errs.ErrAlreadyExists) {
			if v, gerr := s.st.GetVMByIdempotencyKey(ctx, m.ProjectId, m.IdempotencyKey); gerr == nil {
				return connect.NewResponse(vmToProto(v)), nil
			}
		}
		return nil, err
	}
	return connect.NewResponse(vmToProto(vm)), nil
}

func (s *VMServer) GetVM(ctx context.Context, req *connect.Request[vmdv1.GetVMRequest]) (*connect.Response[vmdv1.VM], error) {
	m := req.Msg
	var out *domain.VM
	want := vmStateFromProto(m.WaitForState)
	err := poll(ctx, m.WaitSeconds, func(ctx context.Context) (bool, error) {
		v, err := s.st.GetVM(ctx, m.Id)
		if err != nil {
			return false, err
		}
		if err := authz.RequireResource(ctx, authz.VMGet, v.ProjectID, "vm", m.Id); err != nil {
			return false, err
		}
		out = v
		return want == "" || v.State == want || v.State == domain.VMError, nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(vmToProto(out)), nil
}

func (s *VMServer) ListVMs(ctx context.Context, req *connect.Request[vmdv1.ListVMsRequest]) (*connect.Response[vmdv1.ListVMsResponse], error) {
	m := req.Msg
	if err := validProject(m.ProjectId); err != nil {
		return nil, err
	}
	if err := authz.Require(ctx, authz.VMList, m.ProjectId); err != nil {
		return nil, err
	}
	limit := s.pageSize(m.PageSize)
	vms, err := s.st.ListVMs(ctx, domain.VMFilter{ProjectID: m.ProjectId, Labels: m.LabelSelector, After: m.PageToken, Limit: limit + 1})
	if err != nil {
		return nil, err
	}
	vms, next := nextToken(vms, limit, func(v *domain.VM) string { return v.ID })
	res := &vmdv1.ListVMsResponse{NextPageToken: next}
	for _, v := range vms {
		res.Vms = append(res.Vms, vmToProto(v))
	}
	return connect.NewResponse(res), nil
}

func (s *VMServer) UpdateVM(ctx context.Context, req *connect.Request[vmdv1.UpdateVMRequest]) (*connect.Response[vmdv1.VM], error) {
	m := req.Msg
	v, err := s.mutateVM(ctx, m.Id, authz.VMUpdate, func(v *domain.VM) error {
		if m.ExpectedGeneration != 0 && v.Generation != m.ExpectedGeneration {
			return fmt.Errorf("%w: vm is at generation %d, not %d", errs.ErrConflict, v.Generation, m.ExpectedGeneration)
		}
		changed := false
		if m.Cpus != nil || m.MemoryMb != nil {
			if v.State != domain.VMStopped || v.Desired != domain.PowerStopped {
				return errs.Precondition("a VM can only be resized while stopped (state %s)", v.State)
			}
			cpus, mem := v.CPUs, v.MemoryMB
			if m.Cpus != nil {
				cpus = int(*m.Cpus)
			}
			if m.MemoryMb != nil {
				mem = *m.MemoryMb
			}
			if err := s.checkVMSpec(cpus, mem); err != nil {
				return err
			}
			if cpus != v.CPUs || mem != v.MemoryMB {
				v.CPUs, v.MemoryMB, v.Generation, changed = cpus, mem, v.Generation+1, true
			}
		}
		if m.ReplaceLabels {
			if !sameLabels(v.Labels, m.Labels) {
				v.Labels, changed = m.Labels, true
			}
		} else if len(m.Labels) > 0 {
			for k, val := range m.Labels {
				if cur, ok := v.Labels[k]; !ok || cur != val {
					if v.Labels == nil {
						v.Labels = map[string]string{}
					}
					v.Labels[k] = val
					changed = true
				}
			}
		}
		if !changed {
			return errUnchanged
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(vmToProto(v)), nil
}

func (s *VMServer) StartVM(ctx context.Context, req *connect.Request[vmdv1.StartVMRequest]) (*connect.Response[vmdv1.VM], error) {
	v, err := s.mutateVM(ctx, req.Msg.Id, authz.VMStart, func(v *domain.VM) error {
		switch {
		case v.Desired == domain.PowerDeleted || v.State == domain.VMDeleting:
			return errs.Precondition("vm %s is being deleted", v.ID)
		case v.Desired == domain.PowerRunning && v.State == domain.VMError:
			v.Failures, v.Crashes, v.NextReconcile = 0, 0, s.now() // "try again now" instead of waiting for the slow retry
			return nil
		case v.Desired == domain.PowerRunning:
			return errUnchanged
		}
		if err := s.checkQuota(ctx, v.ProjectID, 0, v.CPUs, v.MemoryMB, 0); err != nil {
			return err
		}
		v.Desired, v.StopForce, v.Failures, v.Crashes, v.NextReconcile = domain.PowerRunning, false, 0, 0, s.now()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(vmToProto(v)), nil
}

func (s *VMServer) StopVM(ctx context.Context, req *connect.Request[vmdv1.StopVMRequest]) (*connect.Response[vmdv1.VM], error) {
	v, err := s.mutateVM(ctx, req.Msg.Id, authz.VMStop, func(v *domain.VM) error {
		switch {
		case v.Desired == domain.PowerDeleted || v.State == domain.VMDeleting:
			return errs.Precondition("vm %s is being deleted", v.ID)
		case v.Desired == domain.PowerStopped && (!req.Msg.Force || v.StopForce):
			return errUnchanged
		}
		v.Desired, v.StopForce, v.NextReconcile = domain.PowerStopped, req.Msg.Force, s.now()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(vmToProto(v)), nil
}

func (s *VMServer) DeleteVM(ctx context.Context, req *connect.Request[vmdv1.DeleteVMRequest]) (*connect.Response[vmdv1.DeleteVMResponse], error) {
	_, err := s.mutateVM(ctx, req.Msg.Id, authz.VMDelete, func(v *domain.VM) error {
		if v.Desired == domain.PowerDeleted {
			return errUnchanged
		}
		if len(v.Holds) > 0 && !req.Msg.Force {
			return errs.Precondition("vm %s is held by %s (force=true overrides)", v.ID, strings.Join(v.Holds, ", "))
		}
		v.Desired, v.State, v.StopForce, v.DeleteDisks = domain.PowerDeleted, domain.VMDeleting, true, req.Msg.DeleteDisks
		v.NextReconcile = s.now()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&vmdv1.DeleteVMResponse{}), nil
}

func (s *VMServer) AddHold(ctx context.Context, req *connect.Request[vmdv1.HoldRequest]) (*connect.Response[vmdv1.VM], error) {
	if req.Msg.Holder == "" {
		return nil, errs.Invalid("holder is required")
	}
	v, err := s.mutateVM(ctx, req.Msg.Id, authz.VMHold, func(v *domain.VM) error {
		if v.Desired == domain.PowerDeleted {
			return errs.Precondition("vm %s is being deleted", v.ID)
		}
		holds := addHold(v.Holds, req.Msg.Holder)
		if len(holds) == len(v.Holds) {
			return errUnchanged
		}
		v.Holds = holds
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(vmToProto(v)), nil
}

func (s *VMServer) RemoveHold(ctx context.Context, req *connect.Request[vmdv1.HoldRequest]) (*connect.Response[vmdv1.VM], error) {
	if req.Msg.Holder == "" {
		return nil, errs.Invalid("holder is required")
	}
	v, err := s.mutateVM(ctx, req.Msg.Id, authz.VMHold, func(v *domain.VM) error {
		holds := removeHold(v.Holds, req.Msg.Holder)
		if len(holds) == len(v.Holds) {
			return errUnchanged
		}
		v.Holds = holds
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(vmToProto(v)), nil
}

func hasNode(infos []nodeInfo, id string) bool {
	for _, n := range infos {
		if n.ID == id {
			return true
		}
	}
	return false
}

func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
