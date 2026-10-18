package postgres

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
)

var nodeCols = []string{"id", "address", "state", "drain", "cpus", "memory_mb", "disk_bytes_total", "disk_bytes_free", "last_heartbeat"}

var (
	nodeSelect = selectSQL("nodes", nodeCols)
	// drain is excluded from the update: only SetNodeDrain changes it on an existing node.
	nodeUpsert = insertSQL("nodes", nodeCols) + " ON CONFLICT (id) DO UPDATE SET " + excludedSet(withoutCol(nodeCols, "drain"))
)

// excludedSet returns "col = EXCLUDED.col" for every column except id, for an upsert.
func excludedSet(cols []string) string {
	var sets []string
	for _, c := range cols {
		if c != "id" {
			sets = append(sets, c+" = EXCLUDED."+c)
		}
	}
	return strings.Join(sets, ", ")
}

func withoutCol(cols []string, drop string) []string {
	var out []string
	for _, c := range cols {
		if c != drop {
			out = append(out, c)
		}
	}
	return out
}

// SetNodeDrain records the operator's drain decision.
func (s *Store) SetNodeDrain(ctx context.Context, id string, drain bool) error {
	tag, err := s.db.Exec(ctx, "UPDATE nodes SET drain = $2 WHERE id = $1", id, drain)
	if err != nil {
		return writeErr(err, "update", "node", id)
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("node %s", id)
	}
	return nil
}

// UpsertNode inserts n, or replaces the stored node with the same ID.
func (s *Store) UpsertNode(ctx context.Context, n *domain.Node) error {
	args := pgx.NamedArgs{
		"id": n.ID, "address": n.Address, "state": string(n.State), "drain": n.Drain, "cpus": n.CPUs,
		"memory_mb": n.MemoryMB, "disk_bytes_total": n.DiskBytesTotal, "disk_bytes_free": n.DiskBytesFree,
		"last_heartbeat": n.LastHeartbeat.UTC(),
	}
	_, err := s.db.Exec(ctx, nodeUpsert, args)
	return writeErr(err, "upsert", "node", n.ID)
}

// GetNode returns the node with the given ID.
func (s *Store) GetNode(ctx context.Context, id string) (*domain.Node, error) {
	return getOne(ctx, s.db, "node", id, nodeSelect+" WHERE id = $1", scanNode, id)
}

// ListNodes returns every node ordered by ID.
func (s *Store) ListNodes(ctx context.Context) ([]*domain.Node, error) {
	return listRows(ctx, s.db, nodeSelect+" ORDER BY id", scanNode)
}

func scanNode(r scanner) (*domain.Node, error) {
	var n domain.Node
	var state string
	if err := r.Scan(&n.ID, &n.Address, &state, &n.Drain, &n.CPUs, &n.MemoryMB, &n.DiskBytesTotal, &n.DiskBytesFree, &n.LastHeartbeat); err != nil {
		return nil, err
	}
	n.State = domain.NodeState(state)
	n.LastHeartbeat = n.LastHeartbeat.UTC()
	return &n, nil
}
