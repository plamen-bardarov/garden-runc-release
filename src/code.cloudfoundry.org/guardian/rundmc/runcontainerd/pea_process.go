package runcontainerd

import (
	"errors"

	"code.cloudfoundry.org/lager/v3"
)

type PeaProcess struct {
	Process
	peaManager PeaManager
	volumizer  Volumizer
	// bestEffortCleanup runs every cleanup step even when waiting or an
	// earlier step failed. Used with VM runtimes, where a pea's task goes
	// away together with the sandbox VM.
	bestEffortCleanup bool
}

func NewPeaProcess(log lager.Logger, process Process, peaManager PeaManager, volumizer Volumizer) *PeaProcess {
	return &PeaProcess{
		Process:    process,
		peaManager: peaManager,
		volumizer:  volumizer,
	}
}

func (p *PeaProcess) Wait() (int, error) {
	if p.bestEffortCleanup {
		return p.waitAndCleanUpBestEffort()
	}

	exitCode, err := p.Process.Wait()
	if err != nil {
		return 0, err
	}

	if err := p.peaManager.Delete(p.log, p.ID()); err != nil {
		return 0, err
	}

	if err := p.peaManager.RemoveBundle(p.log, p.ID()); err != nil {
		return 0, err
	}

	if err := p.volumizer.Destroy(p.log, p.ID()); err != nil {
		return 0, err
	}

	return exitCode, nil
}

func (p *PeaProcess) waitAndCleanUpBestEffort() (int, error) {
	exitCode, waitErr := p.Process.Wait()

	cleanupErr := errors.Join(
		p.peaManager.Delete(p.log, p.ID()),
		p.peaManager.RemoveBundle(p.log, p.ID()),
		p.volumizer.Destroy(p.log, p.ID()),
	)
	if waitErr != nil {
		if cleanupErr != nil {
			p.log.Error("pea-cleanup-failed", cleanupErr, lager.Data{"pea": p.ID()})
		}
		return 0, waitErr
	}
	if cleanupErr != nil {
		return 0, cleanupErr
	}
	return exitCode, nil
}
