package vmcli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/cli"
)

// MaxFileBytes is the most content a single PutFile or GetFile carries: the vmd limit, 8 MiB. Both directions are
// checked against it in the client, so an oversize file is refused before anything is sent.
const MaxFileBytes = 8 << 20

// defaultStdinMode is the mode of an upload from stdin, which has no file mode of its own.
const defaultStdinMode = 0o644

// defaultDownloadMode is the mode of a file written by a download. GetFile does not return the file's mode.
const defaultDownloadMode = 0o644

// operand is one side of a cp: a local path ("-" is stdin or stdout), or a VM path NAME:/absolute/path.
type operand struct {
	remote bool
	local  string // local path when !remote
	vm     string // VM name when remote
	path   string // absolute path inside the VM when remote
}

// parseOperand classifies an operand. It is remote when it has a colon, unless it starts with "/", "./" or "../".
// "-" is the local standard stream.
func parseOperand(s string) (operand, error) {
	if s == "" {
		return operand{}, cli.Usagef("an operand is empty")
	}
	if s == "-" || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") {
		return operand{local: s}, nil
	}
	name, path, ok := strings.Cut(s, ":")
	if !ok {
		return operand{local: s}, nil
	}
	if name == "" {
		return operand{}, cli.Usagef("%q: a VM path needs the VM name before the colon", s)
	}
	if !strings.HasPrefix(path, "/") {
		return operand{}, cli.Usagef("%q: the path inside the VM must be absolute, for example %s:/root/file "+
			"(a local file whose name contains a colon needs a leading ./)", s, name)
	}
	if strings.ContainsRune(path, 0) {
		return operand{}, cli.Usagef("%q: the path inside the VM must not contain NUL bytes", s)
	}
	return operand{remote: true, vm: name, path: path}, nil
}

// parseMode parses octal permission bits. An empty value means none was given.
func parseMode(s string) (uint32, bool, error) {
	if s == "" {
		return 0, false, nil
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil || v > 0o7777 {
		return 0, false, cli.Usagef("--mode %q: want octal permission bits such as 0644", s)
	}
	return uint32(v), true, nil
}

// runCp copies a file to a VM (upload) or from a VM (download). Exactly one operand is a VM path.
func runCp(ctx context.Context, env *cli.Env, args []string) error {
	fs := env.Flags()
	modeFlag := fs.String("mode", "", "permission bits of the uploaded file, in octal, for example 0600 "+
		"(default: the local file's mode, or 0644 for stdin)")
	if err := env.Parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return cli.Usagef("cp needs two operands, SRC and DST, got %d", fs.NArg())
	}
	src, err := parseOperand(fs.Arg(0))
	if err != nil {
		return err
	}
	dst, err := parseOperand(fs.Arg(1))
	if err != nil {
		return err
	}
	if src.remote == dst.remote {
		return cli.Usagef("exactly one of SRC and DST must be a VM path, NAME:/absolute/path")
	}
	if src.remote && *modeFlag != "" {
		return cli.Usagef("--mode applies to uploads only")
	}
	c, err := newClients(env)
	if err != nil {
		return err
	}
	if dst.remote {
		mode, modeSet, err := parseMode(*modeFlag)
		if err != nil {
			return err
		}
		return upload(ctx, env, c, src, dst, mode, modeSet)
	}
	return download(ctx, env, c, src, dst)
}

// upload sends a local file, or stdin, to a VM with PutFile.
func upload(ctx context.Context, env *cli.Env, c clients, src, dst operand, mode uint32, modeSet bool) error {
	var data []byte
	if src.local == "-" {
		d, err := readBounded(env.Stdin, "stdin")
		if err != nil {
			return err
		}
		data = d
		if !modeSet {
			mode = defaultStdinMode
		}
	} else {
		f, err := os.Open(src.local)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", src.local)
		}
		if info.Size() > MaxFileBytes {
			return tooLarge(src.local)
		}
		if !modeSet {
			mode = uint32(info.Mode().Perm())
		}
		d, err := readBounded(f, src.local)
		if err != nil {
			return err
		}
		data = d
	}
	vmID, err := resolveVM(ctx, c.vms, env.Config.Project, dst.vm)
	if err != nil {
		return err
	}
	if _, err := c.exec.PutFile(ctx, connect.NewRequest(&vmdv1.PutFileRequest{
		VmId: vmID, Path: dst.path, Content: data, Mode: mode,
	})); err != nil {
		return cli.RPCError(err)
	}
	fmt.Fprintf(env.Stderr, "wrote %d bytes to %s:%s\n", len(data), dst.vm, dst.path)
	return nil
}

// download reads a file from a VM with GetFile and writes it to a local path, or to stdout for "-". A file larger
// than the limit is refused and nothing is written.
func download(ctx context.Context, env *cli.Env, c clients, src, dst operand) error {
	vmID, err := resolveVM(ctx, c.vms, env.Config.Project, src.vm)
	if err != nil {
		return err
	}
	res, err := c.exec.GetFile(ctx, connect.NewRequest(&vmdv1.GetFileRequest{
		VmId: vmID, Path: src.path, MaxBytes: MaxFileBytes,
	}))
	if err != nil {
		return cli.RPCError(err)
	}
	data := res.Msg.GetContent()
	if res.Msg.GetTruncated() || len(data) > MaxFileBytes {
		return fmt.Errorf("%s:%s is larger than 8 MiB; nothing was written", src.vm, src.path)
	}
	if dst.local == "-" {
		if _, err := env.Stdout.Write(data); err != nil {
			return err
		}
	} else if err := writeFileAtomic(dst.local, data, defaultDownloadMode); err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "read %d bytes from %s:%s\n", len(data), src.vm, src.path)
	return nil
}

// readBounded reads at most MaxFileBytes from r. It fails when there is more.
func readBounded(r io.Reader, what string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	if len(data) > MaxFileBytes {
		return nil, tooLarge(what)
	}
	return data, nil
}

func tooLarge(what string) error {
	return fmt.Errorf("%s is larger than 8 MiB; uploads are limited to %d bytes", what, MaxFileBytes)
}
