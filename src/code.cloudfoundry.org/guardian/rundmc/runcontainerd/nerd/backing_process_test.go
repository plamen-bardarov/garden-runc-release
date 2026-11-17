package nerd_test

import (
	"context"
	"errors"
	"github.com/containerd/errdefs"
	"time"

	"github.com/containerd/containerd/v2/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"code.cloudfoundry.org/guardian/rundmc/runcontainerd/nerd"
	"code.cloudfoundry.org/guardian/rundmc/runcontainerd/nerd/nerdfakes"
	"code.cloudfoundry.org/lager/v3/lagertest"
)

var _ = Describe("BackingProcess", func() {
	var (
		containerdProcess *nerdfakes.FakeProcess
		containerdIO      *nerdfakes.FakeIO
		backingProcess    nerd.BackingProcess
	)

	BeforeEach(func() {
		containerdProcess = new(nerdfakes.FakeProcess)
		containerdIO = new(nerdfakes.FakeIO)
		containerdProcess.IOReturns(containerdIO)
		backingProcess = nerd.NewBackingProcess(lagertest.NewTestLogger("backing-process"), containerdProcess, context.Background())
	})

	Describe("Wait", func() {
		var (
			exitCode   int
			exitCh     chan client.ExitStatus
			exitStatus client.ExitStatus
			err        error
		)

		BeforeEach(func() {
			exitCh = make(chan client.ExitStatus, 1)
			exitStatus = *client.NewExitStatus(123, time.Now(), nil)
			containerdProcess.WaitReturns(exitCh, nil)
		})

		JustBeforeEach(func() {
			exitCh <- exitStatus
			exitCode, err = backingProcess.Wait()
		})

		It("returns the exit code", func() {
			Expect(err).NotTo(HaveOccurred())
			Expect(exitCode).To(Equal(123))
		})

		It("waits for IO", func() {
			Expect(containerdIO.WaitCallCount()).To(Equal(1))
		})

		When("waiting for the process fails", func() {
			BeforeEach(func() {
				exitStatus = *client.NewExitStatus(9, time.Now(), errors.New("wait-failed"))
			})

			It("returns the error", func() {
				Expect(err).To(MatchError("wait-failed"))
			})

			It("does not wait for the IO (because it does not make sense)", func() {
				Expect(containerdIO.WaitCallCount()).To(BeZero())
			})
		})
	})
	Describe("Delete", func() {
		It("treats a task that no longer exists as deleted", func() {
			containerdProcess.DeleteReturns(nil, errdefs.ErrNotFound)
			Expect(backingProcess.Delete()).To(Succeed())
		})

		It("treats a container already removed from its Kata sandbox as deleted", func() {
			containerdProcess.DeleteReturns(nil, errors.New("failed to delete task: do delete: failed to find container sandbox-envoy"))
			Expect(backingProcess.Delete()).To(Succeed())
		})

		It("returns other errors", func() {
			containerdProcess.DeleteReturns(nil, errors.New("boom"))
			Expect(backingProcess.Delete()).To(MatchError("boom"))
		})
	})
})
