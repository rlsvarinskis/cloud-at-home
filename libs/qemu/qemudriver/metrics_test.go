package qemudriver

import (
	"context"
	"errors"
	"strings"
	"testing"

	nodev1 "github.com/rlsvarinskis/cloud-at-home/api/gen/homecloud/node/v1"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
)

func TestGetVMMetricsAssemblesResponse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.mkDisk("d1")
	h.mkDisk("d2")
	h.cmd.sizes["disks/d1.qcow2"] = 10 << 30
	h.cmd.sizes["disks/d2.qcow2"] = 20 << 30
	h.start("vm1", "d1", "d2")
	h.bootGuest("vm1")

	resp, err := h.d.GetVMMetrics(ctx, &nodev1.GetVMMetricsRequest{VmId: "vm1", DiskIds: []string{"d1", "d2"}})
	if err != nil {
		t.Fatalf("GetVMMetrics: %v", err)
	}
	if resp.GetCpuCores() != 8 {
		t.Errorf("cpu cores = %d, want 8", resp.GetCpuCores())
	}
	if resp.GetCpuPercent() <= 0 || resp.GetCpuPercent() > 800 {
		t.Errorf("cpu percent = %v, want within 0..800", resp.GetCpuPercent())
	}
	if resp.GetLoad1() != 0.52 || resp.GetLoad15() != 0.59 {
		t.Errorf("load = %v %v", resp.GetLoad1(), resp.GetLoad15())
	}
	if resp.GetMemTotalBytes() != 16318020*1024 || resp.GetMemAvailableBytes() != 11925468*1024 {
		t.Errorf("memory total/available = %d/%d", resp.GetMemTotalBytes(), resp.GetMemAvailableBytes())
	}
	if resp.GetUptimeSeconds() != 123456 {
		t.Errorf("uptime = %d", resp.GetUptimeSeconds())
	}
	if len(resp.GetFilesystems()) != 2 {
		t.Fatalf("filesystems = %d, want 2", len(resp.GetFilesystems()))
	}
	if fs := resp.GetFilesystems()[1]; fs.GetDiskId() != "d2" || fs.GetMountPoint() != "/srv/data" {
		t.Errorf("second filesystem = %v, want disk d2 at /srv/data", fs)
	}
	if len(resp.GetDisks()) != 2 {
		t.Fatalf("disk allocations = %d, want 2", len(resp.GetDisks()))
	}
	if a := resp.GetDisks()[0]; a.GetDiskId() != "d1" || a.GetVirtualSizeBytes() != 10<<30 || a.GetAllocatedBytes() != (10<<30)/4 {
		t.Errorf("allocation d1 = %v", a)
	}
	if got := h.guest("vm1").cmds; !containsPrefix(got, "bash -c '#!/bin/bash") {
		t.Errorf("metrics script was not run as bash -c: %q", got)
	}
}

