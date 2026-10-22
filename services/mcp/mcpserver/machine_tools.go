package mcpserver

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/jsonschema-go/jsonschema"

	agentdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/agentd/v1"
	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
)

// Machine limits and defaults. A machine is a VM whose name is the tool's name argument.
const (
	machineMaxCPUs  = 32
	machineMaxMemGB = 56
	// machineMaxDiskGB is vmd's default limit (MaxDiskSizeGB).
	machineMaxDiskGB = 4096
	machineDefCPUs   = 1
	machineDefMemGB  = 2
	machineDefDisk   = 40
	// machinePageSize is the page size used when listing VMs, images, agents, tasks and jobs.
	machinePageSize = 200
	// waitReserve is kept back from the MaxWait budget so the tool returns before the client gives up.
	waitReserve = 5 * time.Second
)

// machineNameRE is the machine name rule: vmd's rule (a letter first, a letter or digit last) with a 31 character
// maximum, so every name this server accepts is also a valid vmd name.
var machineNameRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,29}[a-z0-9])?$`)

// machineNameRule is the rule in words, for error messages and schema descriptions.
const machineNameRule = "1-31 lowercase letters, digits or hyphens, starting with a letter and not ending with a hyphen"

// checkMachineName rejects names that vmd would reject. The schema pattern catches them first; this is the same rule
// for direct calls.
func checkMachineName(name string) error {
	if !machineNameRE.MatchString(name) {
		return fmt.Errorf("name %q is invalid: use %s", name, machineNameRule)
	}
	return nil
}

type agentCreateArgs struct {
	Name     string `json:"name" jsonschema:"machine name: 1-31 lowercase letters, digits or hyphens, starting with a letter and not ending with a hyphen"`
	CPUs     int    `json:"cpus,omitempty" jsonschema:"number of vCPUs, 1 to 32; 0 or omitted means 1"`
	MemoryGB int    `json:"memory_gb,omitempty" jsonschema:"memory in GiB, 1 to 56; 0 or omitted means 2"`
	DiskGB   int    `json:"disk_gb,omitempty" jsonschema:"boot disk size in GiB, 1 to 4096; 0 or omitted means 40"`
	Image    string `json:"image,omitempty" jsonschema:"image to boot from, by name or ID; omitted means the server's default image; a name must match exactly one image"`
	Project  string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
}

type agentListArgs struct {
	Project string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
}

type agentStopArgs struct {
	Name    string `json:"name" jsonschema:"name of the machine to stop (see agent_list)"`
	Project string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
}

type agentDestroyArgs struct {
	Name    string `json:"name" jsonschema:"name of the machine to destroy (see agent_list)"`
	Project string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
	Force   bool   `json:"force,omitempty" jsonschema:"also delete the machine when other services hold it (default false)"`
}

type agentResizeArgs struct {
	Name     string `json:"name" jsonschema:"name of the machine to resize (see agent_list)"`
	Project  string `json:"project,omitempty" jsonschema:"project ID; default is the server's default project"`
	CPUs     int    `json:"cpus,omitempty" jsonschema:"new number of vCPUs, 1 to 32; 0 or omitted keeps the current value"`
	MemoryGB int    `json:"memory_gb,omitempty" jsonschema:"new memory in GiB, 1 to 56; 0 or omitted keeps the current value"`
	Force    bool   `json:"force,omitempty" jsonschema:"stop the machine even if it runs exec jobs (agent_exec); those jobs end as lost (default false)"`
}

// waitText is the wait one call may spend on a machine state change, as shown to the model. It comes from the
// server's MaxWait (see waitBudget), so the description cannot go stale when the flag changes.
func (s *Server) waitText() string {
	return fmt.Sprintf("about %d s", int(s.waitBudget()/time.Second))
}

func (s *Server) registerMachineTools() {
	addToolSchema(s, "agent_create",
		"Create a machine (VM) named name and start it; if a stopped machine with that name exists, start it again instead. "+
			"Image: the image argument (a name or an ID), else the server's default image; a name must match exactly one image. "+
			"Errors: `no image given` when neither is set; `no image \"x\" in project \"p\"; available images: ...` for an unknown "+
			"name; vmd refuses an image that is not ready (failed_precondition). Defaults: 1 CPU, 2 GiB memory, 40 GiB disk. "+
			"Limits: 32 CPUs, 56 GiB memory; vmd enforces the project quota (resource_exhausted). "+
			"Existing machine: a running or starting one fails (use agent_resize). A stopped one is started with its current size "+
			"and disk; cpus, memory_gb, disk_gb and image are then ignored. Other states (pending, stopping, error, deleting) fail "+
			"with `machine 'x' is <state>` and the reason. An errored machine is not restarted here; vmd re-checks it in the "+
			"background, or agent_destroy it and create it again. "+
			"Waits "+s.waitText()+" for the machine to be running. Results: `machine 'web' is running (1 CPU, 2 GiB)`, or "+
			"`machine 'web' is booting (agent_list shows when it is ready)` if still booting. A boot that ends in ERROR fails "+
			"with `machine 'web' is error: <reason>`.",
		func(root *jsonschema.Schema) {
			machMachineName(prop(root, "name"))
			intRange(root, "cpus", 0, machineMaxCPUs)
			intRange(root, "memory_gb", 0, machineMaxMemGB)
			intRange(root, "disk_gb", 0, 4096)
		}, s.agentCreate)
	addTool(s, "agent_list",
		"List the machines in a project, one line each, e.g. `web: running, 8 CPUs / 16 GiB, task: done` or "+
			"`db: error, 4 CPUs / 2.5 GiB, reason: boot failed, task: none`. The state is pending, starting, running, stopping, "+
			"stopped, error or deleting. Memory is in GiB with at most one decimal. The reason is shown only when the machine "+
			"has one. task is the state of the newest agentd task of the machine's agents: queued, running, done, failed, "+
			"gave_up, cancelled or error; none when the machine has no agent or no task; unknown when agentd cannot be reached, "+
			"when listing a machine's tasks fails, or when the newest task is succeeded_unverified. Prints `(no machines)` when "+
			"the project has none.", s.agentList)
	addToolSchema(s, "agent_stop",
		"Gracefully stop a machine: the guest is asked to power off over SSH and is killed if it is still up after about 60 s. "+
			"Its disk and data are kept; agent_create starts it again. Returns once the stop is requested, so the machine may "+
			"still be stopping. Results: `machine 'web' is stopping`, or `machine 'web' is stopped` if it already was. On a "+
			"machine in ERROR the call fails with `machine 'web' is error: <reason>`, but the stop is still recorded. "+
			"Errors: `not_found: no machine named \"web\" in project \"p\"`.",
		func(root *jsonschema.Schema) { machMachineName(prop(root, "name")) }, s.agentStop)
	addToolSchema(s, "agent_destroy",
		"PERMANENTLY destroy a machine. Order: its agentd agents are deleted first (running tasks are cancelled, and the "+
			"agents' task history, steps and messages are deleted). Then the VM is powered off immediately, not gracefully, "+
			"and its VM record and boot disk are deleted; the data on the disk cannot be recovered. The call returns once "+
			"deletion is requested, not when it is done: `machine 'web' deletion requested; it is being deleted (agent_list "+
			"shows deleting until it is gone)`. If an agent cannot be deleted the call fails with `delete agent <id>: ...` and "+
			"the VM is kept; agents deleted before the failure stay deleted, so retry. Without force, a machine that another "+
			"service holds is refused (failed_precondition, \"is held by ...\"); force=true deletes it anyway. "+
			"Errors: `not_found: no machine named ...`.",
		func(root *jsonschema.Schema) { machMachineName(prop(root, "name")) }, s.agentDestroy)
	addToolSchema(s, "agent_resize",
		"Change a machine's CPUs and/or memory: give cpus, memory_gb or both; 0 or omitted keeps a value. Limits (this "+
			"server's): 1-32 CPUs, 1-56 GiB. A running or starting machine is stopped gracefully (the guest is powered off over "+
			"SSH, killed after about 60 s), resized and started again; the stop and the restart share one wait of "+
			s.waitText()+". An unchanged size takes the same stop and start. A stopped machine is resized and stays stopped. "+
			"If the guest has not stopped in time the call fails with `machine 'web' is still stopping after the wait; resize "+
			"not applied (try again later)`; the stop continues, so retry later. A running machine is refused while it runs exec "+
			"jobs (agent_exec) unless force=true, which stops it anyway; the jobs end as lost. Agent tasks are not checked. "+
			"Refused for machines that are pending, stopping, error or deleting. Results: `machine 'web' resized from 8 CPUs / "+
			"16 GiB to 4 CPUs / 16 GiB; running again`, `...; booting again (agent_list shows when it is ready)`, or, for a "+
			"stopped machine, `...; applied; machine is stopped (agent_create starts it)`. If the update or the restart fails "+
			"the machine is left stopped; agent_create starts it.",
		func(root *jsonschema.Schema) {
			machMachineName(prop(root, "name"))
			intRange(root, "cpus", 0, machineMaxCPUs)
			intRange(root, "memory_gb", 0, machineMaxMemGB)
		}, s.agentResize)
}

