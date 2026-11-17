package runcontainerd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/guardian/idmapper"
	"code.cloudfoundry.org/guardian/rundmc/goci"
	"code.cloudfoundry.org/lager/v3"
	uuid "github.com/nu7hatch/gouuid"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// OCI annotations understood by Kata Containers (and other sandbox based
// runtimes) to group several containers into one sandbox (VM). They are the
// annotations the containerd CRI plugin sets for Kubernetes pods.
const (
	SandboxContainerTypeAnnotation = "io.kubernetes.cri.container-type"
	SandboxIDAnnotation            = "io.kubernetes.cri.sandbox-id"
	SandboxContainerTypeSandbox    = "sandbox"
	// SandboxMemoryAnnotation is the memory limit of the sandbox in bytes.
	// Kata Containers runtime-rs sizes the VM of a sandbox from it (static
	// sandbox resource management: limit + overhead_memory); without it the
	// VM gets default_memory, whatever the container limit is.
	SandboxMemoryAnnotation       = "io.kubernetes.cri.sandbox-memory"
	SandboxContainerTypeContainer = "container"

	// VMTarPathInContainer is where garden bind mounts its static tar binary
	// into containers of VM runtimes (see RunContainerd.StreamIn).
	VMTarPathInContainer = "/tmp/garden-tar"
)

// VMRuntimeConfig configures RunContainerd for a VM based runtime such as
// Kata Containers, where container processes run inside a guest VM and are
// not reachable through their host PID (/proc/<pid>/...).
type VMRuntimeConfig struct {
	// TarPathInContainer is the path, inside every container, of a static
	// tar binary bind mounted by garden. It is used to implement
	// StreamIn/StreamOut by executing tar inside the guest.
	TarPathInContainer string

	// RootfsUIDMappings and RootfsGIDMappings map container IDs to the owner
	// IDs of files in the container rootfs on the host, when the rootfs is
	// id-shifted and the runtime translates IDs on the shared filesystem
	// (virtiofsd --translate-uid/--translate-gid). Empty for unshifted rootfs.
	RootfsUIDMappings idmapper.MappingList
	RootfsGIDMappings idmapper.MappingList

	// Reaper removes processes and VMM users the runtime left behind for a
	// container. Optional.
	Reaper VMSandboxReaper
}

//counterfeiter:generate . VMSandboxReaper
type VMSandboxReaper interface {
	Reap(log lager.Logger, handle string) error
}

type VMTaskManager interface {
	KillAll(log lager.Logger, id string, kill bool) error
}

// EnableVMRuntime switches RunContainerd into VM runtime mode.
func (r *RunContainerd) EnableVMRuntime(config VMRuntimeConfig, taskManager VMTaskManager) {
	r.vmRuntime = &config
	r.vmTaskManager = taskManager
}

func (r *RunContainerd) isVMRuntime() bool {
	return r.vmRuntime != nil
}

// deleteVM deletes the task of a container run by a VM runtime.
//
// Peas are containers in the sandbox VM of the container they belong to.
// Their own cleanup (PeaProcess.Wait) runs only after their process exits,
// which for long running peas (envoy, liveness checks) happens when the VM
// is torn down, and then the shim cannot be reached any more. Their tasks and
// containerd records are therefore removed here first, while the sandbox's
// shim is still alive.
//
// Afterwards anything the runtime left in the container's cgroup is reaped:
// when a VM fails to start, Kata leaves virtiofsd (and the rootless VMM user)
// behind, and the cgroup could never be removed.
func (r *RunContainerd) deleteVM(log lager.Logger, id string) error {
	peas, err := r.ContainerPeaHandles(log, id)
	if err != nil {
		log.Error("listing-peas-failed", err)
	}
	for _, pea := range peas {
		r.removePea(log, pea)
	}

	err = r.containerManager.Delete(log, id)
	if r.vmRuntime.Reaper == nil {
		return err
	}
	if reapErr := r.vmRuntime.Reaper.Reap(log, id); reapErr != nil {
		log.Error("reaping-failed", reapErr)
		if err == nil {
			return reapErr
		}
	}
	if err != nil {
		// the shim may have been stuck and is gone now
		log.Info("retrying-delete-after-reaping", lager.Data{"error": err.Error()})
		err = r.containerManager.Delete(log, id)
	}
	return err
}

