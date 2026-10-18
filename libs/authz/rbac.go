// Package auth: authentication (signed tokens) and authorization (project-scoped RBAC).
//
// Model: a Principal holds Bindings (project, role). A role grants a fixed set of Permissions.
// A binding on project "*" applies to every project. Handlers call Require/RequireVisible with the
// permission they need and the project of the resource they touch; there is no ambient access.
package authz

import "sort"

type Permission string

const (
	VMCreate Permission = "vm.create"
	VMGet    Permission = "vm.get"
	VMList   Permission = "vm.list"
	VMUpdate Permission = "vm.update"
	VMStart  Permission = "vm.start"
	VMStop   Permission = "vm.stop"
	VMDelete Permission = "vm.delete"
	VMHold   Permission = "vm.hold"

	DiskCreate   Permission = "disk.create"
	DiskGet      Permission = "disk.get"
	DiskList     Permission = "disk.list"
	DiskUpdate   Permission = "disk.update"
	DiskDelete   Permission = "disk.delete"
	DiskSnapshot Permission = "disk.snapshot"

	ImageRegister Permission = "image.register" // project images; empty project needs a "*" binding
	ImageGet      Permission = "image.get"
	ImageList     Permission = "image.list"
	ImageDelete   Permission = "image.delete"

	ExecRun    Permission = "exec.run"
	ExecGet    Permission = "exec.get"
	ExecCancel Permission = "exec.cancel"
	ExecList   Permission = "exec.list"
	FileRead   Permission = "file.read"
	FileWrite  Permission = "file.write"

	AgentCreate Permission = "agent.create"
	AgentGet    Permission = "agent.get"
	AgentList   Permission = "agent.list"
	AgentUpdate Permission = "agent.update"
	AgentDelete Permission = "agent.delete"
	TaskCreate  Permission = "task.create"
	TaskGet     Permission = "task.get"
	TaskList    Permission = "task.list"
	TaskCancel  Permission = "task.cancel"
	ModelList   Permission = "model.list"

	VMMetrics   Permission = "vm.metrics"   // per-VM CPU/RAM/disk usage
	CapacityGet Permission = "capacity.get" // cluster-wide capacity; operators only (global scope)
	NodeDrain   Permission = "node.drain"   // stop/resume placing new VM starts on a node; operators only

	NodeManage Permission = "node.manage" // the node API; only the vmd control plane holds it
)

type Role string

const (
	RoleViewer   Role = "viewer"
	RoleEditor   Role = "editor"
	RoleAdmin    Role = "admin"
	RoleService  Role = "service"  // agentd -> vmd: run commands and place holds, nothing destructive
	RoleNode     Role = "node"     // vmd control plane -> node daemon
	RoleOperator Role = "operator" // global: cluster capacity and nothing inside projects
)

var viewer = []Permission{VMGet, VMList, VMMetrics, DiskGet, DiskList, ImageGet, ImageList, ExecGet, ExecList,
	AgentGet, AgentList, TaskGet, TaskList, ModelList}

var editor = append(append([]Permission{}, viewer...),
	VMCreate, VMUpdate, VMStart, VMStop, VMDelete, DiskCreate, DiskUpdate, DiskDelete, DiskSnapshot,
	ExecRun, ExecCancel, FileRead, FileWrite,
	AgentCreate, AgentUpdate, AgentDelete, TaskCreate, TaskCancel)

var rolePerms = map[Role]map[Permission]bool{
	RoleViewer: set(viewer),
	RoleEditor: set(editor),
	RoleAdmin:  set(append(append([]Permission{}, editor...), VMHold, ImageRegister, ImageDelete)),
	RoleService: set([]Permission{VMGet, VMList, VMMetrics, VMHold, ExecRun, ExecGet, ExecCancel, ExecList,
		FileRead, FileWrite, ModelList}),
	RoleNode:     set([]Permission{NodeManage}),
	RoleOperator: set([]Permission{CapacityGet, NodeDrain}),
}

func set(p []Permission) map[Permission]bool {
	m := make(map[Permission]bool, len(p))
	for _, x := range p {
		m[x] = true
	}
	return m
}

// Roles lists every defined role, sorted by name.
func Roles() []Role {
	out := make([]Role, 0, len(rolePerms))
	for r := range rolePerms {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Permissions lists what the role grants, sorted.
func (r Role) Permissions() []Permission {
	out := make([]Permission, 0, len(rolePerms[r]))
	for p := range rolePerms[r] {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// KnownRole reports whether r is a defined role.
func KnownRole(r Role) bool { _, ok := rolePerms[r]; return ok }

// Grants reports whether the role includes the permission.
func (r Role) Grants(p Permission) bool { return rolePerms[r][p] }
