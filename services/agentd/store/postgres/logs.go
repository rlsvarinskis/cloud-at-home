package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
)

// AppendStep stores a copy of st with the next dense Seq of its task, which is also written back into st.
// The task row is locked for the duration of the transaction, so concurrent appends to one task are
// serialised and never share a Seq. An unknown task yields errs.ErrNotFound.
func (s *Store) AppendStep(ctx context.Context, st *domain.Step) (int, error) {
	var seq int
	err := s.withTx(ctx, func(t *Store) error {
		if err := lockTask(ctx, t.q, st.TaskID); err != nil {
			return err
		}
		n, err := nextSeq(ctx, t.q, "steps", st.TaskID)
		if err != nil {
			return err
		}
		var exitCode *int
		if st.ExitCode != nil {
			code := *st.ExitCode
			exitCode = &code
		}
		_, err = t.q.Exec(ctx,
			"INSERT INTO steps (task_id, seq, command, output, exit_code, at) VALUES ($1, $2, $3, $4, $5, $6)",
			st.TaskID, n, st.Command, st.Output, exitCode, st.At)
		if err != nil {
			return fmt.Errorf("postgres: insert step: %w", err)
		}
		seq = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	st.Seq = seq
	return seq, nil
}

// ListSteps returns the steps of taskID with Seq greater than afterSeq, in Seq order. limit <= 0 means no
// limit. An unknown task yields errs.ErrNotFound.
func (s *Store) ListSteps(ctx context.Context, taskID string, afterSeq, limit int) ([]*domain.Step, error) {
	var f filter
	f.add("task_id = $%d", taskID)
	f.add("seq > $%d", afterSeq)
	sql, args := f.selectSQL("task_id, seq, command, output, exit_code, at", "steps", "seq", limit)
	rows, err := s.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: list steps: %w", err)
	}
	defer rows.Close()
	var out []*domain.Step
	for rows.Next() {
		var (
			st       domain.Step
			exitCode *int
			at       time.Time
		)
		if err := rows.Scan(&st.TaskID, &st.Seq, &st.Command, &st.Output, &exitCode, &at); err != nil {
			return nil, fmt.Errorf("postgres: scan step: %w", err)
		}
		st.ExitCode = exitCode
		st.At = at.UTC()
		out = append(out, &st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list steps: %w", err)
	}
	if len(out) == 0 {
		if err := s.requireTask(ctx, taskID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AppendMessage stores a copy of m with the next dense Seq of its task, which is also written back into m.
// It locks the task row like AppendStep. An unknown task yields errs.ErrNotFound.
func (s *Store) AppendMessage(ctx context.Context, m *domain.Message) (int, error) {
	var seq int
	err := s.withTx(ctx, func(t *Store) error {
		if err := lockTask(ctx, t.q, m.TaskID); err != nil {
			return err
		}
		n, err := nextSeq(ctx, t.q, "messages", m.TaskID)
		if err != nil {
			return err
		}
		// A nil slice would be stored as an empty one; bytea NOT NULL needs a non-nil value.
		body := m.JSON
		if body == nil {
			body = []byte{}
		}
		_, err = t.q.Exec(ctx,
			"INSERT INTO messages (task_id, seq, json, at) VALUES ($1, $2, $3, $4)",
			m.TaskID, n, body, m.At)
		if err != nil {
			return fmt.Errorf("postgres: insert message: %w", err)
		}
		seq = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	m.Seq = seq
	return seq, nil
}

// ListMessages returns every message of taskID in Seq order. An unknown task yields errs.ErrNotFound.
func (s *Store) ListMessages(ctx context.Context, taskID string) ([]*domain.Message, error) {
	rows, err := s.q.Query(ctx,
		"SELECT task_id, seq, json, at FROM messages WHERE task_id = $1 ORDER BY seq", taskID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list messages: %w", err)
	}
	defer rows.Close()
	var out []*domain.Message
	for rows.Next() {
		var (
			m  domain.Message
			at time.Time
		)
		if err := rows.Scan(&m.TaskID, &m.Seq, &m.JSON, &at); err != nil {
			return nil, fmt.Errorf("postgres: scan message: %w", err)
		}
		m.At = at.UTC()
		out = append(out, &m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list messages: %w", err)
	}
	if len(out) == 0 {
		if err := s.requireTask(ctx, taskID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// lockTask takes the row lock of taskID for the rest of the transaction, or returns errs.ErrNotFound.
func lockTask(ctx context.Context, q querier, taskID string) error {
	var one int
	err := q.QueryRow(ctx, "SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE", taskID).Scan(&one)
	if err != nil {
		return notFound(err, "task %s", taskID)
	}
	return nil
}

// nextSeq returns one more than the highest Seq stored for taskID in table. The caller must hold the task's
// row lock.
func nextSeq(ctx context.Context, q querier, table, taskID string) (int, error) {
	var n int
	err := q.QueryRow(ctx,
		"SELECT COALESCE(MAX(seq), 0) + 1 FROM "+table+" WHERE task_id = $1", taskID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("postgres: next seq: %w", err)
	}
	return n, nil
}

// requireTask returns errs.ErrNotFound if taskID does not exist.
func (s *Store) requireTask(ctx context.Context, taskID string) error {
	var exists bool
	if err := s.q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1)", taskID).Scan(&exists); err != nil {
		return fmt.Errorf("postgres: check task %s: %w", taskID, err)
	}
	if !exists {
		return errs.NotFound("task %s", taskID)
	}
	return nil
}
