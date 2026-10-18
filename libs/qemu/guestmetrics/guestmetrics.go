// Package guestmetrics collects CPU, memory, load, uptime and disk usage from
// inside a Linux guest.
//
// Script is a bash program that the host runs in the guest with `bash -c`.
// Parse turns its marker-delimited output into a Sample.
package guestmetrics

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Script is the bash program to run inside the guest. It prints the sections
// listed in sectionOrder, each introduced by a ===NAME=== marker line. The
// script tolerates a missing df: the DF marker is printed regardless.
const Script = `#!/bin/bash
echo ===STAT1===
head -1 /proc/stat
echo ===STAT2===
sleep 1
head -1 /proc/stat
echo ===MEMINFO===
cat /proc/meminfo
echo ===LOADAVG===
cat /proc/loadavg
echo ===UPTIME===
cat /proc/uptime
echo ===NPROC===
nproc
echo ===DF===
df -B1 --output=source,size,used,target -x tmpfs -x devtmpfs -x squashfs -x overlay 2>/dev/null || true
echo ===END===
`

// Filesystem describes one block-device filesystem in the guest.
type Filesystem struct {
	// Device is the block device path, for example /dev/vda1.
	Device string
	// MountPoint is the directory the device is mounted on. It may contain spaces.
	MountPoint string
	// SizeBytes is the total size of the filesystem in bytes.
	SizeBytes int64
	// UsedBytes is the space used on the filesystem in bytes.
	UsedBytes int64
	// DiskID is the host-side identifier of the disk backing Device, or empty
	// when it cannot be determined.
	DiskID string
}

// Sample is one set of usage numbers read from a guest.
type Sample struct {
	// CPUPercent is CPU utilisation in the style of top: 100 per fully busy vCPU, so a guest with
	// 8 vCPUs can reach 800. The range is 0 to 100*CPUCores.
	CPUPercent float64
	// CPUCores is the number of online vCPUs.
	CPUCores int
	// Load1, Load5 and Load15 are the 1, 5 and 15 minute load averages.
	Load1, Load5, Load15 float64
	// MemTotal and MemAvailable are memory figures in bytes. MemUsed is
	// MemTotal minus MemAvailable, clamped at zero.
	MemTotal, MemUsed, MemAvailable int64
	// SwapTotal and SwapUsed are swap figures in bytes. SwapUsed is SwapTotal
	// minus SwapFree, clamped at zero.
	SwapTotal, SwapUsed int64
	// UptimeSeconds is the guest uptime truncated to whole seconds.
	UptimeSeconds int64
	// Filesystems lists the /dev/ filesystems, without duplicate
	// (device, mount point) pairs.
	Filesystems []Filesystem
}

// sectionOrder lists the marker names in the order Script prints them.
var sectionOrder = []string{"STAT1", "STAT2", "MEMINFO", "LOADAVG", "UPTIME", "NPROC", "DF", "END"}

// Indexes of the /proc/stat counters used for CPU accounting.
const (
	cpuCounted     = 8 // user nice system idle iowait irq softirq steal
	cpuIdleField   = 3
	cpuIOWaitField = 4
	cpuMinCounters = 4
)

// memKeys are the /proc/meminfo keys this package reads.
var memKeys = map[string]bool{
	"MemTotal":     true,
	"MemAvailable": true,
	"SwapTotal":    true,
	"SwapFree":     true,
}

// Parse converts the output of Script into a Sample.
//
// diskIDs maps a block device's letter index to a host-side disk identifier:
// /dev/vda1 and /dev/vda select diskIDs[0], /dev/vdb1 selects diskIDs[1], and
// so on. A device whose index is out of range gets an empty DiskID.
//
// Parse returns an error naming the offending section when a section marker is
// missing, unknown, duplicated or out of order, or when a value in a section is
// missing or malformed.
func Parse(output string, diskIDs []string) (Sample, error) {
	secs, err := splitSections(output)
	if err != nil {
		return Sample{}, err
	}

	var s Sample
	if err := applyCores(&s, secs["NPROC"]); err != nil {
		return Sample{}, err
	}
	if err := applyCPU(&s, secs["STAT1"], secs["STAT2"]); err != nil {
		return Sample{}, err
	}
	if err := applyMemory(&s, secs["MEMINFO"]); err != nil {
		return Sample{}, err
	}
	if err := applyLoadavg(&s, secs["LOADAVG"]); err != nil {
		return Sample{}, err
	}
	if err := applyUptime(&s, secs["UPTIME"]); err != nil {
		return Sample{}, err
	}
	fss, err := parseDF(secs["DF"], diskIDs)
	if err != nil {
		return Sample{}, err
	}
	s.Filesystems = fss
	return s, nil
}

// sectionErr builds an error that names the section it came from.
func sectionErr(section, msg string) error {
	return fmt.Errorf("guestmetrics: section %s: %s", section, msg)
}

