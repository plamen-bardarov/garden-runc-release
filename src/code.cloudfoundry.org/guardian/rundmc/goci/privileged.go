package goci

// UnprivilegedAnnotation marks a container as unprivileged when its OCI spec
// cannot express that with a user namespace. VM based runtimes such as Kata
// Containers do not support user namespaces inside the guest; the VM is the
// isolation boundary instead.
const UnprivilegedAnnotation = "garden.unprivileged"

// IsPrivileged reports whether a container spec describes a privileged
// container: one without a user namespace, unless explicitly annotated as
// unprivileged.
func IsPrivileged(bndl Bndl) bool {
	if bndl.Spec.Annotations[UnprivilegedAnnotation] == "true" {
		return false
	}
	if bndl.Spec.Linux == nil {
		return true
	}
	for _, ns := range bndl.Spec.Linux.Namespaces {
		if ns.Type == UserNamespace.Type {
			return false
		}
	}
	return true
}