func (r *RunContainerd) removePea(log lager.Logger, pea string) {
	if err := r.containerManager.Delete(log, pea); err != nil {
		log.Error("deleting-pea-task-failed", err, lager.Data{"pea": pea})
	}
	if err := r.containerManager.RemoveBundle(log, pea); err != nil {
		log.Error("removing-pea-container-failed", err, lager.Data{"pea": pea})
	}
}

// RemoveOrphanedPeas removes the tasks and containerd records of peas whose
// sandbox container no longer exists, and returns their handles so that their
// volumes can be destroyed. Only VM runtimes leave such records behind.
func (r *RunContainerd) RemoveOrphanedPeas(log lager.Logger) ([]string, error) {
	if !r.isVMRuntime() {
		return nil, nil
	}
	log = log.Session("remove-orphaned-peas")

	all, err := r.containerManager.BundleIDs(ContainerFilter{Label: "container-type", Value: "pea", ComparisonOp: "=="})
	if err != nil {
		return nil, err
	}
	sandboxes, err := r.ContainerHandles()
	if err != nil {
		return nil, err
	}
	owned := map[string]bool{}
	for _, sandbox := range sandboxes {
		peas, err := r.ContainerPeaHandles(log, sandbox)
		if err != nil {
			return nil, err
		}
		for _, pea := range peas {
			owned[pea] = true
		}
	}

	var orphans []string
	for _, pea := range all {
		if owned[pea] {
			continue
		}
		r.removePea(log, pea)
		orphans = append(orphans, pea)
	}
	if len(orphans) > 0 {
		log.Info("removed", lager.Data{"peas": orphans})
	}
	return orphans, nil
}

// addSandboxAnnotations makes the VM runtime run garden containers in their
// own sandbox (VM) and peas (e.g. envoy and health checks) as additional
// containers inside the sandbox of the container they belong to, so that they
// share its network namespace, just like with runc.
func addSandboxAnnotations(bundle *goci.Bndl, id string) {
	if bundle.Spec.Annotations == nil {
		bundle.Spec.Annotations = map[string]string{}
	}

	if bundle.Spec.Annotations["container-type"] == "pea" {
		bundle.Spec.Annotations[SandboxContainerTypeAnnotation] = SandboxContainerTypeContainer
		bundle.Spec.Annotations[SandboxIDAnnotation] = bundle.Spec.Annotations["sandbox-container"]
		return
	}

	bundle.Spec.Annotations[SandboxContainerTypeAnnotation] = SandboxContainerTypeSandbox
	bundle.Spec.Annotations[SandboxIDAnnotation] = id
	if limit := memoryLimit(bundle); limit > 0 {
		bundle.Spec.Annotations[SandboxMemoryAnnotation] = strconv.FormatInt(limit, 10)
	}
}

func memoryLimit(bundle *goci.Bndl) int64 {
	linux := bundle.Spec.Linux
	if linux == nil || linux.Resources == nil || linux.Resources.Memory == nil || linux.Resources.Memory.Limit == nil {
		return 0
	}
	return *linux.Resources.Memory.Limit
}

// clearConsoleSizeWithoutTerminal drops the console size of processes without
// a terminal. Per the OCI spec consoleSize only applies with terminal=true and
// runc ignores it, but the Kata shim resizes the (non-existent) tty of every
// exec with a console size, which the agent rejects with "no tty".
func clearConsoleSizeWithoutTerminal(process *specs.Process) {
	if process != nil && !process.Terminal {
		process.ConsoleSize = nil
	}
}

