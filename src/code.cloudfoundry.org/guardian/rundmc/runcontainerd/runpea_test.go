package runcontainerd_test

import (
	"errors"
	"strings"

	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/guardian/rundmc/goci"
	"code.cloudfoundry.org/guardian/rundmc/runcontainerd"
	"code.cloudfoundry.org/guardian/rundmc/runcontainerd/runcontainerdfakes"
	"code.cloudfoundry.org/lager/v3/lagertest"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/opencontainers/runtime-spec/specs-go"
)

var _ = Describe("RunContainerPea", func() {

	var (
		fakePeaManager     *runcontainerdfakes.FakePeaManager
		fakeProcessManager *runcontainerdfakes.FakeProcessManager
		fakeVolumizer      *runcontainerdfakes.FakeVolumizer

		processID string

		runContainerPea *runcontainerd.RunContainerPea
	)

	BeforeEach(func() {
		fakePeaManager = new(runcontainerdfakes.FakePeaManager)
		fakeProcessManager = new(runcontainerdfakes.FakeProcessManager)
		fakeVolumizer = new(runcontainerdfakes.FakeVolumizer)

		var err error
		Expect(err).NotTo(HaveOccurred())

		runContainerPea = runcontainerd.NewRunContainerPea(fakePeaManager, fakeProcessManager, fakeVolumizer, false)
	})

	Describe("RunPea", func() {
		It("creates a container using process args", func() {
			bundle := goci.Bndl{Spec: specs.Spec{Version: "test-version"}}
			_, err := runContainerPea.RunPea(lagertest.NewTestLogger("test-logger"), processID, bundle, "sandbox-id", garden.ProcessIO{}, false, strings.NewReader(""), nil)
			Expect(err).NotTo(HaveOccurred())

			Expect(fakePeaManager.CreateCallCount()).To(Equal(1))
			_, actualProcessID, actualBundle, _ := fakePeaManager.CreateArgsForCall(0)
			Expect(actualProcessID).To(Equal(processID))
			Expect(actualBundle.Spec.Version).To(Equal("test-version"))
			Expect(actualBundle.Spec.Annotations["container-type"]).To(Equal("pea"))
			Expect(actualBundle.Spec.Annotations["sandbox-container"]).To(Equal("sandbox-id"))
		})

		When("the creator returns an error", func() {
			BeforeEach(func() {
				fakePeaManager.CreateReturns(errors.New("error-creating"))
			})

			It("returns the error", func() {
				_, err := runContainerPea.RunPea(lagertest.NewTestLogger("test-logger"), processID, goci.Bndl{}, "", garden.ProcessIO{}, false, strings.NewReader(""), nil)

				Expect(err).To(MatchError("error-creating"))
			})
		})
	})
	Describe("best-effort cleanup (VM runtimes)", func() {
		var backing *runcontainerdfakes.FakeBackingProcess

		BeforeEach(func() {
			backing = new(runcontainerdfakes.FakeBackingProcess)
			fakeProcessManager.GetTaskReturns(backing, nil)
			runContainerPea.SetBestEffortCleanup(true)
		})

		It("cleans up the pea even when waiting for it fails (its VM is gone)", func() {
			backing.WaitReturns(0, errors.New("ttrpc: closed"))
			process, err := runContainerPea.RunPea(lagertest.NewTestLogger("test-logger"), "pea", goci.Bndl{}, "sandbox-id", garden.ProcessIO{}, false, strings.NewReader(""), nil)
			Expect(err).NotTo(HaveOccurred())

			_, err = process.Wait()
			Expect(err).To(MatchError("ttrpc: closed"))
			Expect(fakePeaManager.DeleteCallCount()).To(Equal(1))
			Expect(fakePeaManager.RemoveBundleCallCount()).To(Equal(1))
			Expect(fakeVolumizer.DestroyCallCount()).To(Equal(1))
		})

		It("runs all cleanup steps when one fails", func() {
			backing.WaitReturns(137, nil)
			fakePeaManager.DeleteReturns(errors.New("task not stoppable"))
			process, err := runContainerPea.RunPea(lagertest.NewTestLogger("test-logger"), "pea", goci.Bndl{}, "sandbox-id", garden.ProcessIO{}, false, strings.NewReader(""), nil)
			Expect(err).NotTo(HaveOccurred())

			_, err = process.Wait()
			Expect(err).To(MatchError(ContainSubstring("task not stoppable")))
			Expect(fakePeaManager.RemoveBundleCallCount()).To(Equal(1))
			Expect(fakeVolumizer.DestroyCallCount()).To(Equal(1))
		})

		It("returns the exit code when cleanup succeeds", func() {
			backing.WaitReturns(137, nil)
			process, err := runContainerPea.RunPea(lagertest.NewTestLogger("test-logger"), "pea", goci.Bndl{}, "sandbox-id", garden.ProcessIO{}, false, strings.NewReader(""), nil)
			Expect(err).NotTo(HaveOccurred())

			Expect(process.Wait()).To(Equal(137))
		})
	})
})
