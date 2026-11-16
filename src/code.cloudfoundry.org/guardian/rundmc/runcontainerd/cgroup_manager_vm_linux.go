//go:build linux

package runcontainerd

import (
	"code.cloudfoundry.org/guardian/rundmc/goci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// vmCgroupManager is the CgroupManager used with VM based runtimes such as
// Kata Containers.
//
// Kata places the hypervisor, its vCPU threads and helper processes into a
// "kata_<name>" child of the container's cgroup parent and does not apply any
// limits itself: it relies on the parent created by the container engine.
// Guest memory used by the VM is therefore charged to the cgroup that garden
// sizes to the application memory limit. The VM overhead (guest kernel, agent,
// page cache, hypervisor, virtiofsd) is added to that parent cgroup so the
// hypervisor is not OOM killed on the host before the application reaches its
// own limit, which is enforced inside the guest.
type vmCgroupManager struct {
	cgroupManager
	memoryOverheadBytes int64
}

func NewVMCgroupManager(runcRoot, namespace string, memoryOverheadBytes int64) CgroupManager {
	return vmCgroupManager{
		cgroupManager:       cgroupManager{runcRoot: runcRoot, namespace: namespace},
		memoryOverheadBytes: memoryOverheadBytes,
	}
}

// SetUseMemoryHierarchy reads runc state, which does not exist for VM
// runtimes. memory.use_hierarchy is a cgroups v1 setting only.
func (m vmCgroupManager) SetUseMemoryHierarchy(handle string) error {
	return nil
}

func (m vmCgroupManager) SetUnifiedResources(bundle goci.Bndl) error {
	if m.memoryOverheadBytes <= 0 || bundle.Spec.Linux == nil || bundle.Spec.Linux.Resources == nil || bundle.Spec.Linux.Resources.Memory == nil {
		return m.setUnifiedResources(bundle)
	}

	// Only the host-side (parent) cgroup gets the overhead. The spec passed to
	// the runtime keeps the application limit, which Kata applies to the
	// container cgroup inside the guest and uses to size the guest memory.
	hostBundle := bundle
	hostBundle.Spec.Linux = cloneLinuxWithMemoryOverhead(bundle.Spec.Linux, m.memoryOverheadBytes)
	if err := m.setUnifiedResources(hostBundle); err != nil {
		return err
	}

	// setUnifiedResources moves the container into an "init" child cgroup by
	// mutating CgroupsPath on the (shared) Linux struct; mirror that here.
	bundle.Spec.Linux.CgroupsPath = hostBundle.Spec.Linux.CgroupsPath
	return nil
}

// AddCgroupBindMount would bind mount the host cgroup of the container into
// the container. With a VM runtime that would expose a host cgroupfs
// directory to the guest via the shared filesystem, so it is skipped.
func (m vmCgroupManager) AddCgroupBindMount(bundle *goci.Bndl) {}

func cloneLinuxWithMemoryOverhead(linux *specs.Linux, overhead int64) *specs.Linux {
	clone := *linux
	resources := *linux.Resources
	memory := *linux.Resources.Memory

	if memory.Limit != nil && *memory.Limit > 0 {
		limit := *memory.Limit + overhead
		memory.Limit = &limit
	}
	if memory.Swap != nil && *memory.Swap > 0 {
		swap := *memory.Swap + overhead
		memory.Swap = &swap
	}

	resources.Memory = &memory
	clone.Resources = &resources
	return &clone
}