// machMachineName sets the machine name rule on a property: the pattern, and the length bounds.
func machMachineName(p *jsonschema.Schema) {
	minLen, maxLen := 1, 31
	p.Pattern = machineNameRE.String()
	p.MinLength = &minLen
	p.MaxLength = &maxLen
}

// agentCreate creates the machine, or starts it again when a stopped machine with that name exists.
func (s *Server) agentCreate(ctx context.Context, in agentCreateArgs) (string, error) {
	if err := checkMachineName(in.Name); err != nil {
		return "", err
	}
	cpus, memGB, diskGB := in.CPUs, in.MemoryGB, in.DiskGB
	if cpus == 0 {
		cpus = machineDefCPUs
	}
	if memGB == 0 {
		memGB = machineDefMemGB
	}
	if diskGB == 0 {
		diskGB = machineDefDisk
	}
	if cpus < 1 || cpus > machineMaxCPUs {
		return "", fmt.Errorf("cpus must be between 1 and %d (got %d)", machineMaxCPUs, cpus)
	}
	if memGB < 1 || memGB > machineMaxMemGB {
		return "", fmt.Errorf("memory_gb must be between 1 and %d (got %d)", machineMaxMemGB, memGB)
	}
	if diskGB < 1 || diskGB > machineMaxDiskGB {
		return "", fmt.Errorf("disk_gb must be between 1 and %d (got %d)", machineMaxDiskGB, diskGB)
	}

	project := s.project(in.Project)
	deadline := time.Now().Add(s.waitBudget())

	vm, err := s.vmByName(ctx, project, in.Name)
	if err == nil {
		return s.bootExisting(ctx, vm, deadline)
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		return "", err
	}

	imageRef := in.Image
	if imageRef == "" {
		imageRef = s.cfg.DefaultImage
	}
	if imageRef == "" {
		return "", fmt.Errorf("no image given: pass image (a name or an image ID) or configure a default image")
	}
	imageID, err := s.resolveImage(ctx, project, imageRef)
	if err != nil {
		return "", err
	}
	created, err := s.cfg.Clients.VMs.CreateVM(ctx, connect.NewRequest(&vmdv1.CreateVMRequest{
		ProjectId:  project,
		Name:       in.Name,
		Cpus:       int32(cpus),
		MemoryMb:   int64(memGB) * 1024,
		Boot:       &vmdv1.CreateVMRequest_ImageId{ImageId: imageID},
		DiskSizeGb: int32(diskGB),
		Start:      true,
	}))
	if err != nil {
		return "", err
	}
	return s.awaitRunning(ctx, in.Name, created.Msg.GetId(), deadline)
}