// removeUserNamespace drops the user namespace and id mappings from the spec
// of unprivileged containers. Kata Containers fails to create containers with
// a user namespace (EPERM from the guest agent); the VM is the isolation
// boundary instead. The container stays unprivileged for garden through
// goci.UnprivilegedAnnotation, and its root filesystem must not be id-shifted
// (see gardener.UnshiftedRootFSVolumizer).
func removeUserNamespace(bundle *goci.Bndl) {
	if bundle.Spec.Linux == nil {
		return
	}

	hadUserNamespace := false
	namespaces := bundle.Spec.Linux.Namespaces[:0]
	for _, ns := range bundle.Spec.Linux.Namespaces {
		if ns.Type == goci.UserNamespace.Type {
			hadUserNamespace = true
			continue
		}
		namespaces = append(namespaces, ns)
	}
	bundle.Spec.Linux.Namespaces = namespaces
	bundle.Spec.Linux.UIDMappings = nil
	bundle.Spec.Linux.GIDMappings = nil

	if hadUserNamespace {
		bundle.Spec.Annotations[goci.UnprivilegedAnnotation] = "true"
	}
}

// StopAll implements rundmc.ProcessesStopper for VM runtimes by signalling
// all processes of the container task through containerd.
func (r *RunContainerd) StopAll(log lager.Logger, handle string, _ []int, kill bool) error {
	if r.vmTaskManager == nil {
		return fmt.Errorf("stop is not supported for container %s", handle)
	}
	return r.vmTaskManager.KillAll(log, handle, kill)
}

// StreamIn extracts a tar stream inside a container by running garden's
// bind-mounted tar binary inside the guest as the requested user. This
// replaces nstar, which would otherwise enter the mount namespace of the
// container's host PID - the hypervisor process - and write to the host.
//
// The stream is not passed through the process stdin: the Kata shim's CloseIO
// waits for EOF on stdin while holding the shim lock, and EOF on the stdin
// FIFO is not reliably delivered, which deadlocks the whole sandbox. Instead
// the archive is written to a temporary file at the root of the container
// rootfs on the host, which the runtime shares with the guest.
func (r *RunContainerd) StreamIn(log lager.Logger, handle string, spec garden.StreamInSpec) error {
	log = log.Session("vm-stream-in", lager.Data{"handle": handle, "path": spec.Path})

	target, err := r.prepareStreamTarget(log, handle, spec.User, spec.Path, true)
	if err != nil {
		return err
	}

	archive, err := target.createArchiveFile()
	if err != nil {
		return err
	}
	defer archive.remove(log)

	if _, err := io.Copy(archive.file, spec.TarStream); err != nil {
		_ = archive.file.Close()
		return fmt.Errorf("writing archive: %w", err)
	}
	if err := archive.file.Close(); err != nil {
		return fmt.Errorf("closing archive: %w", err)
	}

	return r.runTar(log, handle, target.user, "stream-in",
		"--no-same-permissions", "--no-same-owner", "-xf", archive.pathInContainer, "-C", target.path)
}

// StreamOut creates a tar archive of a path inside a container by running
// garden's bind-mounted tar binary inside the guest, writing to a temporary
// file in the container rootfs which is then streamed from the host (see
// StreamIn for why stdio is avoided).
func (r *RunContainerd) StreamOut(log lager.Logger, handle string, spec garden.StreamOutSpec) (io.ReadCloser, error) {
	log = log.Session("vm-stream-out", lager.Data{"handle": handle, "path": spec.Path})

	target, err := r.prepareStreamTarget(log, handle, spec.User, spec.Path, false)
	if err != nil {
		return nil, err
	}

	sourceDir := filepath.Dir(target.path)
	compress := filepath.Base(target.path)
	if spec.Path != "" && spec.Path[len(spec.Path)-1] == '/' {
		sourceDir = target.path
		compress = "."
	}

	archive, err := target.createArchiveFile()
	if err != nil {
		return nil, err
	}
	_ = archive.file.Close()

	if err := r.runTar(log, handle, target.user, "stream-out",
		"--no-same-permissions", "--no-same-owner", "-cf", archive.pathInContainer, "-C", sourceDir, compress); err != nil {
		archive.remove(log)
		return nil, err
	}

	f, err := os.Open(archive.hostPath)
	if err != nil {
		archive.remove(log)
		return nil, fmt.Errorf("opening archive: %w", err)
	}

	return &removeOnClose{File: f, remove: func() { archive.remove(log) }}, nil
}