// splitSections groups the lines of output by the marker that precedes them.
// It checks that every section is present exactly once and in sectionOrder.
func splitSections(output string) (map[string][]string, error) {
	known := make(map[string]bool, len(sectionOrder))
	for _, name := range sectionOrder {
		known[name] = true
	}

	secs := make(map[string][]string, len(sectionOrder))
	var order []string
	current := ""
	for i, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > 6 && strings.HasPrefix(trimmed, "===") && strings.HasSuffix(trimmed, "===") {
			name := trimmed[3 : len(trimmed)-3]
			if !known[name] {
				return nil, fmt.Errorf("guestmetrics: unknown section marker %s on line %d", name, i+1)
			}
			if _, dup := secs[name]; dup {
				return nil, fmt.Errorf("guestmetrics: duplicate section %s", name)
			}
			secs[name] = nil
			order = append(order, name)
			current = name
			continue
		}
		if current == "" {
			if trimmed != "" {
				return nil, fmt.Errorf("guestmetrics: unexpected data before first section marker (expected ===%s===)", sectionOrder[0])
			}
			continue
		}
		secs[current] = append(secs[current], line)
	}

	for _, name := range sectionOrder {
		if _, ok := secs[name]; !ok {
			return nil, fmt.Errorf("guestmetrics: missing section %s", name)
		}
	}
	for i, name := range order {
		if name != sectionOrder[i] {
			return nil, fmt.Errorf("guestmetrics: section %s out of order (expected %s)", name, sectionOrder[i])
		}
	}
	return secs, nil
}

// firstNonBlank returns the first line that is not all whitespace.
func firstNonBlank(lines []string) (string, bool) {
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			return line, true
		}
	}
	return "", false
}

// parseCount parses a non-negative integer value from the named section.
func parseCount(section, what, s string) (int64, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, sectionErr(section, fmt.Sprintf("%s is not an integer: %q", what, s))
	}
	if v < 0 {
		return 0, sectionErr(section, fmt.Sprintf("%s is negative: %d", what, v))
	}
	return v, nil
}

// parseFloat parses a finite floating-point value from the named section.
func parseFloat(section, what, s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, sectionErr(section, fmt.Sprintf("%s is not a finite number: %q", what, s))
	}
	return v, nil
}

// cpuTimes holds the aggregate idle and total jiffy counts from a cpu line.
type cpuTimes struct {
	idle, total int64
}

// parseCPUTimes reads the aggregate "cpu" line of a /proc/stat snapshot.
// Lines with fewer than cpuMinCounters counters are rejected; missing trailing
// counters count as zero.
func parseCPUTimes(section string, lines []string) (cpuTimes, error) {
	line, ok := firstNonBlank(lines)
	if !ok {
		return cpuTimes{}, sectionErr(section, "no data")
	}
	fields := strings.Fields(line)
	if fields[0] != "cpu" {
		return cpuTimes{}, sectionErr(section, "first line is not an aggregate cpu line")
	}
	vals := fields[1:]
	if len(vals) < cpuMinCounters {
		return cpuTimes{}, sectionErr(section, fmt.Sprintf("cpu line has %d counters, need at least %d", len(vals), cpuMinCounters))
	}
	if len(vals) > cpuCounted {
		vals = vals[:cpuCounted]
	}
	var t cpuTimes
	for i, f := range vals {
		v, err := parseCount(section, fmt.Sprintf("cpu counter %d", i+1), f)
		if err != nil {
			return cpuTimes{}, err
		}
		t.total += v
		if i == cpuIdleField || i == cpuIOWaitField {
			t.idle += v
		}
	}
	return t, nil
}

// applyCPU sets CPUPercent from two /proc/stat snapshots taken one second apart. s.CPUCores must be set.
// The busy fraction of the whole guest is scaled by the core count (100 per busy vCPU, like top).
func applyCPU(s *Sample, stat1, stat2 []string) error {
	a, err := parseCPUTimes("STAT1", stat1)
	if err != nil {
		return err
	}
	b, err := parseCPUTimes("STAT2", stat2)
	if err != nil {
		return err
	}
	dTotal := b.total - a.total
	if dTotal <= 0 {
		s.CPUPercent = 0
		return nil
	}
	dIdle := b.idle - a.idle
	pct := 100 * float64(s.CPUCores) * (1 - float64(dIdle)/float64(dTotal))
	s.CPUPercent = math.Min(100*float64(s.CPUCores), math.Max(0, pct))
	return nil
}

// applyCores sets CPUCores from the output of nproc.
func applyCores(s *Sample, lines []string) error {
	line, ok := firstNonBlank(lines)
	if !ok {
		return sectionErr("NPROC", "no value")
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		return sectionErr("NPROC", fmt.Sprintf("not an integer: %q", strings.TrimSpace(line)))
	}
	if n < 1 {
		return sectionErr("NPROC", fmt.Sprintf("must be a positive integer, got %d", n))
	}
	s.CPUCores = n
	return nil
}