// bootExisting starts a stopped machine that agent_create was asked for; a running one is an error.
func (s *Server) bootExisting(ctx context.Context, vm *vmdv1.VM, deadline time.Time) (string, error) {
	switch vm.GetState() {
	case vmdv1.VMState_VM_STATE_RUNNING, vmdv1.VMState_VM_STATE_STARTING:
		return "", fmt.Errorf("machine '%s' is already running (agent_resize changes CPUs/memory)", vm.GetName())
	case vmdv1.VMState_VM_STATE_STOPPED:
		if _, err := s.cfg.Clients.VMs.StartVM(ctx, connect.NewRequest(&vmdv1.StartVMRequest{Id: vm.GetId()})); err != nil {
			return "", err
		}
		return s.awaitRunning(ctx, vm.GetName(), vm.GetId(), deadline)
	}
	return "", machineStateErr(vm)
}

// awaitRunning waits (until deadline) for the machine to be running and describes the outcome.
func (s *Server) awaitRunning(ctx context.Context, name, id string, deadline time.Time) (string, error) {
	vm, err := s.waitVMState(ctx, id, vmdv1.VMState_VM_STATE_RUNNING, deadline)
	if err != nil {
		return "", err
	}
	switch vm.GetState() {
	case vmdv1.VMState_VM_STATE_RUNNING:
		return fmt.Sprintf("machine '%s' is running (%s, %s GiB)", name, cpuText(vm.GetCpus()), gibText(vm.GetMemoryMb())), nil
	case vmdv1.VMState_VM_STATE_ERROR:
		return "", machineStateErr(vm)
	}
	return fmt.Sprintf("machine '%s' is booting (agent_list shows when it is ready)", name), nil
}

