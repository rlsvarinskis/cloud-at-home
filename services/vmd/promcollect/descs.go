package promcollect

import "github.com/prometheus/client_golang/prometheus"

// newDesc builds a gauge description. Every metric here is a gauge computed at scrape time, so the
// type is fixed; the counters, histograms and sampler gauges are owned by the Collector itself.
func newDesc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc(name, help, labels, nil)
}

// Label sets shared by the per-VM metrics. The VM's own labels come first, then the extra ones.
var (
	vmLabels   = []string{"vm_id", "project", "name", "node"}
	vmFSLabels = []string{"vm_id", "project", "name", "node", "disk_id", "device", "mount_point"}
	vmDiskLbls = []string{"vm_id", "project", "name", "node", "disk_id"}
)

var (
	// Node capacity, one series per node.
	nodeUp              = newDesc("attic_node_up", "1 if the node is ready, 0 if it is draining or down.", "node")
	nodeDraining        = newDesc("attic_node_draining", "1 if the node is draining: it keeps its VMs and takes no new ones.", "node")
	nodeCPUsTotal       = newDesc("attic_node_cpu_threads", "Physical CPU threads on the node.", "node")
	nodeCPUsAllocated   = newDesc("attic_node_cpu_allocated_vcpus", "vCPUs of VMs that should be running on the node.", "node")
	nodeCPUsAvailable   = newDesc("attic_node_cpu_available_vcpus", "vCPUs the scheduler can still hand out on the node, after overcommit.", "node")
	nodeMemTotal        = newDesc("attic_node_memory_total_bytes", "Physical memory on the node, in bytes.", "node")
	nodeMemAllocated    = newDesc("attic_node_memory_allocated_bytes", "Memory of VMs that should be running on the node, in bytes.", "node")
	nodeMemAvailable    = newDesc("attic_node_memory_available_bytes", "Memory the scheduler can still hand out on the node, in bytes, after reserve and overcommit.", "node")
	nodeDiskTotal       = newDesc("attic_node_disk_total_bytes", "Disk capacity on the node, in bytes.", "node")
	nodeDiskFree        = newDesc("attic_node_disk_free_bytes", "Free disk on the node for new disks, in bytes.", "node")
	nodeVMsTotal        = newDesc("attic_node_vms", "VMs placed on the node, in any state.", "node")
	nodeVMsRunning      = newDesc("attic_node_vms_running", "VMs on the node in the running state.", "node")
	nodeLastHeartbeatTS = newDesc("attic_node_last_heartbeat_timestamp_seconds", "Unix time of the node's last successful poll; 0 if it never answered.", "node")

	// Cluster totals over ready nodes.
	clusterCPUsTotal     = newDesc("attic_cluster_cpu_threads", "Physical CPU threads on ready nodes.")
	clusterCPUsAllocated = newDesc("attic_cluster_cpu_allocated_vcpus", "vCPUs of VMs that should be running on ready nodes.")
	clusterCPUsAvailable = newDesc("attic_cluster_cpu_available_vcpus", "vCPUs the scheduler can still hand out on ready nodes.")
	clusterMemTotal      = newDesc("attic_cluster_memory_total_bytes", "Physical memory on ready nodes, in bytes.")
	clusterMemAllocated  = newDesc("attic_cluster_memory_allocated_bytes", "Memory of VMs that should be running on ready nodes, in bytes.")
	clusterMemAvailable  = newDesc("attic_cluster_memory_available_bytes", "Memory the scheduler can still hand out on ready nodes, in bytes.")
	clusterDiskTotal     = newDesc("attic_cluster_disk_total_bytes", "Disk capacity on ready nodes, in bytes.")
	clusterDiskFree      = newDesc("attic_cluster_disk_free_bytes", "Free disk on ready nodes, in bytes.")
	clusterVMsTotal      = newDesc("attic_cluster_vms", "VMs placed on ready nodes, in any state.")
	clusterVMsRunning    = newDesc("attic_cluster_vms_running", "VMs on ready nodes in the running state.")
	clusterNodesReady    = newDesc("attic_cluster_nodes_ready", "Nodes in the ready state.")

	// VM counts across all nodes.
	vmsByState        = newDesc("attic_vms", "VMs per state across all nodes.", "state")
	vmsDesiredRunning = newDesc("attic_vms_desired_running", "VMs whose desired power state is running.")

	// Guest metrics from the sampler cache.
	vmCPUPercent      = newDesc("attic_vm_cpu_percent", "Guest CPU usage like top: 100 per busy vCPU, so an 8-vCPU VM can reach 800.", vmLabels...)
	vmCPUCores        = newDesc("attic_vm_cpu_cores", "vCPUs the guest sees.", vmLabels...)
	vmLoad1           = newDesc("attic_vm_load1", "Guest load average over 1 minute.", vmLabels...)
	vmLoad5           = newDesc("attic_vm_load5", "Guest load average over 5 minutes.", vmLabels...)
	vmLoad15          = newDesc("attic_vm_load15", "Guest load average over 15 minutes.", vmLabels...)
	vmMemTotal        = newDesc("attic_vm_memory_total_bytes", "Guest memory the guest sees, in bytes.", vmLabels...)
	vmMemUsed         = newDesc("attic_vm_memory_used_bytes", "Guest memory in use, in bytes.", vmLabels...)
	vmMemAvailable    = newDesc("attic_vm_memory_available_bytes", "Guest memory available for new work, in bytes.", vmLabels...)
	vmSwapTotal       = newDesc("attic_vm_swap_total_bytes", "Guest swap space, in bytes.", vmLabels...)
	vmSwapUsed        = newDesc("attic_vm_swap_used_bytes", "Guest swap in use, in bytes.", vmLabels...)
	vmUptime          = newDesc("attic_vm_uptime_seconds", "Guest uptime, in seconds.", vmLabels...)
	vmMetricsAge      = newDesc("attic_vm_metrics_age_seconds", "Seconds since the VM's guest metrics were sampled.", vmLabels...)
	vmFSSize          = newDesc("attic_vm_filesystem_size_bytes", "Size of a guest filesystem, in bytes, attributed to the VM disk it lives on.", vmFSLabels...)
	vmFSUsed          = newDesc("attic_vm_filesystem_used_bytes", "Used space of a guest filesystem, in bytes, attributed to the VM disk it lives on.", vmFSLabels...)
	vmDiskVirtualSize = newDesc("attic_vm_disk_virtual_size_bytes", "Virtual size of a VM disk, in bytes.", vmDiskLbls...)
	vmDiskAllocated   = newDesc("attic_vm_disk_allocated_bytes", "Host space a VM disk occupies, in bytes.", vmDiskLbls...)
)

// ownDescs lists every description this Collector sends from Describe and Collect itself. A metric
// added to Collect must be listed here, or registries reject it.
var ownDescs = []*prometheus.Desc{
	nodeUp, nodeDraining, nodeCPUsTotal, nodeCPUsAllocated, nodeCPUsAvailable,
	nodeMemTotal, nodeMemAllocated, nodeMemAvailable, nodeDiskTotal, nodeDiskFree,
	nodeVMsTotal, nodeVMsRunning, nodeLastHeartbeatTS,
	clusterCPUsTotal, clusterCPUsAllocated, clusterCPUsAvailable,
	clusterMemTotal, clusterMemAllocated, clusterMemAvailable,
	clusterDiskTotal, clusterDiskFree, clusterVMsTotal, clusterVMsRunning, clusterNodesReady,
	vmsByState, vmsDesiredRunning,
	vmCPUPercent, vmCPUCores, vmLoad1, vmLoad5, vmLoad15,
	vmMemTotal, vmMemUsed, vmMemAvailable, vmSwapTotal, vmSwapUsed,
	vmUptime, vmMetricsAge, vmFSSize, vmFSUsed, vmDiskVirtualSize, vmDiskAllocated,
}