func (r *RunContainerd) runTar(log lager.Logger, handle, user, prefix string, args ...string) error {
	var output bytes.Buffer
	proc, err := r.Exec(log, handle, garden.ProcessSpec{
		ID:   streamProcessID(prefix),
		Path: r.vmRuntime.TarPathInContainer,
		Args: args,
		User: user,
		Dir:  "/",
	}, garden.ProcessIO{Stdout: &output, Stderr: &output})
	if err != nil {
		return fmt.Errorf("starting tar: %w", err)
	}

	exitCode, err := proc.Wait()
	if err != nil {
		return fmt.Errorf("waiting for tar: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("tar exited with %d: %s", exitCode, output.String())
	}
	return nil
}

type streamTarget struct {
	user       string
	path       string
	rootfsPath string
	hostUID    int
	hostGID    int
}

type streamArchive struct {
	file            *os.File
	hostPath        string
	pathInContainer string
}

// createArchiveFile creates an empty archive file at the root of the
// container rootfs on the host, owned by the streaming user. O_EXCL and
// O_NOFOLLOW make sure no existing file or symlink is followed.
func (t streamTarget) createArchiveFile() (*streamArchive, error) {
	name := "." + streamProcessID("garden-stream") + ".tar"
	hostPath := filepath.Join(t.rootfsPath, name)

	f, err := os.OpenFile(hostPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY|openNoFollow, 0600)
	if err != nil {
		return nil, fmt.Errorf("creating archive file: %w", err)
	}
	if err := f.Chown(t.hostUID, t.hostGID); err != nil {
		_ = f.Close()
		_ = os.Remove(hostPath)
		return nil, fmt.Errorf("chowning archive file: %w", err)
	}

	return &streamArchive{file: f, hostPath: hostPath, pathInContainer: "/" + name}, nil
}

func (a *streamArchive) remove(log lager.Logger) {
	if err := os.Remove(a.hostPath); err != nil && !os.IsNotExist(err) {
		log.Error("removing-archive-file", err, lager.Data{"path": a.hostPath})
	}
}

type removeOnClose struct {
	*os.File
	remove func()
}

func (r *removeOnClose) Close() error {
	err := r.File.Close()
	r.remove()
	return err
}

// prepareStreamTarget resolves the streaming user and path (relative paths
// are relative to the user's home, as with nstar) using the container rootfs
// on the host, and optionally creates the destination directory owned by
// that user.
func (r *RunContainerd) prepareStreamTarget(log lager.Logger, handle, user, path string, create bool) (streamTarget, error) {
	if user == "" {
		user = "root"
	}

	bundle, err := r.getBundle(log, handle)
	if err != nil {
		return streamTarget{}, err
	}
	rootfsPath := bundle.Spec.Root.Path

	execUser, err := r.userLookupper.Lookup(rootfsPath, user)
	if err != nil {
		return streamTarget{}, fmt.Errorf("looking up user %s: %w", user, err)
	}

	if !filepath.IsAbs(path) {
		path = filepath.Join(execUser.Home, path)
	}

	// The spec carries no id mappings in VM mode (see removeUserNamespace);
	// the rootfs owner ids are given by the configured rootfs mappings.
	uidMappings, gidMappings := r.vmRuntime.RootfsUIDMappings, r.vmRuntime.RootfsGIDMappings
	if len(uidMappings) == 0 {
		uidMappings = bundle.Spec.Linux.UIDMappings
	}
	if len(gidMappings) == 0 {
		gidMappings = bundle.Spec.Linux.GIDMappings
	}
	hostUID := idmapper.MappingList(uidMappings).Map(execUser.Uid)
	hostGID := idmapper.MappingList(gidMappings).Map(execUser.Gid)

	if create {
		if err := r.mkdirer.MkdirAs(rootfsPath, hostUID, hostGID, 0755, false, path); err != nil {
			return streamTarget{}, fmt.Errorf("creating %s: %w", path, err)
		}
	}

	return streamTarget{user: user, path: path, rootfsPath: rootfsPath, hostUID: hostUID, hostGID: hostGID}, nil
}

func streamProcessID(prefix string) string {
	id, err := uuid.NewV4()
	if err != nil {
		return prefix
	}
	return fmt.Sprintf("%s-%s", prefix, id.String())
}