// agentList lists the project's machines with their latest agentd task.
func (s *Server) agentList(ctx context.Context, in agentListArgs) (string, error) {
	project := s.project(in.Project)
	vms, err := s.listVMs(ctx, project)
	if err != nil {
		return "", err
	}
	if len(vms) == 0 {
		return "(no machines)", nil
	}
	tasks := s.taskLabels(ctx, project, vms)
	lines := make([]string, 0, len(vms))
	for _, v := range vms {
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %s, %s / %s GiB", v.GetName(), vmStateLabel(v.GetState()), cpuText(v.GetCpus()), gibText(v.GetMemoryMb()))
		if r := v.GetStateReason(); r != "" {
			fmt.Fprintf(&b, ", reason: %s", r)
		}
		if tasks != nil {
			fmt.Fprintf(&b, ", task: %s", tasks[v.GetId()])
		}
		lines = append(lines, b.String())
	}
	return strings.Join(lines, "\n"), nil
}

// taskLabels maps each VM ID to the label of its newest task. It returns nil when agentd is not configured, and
// "unknown" for every VM when agentd cannot be reached, so the machine list itself still works.
func (s *Server) taskLabels(ctx context.Context, project string, vms []*vmdv1.VM) map[string]string {
	if s.cfg.Clients.Agents == nil || s.cfg.Clients.Tasks == nil {
		return nil
	}
	out := make(map[string]string, len(vms))
	agents, err := s.listAgents(ctx, project, "")
	if err != nil {
		s.cfg.Log.Warn("agent_list: cannot list agents", "project", project, "err", err)
		for _, v := range vms {
			out[v.GetId()] = "unknown"
		}
		return out
	}
	agentsByVM := map[string][]string{}
	for _, a := range agents {
		agentsByVM[a.GetVmId()] = append(agentsByVM[a.GetVmId()], a.GetId())
	}
	for _, v := range vms {
		out[v.GetId()] = s.latestTaskLabel(ctx, agentsByVM[v.GetId()])
	}
	return out
}

// latestTaskLabel returns the label of the newest task among the given agents, "none" when there is none, or
// "unknown" when agentd fails.
func (s *Server) latestTaskLabel(ctx context.Context, agentIDs []string) string {
	var latest *agentdv1.Task
	for _, id := range agentIDs {
		t, err := s.latestTask(ctx, id)
		if err != nil {
			s.cfg.Log.Warn("agent_list: cannot list tasks", "agent", id, "err", err)
			return "unknown"
		}
		// Task IDs are ULIDs, so they sort by creation time.
		if t != nil && (latest == nil || t.GetId() > latest.GetId()) {
			latest = t
		}
	}
	if latest == nil {
		return "none"
	}
	return taskStateLabel(latest.GetState())
}

// agentStop asks vmd for a graceful stop of the machine.
func (s *Server) agentStop(ctx context.Context, in agentStopArgs) (string, error) {
	if err := checkMachineName(in.Name); err != nil {
		return "", err
	}
	vm, err := s.vmByName(ctx, s.project(in.Project), in.Name)
	if err != nil {
		return "", err
	}
	res, err := s.cfg.Clients.VMs.StopVM(ctx, connect.NewRequest(&vmdv1.StopVMRequest{Id: vm.GetId(), Force: false}))
	if err != nil {
		return "", err
	}
	switch res.Msg.GetState() {
	case vmdv1.VMState_VM_STATE_STOPPED:
		return fmt.Sprintf("machine '%s' is stopped", in.Name), nil
	case vmdv1.VMState_VM_STATE_ERROR:
		return "", machineStateErr(res.Msg)
	}
	return fmt.Sprintf("machine '%s' is stopping", in.Name), nil
}

