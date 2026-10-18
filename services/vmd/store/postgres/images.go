package postgres

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
)

var imageCols = []string{"id", "project_id", "name", "node_id", "state", "state_reason", "size_bytes", "source_disk_id", "path", "version", "created_at", "updated_at"}

var (
	imageSelect = selectSQL("images", imageCols)
	imageInsert = insertSQL("images", imageCols)
	imageUpdate = updateSQL("images", imageCols) + " WHERE id = @id AND version = @expected RETURNING version"
)

func imageArgs(i *domain.Image) pgx.NamedArgs {
	return pgx.NamedArgs{
		"id": i.ID, "project_id": i.ProjectID, "name": i.Name, "node_id": i.NodeID,
		"state": string(i.State), "state_reason": i.StateReason, "size_bytes": i.SizeBytes,
		"source_disk_id": i.SourceDiskID, "path": i.Path, "version": i.Version,
		"created_at": i.CreatedAt.UTC(), "updated_at": i.UpdatedAt.UTC(),
	}
}

// CreateImage inserts i with Version 1 and sets i.Version.
func (s *Store) CreateImage(ctx context.Context, i *domain.Image) error {
	args := imageArgs(i)
	args["version"] = int64(1)
	if err := s.insert(ctx, "image", i.ID, imageInsert, args); err != nil {
		return err
	}
	i.Version = 1
	return nil
}

// GetImage returns the image with the given ID.
func (s *Store) GetImage(ctx context.Context, id string) (*domain.Image, error) {
	return getOne(ctx, s.db, "image", id, imageSelect+" WHERE id = $1", scanImage, id)
}

// ListImages returns the project's images and, if includeShared, the shared ones, ordered by ID.
func (s *Store) ListImages(ctx context.Context, project string, includeShared bool, after string, limit int) ([]*domain.Image, error) {
	var f filter
	var scope []string
	if project != "" {
		scope = append(scope, "project_id = "+f.arg(project))
	}
	if includeShared {
		scope = append(scope, "project_id = ''")
	}
	if len(scope) == 0 {
		return nil, nil
	}
	f.add("(" + strings.Join(scope, " OR ") + ")")
	f.keyset(after)
	sql, args := f.selectWhere(imageCols, "images", limit)
	return listRows(ctx, s.db, sql, scanImage, args...)
}

// ListImagesByState returns the images in state, ordered by ID.
func (s *Store) ListImagesByState(ctx context.Context, state domain.ImageState, limit int) ([]*domain.Image, error) {
	var f filter
	f.add("state = " + f.arg(string(state)))
	sql, args := f.selectWhere(imageCols, "images", limit)
	return listRows(ctx, s.db, sql, scanImage, args...)
}

// UpdateImage is a compare-and-swap on Version; see package store.
func (s *Store) UpdateImage(ctx context.Context, i *domain.Image) error {
	args := imageArgs(i)
	args["expected"] = i.Version
	v, err := s.update(ctx, "image", "images", i.ID, imageUpdate, args)
	if err != nil {
		return err
	}
	i.Version = v
	return nil
}

// DeleteImage removes the image with the given ID.
func (s *Store) DeleteImage(ctx context.Context, id string) error {
	return s.remove(ctx, "image", "images", id)
}

func scanImage(r scanner) (*domain.Image, error) {
	var i domain.Image
	var state string
	if err := r.Scan(&i.ID, &i.ProjectID, &i.Name, &i.NodeID, &state, &i.StateReason, &i.SizeBytes,
		&i.SourceDiskID, &i.Path, &i.Version, &i.CreatedAt, &i.UpdatedAt); err != nil {
		return nil, err
	}
	i.State = domain.ImageState(state)
	i.CreatedAt, i.UpdatedAt = i.CreatedAt.UTC(), i.UpdatedAt.UTC()
	return &i, nil
}
