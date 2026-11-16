package runcontainerd_test

import (
	"errors"

	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/guardian/rundmc/goci"
	"code.cloudfoundry.org/guardian/rundmc/runcontainerd"
	"code.cloudfoundry.org/guardian/rundmc/runcontainerd/runcontainerdfakes"
	"code.cloudfoundry.org/guardian/rundmc/users"
	"code.cloudfoundry.org/guardian/rundmc/users/usersfakes"
	"code.cloudfoundry.org/lager/v3"
	"code.cloudfoundry.org/lager/v3/lagertest"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

type fakeVMTaskManager struct {
	killed []string
	kill   []bool
}

func (f *fakeVMTaskManager) KillAll(_ lager.Logger, id string, kill bool) error {
	f.killed = append(f.killed, id)
	f.kill = append(f.kill, kill)
	return nil
}

var _ = Describe("Runcontainerd with a VM runtime", func() {
	var (
		log              lager.Logger
		containerManager *runcontainerdfakes.FakeContainerManager
		processManager   *runcontainerdfakes.FakeProcessManager
		processBuilder   *runcontainerdfakes.FakeProcessBuilder
		userLookupper    *usersfakes.FakeUserLookupper
		cgroupManager    *runcontainerdfakes.FakeCgroupManager
		mkdirer          *runcontainerdfakes.FakeMkdirer
		taskManager      *fakeVMTaskManager
		runContainerd    *runcontainerd.RunContainerd
	)

	BeforeEach(func() {
		log = lagertest.NewTestLogger("test")
		containerManager = new(runcontainerdfakes.FakeContainerManager)
		processManager = new(runcontainerdfakes.FakeProcessManager)
		processBuilder = new(runcontainerdfakes.FakeProcessBuilder)
		userLookupper = new(usersfakes.FakeUserLookupper)
		cgroupManager = new(runcontainerdfakes.FakeCgroupManager)
		mkdirer = new(runcontainerdfakes.FakeMkdirer)
		taskManager = &fakeVMTaskManager{}

		runContainerd = runcontainerd.New(containerManager, processManager, processBuilder, userLookupper,
			new(runcontainerdfakes.FakeExecer), new(runcontainerdfakes.FakeStatser), true, cgroupManager, mkdirer,
			nil, false, new(runcontainerdfakes.FakeRuntimeStopper))
		runContainerd.EnableVMRuntime(runcontainerd.VMRuntimeConfig{TarPathInContainer: "/tmp/garden-tar"}, taskManager)
	})

	Describe("Create", func() {
		It("runs garden containers as their own sandbox", func() {
			bundle := goci.Bndl{Spec: specs.Spec{Linux: &specs.Linux{}}}
			Expect(runContainerd.Create(log, "app-handle", bundle, garden.ProcessIO{})).To(Succeed())

			_, _, spec, _, _, _ := containerManager.CreateArgsForCall(0)
			Expect(spec.Annotations).To(HaveKeyWithValue(runcontainerd.SandboxContainerTypeAnnotation, runcontainerd.SandboxContainerTypeSandbox))
			Expect(spec.Annotations).To(HaveKeyWithValue(runcontainerd.SandboxIDAnnotation, "app-handle"))
		})

		It("runs peas as containers inside the sandbox of the container they belong to", func() {
			bundle := goci.Bndl{Spec: specs.Spec{
				Linux:       &specs.Linux{},
				Annotations: map[string]string{"container-type": "pea", "sandbox-container": "app-handle"},
			}}
			Expect(runContainerd.Create(log, "envoy-pea", bundle, garden.ProcessIO{})).To(Succeed())

			_, _, spec, _, _, _ := containerManager.CreateArgsForCall(0)
			Expect(spec.Annotations).To(HaveKeyWithValue(runcontainerd.SandboxContainerTypeAnnotation, runcontainerd.SandboxContainerTypeContainer))
			Expect(spec.Annotations).To(HaveKeyWithValue(runcontainerd.SandboxIDAnnotation, "app-handle"))
		})
	})

	Describe("Exec", func() {
		BeforeEach(func() {
			containerManager.SpecReturns(&specs.Spec{
				Root:  &specs.Root{Path: "/var/vcap/data/grootfs/store/images/app-handle/rootfs"},
				Linux: &specs.Linux{},
			}, nil)
			containerManager.GetContainerPIDReturns(1234, nil)
			userLookupper.LookupReturns(&users.ExecUser{Uid: 2000, Gid: 2000, Home: "/home/vcap"}, nil)
			processBuilder.BuildProcessReturns(&specs.Process{})
			containerManager.ExecReturns(new(runcontainerdfakes.FakeBackingProcess), nil)
		})

		It("resolves users and creates the working dir in the container rootfs on the host, not in /proc/<hypervisor-pid>/root", func() {
			_, err := runContainerd.Exec(log, "app-handle", garden.ProcessSpec{User: "vcap"}, garden.ProcessIO{})
			Expect(err).NotTo(HaveOccurred())

			rootfsPath, user := userLookupper.LookupArgsForCall(0)
			Expect(rootfsPath).To(Equal("/var/vcap/data/grootfs/store/images/app-handle/rootfs"))
			Expect(user).To(Equal("vcap"))

			mkdirRootfs, _, _, _, _, _ := mkdirer.MkdirAsArgsForCall(0)
			Expect(mkdirRootfs).To(Equal("/var/vcap/data/grootfs/store/images/app-handle/rootfs"))
		})
	})

	Describe("StreamIn", func() {
		It("fails without touching the host when the container spec cannot be loaded", func() {
			containerManager.SpecReturns(nil, errors.New("not found"))
			err := runContainerd.StreamIn(log, "app-handle", garden.StreamInSpec{Path: "/home/vcap"})
			Expect(err).To(HaveOccurred())
			Expect(mkdirer.MkdirAsCallCount()).To(Equal(0))
			Expect(containerManager.ExecCallCount()).To(Equal(0))
		})

		It("creates the destination as the user and runs the bind mounted tar inside the container", func() {
			containerManager.SpecReturns(&specs.Spec{
				Root:  &specs.Root{Path: "/rootfs"},
				Linux: &specs.Linux{},
			}, nil)
			userLookupper.LookupReturns(&users.ExecUser{Uid: 2000, Gid: 2000, Home: "/home/vcap"}, nil)
			processBuilder.BuildProcessStub = func(_ goci.Bndl, spec garden.ProcessSpec, _ *users.ExecUser) *specs.Process {
				return &specs.Process{Args: append([]string{spec.Path}, spec.Args...)}
			}
			backing := new(runcontainerdfakes.FakeBackingProcess)
			backing.WaitReturns(0, nil)
			containerManager.ExecReturns(backing, nil)

			Expect(runContainerd.StreamIn(log, "app-handle", garden.StreamInSpec{Path: "app", User: "vcap"})).To(Succeed())

			rootfs, uid, gid, _, _, paths := mkdirer.MkdirAsArgsForCall(0)
			Expect(rootfs).To(Equal("/rootfs"))
			Expect(uid).To(Equal(2000))
			Expect(gid).To(Equal(2000))
			Expect(paths).To(Equal([]string{"/home/vcap/app"}))

			_, _, _, process, _ := containerManager.ExecArgsForCall(0)
			Expect(process.Args).To(Equal([]string{"/tmp/garden-tar", "--no-same-permissions", "--no-same-owner", "-xf", "-", "-C", "/home/vcap/app"}))
		})
	})

	Describe("StopAll", func() {
		It("kills the container task through containerd", func() {
			Expect(runContainerd.StopAll(log, "app-handle", nil, true)).To(Succeed())
			Expect(taskManager.killed).To(Equal([]string{"app-handle"}))
			Expect(taskManager.kill).To(Equal([]bool{true}))
		})
	})
})
