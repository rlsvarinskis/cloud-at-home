package qemu

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fullSpec is a VM with every optional feature enabled.
func fullSpec() VMSpec {
	return VMSpec{
		Name:        "vm1",
		CPUs:        2,
		MemoryMB:    4096,
		DiskPaths:   []string{"/d/disk.qcow2"},
		SeedISO:     "/d/seed.iso",
		SSHPort:     2222,
		ConsolePath: "/d/console.log",
		SharedDir:   "/d/shared",
		KVM:         true,
	}
}

func TestArgsFullKVM(t *testing.T) {
	want := []string{
		"-enable-kvm", "-cpu", "host",
		"-machine", "q35",
		"-smp", "2",
		"-m", "4096M",
		"-display", "none",
		"-serial", "file:/d/console.log",
		"-drive", "file=/d/disk.qcow2,if=virtio,format=qcow2",
		"-drive", "file=/d/seed.iso,if=virtio,format=raw,readonly=on",
		"-netdev", "user,id=n0,ipv6=off,hostfwd=tcp:127.0.0.1:2222-:22",
		"-device", "virtio-net-pci,netdev=n0",
		"-fsdev", "local,id=fs0,path=/d/shared,security_model=mapped-xattr",
		"-device", "virtio-9p-pci,fsdev=fs0,mount_tag=shared",
	}
	got, err := Args(fullSpec())
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("Args mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestArgsOptionalFeatures checks the exact argv with each optional feature
// on or off, under both acceleration modes.
func TestArgsOptionalFeatures(t *testing.T) {
	const (
		kvmPrefix = "-enable-kvm -cpu host"
		tcgPrefix = "-accel tcg -cpu max"
	)
	base := []string{
		"-machine", "q35", "-smp", "2", "-m", "4096M", "-display", "none",
	}
	disk := []string{"-drive", "file=/d/disk.qcow2,if=virtio,format=qcow2"}
	seed := []string{"-drive", "file=/d/seed.iso,if=virtio,format=raw,readonly=on"}
	console := []string{"-serial", "file:/d/console.log"}
	noConsole := []string{"-serial", "none"}
	net := []string{
		"-netdev", "user,id=n0,ipv6=off,hostfwd=tcp:127.0.0.1:2222-:22",
		"-device", "virtio-net-pci,netdev=n0",
	}
	shared := []string{
		"-fsdev", "local,id=fs0,path=/d/shared,security_model=mapped-xattr",
		"-device", "virtio-9p-pci,fsdev=fs0,mount_tag=shared",
	}

	tests := []struct {
		name   string
		mutate func(*VMSpec)
		accel  []string
		want   []string
	}{
		{
			name:   "tcg no extras",
			mutate: func(s *VMSpec) { s.KVM = false; s.SeedISO = ""; s.ConsolePath = ""; s.SharedDir = "" },
			accel:  strings.Fields(tcgPrefix),
			want:   slices.Concat(base, noConsole, disk, net),
		},
		{
			name:   "kvm no extras",
			mutate: func(s *VMSpec) { s.SeedISO = ""; s.ConsolePath = ""; s.SharedDir = "" },
			accel:  strings.Fields(kvmPrefix),
			want:   slices.Concat(base, noConsole, disk, net),
		},
		{
			name:   "kvm seed only",
			mutate: func(s *VMSpec) { s.ConsolePath = ""; s.SharedDir = "" },
			accel:  strings.Fields(kvmPrefix),
			want:   slices.Concat(base, noConsole, disk, seed, net),
		},
		{
			name:   "kvm console only",
			mutate: func(s *VMSpec) { s.SeedISO = ""; s.SharedDir = "" },
			accel:  strings.Fields(kvmPrefix),
			want:   slices.Concat(base, console, disk, net),
		},
		{
			name:   "kvm shared only",
			mutate: func(s *VMSpec) { s.SeedISO = ""; s.ConsolePath = "" },
			accel:  strings.Fields(kvmPrefix),
			want:   slices.Concat(base, noConsole, disk, net, shared),
		},
		{
			name:   "tcg all extras",
			mutate: func(s *VMSpec) { s.KVM = false },
			accel:  strings.Fields(tcgPrefix),
			want:   slices.Concat(base, console, disk, seed, net, shared),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fullSpec()
			tt.mutate(&s)
			got, err := Args(s)
			if err != nil {
				t.Fatalf("Args: %v", err)
			}
			// Acceleration flags always come first, so build the expected argv
			// with them in front.
			want := slices.Concat(tt.accel, tt.want)
			if !slices.Equal(got, want) {
				t.Errorf("Args mismatch\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

func TestArgsMinimalWithoutKVM(t *testing.T) {
	s := VMSpec{Name: "ci", CPUs: 1, MemoryMB: 256, DiskPaths: []string{"/d/a.qcow2"}, SSHPort: 1024}
	want := []string{
		"-accel", "tcg", "-cpu", "max",
		"-machine", "q35",
		"-smp", "1",
		"-m", "256M",
		"-display", "none",
		"-serial", "none",
		"-drive", "file=/d/a.qcow2,if=virtio,format=qcow2",
		"-netdev", "user,id=n0,ipv6=off,hostfwd=tcp:127.0.0.1:1024-:22",
		"-device", "virtio-net-pci,netdev=n0",
	}
	got, err := Args(s)
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("Args mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestArgsNeverEnablesKVMWhenDisabled checks across feature combinations that
// -enable-kvm is absent whenever KVM is false.
func TestArgsNeverEnablesKVMWhenDisabled(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		s := fullSpec()
		s.KVM = false
		if mask&1 != 0 {
			s.SeedISO = ""
		}
		if mask&2 != 0 {
			s.ConsolePath = ""
		}
		if mask&4 != 0 {
			s.SharedDir = ""
		}
		got, err := Args(s)
		if err != nil {
			t.Fatalf("mask %d: Args: %v", mask, err)
		}
		if slices.Contains(got, "-enable-kvm") {
			t.Errorf("mask %d: -enable-kvm present with KVM=false: %q", mask, got)
		}
		if slices.Contains(got, "host") {
			t.Errorf("mask %d: -cpu host present with KVM=false: %q", mask, got)
		}
	}
}

// driveValues returns the value following each "-drive" flag, in order.
func driveValues(args []string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-drive" {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestArgsMultipleDisksInOrder(t *testing.T) {
	s := fullSpec()
	s.DiskPaths = []string{"/d/boot.qcow2", "/d/data1.qcow2", "/d/data2.qcow2"}
	got, err := Args(s)
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	want := []string{
		"file=/d/boot.qcow2,if=virtio,format=qcow2",
		"file=/d/data1.qcow2,if=virtio,format=qcow2",
		"file=/d/data2.qcow2,if=virtio,format=qcow2",
		"file=/d/seed.iso,if=virtio,format=raw,readonly=on",
	}
	if drives := driveValues(got); !slices.Equal(drives, want) {
		t.Errorf("-drive order mismatch\n got: %q\nwant: %q", drives, want)
	}
}

func TestArgsEscapesCommasInPaths(t *testing.T) {
	s := fullSpec()
	s.DiskPaths = []string{"/d/a,b.qcow2"}
	s.SharedDir = "/d/x,y"
	got, err := Args(s)
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	if drives := driveValues(got); drives[0] != "file=/d/a,,b.qcow2,if=virtio,format=qcow2" {
		t.Errorf("disk path not escaped: %q", drives[0])
	}
	if !slices.Contains(got, "local,id=fs0,path=/d/x,,y,security_model=mapped-xattr") {
		t.Errorf("shared dir not escaped: %q", got)
	}
}

func TestArgsPortForwardBoundToLoopback(t *testing.T) {
	for _, port := range []int{1024, 2222, 65535} {
		s := fullSpec()
		s.SSHPort = port
		got, err := Args(s)
		if err != nil {
			t.Fatalf("port %d: Args: %v", port, err)
		}
		var netdev string
		for i := 0; i+1 < len(got); i++ {
			if got[i] == "-netdev" {
				netdev = got[i+1]
			}
		}
		want := "hostfwd=tcp:127.0.0.1:" + strconv.Itoa(port) + "-:22"
		if !strings.Contains(netdev, want) {
			t.Errorf("port %d: netdev %q lacks %q", port, netdev, want)
		}
		if strings.Contains(netdev, "hostfwd=tcp::") || strings.Contains(netdev, "0.0.0.0") {
			t.Errorf("port %d: forward not bound to loopback: %q", port, netdev)
		}
	}
}

func TestVMSpecValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*VMSpec)
		wantErr bool
	}{
		{name: "valid", mutate: func(*VMSpec) {}},
		{name: "min cpus and memory and port", mutate: func(s *VMSpec) {
			s.CPUs, s.MemoryMB, s.SSHPort = 1, 256, 1024
		}},
		{name: "max port", mutate: func(s *VMSpec) { s.SSHPort = 65535 }},
		{name: "zero cpus", mutate: func(s *VMSpec) { s.CPUs = 0 }, wantErr: true},
		{name: "memory below minimum", mutate: func(s *VMSpec) { s.MemoryMB = 255 }, wantErr: true},
		{name: "no disks", mutate: func(s *VMSpec) { s.DiskPaths = nil }, wantErr: true},
		{name: "empty disk path", mutate: func(s *VMSpec) { s.DiskPaths = []string{"/d/a", ""} }, wantErr: true},
		{name: "port below range", mutate: func(s *VMSpec) { s.SSHPort = 1023 }, wantErr: true},
		{name: "port above range", mutate: func(s *VMSpec) { s.SSHPort = 65536 }, wantErr: true},
		{name: "empty name", mutate: func(s *VMSpec) { s.Name = "" }, wantErr: true},
		{name: "name with space", mutate: func(s *VMSpec) { s.Name = "my vm" }, wantErr: true},
		{name: "name with tab", mutate: func(s *VMSpec) { s.Name = "my\tvm" }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fullSpec()
			tt.mutate(&s)
			err := s.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			_, argsErr := Args(s)
			if (argsErr != nil) != tt.wantErr {
				t.Errorf("Args() error = %v, wantErr %v", argsErr, tt.wantErr)
			}
		})
	}
}

func TestArgsRejectsInvalidSpec(t *testing.T) {
	s := fullSpec()
	s.CPUs = 0
	got, err := Args(s)
	if err == nil {
		t.Fatal("Args() succeeded for invalid spec")
	}
	if got != nil {
		t.Errorf("Args() returned argv %q alongside error", got)
	}
}

func TestBinary(t *testing.T) {
	if got := Binary(); got != "qemu-system-x86_64" {
		t.Errorf("Binary() = %q", got)
	}
}

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey robert@host"

func TestUserData(t *testing.T) {
	const wantBase = `#cloud-config
users:
  - name: agent
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey robert@host"]
ssh_pwauth: false
`
	tests := []struct {
		name     string
		key      string
		packages []string
		want     string
		wantErr  bool
	}{
		{name: "no packages", key: testKey, want: wantBase},
		{name: "nil packages", key: testKey, packages: nil, want: wantBase},
		{
			name:     "with packages",
			key:      testKey,
			packages: []string{"git", "curl", "build-essential", "g++", "python3.12"},
			want:     wantBase + "package_update: true\npackages: [git, curl, build-essential, g++, python3.12]\n",
		},
		{name: "key with newline", key: testKey + "\n", wantErr: true},
		{name: "key with embedded newline", key: "ssh-ed25519 AAAA\nssh-rsa BBBB", wantErr: true},
		{name: "key with double quote", key: `ssh-ed25519 AAAA"x`, wantErr: true},
		{name: "key with backslash", key: `ssh-ed25519 AAAA\x`, wantErr: true},
		{name: "key with tab", key: "ssh-ed25519\tAAAA", wantErr: true},
		{name: "key with non-ascii", key: "ssh-ed25519 AAAÄ", wantErr: true},
		{name: "key with no body", key: "ssh-ed25519", wantErr: true},
		{name: "empty key", key: "", wantErr: true},
		{name: "bad package with bracket", key: testKey, packages: []string{"git]"}, wantErr: true},
		{name: "bad package with comma", key: testKey, packages: []string{"git, curl"}, wantErr: true},
		{name: "bad package uppercase", key: testKey, packages: []string{"Git"}, wantErr: true},
		{name: "empty package", key: testKey, packages: []string{""}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := UserData(tt.key, tt.packages)
			if (err != nil) != tt.wantErr {
				t.Fatalf("UserData() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("UserData() mismatch\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestMetaData(t *testing.T) {
	tests := []struct {
		name     string
		id, host string
		want     string
		wantErr  bool
	}{
		{name: "valid", id: "vm-1_a.b", host: "vm-1", want: "instance-id: vm-1_a.b\nlocal-hostname: vm-1\n"},
		{name: "single char", id: "x", host: "y", want: "instance-id: x\nlocal-hostname: y\n"},
		{name: "empty id", id: "", host: "vm", wantErr: true},
		{name: "empty host", id: "vm", host: "", wantErr: true},
		{name: "space in id", id: "a b", host: "vm", wantErr: true},
		{name: "slash in id", id: "a/b", host: "vm", wantErr: true},
		{name: "colon in host", id: "vm", host: "vm:1", wantErr: true},
		{name: "newline in host", id: "vm", host: "vm\n", wantErr: true},
		{name: "newline injection in id", id: "vm\nruncmd: [x]", host: "vm", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MetaData(tt.id, tt.host)
			if (err != nil) != tt.wantErr {
				t.Fatalf("MetaData() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("MetaData() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Fixtures are shaped like real `qemu-img info --output=json` output.
const (
	infoPlainQcow2 = `{
    "virtual-size": 64424509440,
    "filename": "/state/base.qcow2",
    "cluster-size": 65536,
    "format": "qcow2",
    "actual-size": 2353664,
    "format-specific": {
        "type": "qcow2",
        "data": {"compat": "1.1", "lazy-refcounts": false, "refcount-bits": 16, "corrupt": false, "extended-l2": false}
    },
    "dirty-flag": false
}`

	infoOverlay = `{
    "virtual-size": 64424509440,
    "filename": "/state/agents/a1/disk.qcow2",
    "cluster-size": 65536,
    "format": "qcow2",
    "actual-size": 196624,
    "backing-filename": "/state/base.qcow2",
    "full-backing-filename": "/state/base.qcow2",
    "backing-filename-format": "qcow2",
    "format-specific": {
        "type": "qcow2",
        "data": {"compat": "1.1", "lazy-refcounts": false, "refcount-bits": 16, "corrupt": false, "extended-l2": false}
    },
    "dirty-flag": false
}`

	infoRaw = `{
    "virtual-size": 1073741824,
    "filename": "seed.iso",
    "format": "raw",
    "actual-size": 372736,
    "dirty-flag": false
}`
)

func TestParseImageInfo(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want ImageInfo
	}{
		{
			name: "plain qcow2",
			in:   infoPlainQcow2,
			want: ImageInfo{VirtualSize: 64424509440, ActualSize: 2353664, Format: "qcow2"},
		},
		{
			name: "overlay with backing",
			in:   infoOverlay,
			want: ImageInfo{VirtualSize: 64424509440, ActualSize: 196624, Format: "qcow2", BackingFile: "/state/base.qcow2"},
		},
		{
			name: "raw",
			in:   infoRaw,
			want: ImageInfo{VirtualSize: 1073741824, ActualSize: 372736, Format: "raw"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseImageInfo([]byte(tt.in))
			if err != nil {
				t.Fatalf("ParseImageInfo: %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseImageInfo() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseImageInfoErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "empty input", in: ""},
		{name: "truncated json", in: `{"virtual-size": 10, "format"`},
		{name: "not an object", in: `[]`},
		{name: "missing virtual-size", in: `{"format": "qcow2", "actual-size": 1}`},
		{name: "null virtual-size", in: `{"virtual-size": null, "format": "raw"}`},
		{name: "string virtual-size", in: `{"virtual-size": "big", "format": "raw"}`},
		{name: "negative virtual-size", in: `{"virtual-size": -1, "format": "raw"}`},
		{name: "negative actual-size", in: `{"virtual-size": 1, "actual-size": -5, "format": "raw"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseImageInfo([]byte(tt.in)); err == nil {
				t.Error("ParseImageInfo() succeeded, want error")
			}
		})
	}
}

func TestImgCreateArgs(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		backing string
		sizeGB  int
		want    []string
		wantErr bool
	}{
		{
			name:   "new image",
			path:   "/i/d.qcow2",
			sizeGB: 10,
			want:   []string{"create", "-q", "-f", "qcow2", "/i/d.qcow2", "10G"},
		},
		{
			name:    "new image without size",
			path:    "/i/d.qcow2",
			wantErr: true,
		},
		{
			name:    "overlay without size keeps backing size",
			path:    "/i/o.qcow2",
			backing: "/s/base.qcow2",
			want:    []string{"create", "-q", "-f", "qcow2", "-b", "/s/base.qcow2", "-F", "qcow2", "/i/o.qcow2"},
		},
		{
			name:    "overlay with size",
			path:    "/i/o.qcow2",
			backing: "/s/base.qcow2",
			sizeGB:  60,
			want:    []string{"create", "-q", "-f", "qcow2", "-b", "/s/base.qcow2", "-F", "qcow2", "/i/o.qcow2", "60G"},
		},
		{name: "empty path", path: "", sizeGB: 1, wantErr: true},
		{name: "negative size", path: "/i/d.qcow2", sizeGB: -1, wantErr: true},
		{name: "negative size with backing", path: "/i/o.qcow2", backing: "/s/b", sizeGB: -1, wantErr: true},
		{name: "path starts with dash", path: "-x.qcow2", sizeGB: 1, wantErr: true},
		{name: "backing starts with dash", path: "/i/o.qcow2", backing: "-b", wantErr: true},
		{name: "path with NUL", path: "/i/\x00.qcow2", sizeGB: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ImgCreateArgs(tt.path, tt.backing, tt.sizeGB)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ImgCreateArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !slices.Equal(got, tt.want) {
				t.Errorf("ImgCreateArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestImgResizeArgs(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		sizeGB  int
		want    []string
		wantErr bool
	}{
		{name: "grow", path: "/i/d", sizeGB: 20, want: []string{"resize", "/i/d", "20G"}},
		{name: "minimum size", path: "/i/d", sizeGB: 1, want: []string{"resize", "/i/d", "1G"}},
		{name: "zero size", path: "/i/d", sizeGB: 0, wantErr: true},
		{name: "negative size", path: "/i/d", sizeGB: -3, wantErr: true},
		{name: "empty path", path: "", sizeGB: 5, wantErr: true},
		{name: "path starts with dash", path: "--force", sizeGB: 5, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ImgResizeArgs(tt.path, tt.sizeGB)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ImgResizeArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !slices.Equal(got, tt.want) {
				t.Errorf("ImgResizeArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestImgConvertArgs(t *testing.T) {
	tests := []struct {
		name     string
		src, dst string
		want     []string
		wantErr  bool
	}{
		{
			name: "flatten overlay",
			src:  "/i/o.qcow2",
			dst:  "/i/flat.qcow2",
			want: []string{"convert", "-q", "-O", "qcow2", "/i/o.qcow2", "/i/flat.qcow2"},
		},
		{name: "empty source", src: "", dst: "/i/x", wantErr: true},
		{name: "empty destination", src: "/i/x", dst: "", wantErr: true},
		{name: "same source and destination", src: "/i/x", dst: "/i/x", wantErr: true},
		{name: "source starts with dash", src: "-o", dst: "/i/x", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ImgConvertArgs(tt.src, tt.dst)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ImgConvertArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !slices.Equal(got, tt.want) {
				t.Errorf("ImgConvertArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}