// agentDestroy deletes the machine's agents, then the VM and its disk.
func (s *Server) agentDestroy(ctx context.Context, in agentDestroyArgs) (string, error) {
	if err := checkMachineName(in.Name); err != nil {
		return "", err
	}
	project := s.project(in.Project)
	vm, err := s.vmByName(ctx, project, in.Name)
	if err != nil {
		return "", err
	}
	agents, err := s.listAgents(ctx, project, vm.GetId())
	if err != nil {
		return "", err
	}
	for _, a := range agents {
		_, err := s.cfg.Clients.Agents.DeleteAgent(ctx, connect.NewRequest(&agentdv1.DeleteAgentRequest{Id: a.GetId()}))
		if err != nil && connect.CodeOf(err) != connect.CodeNotFound {
			return "", fmt.Errorf("delete agent %s: %v", a.GetId(), err)
		}
	}
	if _, err := s.cfg.Clients.VMs.DeleteVM(ctx, connect.NewRequest(&vmdv1.DeleteVMRequest{
		Id: vm.GetId(), Force: in.Force, DeleteDisks: true})); err != nil {
		return "", err
	}
	return fmt.Sprintf("machine '%s' deletion requested; it is being deleted (agent_list shows deleting until it is gone)", in.Name), nil
}

// agentResize changes the size of a machine, stopping and restarting it when it is running.
func (s *Server) agentResize(ctx context.Context, in agentResizeArgs) (string, error) {
	if err := checkMachineName(in.Name); err != nil {
		return "", err
	}
	if in.CPUs == 0 && in.MemoryGB == 0 {
		return "", fmt.Errorf("give cpus, memory_gb or both")
	}
	if in.CPUs < 0 || in.CPUs > machineMaxCPUs {
		return "", fmt.Errorf("cpus must be between 1 and %d (got %d)", machineMaxCPUs, in.CPUs)
	}
	if in.MemoryGB < 0 || in.MemoryGB > machineMaxMemGB {
		return "", fmt.Errorf("memory_gb must be between 1 and %d (got %d)", machineMaxMemGB, in.MemoryGB)
	}
	project := s.project(in.Project)
	deadline := time.Now().Add(s.waitBudget())

	vm, err := s.vmByName(ctx, project, in.Name)
	if err != nil {
		return "", err
	}
	wasRunning := false
	switch vm.GetState() {
	case vmdv1.VMState_VM_STATE_RUNNING, vmdv1.VMState_VM_STATE_STARTING:
		wasRunning = true
	case vmdv1.VMState_VM_STATE_STOPPED:
	default:
		return "", fmt.Errorf("%v; agent_resize needs the machine running or stopped", machineStateErr(vm))
	}
	change := fmt.Sprintf("%s / %s GiB", cpuText(vm.GetCpus()), gibText(vm.GetMemoryMb()))

	cur := vm
	if wasRunning {
		if !in.Force {
			jobs, err := s.runningJobs(ctx, vm.GetId())
			if err != nil {
				return "", err
			}
			if len(jobs) > 0 {
				return "", fmt.Errorf("machine '%s' is running %d job(s) (%s); refusing to resize it. Wait for them, or pass force=true to stop it, which kills them",
					in.Name, len(jobs), strings.Join(jobs, ", "))
			}
		}
		if _, err := s.cfg.Clients.VMs.StopVM(ctx, connect.NewRequest(&vmdv1.StopVMRequest{Id: vm.GetId(), Force: false})); err != nil {
			return "", err
		}
		cur, err = s.waitVMState(ctx, vm.GetId(), vmdv1.VMState_VM_STATE_STOPPED, deadline)
		if err != nil {
			return "", err
		}
		if cur.GetState() == vmdv1.VMState_VM_STATE_ERROR {
			return "", machineStateErr(cur)
		}
		if cur.GetState() != vmdv1.VMState_VM_STATE_STOPPED {
			return "", fmt.Errorf("machine '%s' is still stopping after the wait; resize not applied (try again later)", in.Name)
		}
	}

	req := &vmdv1.UpdateVMRequest{Id: vm.GetId(), ExpectedGeneration: cur.GetGeneration()}
	if in.CPUs > 0 {
		c := int32(in.CPUs)
		req.Cpus = &c
	}
	if in.MemoryGB > 0 {
		m := int64(in.MemoryGB) * 1024
		req.MemoryMb = &m
	}
	upd, err := s.cfg.Clients.VMs.UpdateVM(ctx, connect.NewRequest(req))
	if err != nil {
		if wasRunning {
			return "", fmt.Errorf("resize of machine '%s' failed; the machine is left stopped (agent_create starts it): %v", in.Name, err)
		}
		return "", err
	}
	newSize := fmt.Sprintf("%s / %s GiB", cpuText(upd.Msg.GetCpus()), gibText(upd.Msg.GetMemoryMb()))
	summary := fmt.Sprintf("machine '%s' resized from %s to %s", in.Name, change, newSize)
	if !wasRunning {
		return summary + "; applied; machine is stopped (agent_create starts it)", nil
	}

	if _, err := s.cfg.Clients.VMs.StartVM(ctx, connect.NewRequest(&vmdv1.StartVMRequest{Id: vm.GetId()})); err != nil {
		return "", fmt.Errorf("%s, but it could not be started again and is left stopped (agent_create starts it): %v", summary, err)
	}
	started, err := s.waitVMState(ctx, vm.GetId(), vmdv1.VMState_VM_STATE_RUNNING, deadline)
	if err != nil {
		return "", err
	}
	switch started.GetState() {
	case vmdv1.VMState_VM_STATE_RUNNING:
		return summary + "; running again", nil
	case vmdv1.VMState_VM_STATE_ERROR:
		return "", fmt.Errorf("%s, but it failed to start: %v", summary, machineStateErr(started))
	}
	return summary + "; booting again (agent_list shows when it is ready)", nil
}

