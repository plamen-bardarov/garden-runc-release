package runcontainerd

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"

	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/guardian/idmapper"
	"code.cloudfoundry.org/guardian/rundmc/goci"
	"code.cloudfoundry.org/lager/v3"
	uuid "github.com/nu7hatch/gouuid"
)

// OCI annotations understood by Kata Containers (and other sandbox based
// runtimes) to group several containers into one sandbox (VM). They are the
// annotations the containerd CRI plugin sets for Kubernetes pods.
const (
	SandboxContainerTypeAnnotation = "io.kubernetes.cri.container-type"
	SandboxIDAnnotation            = "io.kubernetes.cri.sandbox-id"
	SandboxContainerTypeSandbox    = "sandbox"
	SandboxContainerTypeContainer  = "container"

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
func (r *RunContainerd) StreamIn(log lager.Logger, handle string, spec garden.StreamInSpec) error {
	log = log.Session("vm-stream-in", lager.Data{"handle": handle, "path": spec.Path})

	user, dest, err := r.prepareStreamDestination(log, handle, spec.User, spec.Path, true)
	if err != nil {
		return err
	}

	var output bytes.Buffer
	proc, err := r.Exec(log, handle, garden.ProcessSpec{
		ID:   streamProcessID("stream-in"),
		Path: r.vmRuntime.TarPathInContainer,
		Args: []string{"--no-same-permissions", "--no-same-owner", "-xf", "-", "-C", dest},
		User: user,
		Dir:  "/",
	}, garden.ProcessIO{Stdin: spec.TarStream, Stdout: &output, Stderr: &output})
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

// StreamOut creates a tar stream of a path inside a container by running
// garden's bind-mounted tar binary inside the guest.
func (r *RunContainerd) StreamOut(log lager.Logger, handle string, spec garden.StreamOutSpec) (io.ReadCloser, error) {
	log = log.Session("vm-stream-out", lager.Data{"handle": handle, "path": spec.Path})

	user, source, err := r.prepareStreamDestination(log, handle, spec.User, spec.Path, false)
	if err != nil {
		return nil, err
	}

	sourceDir := filepath.Dir(source)
	compress := filepath.Base(source)
	if spec.Path != "" && spec.Path[len(spec.Path)-1] == '/' {
		sourceDir = source
		compress = "."
	}

	reader, writer := io.Pipe()
	var stderr bytes.Buffer
	proc, err := r.Exec(log, handle, garden.ProcessSpec{
		ID:   streamProcessID("stream-out"),
		Path: r.vmRuntime.TarPathInContainer,
		Args: []string{"--no-same-permissions", "--no-same-owner", "-cf", "-", "-C", sourceDir, compress},
		User: user,
		Dir:  "/",
	}, garden.ProcessIO{Stdout: writer, Stderr: &stderr})
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("starting tar: %w", err)
	}

	go func() {
		exitCode, err := proc.Wait()
		if err == nil && exitCode != 0 {
			err = fmt.Errorf("tar exited with %d: %s", exitCode, stderr.String())
		}
		if err != nil {
			log.Error("tar-failed", err)
		}
		_ = writer.CloseWithError(err)
	}()

	return reader, nil
}

// prepareStreamDestination resolves the streaming user and path (relative
// paths are relative to the user's home, as with nstar) using the container
// rootfs on the host, and optionally creates the destination directory owned
// by that user.
func (r *RunContainerd) prepareStreamDestination(log lager.Logger, handle, user, path string, create bool) (string, string, error) {
	if user == "" {
		user = "root"
	}

	bundle, err := r.getBundle(log, handle)
	if err != nil {
		return "", "", err
	}
	rootfsPath := bundle.Spec.Root.Path

	execUser, err := r.userLookupper.Lookup(rootfsPath, user)
	if err != nil {
		return "", "", fmt.Errorf("looking up user %s: %w", user, err)
	}

	if !filepath.IsAbs(path) {
		path = filepath.Join(execUser.Home, path)
	}

	if create {
		hostUID := idmapper.MappingList(bundle.Spec.Linux.UIDMappings).Map(execUser.Uid)
		hostGID := idmapper.MappingList(bundle.Spec.Linux.GIDMappings).Map(execUser.Gid)
		if err := r.mkdirer.MkdirAs(rootfsPath, hostUID, hostGID, 0755, false, path); err != nil {
			return "", "", fmt.Errorf("creating %s: %w", path, err)
		}
	}

	return user, path, nil
}

func streamProcessID(prefix string) string {
	id, err := uuid.NewV4()
	if err != nil {
		return prefix
	}
	return fmt.Sprintf("%s-%s", prefix, id.String())
}
