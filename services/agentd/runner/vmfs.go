package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/agentd/runner/applypatch"
)

// maxPatchFileBytes bounds how much of one file an apply_patch reads from the VM.
const maxPatchFileBytes = 4 << 20

var _ applypatch.FS = (*vmFS)(nil)

// vmFS is the applypatch file system of one agent's VM. Content moves through the file API (PutFile and GetFile,
// which are binary safe); paths of shell commands are passed base64-encoded, so no quoting of names is needed.
type vmFS struct {
	r     *Runner
	ctx   context.Context
	agent *domain.Agent
	key   string // idempotency key prefix of the commands this file system runs
	n     int    // commands run so far
	// err is the first infrastructure failure (the VM is unreachable or the task was stopped). The runner checks it
	// after the patch, so that a broken VM is reported as an error and a patch that does not apply is not.
	err error
}

// Read returns the content of p. A missing file is reported as not existing, not as an error.
func (f *vmFS) Read(p string) ([]byte, bool, error) {
	data, truncated, err := f.r.cfg.VM.GetFile(f.ctx, f.agent.VMID, p, maxPatchFileBytes)
	switch {
	case errors.Is(err, errs.ErrNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, f.wrap(err)
	case truncated:
		return nil, false, fmt.Errorf("%s is larger than %d bytes", p, maxPatchFileBytes)
	}
	return data, true, nil
}

// Write stores data at p. When the write fails, the parent directory is created and the write is tried once more.
func (f *vmFS) Write(p string, data []byte) error {
	err := f.r.cfg.VM.PutFile(f.ctx, f.agent.VMID, p, data, 0o644)
	if err == nil {
		return nil
	}
	if f.ctx.Err() != nil {
		return f.wrap(err)
	}
	if merr := f.run("mkdir -p -- \"$(printf %s " + b64(path.Dir(p)) + " | base64 -d)\""); merr != nil {
		return merr
	}
	if err := f.r.cfg.VM.PutFile(f.ctx, f.agent.VMID, p, data, 0o644); err != nil {
		return f.wrap(err)
	}
	return nil
}

// Delete removes p. Removing a file that is not there is not an error.
func (f *vmFS) Delete(p string) error {
	return f.run("rm -f -- \"$(printf %s " + b64(p) + " | base64 -d)\"")
}

// run runs a short shell command in the VM. A command that exits non-zero is an ordinary error of the patch.
func (f *vmFS) run(cmd string) error {
	f.n++
	res, err := f.r.runCommand(f.ctx, f.agent, fmt.Sprintf("%s-fs-%d", f.key, f.n), cmd, checkTimeout)
	if err != nil {
		return f.wrap(err)
	}
	if !res.passed() {
		return fmt.Errorf("command failed (%s): %s", res.status(), tailBytes(res.Stderr, 300))
	}
	return nil
}

// wrap records err as an infrastructure failure when the VM is unreachable or the task is stopping.
func (f *vmFS) wrap(err error) error {
	if errors.Is(err, errs.ErrUnavailable) || f.ctx.Err() != nil {
		if f.err == nil {
			f.err = err
		}
	}
	return err
}

// b64 is s base64-encoded and single-quoted, ready to be used as a shell word.
func b64(s string) string {
	return "'" + base64.StdEncoding.EncodeToString([]byte(s)) + "'"
}