// waitBudget is how long one tool call may wait for machine state changes.
func (s *Server) waitBudget() time.Duration {
	return max(s.cfg.MaxWait-waitReserve, 0)
}

// waitVMState long-polls GetVM until the machine reaches want or ERROR, or until deadline passes. It returns the
// last machine seen; the caller decides what a timeout means.
func (s *Server) waitVMState(ctx context.Context, id string, want vmdv1.VMState, deadline time.Time) (*vmdv1.VM, error) {
	for {
		secs := int(math.Ceil(time.Until(deadline).Seconds()))
		res, err := s.cfg.Clients.VMs.GetVM(ctx, connect.NewRequest(&vmdv1.GetVMRequest{
			Id: id, WaitForState: want, WaitSeconds: int32(s.waitSeconds(secs))}))
		if err != nil {
			return nil, err
		}
		st := res.Msg.GetState()
		if st == want || st == vmdv1.VMState_VM_STATE_ERROR || !time.Now().Before(deadline) {
			return res.Msg, nil
		}
	}
}

// listVMs returns every VM in project.
func (s *Server) listVMs(ctx context.Context, project string) ([]*vmdv1.VM, error) {
	var out []*vmdv1.VM
	token := ""
	for {
		res, err := s.cfg.Clients.VMs.ListVMs(ctx, connect.NewRequest(&vmdv1.ListVMsRequest{
			ProjectId: project, PageSize: machinePageSize, PageToken: token}))
		if err != nil {
			return nil, err
		}
		out = append(out, res.Msg.GetVms()...)
		token = res.Msg.GetNextPageToken()
		if token == "" {
			return out, nil
		}
	}
}

// listAgents returns the agentd agents of project, or only those bound to vmID when it is not empty.
func (s *Server) listAgents(ctx context.Context, project, vmID string) ([]*agentdv1.Agent, error) {
	var out []*agentdv1.Agent
	token := ""
	for {
		res, err := s.cfg.Clients.Agents.ListAgents(ctx, connect.NewRequest(&agentdv1.ListAgentsRequest{
			ProjectId: project, VmId: vmID, PageSize: machinePageSize, PageToken: token}))
		if err != nil {
			return nil, err
		}
		out = append(out, res.Msg.GetAgents()...)
		token = res.Msg.GetNextPageToken()
		if token == "" {
			return out, nil
		}
	}
}

