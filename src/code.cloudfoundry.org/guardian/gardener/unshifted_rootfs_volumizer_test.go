package gardener_test

import (
	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/guardian/gardener"
	fakes "code.cloudfoundry.org/guardian/gardener/gardenerfakes"
	"code.cloudfoundry.org/lager/v3/lagertest"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("UnshiftedRootFSVolumizer", func() {
	It("creates the root filesystem from the privileged (unshifted) store and delegates everything else", func() {
		inner := new(fakes.FakeVolumizer)
		volumizer := gardener.UnshiftedRootFSVolumizer{Volumizer: inner}

		_, err := volumizer.Create(lagertest.NewTestLogger("test"), garden.ContainerSpec{Handle: "h", Privileged: false})
		Expect(err).NotTo(HaveOccurred())

		_, spec := inner.CreateArgsForCall(0)
		Expect(spec.Handle).To(Equal("h"))
		Expect(spec.Privileged).To(BeTrue())

		Expect(volumizer.Destroy(lagertest.NewTestLogger("test"), "h")).To(Succeed())
		Expect(inner.DestroyCallCount()).To(Equal(1))
	})
})
