package privchecker

import (
	"code.cloudfoundry.org/guardian/rundmc/goci"
	"code.cloudfoundry.org/lager/v3"
	"github.com/opencontainers/runtime-spec/specs-go"
)

//counterfeiter:generate . ContainerManager
type ContainerManager interface {
	Spec(log lager.Logger, containerID string) (*specs.Spec, error)
}

type PrivilegeChecker struct {
	Log              lager.Logger
	ContainerManager ContainerManager
}

func (p *PrivilegeChecker) Privileged(id string) (bool, error) {
	spec, err := p.ContainerManager.Spec(p.Log, id)
	if err != nil {
		return false, err
	}

	return goci.IsPrivileged(goci.Bndl{Spec: *spec}), nil
}
