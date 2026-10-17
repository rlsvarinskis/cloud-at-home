package qemudriver

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// osCommander runs host commands with os/exec.
type osCommander struct{}

// Run implements Commander. On failure the error includes the command's stderr.
func (osCommander) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Start implements Commander. The process gets its own session, its standard input is /dev/null, and
// its output is appended to logPath. The child is reaped by a goroutine when it exits.
func (osCommander) Start(name string, args []string, logPath, runAs string) (int, error) {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer log.Close()

	cmd := exec.Command(name, args...)
	cmd.Dir = filepath.Dir(logPath)
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if runAs != "" {
		cred, err := credentialFor(runAs)
		if err != nil {
			return 0, err
		}
		cmd.SysProcAttr.Credential = cred
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}

// credentialFor returns the credentials of the unprivileged user name. The user is also put in the
// group that owns /dev/kvm, when there is one, so that KVM can be used.
func credentialFor(name string) (*syscall.Credential, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("look up user %s: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("user %s: bad uid %q: %w", name, u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("user %s: bad gid %q: %w", name, u.Gid, err)
	}
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	if fi, err := os.Stat("/dev/kvm"); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			cred.Groups = []uint32{st.Gid}
		}
	}
	return cred, nil
}

// osProcs inspects processes through /proc and signals them with kill(2).
type osProcs struct{}

// Alive implements Procs. A zombie or dead process is not alive.
func (osProcs) Alive(pid int, cmdlineContains string) bool {
	if pid <= 0 {
		return false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// The state is the first field after the parenthesised command name.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return false
	}
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) == 0 || fields[0] == "Z" || fields[0] == "X" {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return bytes.Contains(cmdline, []byte(cmdlineContains))
}

// Signal implements Procs.
func (osProcs) Signal(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

// osSystem reads host facts from the real host.
type osSystem struct{}

// ReadFile implements system.
func (osSystem) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(name)
}

// Statfs implements system. Sizes are in bytes.
func (osSystem) Statfs(path string) (total, avail int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize)
	return int64(st.Blocks) * bs, int64(st.Bavail) * bs, nil
}

// NumCPU implements system.
func (osSystem) NumCPU() int {
	return runtime.NumCPU()
}

// FreePort implements system. It asks the kernel for a port on 127.0.0.1 and releases it.
func (osSystem) FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