// applyMemory sets the memory and swap fields from /proc/meminfo. Values are
// reported in kB and converted to bytes. MemTotal and MemAvailable are
// required. SwapTotal and SwapFree are optional, but must be given together.
func applyMemory(s *Sample, lines []string) error {
	const section = "MEMINFO"
	vals := make(map[string]int64, len(memKeys))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		if !memKeys[key] {
			continue
		}
		if len(fields) < 2 {
			return sectionErr(section, key+" has no value")
		}
		v, err := parseCount(section, key, fields[1])
		if err != nil {
			return err
		}
		vals[key] = v * 1024
	}

	total, ok := vals["MemTotal"]
	if !ok {
		return sectionErr(section, "missing MemTotal")
	}
	avail, ok := vals["MemAvailable"]
	if !ok {
		return sectionErr(section, "missing MemAvailable")
	}
	s.MemTotal = total
	s.MemAvailable = avail
	s.MemUsed = max(total-avail, 0)

	swapTotal, hasTotal := vals["SwapTotal"]
	swapFree, hasFree := vals["SwapFree"]
	switch {
	case hasTotal && hasFree:
		s.SwapTotal = swapTotal
		s.SwapUsed = max(swapTotal-swapFree, 0)
	case hasTotal || hasFree:
		return sectionErr(section, "SwapTotal and SwapFree must both be present or both absent")
	}
	return nil
}

// applyLoadavg sets the three load averages from /proc/loadavg.
func applyLoadavg(s *Sample, lines []string) error {
	const section = "LOADAVG"
	line, ok := firstNonBlank(lines)
	if !ok {
		return sectionErr(section, "no data")
	}
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return sectionErr(section, fmt.Sprintf("need 3 load averages, got %d fields", len(fields)))
	}
	var err error
	if s.Load1, err = parseFloat(section, "load1", fields[0]); err != nil {
		return err
	}
	if s.Load5, err = parseFloat(section, "load5", fields[1]); err != nil {
		return err
	}
	if s.Load15, err = parseFloat(section, "load15", fields[2]); err != nil {
		return err
	}
	return nil
}

// applyUptime sets UptimeSeconds from the first field of /proc/uptime,
// truncated to whole seconds.
func applyUptime(s *Sample, lines []string) error {
	const section = "UPTIME"
	line, ok := firstNonBlank(lines)
	if !ok {
		return sectionErr(section, "no data")
	}
	fields := strings.Fields(line)
	v, err := parseFloat(section, "uptime", fields[0])
	if err != nil {
		return err
	}
	if v < 0 {
		return sectionErr(section, fmt.Sprintf("uptime is negative: %v", v))
	}
	s.UptimeSeconds = int64(math.Trunc(v))
	return nil
}

// parseDF reads the output of df -B1 --output=source,size,used,target. The
// first non-blank line is a header and is skipped. Rows whose source does not
// start with /dev/ are ignored. Mount points may contain spaces, so every field
// after the third is joined back together. Repeated (device, mount point)
// pairs are kept only once.
func parseDF(lines []string, diskIDs []string) ([]Filesystem, error) {
	const section = "DF"
	var fss []Filesystem
	seen := make(map[[2]string]bool)
	header := true
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if header {
			header = false
			continue
		}
		if !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}
		if len(fields) < 4 {
			return nil, sectionErr(section, fmt.Sprintf("row %q has %d fields, need at least 4", line, len(fields)))
		}
		size, err := parseCount(section, "size", fields[1])
		if err != nil {
			return nil, err
		}
		used, err := parseCount(section, "used", fields[2])
		if err != nil {
			return nil, err
		}
		mount := strings.Join(fields[3:], " ")
		key := [2]string{fields[0], mount}
		if seen[key] {
			continue
		}
		seen[key] = true
		fss = append(fss, Filesystem{
			Device:     fields[0],
			MountPoint: mount,
			SizeBytes:  size,
			UsedBytes:  used,
			DiskID:     diskIDFor(fields[0], diskIDs),
		})
	}
	return fss, nil
}

// diskIDFor returns the host-side disk identifier for a /dev/ device path, or
// "" when no identifier applies.
//
// The name after "/dev/" must be a vd, sd or xvd device. Trailing digits (the
// partition number) are removed, and the single letter that remains selects
// diskIDs by index (a=0, b=1, ...). Names such as nvme0n1p1 do not match any
// of these prefixes and their base name ends in a digit, so they yield "".
func diskIDFor(device string, diskIDs []string) string {
	name, ok := strings.CutPrefix(device, "/dev/")
	if !ok {
		return ""
	}
	prefix := ""
	for _, p := range []string{"xvd", "vd", "sd"} {
		if strings.HasPrefix(name, p) {
			prefix = p
			break
		}
	}
	if prefix == "" {
		return ""
	}
	base := strings.TrimRight(name, "0123456789")
	if len(base) != len(prefix)+1 || !strings.HasPrefix(base, prefix) {
		return ""
	}
	letter := base[len(base)-1]
	if letter < 'a' || letter > 'z' {
		return ""
	}
	idx := int(letter - 'a')
	if idx >= len(diskIDs) {
		return ""
	}
	return diskIDs[idx]
}
