package gardener

import (
	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/lager/v3"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// UnshiftedRootFSVolumizer creates container root filesystems without user
// namespace id shifting (i.e. from the privileged image store), while the
// containers themselves remain unprivileged.
//
// It is used with VM based runtimes (e.g. Kata Containers), which do not
// support user namespaces: container uids are guest uids, so the root
// filesystem must be owned by the real (unmapped) ids.
type UnshiftedRootFSVolumizer struct {
	Volumizer
}

func (v UnshiftedRootFSVolumizer) Create(log lager.Logger, spec garden.ContainerSpec) (specs.Spec, error) {
	spec.Privileged = true
	return v.Volumizer.Create(log, spec)
}
