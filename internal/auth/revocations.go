package auth

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// FileRevocations is a token denylist kept in a text file: one token id (jti) per line, '#' starts a comment.
// Operators add a line to revoke a token before it expires; servers notice the change within the watch interval.
// If a later reload fails (the file vanished or became unreadable) the last good list stays in force: a
// transient filesystem problem must never un-revoke tokens.
type FileRevocations struct {
	path string
	set  atomic.Pointer[map[string]struct{}]
	mod  atomic.Int64 // mtime (unix nanos) of the file as last loaded
}

// NewFileRevocations loads path, which must exist and be readable.
func NewFileRevocations(path string) (*FileRevocations, error) {
	f := &FileRevocations{path: path}
	if err := f.Reload(); err != nil {
		return nil, err
	}
	return f, nil
}

// Revoked reports whether the token id is on the list.
func (f *FileRevocations) Revoked(jti string) bool {
	m := f.set.Load()
	if m == nil {
		return false
	}
	_, ok := (*m)[jti]
	return ok
}

// Len is the number of revoked ids currently in force.
func (f *FileRevocations) Len() int {
	if m := f.set.Load(); m != nil {
		return len(*m)
	}
	return 0
}

// Reload re-reads the file and replaces the list atomically.
func (f *FileRevocations) Reload() error {
	st, err := os.Stat(f.path)
	if err != nil {
		return fmt.Errorf("revocations: %w", err)
	}
	file, err := os.Open(f.path)
	if err != nil {
		return fmt.Errorf("revocations: %w", err)
	}
	defer file.Close()
	set := map[string]struct{}{}
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if line = strings.TrimSpace(line); line != "" {
			set[line] = struct{}{}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("revocations: %w", err)
	}
	f.set.Store(&set)
	f.mod.Store(st.ModTime().UnixNano())
	return nil
}

// Watch reloads the file whenever its modification time changes, until ctx ends.
func (f *FileRevocations) Watch(ctx context.Context, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := os.Stat(f.path)
		if err != nil {
			if log != nil {
				log.Warn("revocation list unreadable; keeping the last good list", "path", f.path, "err", err)
			}
			continue
		}
		if st.ModTime().UnixNano() == f.mod.Load() {
			continue
		}
		if err := f.Reload(); err != nil {
			if log != nil {
				log.Warn("revocation list reload failed; keeping the last good list", "path", f.path, "err", err)
			}
			continue
		}
		if log != nil {
			log.Info("revocation list reloaded", "path", f.path, "entries", f.Len())
		}
	}
}
