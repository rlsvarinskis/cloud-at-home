package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
)

var vmCols = []string{
	"id", "project_id", "name", "node_id", "cpus", "memory_mb", "disk_ids", "desired", "state", "state_reason",
	"holds", "labels", "generation", "version", "stop_force", "delete_disks", "idempotency_key",
	"next_reconcile", "lease_owner", "lease_until", "failures", "crashes", "created_at", "updated_at",
}

var (
	vmSelect = selectSQL("vms", vmCols)
	vmInsert = insertSQL("vms", vmCols)
	vmUpdate = updateSQL("vms", vmCols) + " WHERE id = @id AND version = @expected RETURNING version"

	// claimSQL leases due VMs in one statement. SKIP LOCKED lets concurrent claimers pass over rows
	// that another claimer is locking instead of waiting for them.
	claimSQL = "WITH due AS (" +
		"SELECT id FROM vms WHERE next_reconcile IS NOT NULL AND next_reconcile <= @now AND lease_until <= @now " +
		"ORDER BY next_reconcile, id LIMIT @limit FOR UPDATE SKIP LOCKED) " +
		"UPDATE vms SET lease_owner = @owner, lease_until = @lease_until, version = version + 1 " +
		"FROM due WHERE vms.id = due.id RETURNING " + qualify("vms", vmCols)
)

// labelsJSON encodes a label map. Encoding a map[string]string cannot fail.
func labelsJSON(l map[string]string) []byte {
	b, _ := json.Marshal(l)
	return b
}

func vmArgs(v *domain.VM) pgx.NamedArgs {
	return pgx.NamedArgs{
		"id": v.ID, "project_id": v.ProjectID, "name": v.Name, "node_id": v.NodeID,
		"cpus": v.CPUs, "memory_mb": v.MemoryMB, "disk_ids": v.DiskIDs,
		"desired": string(v.Desired), "state": string(v.State), "state_reason": v.StateReason,
		"holds": v.Holds, "labels": labelsJSON(v.Labels), "generation": v.Generation,
		"version": v.Version, "stop_force": v.StopForce, "delete_disks": v.DeleteDisks,
		"idempotency_key": v.IdempotencyKey, "next_reconcile": nullableTime(v.NextReconcile),
		"lease_owner": v.LeaseOwner, "lease_until": leaseValue(v.LeaseUntil), "failures": v.Failures, "crashes": v.Crashes,
		"created_at": v.CreatedAt.UTC(), "updated_at": v.UpdatedAt.UTC(),
	}
}

// CreateVM inserts v with Version 1 and sets v.Version.
func (s *Store) CreateVM(ctx context.Context, v *domain.VM) error {
	args := vmArgs(v)
	args["version"] = int64(1)
	if err := s.insert(ctx, "vm", v.ID, vmInsert, args); err != nil {
		return err
	}
	v.Version = 1
	return nil
}

// GetVM returns the VM with the given ID.
func (s *Store) GetVM(ctx context.Context, id string) (*domain.VM, error) {
	return getOne(ctx, s.db, "vm", id, vmSelect+" WHERE id = $1", scanVM, id)
}

// GetVMByIdempotencyKey returns the project's VM created with key. An empty key matches nothing.
func (s *Store) GetVMByIdempotencyKey(ctx context.Context, project, key string) (*domain.VM, error) {
	if key == "" {
		return nil, errs.NotFound("vm idempotency key %q", key)
	}
	return getOne(ctx, s.db, "vm", "idempotency key "+key, vmSelect+" WHERE project_id = $1 AND idempotency_key = $2", scanVM, project, key)
}

// ListVMs returns the VMs of f.ProjectID whose labels match f.Labels exactly, after f.After, ordered
// by ID. A label missing from a VM counts as the empty value, as in the memory store.
func (s *Store) ListVMs(ctx context.Context, f domain.VMFilter) ([]*domain.VM, error) {
	var fl filter
	fl.add("project_id = " + fl.arg(f.ProjectID))
	for _, k := range slices.Sorted(maps.Keys(f.Labels)) {
		fl.add("COALESCE(labels ->> " + fl.arg(k) + "::text, '') = " + fl.arg(f.Labels[k]) + "::text")
	}
	fl.keyset(f.After)
	sql, args := fl.selectWhere(vmCols, "vms", f.Limit)
	return listRows(ctx, s.db, sql, scanVM, args...)
}

// ListVMsByNode returns the VMs placed on nodeID, ordered by ID.
func (s *Store) ListVMsByNode(ctx context.Context, nodeID string) ([]*domain.VM, error) {
	var f filter
	f.add("node_id = " + f.arg(nodeID))
	sql, args := f.selectWhere(vmCols, "vms", 0)
	return listRows(ctx, s.db, sql, scanVM, args...)
}

// UpdateVM is a compare-and-swap on Version; see package store.
func (s *Store) UpdateVM(ctx context.Context, v *domain.VM) error {
	args := vmArgs(v)
	args["expected"] = v.Version
	nv, err := s.update(ctx, "vm", "vms", v.ID, vmUpdate, args)
	if err != nil {
		return err
	}
	v.Version = nv
	return nil
}

// DeleteVM removes the VM with the given ID.
func (s *Store) DeleteVM(ctx context.Context, id string) error {
	return s.remove(ctx, "vm", "vms", id)
}

// ClaimVMs leases up to limit due VMs (limit <= 0 means all) and returns them ordered by
// NextReconcile then ID. Each claimed VM's version is bumped, so the returned copies are the ones to
// update.
func (s *Store) ClaimVMs(ctx context.Context, owner string, now time.Time, ttl time.Duration, limit int) ([]*domain.VM, error) {
	var lim any // nil means no LIMIT
	if limit > 0 {
		lim = int64(limit)
	}
	args := pgx.NamedArgs{
		"now": now.UTC(), "owner": owner, "lease_until": now.Add(ttl).UTC(), "limit": lim,
	}
	claimed, err := listRows(ctx, s.db, claimSQL, scanVM, args)
	if err != nil {
		return nil, fmt.Errorf("claim vms: %w", err)
	}
	slices.SortFunc(claimed, func(a, b *domain.VM) int {
		if c := a.NextReconcile.Compare(b.NextReconcile); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return claimed, nil
}

func scanVM(r scanner) (*domain.VM, error) {
	var (
		v              domain.VM
		desired, state string
		labels         []byte
		next           *time.Time
		lease          time.Time
	)
	if err := r.Scan(&v.ID, &v.ProjectID, &v.Name, &v.NodeID, &v.CPUs, &v.MemoryMB, &v.DiskIDs,
		&desired, &state, &v.StateReason, &v.Holds, &labels, &v.Generation, &v.Version, &v.StopForce,
		&v.DeleteDisks, &v.IdempotencyKey, &next, &v.LeaseOwner, &lease, &v.Failures, &v.Crashes,
		&v.CreatedAt, &v.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(labels, &v.Labels); err != nil {
		return nil, fmt.Errorf("vm labels: %w", err)
	}
	v.Desired = domain.PowerState(desired)
	v.State = domain.VMState(state)
	if next != nil {
		v.NextReconcile = next.UTC()
	}
	v.LeaseUntil = fromLease(lease)
	v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
	return &v, nil
}