// runningJobs returns the IDs of the jobs on vmID that are still running.
func (s *Server) runningJobs(ctx context.Context, vmID string) ([]string, error) {
	var out []string
	token := ""
	for {
		res, err := s.cfg.Clients.Exec.ListJobs(ctx, connect.NewRequest(&vmdv1.ListJobsRequest{
			VmId: vmID, PageSize: machinePageSize, PageToken: token}))
		if err != nil {
			return nil, err
		}
		for _, j := range res.Msg.GetJobs() {
			if j.GetState() == vmdv1.JobState_JOB_STATE_RUNNING {
				out = append(out, j.GetId())
			}
		}
		token = res.Msg.GetNextPageToken()
		if token == "" {
			return out, nil
		}
	}
}

// resolveImage turns an image name or ID into an image ID. An exact ID match wins; otherwise the name must match
// exactly one image.
func (s *Server) resolveImage(ctx context.Context, project, ref string) (string, error) {
	var images []*vmdv1.Image
	token := ""
	for {
		res, err := s.cfg.Clients.Images.ListImages(ctx, connect.NewRequest(&vmdv1.ListImagesRequest{
			ProjectId: project, PageSize: machinePageSize, PageToken: token}))
		if err != nil {
			return "", err
		}
		images = append(images, res.Msg.GetImages()...)
		token = res.Msg.GetNextPageToken()
		if token == "" {
			break
		}
	}
	var matches []*vmdv1.Image
	for _, im := range images {
		if im.GetId() == ref {
			return ref, nil
		}
		if im.GetName() == ref {
			matches = append(matches, im)
		}
	}
	switch len(matches) {
	case 0:
		var names []string
		for _, im := range images {
			if !slices.Contains(names, im.GetName()) {
				names = append(names, im.GetName())
			}
		}
		if len(names) == 0 {
			return "", fmt.Errorf("no image %q in project %q, and the project has no images", ref, project)
		}
		slices.Sort(names)
		return "", fmt.Errorf("no image %q in project %q; available images: %s", ref, project, strings.Join(names, ", "))
	case 1:
		return matches[0].GetId(), nil
	}
	return "", fmt.Errorf("%d images are named %q; pass the image ID instead", len(matches), ref)
}

// vmStateLabel is the lower-case state name without the VM_STATE_ prefix.
func vmStateLabel(st vmdv1.VMState) string {
	return strings.ToLower(strings.TrimPrefix(st.String(), "VM_STATE_"))
}

// taskStateLabel maps an agentd task state to the short label shown by agent_list.
func taskStateLabel(st agentdv1.TaskState) string {
	switch st {
	case agentdv1.TaskState_TASK_STATE_QUEUED:
		return "queued"
	case agentdv1.TaskState_TASK_STATE_RUNNING:
		return "running"
	case agentdv1.TaskState_TASK_STATE_SUCCEEDED:
		return "done"
	case agentdv1.TaskState_TASK_STATE_FAILED:
		return "failed"
	case agentdv1.TaskState_TASK_STATE_GAVE_UP:
		return "gave_up"
	case agentdv1.TaskState_TASK_STATE_CANCELLED:
		return "cancelled"
	case agentdv1.TaskState_TASK_STATE_ERROR:
		return "error"
	}
	return "unknown"
}

// machineStateErr describes a machine that is not in the state the caller needed, including its state reason.
func machineStateErr(vm *vmdv1.VM) error {
	if r := vm.GetStateReason(); r != "" {
		return fmt.Errorf("machine '%s' is %s: %s", vm.GetName(), vmStateLabel(vm.GetState()), r)
	}
	return fmt.Errorf("machine '%s' is %s", vm.GetName(), vmStateLabel(vm.GetState()))
}

// gibText renders megabytes as GiB without trailing zeros (16384 -> "16", 1536 -> "1.5").
func gibText(mb int64) string {
	return strconv.FormatFloat(float64(mb)/1024, 'f', -1, 64)
}

// cpuText writes a vCPU count with the right plural: "1 CPU", "8 CPUs".
func cpuText(n int32) string {
	if n == 1 {
		return "1 CPU"
	}
	return fmt.Sprintf("%d CPUs", n)
}