func containsPrefix(cmds []string, prefix string) bool {
	for _, c := range cmds {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestGetVMMetricsRequiresRunningVM(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	_, err := h.d.GetVMMetrics(context.Background(), &nodev1.GetVMMetricsRequest{VmId: "vm1", DiskIds: []string{"d1"}})
	if !errors.Is(err, errs.ErrFailedPrecondition) {
		t.Errorf("booting VM: err = %v, want precondition", err)
	}
}

func TestGetVMMetricsFailures(t *testing.T) {
	h := newHarness(t)
	h.mkDisk("d1")
	h.start("vm1", "d1")
	h.bootGuest("vm1")
	ctx := context.Background()

	h.guest("vm1").metricsCode = 2
	if _, err := h.d.GetVMMetrics(ctx, &nodev1.GetVMMetricsRequest{VmId: "vm1", DiskIds: []string{"d1"}}); !errors.Is(err, errs.ErrUnavailable) {
		t.Errorf("script exit 2: err = %v, want unavailable", err)
	}
	h.guest("vm1").metricsCode = 0
	h.guest("vm1").metrics = "not the script output"
	if _, err := h.d.GetVMMetrics(ctx, &nodev1.GetVMMetricsRequest{VmId: "vm1", DiskIds: []string{"d1"}}); err == nil {
		t.Error("garbage output: no error")
	}
	h.guest("vm1").metrics = defaultMetricsOutput
	if _, err := h.d.GetVMMetrics(ctx, &nodev1.GetVMMetricsRequest{VmId: "vm1", DiskIds: []string{"bad/id"}}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("bad disk id: err = %v, want invalid", err)
	}
	if _, err := h.d.GetVMMetrics(ctx, &nodev1.GetVMMetricsRequest{VmId: "vm1", DiskIds: []string{"gone"}}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown disk: err = %v, want not found", err)
	}
}

func TestStartJobRequiresRunningVM(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.mkDisk("d1")
	h.start("vm1", "d1")
	job := &nodev1.StartJobRequest{VmId: "vm1", Spec: &nodev1.JobSpec{JobId: "j1", Command: "echo hi", TimeoutSeconds: 5}}
	if _, err := h.d.StartJob(ctx, job); !errors.Is(err, errs.ErrFailedPrecondition) {
		t.Fatalf("StartJob on booting VM: err = %v, want precondition", err)
	}
	h.bootGuest("vm1")
	if _, err := h.d.StartJob(ctx, job); err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if got := h.guest("vm1").cmds; !containsSubstring(got, "setsid") {
		t.Error("job launch script was not sent to the guest")
	}
}

func containsSubstring(cmds []string, sub string) bool {
	for _, c := range cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func TestPollJobStates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.mkDisk("d1")
	h.start("vm1", "d1")
	poll := &nodev1.PollJobRequest{VmId: "vm1", JobId: "j1", MaxOutput: 100}

	resp, err := h.d.PollJob(ctx, poll)
	if err != nil || resp.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_MISSING {
		t.Fatalf("poll on booting VM = %v, %v; want MISSING", resp, err)
	}

	h.bootGuest("vm1")
	g := h.guest("vm1")
	g.pollOut = "missing\n"
	if resp, _ := h.d.PollJob(ctx, poll); resp.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_MISSING {
		t.Errorf("unknown job: state = %v, want MISSING", resp.GetState())
	}
	g.pollOut = "running - 0 5 0 0 0 0\n\n\n"
	if resp, _ := h.d.PollJob(ctx, poll); resp.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_RUNNING || resp.GetElapsedSeconds() != 5 {
		t.Errorf("running job: %v", resp)
	}
	// exited 7, two bytes of stdout ("hi\n" in base64)
	g.pollOut = "exited 7 0 2 3 0 0 0\naGkK\n\n"
	resp, _ = h.d.PollJob(ctx, poll)
	if resp.GetState() != nodev1.GuestJobState_GUEST_JOB_STATE_EXITED || resp.GetExitCode() != 7 || string(resp.GetStdout()) != "hi\n" {
		t.Errorf("exited job: %v", resp)
	}
	if _, err := h.d.PollJob(ctx, &nodev1.PollJobRequest{VmId: "vm1", JobId: "j1", StdoutOffset: -1}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("negative offset: err = %v, want invalid", err)
	}
}

func TestCancelJobIsNoOpWhenVMNotRunning(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.mkDisk("d1")
	h.start("vm1", "d1")
	if _, err := h.d.CancelJob(ctx, &nodev1.CancelJobRequest{VmId: "vm1", JobId: "j1"}); err != nil {
		t.Fatalf("cancel on booting VM: %v", err)
	}
	if got := h.guest("vm1").cmds; len(got) != 0 {
		// The probe is the only command a booting VM may see.
		for _, c := range got {
			if c != "true" {
				t.Errorf("cancel sent %q to a booting VM", c)
			}
		}
	}
	h.bootGuest("vm1")
	if _, err := h.d.CancelJob(ctx, &nodev1.CancelJobRequest{VmId: "vm1", JobId: "j1"}); err != nil {
		t.Fatalf("cancel on running VM: %v", err)
	}
	if !containsSubstring(h.guest("vm1").cmds, "pkill") {
		t.Error("cancel on a running VM did not run the cancel script")
	}
}

func TestPutAndGetFile(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.mkDisk("d1")
	h.start("vm1", "d1")
	put := &nodev1.PutFileRequest{VmId: "vm1", Path: "/home/agent/x.txt", Content: []byte("data"), Mode: 0o600}
	if _, err := h.d.PutFile(ctx, put); !errors.Is(err, errs.ErrFailedPrecondition) {
		t.Fatalf("PutFile on booting VM: err = %v, want precondition", err)
	}
	h.bootGuest("vm1")
	if _, err := h.d.PutFile(ctx, put); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if got := string(h.guest("vm1").lastPut); got != "data" {
		t.Errorf("guest received %q, want data", got)
	}

	g := h.guest("vm1")
	g.getOut = "hello"
	resp, err := h.d.GetFile(ctx, &nodev1.GetFileRequest{VmId: "vm1", Path: "/home/agent/x.txt"})
	if err != nil || string(resp.GetContent()) != "hello" || resp.GetTruncated() {
		t.Errorf("GetFile = %v, %v", resp, err)
	}
	g.getCode = 3
	if _, err := h.d.GetFile(ctx, &nodev1.GetFileRequest{VmId: "vm1", Path: "/nope"}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing guest file: err = %v, want not found", err)
	}
	if _, err := h.d.PutFile(ctx, &nodev1.PutFileRequest{VmId: "vm1"}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("empty path: err = %v, want invalid", err)
	}
}
