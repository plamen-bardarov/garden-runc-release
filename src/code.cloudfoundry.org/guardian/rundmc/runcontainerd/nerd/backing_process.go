package nerd

import (
	"context"
	"strings"
	"syscall"

	"code.cloudfoundry.org/lager/v3"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate
//counterfeiter:generate github.com/containerd/containerd/v2/client.Process
//counterfeiter:generate github.com/containerd/containerd/v2/pkg/cio.IO

type BackingProcess struct {
	log               lager.Logger
	context           context.Context
	containerdProcess client.Process
}

func NewBackingProcess(log lager.Logger, p client.Process, ctx context.Context) BackingProcess {
	return BackingProcess{
		log:               log,
		context:           ctx,
		containerdProcess: p,
	}
}

func (p BackingProcess) ID() string {
	return p.containerdProcess.ID()
}

func (p BackingProcess) Wait() (int, error) {
	exitCh, err := p.containerdProcess.Wait(p.context)
	if err != nil {
		return 0, err
	}

	exitStatus := <-exitCh
	if exitStatus.Error() != nil {
		return 0, exitStatus.Error()
	}

	p.containerdProcess.IO().Wait()
	return int(exitStatus.ExitCode()), nil
}

func (p BackingProcess) Signal(signal syscall.Signal) error {
	return p.containerdProcess.Kill(p.context, signal)
}

func (p BackingProcess) Delete() error {
	_, err := p.containerdProcess.Delete(p.context)
	if isAlreadyGone(err) {
		return nil
	}
	return err
}

// isAlreadyGone reports whether deleting failed because the task or container
// no longer exists. Kata runtime-rs reports containers removed together with
// their sandbox VM as "failed to find container".
func isAlreadyGone(err error) bool {
	return err != nil && (errdefs.IsNotFound(err) || strings.Contains(err.Error(), "failed to find container"))
}
