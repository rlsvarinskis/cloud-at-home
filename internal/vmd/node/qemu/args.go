// Package qemu builds QEMU command lines, cloud-init documents and qemu-img
// argument vectors for the QEMU node driver. Everything in this package is
// pure: it returns argv slices and strings and never executes a process or
// touches the filesystem, so the caller owns all execution.
package qemu

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// VMSpec describes a virtual machine to boot.
type VMSpec struct {
	// Name identifies the VM. It must be non-empty and contain no whitespace.
	Name string
	// CPUs is the number of virtual CPUs (at least 1).
	CPUs int
	// MemoryMB is the guest RAM in mebibytes (at least 256).
	MemoryMB int64
	// DiskPaths lists qcow2 disks. The first is the boot disk; each becomes a
	// virtio drive in order, so they appear in the guest as vda, vdb, and so on.
	DiskPaths []string
	// SeedISO is an optional cloud-init NoCloud ISO, attached read-only after
	// the disks.
	SeedISO string
	// SSHPort is the host port, bound to 127.0.0.1 only, that forwards to guest
	// port 22. It must be between 1024 and 65535.
	SSHPort int
	// ConsolePath is the serial console log file. Empty means no console log.
	ConsolePath string
	// SharedDir, when non-empty, is exported to the guest as a 9p share with
	// mount tag "shared" and the mapped-xattr security model.
	SharedDir string
	// KVM selects hardware acceleration (-enable-kvm, -cpu host). When false the
	// VM runs under TCG emulation (-accel tcg, -cpu max), which is used for CI.
	KVM bool
}

const (
	minMemoryMB = 256
	minSSHPort  = 1024
	maxPort     = 65535
)

// Validate reports whether the spec can be turned into a valid QEMU command line.
func (s VMSpec) Validate() error {
	if s.Name == "" {
		return errors.New("qemu: VM name is empty")
	}
	if strings.ContainsFunc(s.Name, unicode.IsSpace) {
		return fmt.Errorf("qemu: VM name %q contains whitespace", s.Name)
	}
	if s.CPUs < 1 {
		return fmt.Errorf("qemu: CPUs must be at least 1, got %d", s.CPUs)
	}
	if s.MemoryMB < minMemoryMB {
		return fmt.Errorf("qemu: memory must be at least %d MB, got %d", minMemoryMB, s.MemoryMB)
	}
	if len(s.DiskPaths) == 0 {
		return errors.New("qemu: at least one disk is required")
	}
	for i, p := range s.DiskPaths {
		if p == "" {
			return fmt.Errorf("qemu: disk %d has an empty path", i)
		}
	}
	if s.SSHPort < minSSHPort || s.SSHPort > maxPort {
		return fmt.Errorf("qemu: SSH port must be in %d..%d, got %d", minSSHPort, maxPort, s.SSHPort)
	}
	return nil
}

// Binary returns the QEMU executable that Args output is meant for.
func Binary() string {
	return "qemu-system-x86_64"
}

// Args returns the qemu-system-x86_64 arguments for s, without the leading
// program name. It returns an error if s fails Validate.
func Args(s VMSpec) ([]string, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}

	var args []string
	if s.KVM {
		args = append(args, "-enable-kvm", "-cpu", "host")
	} else {
		args = append(args, "-accel", "tcg", "-cpu", "max")
	}
	args = append(args,
		"-machine", "q35",
		"-smp", strconv.Itoa(s.CPUs),
		"-m", strconv.FormatInt(s.MemoryMB, 10)+"M",
		"-display", "none",
	)

	if s.ConsolePath != "" {
		args = append(args, "-serial", "file:"+optValue(s.ConsolePath))
	} else {
		args = append(args, "-serial", "none")
	}

	for _, disk := range s.DiskPaths {
		args = append(args, "-drive", "file="+optValue(disk)+",if=virtio,format=qcow2")
	}
	if s.SeedISO != "" {
		args = append(args, "-drive", "file="+optValue(s.SeedISO)+",if=virtio,format=raw,readonly=on")
	}

	// User-mode NAT: the guest can reach the internet, and the only inbound
	// path is the SSH forward, bound to loopback.
	args = append(args,
		"-netdev", fmt.Sprintf("user,id=n0,ipv6=off,hostfwd=tcp:127.0.0.1:%d-:22", s.SSHPort),
		"-device", "virtio-net-pci,netdev=n0",
	)

	if s.SharedDir != "" {
		args = append(args,
			"-fsdev", "local,id=fs0,path="+optValue(s.SharedDir)+",security_model=mapped-xattr",
			"-device", "virtio-9p-pci,fsdev=fs0,mount_tag=shared",
		)
	}
	return args, nil
}

// optValue escapes s for use as a value inside a QEMU comma-separated option
// string, where a literal comma must be doubled.
func optValue(s string) string {
	return strings.ReplaceAll(s, ",", ",,")
}

// userDataTemplate is the cloud-config document for the "agent" user. The
// public key is written inside double quotes, which is why checkSSHKey rejects
// quotes, backslashes and control characters.
const userDataTemplate = `#cloud-config
users:
  - name: agent
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys: ["%s"]
ssh_pwauth: false
`

var pkgNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*$`)

// UserData returns a "#cloud-config" document that creates the "agent" user
// with passwordless sudo and the given SSH public key, and disables password
// authentication. If packages is non-empty it also requests a package update
// and installs those packages.
func UserData(sshPublicKey string, packages []string) (string, error) {
	if err := checkSSHKey(sshPublicKey); err != nil {
		return "", err
	}
	for _, p := range packages {
		if !pkgNameRE.MatchString(p) {
			return "", fmt.Errorf("qemu: invalid package name %q", p)
		}
	}

	doc := fmt.Sprintf(userDataTemplate, sshPublicKey)
	if len(packages) > 0 {
		doc += fmt.Sprintf("package_update: true\npackages: [%s]\n", strings.Join(packages, ", "))
	}
	return doc, nil
}

// checkSSHKey requires a single printable-ASCII line with at least a key type
// and key body, and no quote or backslash characters.
func checkSSHKey(key string) error {
	if len(strings.Fields(key)) < 2 {
		return errors.New("qemu: SSH public key must contain a key type and key body")
	}
	for _, r := range key {
		switch {
		case r < 0x20 || r > 0x7e:
			return fmt.Errorf("qemu: SSH public key contains invalid character %U", r)
		case r == '"' || r == '\\':
			return errors.New("qemu: SSH public key must not contain quotes or backslashes")
		}
	}
	return nil
}

var metaFieldRE = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// MetaData returns a cloud-init meta-data document for the given instance ID
// and hostname. Both values may contain only letters, digits, '.', '_' and '-'.
func MetaData(instanceID, hostname string) (string, error) {
	if !metaFieldRE.MatchString(instanceID) {
		return "", fmt.Errorf("qemu: invalid instance ID %q", instanceID)
	}
	if !metaFieldRE.MatchString(hostname) {
		return "", fmt.Errorf("qemu: invalid hostname %q", hostname)
	}
	return fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", instanceID, hostname), nil
}

// ImageInfo is the subset of `qemu-img info --output=json` that the node needs.
type ImageInfo struct {
	// VirtualSize is the guest-visible size in bytes.
	VirtualSize int64
	// ActualSize is the space the image occupies on disk in bytes.
	ActualSize int64
	// Format is the image format, such as "qcow2" or "raw".
	Format string
	// BackingFile is the backing image path, or empty if there is none.
	BackingFile string
}

// rawImageInfo mirrors the JSON emitted by qemu-img. VirtualSize is a pointer
// so that a missing field can be told apart from a zero size.
type rawImageInfo struct {
	VirtualSize *int64 `json:"virtual-size"`
	ActualSize  int64  `json:"actual-size"`
	Format      string `json:"format"`
	BackingFile string `json:"backing-filename"`
}

// ParseImageInfo parses the output of `qemu-img info --output=json`. It returns
// an error for malformed JSON, a missing virtual-size, or negative sizes.
func ParseImageInfo(qemuImgInfoJSON []byte) (ImageInfo, error) {
	var raw rawImageInfo
	if err := json.Unmarshal(qemuImgInfoJSON, &raw); err != nil {
		return ImageInfo{}, fmt.Errorf("qemu: parse qemu-img info: %w", err)
	}
	if raw.VirtualSize == nil {
		return ImageInfo{}, errors.New("qemu: qemu-img info is missing virtual-size")
	}
	if *raw.VirtualSize < 0 || raw.ActualSize < 0 {
		return ImageInfo{}, errors.New("qemu: qemu-img info reports a negative size")
	}
	return ImageInfo{
		VirtualSize: *raw.VirtualSize,
		ActualSize:  raw.ActualSize,
		Format:      raw.Format,
		BackingFile: raw.BackingFile,
	}, nil
}

// checkImgPath rejects paths that are empty, contain NUL, or begin with '-'
// (which qemu-img would parse as an option).
func checkImgPath(what, p string) error {
	switch {
	case p == "":
		return fmt.Errorf("qemu: %s is empty", what)
	case strings.HasPrefix(p, "-"):
		return fmt.Errorf("qemu: %s %q must not start with '-'", what, p)
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("qemu: %s contains a NUL byte", what)
	}
	return nil
}

// ImgCreateArgs returns qemu-img arguments that create a qcow2 image at path.
// If backingFile is non-empty the image is an overlay on it, and sizeGB is
// optional (0 keeps the backing size). Without a backing file sizeGB must be
// at least 1.
func ImgCreateArgs(path, backingFile string, sizeGB int) ([]string, error) {
	if err := checkImgPath("image path", path); err != nil {
		return nil, err
	}
	if sizeGB < 0 {
		return nil, fmt.Errorf("qemu: size must not be negative, got %d GB", sizeGB)
	}

	args := []string{"create", "-q", "-f", "qcow2"}
	if backingFile != "" {
		if err := checkImgPath("backing file", backingFile); err != nil {
			return nil, err
		}
		args = append(args, "-b", backingFile, "-F", "qcow2")
	} else if sizeGB < 1 {
		return nil, errors.New("qemu: an image without a backing file needs a size of at least 1 GB")
	}

	args = append(args, path)
	if sizeGB > 0 {
		args = append(args, fmt.Sprintf("%dG", sizeGB))
	}
	return args, nil
}

// ImgResizeArgs returns qemu-img arguments that resize the image at path to
// sizeGB gibibytes. sizeGB must be at least 1.
func ImgResizeArgs(path string, sizeGB int) ([]string, error) {
	if err := checkImgPath("image path", path); err != nil {
		return nil, err
	}
	if sizeGB < 1 {
		return nil, fmt.Errorf("qemu: resize size must be at least 1 GB, got %d", sizeGB)
	}
	return []string{"resize", path, fmt.Sprintf("%dG", sizeGB)}, nil
}

// ImgConvertArgs returns qemu-img arguments that convert src into a standalone
// qcow2 image at dst. This flattens a backing chain. src and dst must differ.
func ImgConvertArgs(src, dst string) ([]string, error) {
	if err := checkImgPath("source image", src); err != nil {
		return nil, err
	}
	if err := checkImgPath("destination image", dst); err != nil {
		return nil, err
	}
	if src == dst {
		return nil, fmt.Errorf("qemu: source and destination are both %q", src)
	}
	return []string{"convert", "-q", "-O", "qcow2", src, dst}, nil
}
