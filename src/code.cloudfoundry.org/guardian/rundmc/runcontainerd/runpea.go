package runcontainerd

import (
	"io"

	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/guardian/rundmc/goci"
	"code.cloudfoundry.org/lager/v3"
)

//counterfeiter:generate . PeaManager
type PeaManager interface {
	Create(log lager.Logger, id string, bundle goci.Bndl, io garden.ProcessIO) error
	Delete(log lager.Logger, containerID string) error
	RemoveBundle(log lager.Logger, containerID string) error
}

//counterfeiter:generate . Volumizer
type Volumizer interface {
	Destroy(log lager.Logger, handle string) error
}

type RunContainerPea struct {
	peaManager               PeaManager
	processManager           ProcessManager
	volumizer                Volumizer
	cleanupProcessDirsOnWait bool
	bestEffortCleanup        bool
}

// SetBestEffortCleanup makes peas run all cleanup steps after they exit, even
// if waiting for them or a step fails (see PeaProcess). Used with VM runtimes.
func (r *RunContainerPea) SetBestEffortCleanup(bestEffort bool) {
	r.bestEffortCleanup = bestEffort
}

func NewRunContainerPea(peaManager PeaManager, processManager ProcessManager, volumizer Volumizer, cleanupProcessDirsOnWait bool) *RunContainerPea {
	return &RunContainerPea{
		peaManager:               peaManager,
		processManager:           processManager,
		volumizer:                volumizer,
		cleanupProcessDirsOnWait: cleanupProcessDirsOnWait,
	}
}

func (r *RunContainerPea) RunPea(
	log lager.Logger, processID string, processBundle goci.Bndl, sandboxHandle string,
	pio garden.ProcessIO, tty bool, procJSON io.Reader, extraCleanup func() error,
) (garden.Process, error) {

	if processBundle.Spec.Annotations == nil {
		processBundle.Spec.Annotations = make(map[string]string)
	}
	processBundle.Spec.Annotations["container-type"] = "pea"
	processBundle.Spec.Annotations["sandbox-container"] = sandboxHandle

	log.Info("using runcontainerd.RunPea")

	if err := r.peaManager.Create(log, processID, processBundle, pio); err != nil {
		return &Process{}, err
	}

	process, err := r.processManager.GetTask(log, processID)
	if err != nil {
		return nil, err
	}

	proc := NewProcess(log, process, r.cleanupProcessDirsOnWait)
	peaProcess := NewPeaProcess(log, *proc, r.peaManager, r.volumizer)
	peaProcess.bestEffortCleanup = r.bestEffortCleanup
	return peaProcess, nil
}
