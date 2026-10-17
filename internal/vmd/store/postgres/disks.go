package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"example.com/homecloud/internal/vmd/domain"
)

var diskCols = []string{"id", "project_id", "name", "node_id", "size_gb", "image_id", "state", "state_reason", "attached_vm_id", "version", "created_at", "updated_at"}

var (
	diskSelect = selectSQL("disks", diskCols)
	diskInsert = insertSQL("disks", diskCols)
	diskUpdate = updateSQL("disks", diskCols) + " WHERE id = @id AND version = @expected RETURNING version"
)

func diskArgs(d *domain.Disk) pgx.NamedArgs {
	return pgx.NamedArgs{
		"id": d.ID, "project_id": d.ProjectID, "name": d.Name, "node_id": d.NodeID,
		"size_gb": d.SizeGB, "image_id": d.ImageID, "state": string(d.State), "state_reason": d.StateReason,
		"attached_vm_id": d.AttachedVMID, "version": d.Version,
		"created_at": d.CreatedAt.UTC(), "updated_at": d.UpdatedAt.UTC(),
	}
}

// CreateDisk inserts d with Version 1 and sets d.Version.
func (s *Store) CreateDisk(ctx context.Context, d *domain.Disk) error {
	args := diskArgs(d)
	args["version"] = int64(1)
	if err := s.insert(ctx, "disk", d.ID, diskInsert, args); err != nil {
		return err
	}
	d.Version = 1
	return nil
}

// GetDisk returns the disk with the given ID.
func (s *Store) GetDisk(ctx context.Context, id string) (*domain.Disk, error) {
	return getOne(ctx, s.db, "disk", id, diskSelect+" WHERE id = $1", scanDisk, id)
}

// ListDisks returns the project's disks after the cursor, ordered by ID.
func (s *Store) ListDisks(ctx context.Context, project, after string, limit int) ([]*domain.Disk, error) {
	var f filter
	f.add("project_id = " + f.arg(project))
	f.keyset(after)
	sql, args := f.selectWhere(diskCols, "disks", limit)
	return listRows(ctx, s.db, sql, scanDisk, args...)
}

// ListDisksByState returns the disks in state, ordered by ID.
func (s *Store) ListDisksByState(ctx context.Context, state domain.DiskState, limit int) ([]*domain.Disk, error) {
	var f filter
	f.add("state = " + f.arg(string(state)))
	sql, args := f.selectWhere(diskCols, "disks", limit)
	return listRows(ctx, s.db, sql, scanDisk, args...)
}

// CountDisksUsingImage counts the disks, in any state, whose backing image is imageID.
func (s *Store) CountDisksUsingImage(ctx context.Context, imageID string) (int, error) {
	var n int64
	if err := s.db.QueryRow(ctx, "SELECT COUNT(*) FROM disks WHERE image_id = $1", imageID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count disks using image %s: %w", imageID, err)
	}
	return int(n), nil
}

// UpdateDisk is a compare-and-swap on Version; see package store.
func (s *Store) UpdateDisk(ctx context.Context, d *domain.Disk) error {
	args := diskArgs(d)
	args["expected"] = d.Version
	v, err := s.update(ctx, "disk", "disks", d.ID, diskUpdate, args)
	if err != nil {
		return err
	}
	d.Version = v
	return nil
}

// DeleteDisk removes the disk with the given ID.
func (s *Store) DeleteDisk(ctx context.Context, id string) error {
	return s.remove(ctx, "disk", "disks", id)
}

func scanDisk(r scanner) (*domain.Disk, error) {
	var d domain.Disk
	var state string
	if err := r.Scan(&d.ID, &d.ProjectID, &d.Name, &d.NodeID, &d.SizeGB, &d.ImageID, &state, &d.StateReason,
		&d.AttachedVMID, &d.Version, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	d.State = domain.DiskState(state)
	d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
	return &d, nil
}
