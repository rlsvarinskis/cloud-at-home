package guestmetrics

import (
	"math"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// fixture holds the section bodies Script would print for one guest.
type fixture struct {
	stat1, stat2, meminfo, loadavg, uptime, nproc, df string
}

// defaultFixture returns realistic output for an 8 vCPU guest with two
// virtio disks.
func defaultFixture() fixture {
	return fixture{
		stat1: "cpu  840102 510 493115 89766099 512124 0 64833 0 0 0",
		stat2: "cpu  840602 510 493215 89766999 512124 0 64853 0 0 0",
		meminfo: `MemTotal:       16318020 kB
MemFree:         2214448 kB
MemAvailable:   11925468 kB
Buffers:          512340 kB
Cached:          8756440 kB
SwapCached:            0 kB
SwapTotal:       2097148 kB
SwapFree:        2097148 kB
`,
		loadavg: "0.52 0.58 0.59 2/345 12345",
		uptime:  "123456.78 987654.32",
		nproc:   "8",
		df: `Filesystem        1B-blocks        Used Mounted on
/dev/vda1      10726932480  4294967296 /
/dev/vdb       21474836480  1073741824 /srv/data
`,
	}
}

// bodies maps each section name to its body.
func (f fixture) bodies() map[string]string {
	return map[string]string{
		"STAT1":   f.stat1,
		"STAT2":   f.stat2,
		"MEMINFO": f.meminfo,
		"LOADAVG": f.loadavg,
		"UPTIME":  f.uptime,
		"NPROC":   f.nproc,
		"DF":      f.df,
		"END":     "",
	}
}

// render prints the sections in the given order with their markers.
func render(bodies map[string]string, order []string) string {
	var b strings.Builder
	for _, name := range order {
		b.WriteString("===" + name + "===\n")
		if body := strings.TrimRight(bodies[name], "\n"); body != "" {
			b.WriteString(body + "\n")
		}
	}
	return b.String()
}

// output renders the fixture in Script's section order.
func (f fixture) output() string {
	return render(f.bodies(), sectionOrder)
}

// defaultDiskIDs is the disk ID list used by the default test cases.
var defaultDiskIDs = []string{"id-a", "id-b"}

// defaultSample is the Sample that defaultFixture parses to with defaultDiskIDs.
func defaultSample() Sample {
	return Sample{
		CPUPercent:    326.3157894737, // 40.79% of an 8-vCPU guest, top-style
		CPUCores:      8,
		Load1:         0.52,
		Load5:         0.58,
		Load15:        0.59,
		MemTotal:      16318020 * 1024,
		MemUsed:       (16318020 - 11925468) * 1024,
		MemAvailable:  11925468 * 1024,
		SwapTotal:     2097148 * 1024,
		SwapUsed:      0,
		UptimeSeconds: 123456,
		Filesystems: []Filesystem{
			{Device: "/dev/vda1", MountPoint: "/", SizeBytes: 10726932480, UsedBytes: 4294967296, DiskID: "id-a"},
			{Device: "/dev/vdb", MountPoint: "/srv/data", SizeBytes: 21474836480, UsedBytes: 1073741824, DiskID: "id-b"},
		},
	}
}

// dropLines removes every line of body that starts with one of prefixes.
func dropLines(body string, prefixes ...string) string {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		drop := false
		for _, p := range prefixes {
			if strings.HasPrefix(line, p) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// setMemValue rewrites the meminfo line for key to report value kB.
func setMemValue(body, key, value string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, key+":") {
			lines[i] = key + ": " + value + " kB"
		}
	}
	return strings.Join(lines, "\n")
}

const dfHeader = "Filesystem 1B-blocks Used Mounted on\n"

// parseCase is one row of TestParse. mutate edits the fixture and the expected
// Sample. A nil diskIDs means defaultDiskIDs. A non-empty wantErr means Parse
// must fail with an error containing it.
type parseCase struct {
	name    string
	mutate  func(f *fixture, want *Sample)
	diskIDs []string
	wantErr string
}

func TestParse(t *testing.T) {
	cases := []parseCase{
		{name: "normal sample"},
		{
			name: "idle CPU is zero percent",
			mutate: func(f *fixture, w *Sample) {
				f.stat2 = "cpu  840102 510 493115 89767099 512124 0 64833 0 0 0"
				w.CPUPercent = 0
			},
		},
		{
			name: "fully busy 8-vCPU guest is 800 percent",
			mutate: func(f *fixture, w *Sample) {
				f.stat2 = "cpu  841102 510 493115 89766099 512124 0 64833 0 0 0"
				w.CPUPercent = 800
			},
		},
		{
			name: "zero CPU delta",
			mutate: func(f *fixture, w *Sample) {
				f.stat2 = f.stat1
				w.CPUPercent = 0
			},
		},
		{
			name: "CPU counters went backwards",
			mutate: func(f *fixture, w *Sample) {
				f.stat2 = "cpu  1 0 1 1 0 0 0 0 0 0"
				w.CPUPercent = 0
			},
		},
		{
			name: "older kernel with four CPU fields",
			mutate: func(f *fixture, w *Sample) {
				f.stat1 = "cpu  100 0 100 1000"
				f.stat2 = "cpu  200 0 100 1100"
				w.CPUPercent = 400 // half of 8 vCPUs busy
			},
		},
		{
			name: "CPU line with too few fields",
			mutate: func(f *fixture, w *Sample) {
				f.stat1 = "cpu  100 0 100"
			},
			wantErr: "STAT1",
		},
		{
			name: "negative CPU counter",
			mutate: func(f *fixture, w *Sample) {
				f.stat1 = "cpu  -840102 510 493115 89766099 512124 0 64833 0 0 0"
			},
			wantErr: "STAT1",
		},
		{
			name: "non-numeric CPU counter in second snapshot",
			mutate: func(f *fixture, w *Sample) {
				f.stat2 = "cpu  840602 x 493215 89766999 512124 0 64853 0 0 0"
			},
			wantErr: "STAT2",
		},
		{
			name: "missing MemAvailable",
			mutate: func(f *fixture, w *Sample) {
				f.meminfo = dropLines(f.meminfo, "MemAvailable:")
			},
			wantErr: "MemAvailable",
		},
		{
			name: "swap absent",
			mutate: func(f *fixture, w *Sample) {
				f.meminfo = dropLines(f.meminfo, "SwapTotal:", "SwapFree:")
				w.SwapTotal = 0
				w.SwapUsed = 0
			},
		},
		{
			name: "swap partly in use",
			mutate: func(f *fixture, w *Sample) {
				f.meminfo = setMemValue(f.meminfo, "SwapFree", "1048576")
				w.SwapUsed = (2097148 - 1048576) * 1024
			},
		},
		{
			name: "SwapTotal without SwapFree",
			mutate: func(f *fixture, w *Sample) {
				f.meminfo = dropLines(f.meminfo, "SwapFree:")
			},
			wantErr: "SwapFree",
		},
		{
			name: "mount point containing spaces",
			mutate: func(f *fixture, w *Sample) {
				f.df = dfHeader + "/dev/vdb1 21474836480 1073741824 /mnt/my data disk\n"
				w.Filesystems = []Filesystem{
					{Device: "/dev/vdb1", MountPoint: "/mnt/my data disk", SizeBytes: 21474836480, UsedBytes: 1073741824, DiskID: "id-b"},
				}
			},
		},
		{
			name:    "diskIDs shorter than devices",
			diskIDs: []string{"id-a"},
			mutate: func(f *fixture, w *Sample) {
				w.Filesystems[1].DiskID = ""
			},
		},
		{
			name:    "diskIDs empty",
			diskIDs: []string{},
			mutate: func(f *fixture, w *Sample) {
				w.Filesystems[0].DiskID = ""
				w.Filesystems[1].DiskID = ""
			},
		},
		{
			name: "duplicate device and mount rows ignored",
			mutate: func(f *fixture, w *Sample) {
				f.df = dfHeader +
					"/dev/vda1 10726932480 4294967296 /\n" +
					"/dev/vda1 10726932480 4294967296 /\n" +
					"/dev/vda1 10726932480 4294967296 /mnt/x\n" +
					"/dev/vdb 21474836480 1073741824 /srv/data\n"
				w.Filesystems = []Filesystem{
					{Device: "/dev/vda1", MountPoint: "/", SizeBytes: 10726932480, UsedBytes: 4294967296, DiskID: "id-a"},
					{Device: "/dev/vda1", MountPoint: "/mnt/x", SizeBytes: 10726932480, UsedBytes: 4294967296, DiskID: "id-a"},
					{Device: "/dev/vdb", MountPoint: "/srv/data", SizeBytes: 21474836480, UsedBytes: 1073741824, DiskID: "id-b"},
				}
			},
		},
		{
			name: "non-/dev sources ignored",
			mutate: func(f *fixture, w *Sample) {
				f.df += "tmpfs 8163008512 0 /run\n" +
					"overlay 100 50 /var/lib/docker\n" +
					"server:/export 100 50 /mnt/nfs\n"
			},
		},
		{
			name: "empty DF section",
			mutate: func(f *fixture, w *Sample) {
				f.df = ""
				w.Filesystems = nil
			},
		},
		{
			name: "DF header only",
			mutate: func(f *fixture, w *Sample) {
				f.df = dfHeader
				w.Filesystems = nil
			},
		},
		{
			name: "mapper device kept without disk ID",
			mutate: func(f *fixture, w *Sample) {
				f.df = dfHeader + "/dev/mapper/vg-root 10726932480 4294967296 /\n"
				w.Filesystems = []Filesystem{
					{Device: "/dev/mapper/vg-root", MountPoint: "/", SizeBytes: 10726932480, UsedBytes: 4294967296},
				}
			},
		},
		{
			name: "non-numeric MemTotal",
			mutate: func(f *fixture, w *Sample) {
				f.meminfo = setMemValue(f.meminfo, "MemTotal", "lots")
			},
			wantErr: "MEMINFO",
		},
		{
			name: "meminfo value missing",
			mutate: func(f *fixture, w *Sample) {
				f.meminfo = setMemValue(f.meminfo, "MemAvailable", "")
			},
			wantErr: "MEMINFO",
		},
		{
			name: "non-numeric nproc",
			mutate: func(f *fixture, w *Sample) {
				f.nproc = "many"
			},
			wantErr: "NPROC",
		},
		{
			name: "zero nproc",
			mutate: func(f *fixture, w *Sample) {
				f.nproc = "0"
			},
			wantErr: "NPROC",
		},
		{
			name: "non-numeric load average",
			mutate: func(f *fixture, w *Sample) {
				f.loadavg = "0.52 x 0.59 2/345 12345"
			},
			wantErr: "LOADAVG",
		},
		{
			name: "load average with too few fields",
			mutate: func(f *fixture, w *Sample) {
				f.loadavg = "0.52 0.58"
			},
			wantErr: "LOADAVG",
		},
		{
			name: "NaN load average",
			mutate: func(f *fixture, w *Sample) {
				f.loadavg = "NaN 0.58 0.59 1/1 1"
			},
			wantErr: "LOADAVG",
		},
		{
			name: "non-numeric uptime",
			mutate: func(f *fixture, w *Sample) {
				f.uptime = "soon 1"
			},
			wantErr: "UPTIME",
		},
		{
			name: "infinite uptime",
			mutate: func(f *fixture, w *Sample) {
				f.uptime = "Inf 1"
			},
			wantErr: "UPTIME",
		},
		{
			name: "non-numeric DF size",
			mutate: func(f *fixture, w *Sample) {
				f.df = dfHeader + "/dev/vda1 big 4294967296 /\n"
			},
			wantErr: "DF",
		},
		{
			name: "DF row with too few fields",
			mutate: func(f *fixture, w *Sample) {
				f.df = dfHeader + "/dev/vda1 100 50\n"
			},
			wantErr: "DF",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := defaultFixture()
			want := defaultSample()
			if tc.mutate != nil {
				tc.mutate(&f, &want)
			}
			diskIDs := tc.diskIDs
			if diskIDs == nil {
				diskIDs = defaultDiskIDs
			}

			got, err := Parse(f.output(), diskIDs)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Parse() succeeded with %+v, want error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Parse() error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() unexpected error: %v", err)
			}
			if math.Abs(got.CPUPercent-want.CPUPercent) > 1e-5 {
				t.Errorf("CPUPercent = %v, want %v", got.CPUPercent, want.CPUPercent)
			}
			got.CPUPercent, want.CPUPercent = 0, 0
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Parse() =\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

// TestParseStructure covers malformed section layouts.
func TestParseStructure(t *testing.T) {
	full := defaultFixture().bodies()
	cases := []struct {
		name    string
		output  string
		wantErr string
	}{
		{name: "empty output", output: "", wantErr: "STAT1"},
		{
			name:    "data before first marker",
			output:  "hello\n" + render(full, sectionOrder),
			wantErr: "STAT1",
		},
		{
			name:    "unknown marker",
			output:  "===BOGUS===\n" + render(full, sectionOrder),
			wantErr: "BOGUS",
		},
		{
			name:    "duplicate marker",
			output:  render(full, sectionOrder) + "===STAT1===\n",
			wantErr: "duplicate section STAT1",
		},
		{
			name:    "sections out of order",
			output:  render(full, []string{"STAT1", "STAT2", "LOADAVG", "MEMINFO", "UPTIME", "NPROC", "DF", "END"}),
			wantErr: "out of order",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.output, defaultDiskIDs)
			if err == nil {
				t.Fatalf("Parse() succeeded, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Parse() error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestParseTruncated checks that every section is reported when it is cut off
// and when only its marker line is lost.
func TestParseTruncated(t *testing.T) {
	full := render(defaultFixture().bodies(), sectionOrder)
	for _, name := range sectionOrder {
		marker := "===" + name + "==="
		idx := strings.Index(full, marker)
		if idx < 0 {
			t.Fatalf("fixture has no marker %s", marker)
		}

		t.Run("cut before "+name, func(t *testing.T) {
			_, err := Parse(full[:idx], defaultDiskIDs)
			if err == nil || !strings.Contains(err.Error(), "section "+name) {
				t.Fatalf("Parse() error = %v, want it to name section %s", err, name)
			}
		})

		t.Run("marker "+name+" removed", func(t *testing.T) {
			removed := strings.Replace(full, marker+"\n", "", 1)
			_, err := Parse(removed, defaultDiskIDs)
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("Parse() error = %v, want it to name section %s", err, name)
			}
		})
	}
}

// TestDiskIDFor checks device name to disk ID attribution.
func TestDiskIDFor(t *testing.T) {
	ids := []string{"id-a", "id-b", "id-c"}
	cases := []struct {
		device string
		want   string
	}{
		{"/dev/vda", "id-a"},
		{"/dev/vda1", "id-a"},
		{"/dev/vdb2", "id-b"},
		{"/dev/vdc", "id-c"},
		{"/dev/vdd1", ""},
		{"/dev/sda1", "id-a"},
		{"/dev/sdb", "id-b"},
		{"/dev/xvdb", "id-b"},
		{"/dev/xvdb12", "id-b"},
		{"/dev/nvme0n1", ""},
		{"/dev/nvme0n1p1", ""},
		{"/dev/vd", ""},
		{"/dev/sd1", ""},
		{"/dev/sdaa", ""},
		{"/dev/sdA", ""},
		{"/dev/mapper/vg-root", ""},
		{"/dev/loop0", ""},
		{"vda1", ""},
	}
	for _, tc := range cases {
		if got := diskIDFor(tc.device, ids); got != tc.want {
			t.Errorf("diskIDFor(%q) = %q, want %q", tc.device, got, tc.want)
		}
	}
}

// TestScriptParses runs Script under bash on this machine and parses the result.
func TestScriptParses(t *testing.T) {
	if _, err := os.Stat("/proc/stat"); err != nil {
		t.Skipf("/proc/stat unavailable: %v", err)
	}
	out, err := exec.Command("bash", "-c", Script).Output()
	if err != nil {
		t.Fatalf("running Script: %v", err)
	}
	s, err := Parse(string(out), nil)
	if err != nil {
		t.Fatalf("Parse(Script output) error: %v\noutput:\n%s", err, out)
	}
	if s.CPUCores < 1 {
		t.Errorf("CPUCores = %d, want >= 1", s.CPUCores)
	}
	if s.MemTotal <= 0 {
		t.Errorf("MemTotal = %d, want > 0", s.MemTotal)
	}
}

// A guest whose counters go backwards (migration, CPU hotplug, a wrapped counter) must never
// produce a figure outside 0..100*cores.
func TestParseClampsCPUPercent(t *testing.T) {
	cases := []struct {
		name, stat1, stat2 string
		want               float64
	}{
		{"idle counter went backwards", "cpu  1000 0 1000 5000 0 0 0 0", "cpu  3000 0 1000 4000 0 0 0 0", 800},
		{"busy counter went backwards", "cpu  1000 0 0 1000 0 0 0 0", "cpu  900 0 0 1200 0 0 0 0", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := defaultFixture() // nproc = 8
			f.stat1, f.stat2 = c.stat1, c.stat2
			got, err := Parse(f.output(), defaultDiskIDs)
			if err != nil {
				t.Fatal(err)
			}
			if got.CPUPercent != c.want {
				t.Fatalf("CPUPercent = %v, want %v", got.CPUPercent, c.want)
			}
		})
	}
}

// CPU is scaled by the core count: the same busy fraction reads 100 on one vCPU and 400 on four.
func TestParseCPUPercentScalesWithCores(t *testing.T) {
	for _, cores := range []string{"1", "4", "32"} {
		f := defaultFixture()
		f.nproc = cores
		f.stat1 = "cpu  0 0 0 0 0 0 0 0"
		f.stat2 = "cpu  500 0 0 500 0 0 0 0" // half of the guest busy
		got, err := Parse(f.output(), defaultDiskIDs)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := strconv.Atoi(cores)
		if want := 50 * float64(n); math.Abs(got.CPUPercent-want) > 1e-9 {
			t.Errorf("%s cores: CPUPercent = %v, want %v", cores, got.CPUPercent, want)
		}
	}
}
