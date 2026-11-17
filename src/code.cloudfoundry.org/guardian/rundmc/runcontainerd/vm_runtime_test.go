package runcontainerd_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/guardian/idmapper"
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

		It("sizes the sandbox VM from the container's memory limit", func() {
			limit := int64(1024 * 1024 * 1024)
			bundle := goci.Bndl{Spec: specs.Spec{Linux: &specs.Linux{Resources: &specs.LinuxResources{Memory: &specs.LinuxMemory{Limit: &limit}}}}}
			Expect(runContainerd.Create(log, "app-handle", bundle, garden.ProcessIO{})).To(Succeed())

			_, _, spec, _, _, _ := containerManager.CreateArgsForCall(0)
			Expect(spec.Annotations).To(HaveKeyWithValue(runcontainerd.SandboxMemoryAnnotation, "1073741824"))
		})

		It("does not set a sandbox size without a memory limit", func() {
			bundle := goci.Bndl{Spec: specs.Spec{Linux: &specs.Linux{}}}
			Expect(runContainerd.Create(log, "app-handle", bundle, garden.ProcessIO{})).To(Succeed())

			_, _, spec, _, _, _ := containerManager.CreateArgsForCall(0)
			Expect(spec.Annotations).NotTo(HaveKey(runcontainerd.SandboxMemoryAnnotation))
		})

		It("does not size the sandbox from peas", func() {
			limit := int64(64 * 1024 * 1024)
			bundle := goci.Bndl{Spec: specs.Spec{
				Linux:       &specs.Linux{Resources: &specs.LinuxResources{Memory: &specs.LinuxMemory{Limit: &limit}}},
				Annotations: map[string]string{"container-type": "pea", "sandbox-container": "app-handle"},
			}}
			Expect(runContainerd.Create(log, "envoy-pea", bundle, garden.ProcessIO{})).To(Succeed())

			_, _, spec, _, _, _ := containerManager.CreateArgsForCall(0)
			Expect(spec.Annotations).NotTo(HaveKey(runcontainerd.SandboxMemoryAnnotation))
		})

		It("removes the user namespace and id mappings, which the VM runtime does not support, but keeps the container unprivileged", func() {
			bundle := goci.Bndl{Spec: specs.Spec{Linux: &specs.Linux{}}}.
				WithNamespaces(goci.NetworkNamespace, goci.UserNamespace, goci.PIDNamespace).
				WithUIDMappings(specs.LinuxIDMapping{ContainerID: 0, HostID: 4294967294, Size: 1}).
				WithGIDMappings(specs.LinuxIDMapping{ContainerID: 0, HostID: 4294967294, Size: 1})
			Expect(runContainerd.Create(log, "app-handle", bundle, garden.ProcessIO{})).To(Succeed())

			_, _, spec, rootUID, rootGID, _ := containerManager.CreateArgsForCall(0)
			Expect(spec.Linux.Namespaces).NotTo(ContainElement(goci.UserNamespace))
			Expect(spec.Linux.Namespaces).To(ContainElements(goci.NetworkNamespace, goci.PIDNamespace))
			Expect(spec.Linux.UIDMappings).To(BeEmpty())
			Expect(spec.Linux.GIDMappings).To(BeEmpty())
			Expect(rootUID).To(BeEquivalentTo(0))
			Expect(rootGID).To(BeEquivalentTo(0))
			Expect(spec.Annotations).To(HaveKeyWithValue(goci.UnprivilegedAnnotation, "true"))
			Expect(goci.IsPrivileged(goci.Bndl{Spec: *spec})).To(BeFalse())
		})

		It("does not mark privileged containers (no user namespace) as unprivileged", func() {
			bundle := goci.Bndl{Spec: specs.Spec{Linux: &specs.Linux{}}}.WithNamespaces(goci.NetworkNamespace)
			Expect(runContainerd.Create(log, "priv-handle", bundle, garden.ProcessIO{})).To(Succeed())

			_, _, spec, _, _, _ := containerManager.CreateArgsForCall(0)
			Expect(spec.Annotations).NotTo(HaveKey(goci.UnprivilegedAnnotation))
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

		It("drops the console size of processes without a terminal (Kata would try to resize a non-existent tty)", func() {
			processBuilder.BuildProcessReturns(&specs.Process{Terminal: false, ConsoleSize: &specs.Box{Width: 80, Height: 24}})
			_, err := runContainerd.Exec(log, "app-handle", garden.ProcessSpec{User: "vcap"}, garden.ProcessIO{})
			Expect(err).NotTo(HaveOccurred())

			_, _, _, process, _ := containerManager.ExecArgsForCall(0)
			Expect(process.ConsoleSize).To(BeNil())
		})

		It("keeps the console size of terminal processes", func() {
			processBuilder.BuildProcessReturns(&specs.Process{Terminal: true, ConsoleSize: &specs.Box{Width: 80, Height: 24}})
			_, err := runContainerd.Exec(log, "app-handle", garden.ProcessSpec{User: "vcap", TTY: &garden.TTYSpec{}}, garden.ProcessIO{})
			Expect(err).NotTo(HaveOccurred())

			_, _, _, process, _ := containerManager.ExecArgsForCall(0)
			Expect(process.ConsoleSize).To(Equal(&specs.Box{Width: 80, Height: 24}))
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

		Context("with a container rootfs", func() {
			var (
				rootfs      string
				archiveSeen []byte
				execIO      func() (io.Reader, io.Writer, io.Writer, bool)
			)

			BeforeEach(func() {
				rootfs = GinkgoT().TempDir()
				containerManager.SpecReturns(&specs.Spec{Root: &specs.Root{Path: rootfs}, Linux: &specs.Linux{}}, nil)
				userLookupper.LookupReturns(&users.ExecUser{Uid: os.Getuid(), Gid: os.Getgid(), Home: "/home/vcap"}, nil)
				processBuilder.BuildProcessStub = func(_ goci.Bndl, spec garden.ProcessSpec, _ *users.ExecUser) *specs.Process {
					return &specs.Process{Args: append([]string{spec.Path}, spec.Args...)}
				}
				containerManager.ExecStub = func(_ lager.Logger, _, _ string, process *specs.Process, pio func() (io.Reader, io.Writer, io.Writer, bool)) (runcontainerd.BackingProcess, error) {
					execIO = pio
					// the guest would run tar on the shared archive file
					for i, arg := range process.Args {
						if (arg == "-xf" || arg == "-cf") && i+1 < len(process.Args) {
							archive := filepath.Join(rootfs, process.Args[i+1])
							if arg == "-xf" {
								archiveSeen, _ = os.ReadFile(archive)
							} else {
								Expect(os.WriteFile(archive, []byte("tar-out"), 0600)).To(Succeed())
							}
						}
					}
					backing := new(runcontainerdfakes.FakeBackingProcess)
					backing.WaitReturns(0, nil)
					return backing, nil
				}
			})

			It("writes the stream to an archive file in the rootfs, extracts it with the bind mounted tar without stdin and removes it", func() {
				Expect(runContainerd.StreamIn(log, "app-handle", garden.StreamInSpec{Path: "app", User: "vcap", TarStream: strings.NewReader("tar-in")})).To(Succeed())

				mkdirRootfs, _, _, _, _, paths := mkdirer.MkdirAsArgsForCall(0)
				Expect(mkdirRootfs).To(Equal(rootfs))
				Expect(paths).To(Equal([]string{"/home/vcap/app"}))

				_, _, _, process, _ := containerManager.ExecArgsForCall(0)
				Expect(process.Args[0]).To(Equal("/tmp/garden-tar"))
				Expect(process.Args[1:4]).To(Equal([]string{"--no-same-permissions", "--no-same-owner", "-xf"}))
				Expect(process.Args[4]).To(MatchRegexp(`^/\.garden-stream-.*\.tar$`))
				Expect(process.Args[5:]).To(Equal([]string{"-C", "/home/vcap/app"}))
				Expect(string(archiveSeen)).To(Equal("tar-in"))

				stdin, _, _, _ := execIO()
				Expect(stdin).To(BeNil())

				entries, err := os.ReadDir(rootfs)
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(BeEmpty())
			})

			It("creates files owned by the shifted ids when the rootfs ids are translated", func() {
				runContainerd.EnableVMRuntime(runcontainerd.VMRuntimeConfig{
					TarPathInContainer: "/tmp/garden-tar",
					RootfsUIDMappings:  idmapper.MappingList{{ContainerID: 0, HostID: 4294967294, Size: 1}, {ContainerID: 1, HostID: 1, Size: 4294967293}},
					RootfsGIDMappings:  idmapper.MappingList{{ContainerID: 0, HostID: 4294967294, Size: 1}, {ContainerID: 1, HostID: 1, Size: 4294967293}},
				}, taskManager)
				userLookupper.LookupReturns(&users.ExecUser{Uid: 0, Gid: 0, Home: "/root"}, nil)

				Expect(runContainerd.StreamIn(log, "app-handle", garden.StreamInSpec{Path: "/app", User: "root", TarStream: strings.NewReader("tar-in")})).To(Succeed())

				_, uid, gid, _, _, _ := mkdirer.MkdirAsArgsForCall(0)
				Expect(uid).To(Equal(4294967294))
				Expect(gid).To(Equal(4294967294))
			})

			It("creates the archive in the guest and streams it from the rootfs, removing it on close", func() {
				stream, err := runContainerd.StreamOut(log, "app-handle", garden.StreamOutSpec{Path: "/home/vcap/app/", User: "vcap"})
				Expect(err).NotTo(HaveOccurred())

				_, _, _, process, _ := containerManager.ExecArgsForCall(0)
				Expect(process.Args[3]).To(Equal("-cf"))
				Expect(process.Args[5:]).To(Equal([]string{"-C", "/home/vcap/app/", "."}))

				content, err := io.ReadAll(stream)
				Expect(err).NotTo(HaveOccurred())
				Expect(string(content)).To(Equal("tar-out"))
				Expect(stream.Close()).To(Succeed())

				entries, err := os.ReadDir(rootfs)
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(BeEmpty())
			})
		})
	})

	Describe("Delete", func() {
		var reaper *runcontainerdfakes.FakeVMSandboxReaper

		BeforeEach(func() {
			reaper = new(runcontainerdfakes.FakeVMSandboxReaper)
			runContainerd.EnableVMRuntime(runcontainerd.VMRuntimeConfig{TarPathInContainer: "/tmp/garden-tar", Reaper: reaper}, taskManager)
			containerManager.BundleIDsStub = func(filters ...runcontainerd.ContainerFilter) ([]string, error) {
				if len(filters) == 2 && filters[1].Value == "app-handle" {
					return []string{"app-handle-envoy", "app-handle-liveness-healthcheck-0"}, nil
				}
				return nil, nil
			}
		})

		It("removes the peas of the sandbox before the sandbox, then reaps leftovers", func() {
			Expect(runContainerd.Delete(log, "app-handle")).To(Succeed())

			Expect(containerManager.DeleteCallCount()).To(Equal(3))
			_, first := containerManager.DeleteArgsForCall(0)
			_, second := containerManager.DeleteArgsForCall(1)
			_, last := containerManager.DeleteArgsForCall(2)
			Expect([]string{first, second}).To(ConsistOf("app-handle-envoy", "app-handle-liveness-healthcheck-0"))
			Expect(last).To(Equal("app-handle"))

			Expect(containerManager.RemoveBundleCallCount()).To(Equal(2))
			Expect(reaper.ReapCallCount()).To(Equal(1))
			_, reaped := reaper.ReapArgsForCall(0)
			Expect(reaped).To(Equal("app-handle"))
		})

		It("keeps going when a pea cannot be deleted", func() {
			containerManager.DeleteStub = func(_ lager.Logger, id string) error {
				if id == "app-handle-envoy" {
					return errors.New("shim gone")
				}
				return nil
			}
			Expect(runContainerd.Delete(log, "app-handle")).To(Succeed())
			Expect(containerManager.RemoveBundleCallCount()).To(Equal(2))
		})

		It("retries the delete after reaping when it failed", func() {
			containerManager.DeleteStub = func(_ lager.Logger, id string) error {
				if id == "app-handle" && reaper.ReapCallCount() == 0 {
					return errors.New("vmm exit unconfirmed")
				}
				return nil
			}
			Expect(runContainerd.Delete(log, "app-handle")).To(Succeed())
			Expect(containerManager.DeleteCallCount()).To(Equal(4))
		})

		It("fails when leftovers cannot be reaped", func() {
			reaper.ReapReturns(errors.New("still alive"))
			Expect(runContainerd.Delete(log, "app-handle")).To(MatchError("still alive"))
		})
	})

	Describe("RemoveOrphanedPeas", func() {
		It("removes peas whose sandbox no longer exists and returns them", func() {
			containerManager.BundleIDsStub = func(filters ...runcontainerd.ContainerFilter) ([]string, error) {
				switch {
				case len(filters) == 1 && filters[0].ComparisonOp == "==":
					return []string{"live-envoy", "gone-envoy", "gone-liveness-healthcheck-0"}, nil
				case len(filters) == 1:
					return []string{"live"}, nil
				case filters[1].Value == "live":
					return []string{"live-envoy"}, nil
				}
				return nil, nil
			}

			orphans, err := runContainerd.RemoveOrphanedPeas(log)
			Expect(err).NotTo(HaveOccurred())
			Expect(orphans).To(ConsistOf("gone-envoy", "gone-liveness-healthcheck-0"))
			Expect(containerManager.RemoveBundleCallCount()).To(Equal(2))
			for i := 0; i < containerManager.RemoveBundleCallCount(); i++ {
				_, id := containerManager.RemoveBundleArgsForCall(i)
				Expect(id).NotTo(Equal("live-envoy"))
			}
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
