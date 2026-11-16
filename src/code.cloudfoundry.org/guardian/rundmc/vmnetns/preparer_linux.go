//go:build linux

// Package vmnetns prepares container network namespaces before a container is
// created. VM based runtimes such as Kata Containers scan the network
// namespace only once, when the VM boots, so the network (veth, IPs, routes)
// must already be configured at that point.
package vmnetns

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"code.cloudfoundry.org/lager/v3"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

var sleepBinaryCandidates = []string{"/bin/sleep", "/usr/bin/sleep"}

// BindMountPreparer creates a new network namespace by starting a short-lived
// holder process in a fresh netns and bind-mounting that namespace to
// <Dir>/<handle>, which gives the OCI spec a stable namespace path that
// outlives the holder process.
type BindMountPreparer struct {
	Dir string
}

func New(dir string) *BindMountPreparer {
	return &BindMountPreparer{Dir: dir}
}

func (p *BindMountPreparer) nsPath(handle string) string {
	return filepath.Join(p.Dir, handle)
}

func (p *BindMountPreparer) Prepare(log lager.Logger, handle string) (int, string, func(), error) {
	log = log.Session("prepare-netns", lager.Data{"handle": handle})

	if err := os.MkdirAll(p.Dir, 0700); err != nil {
		return 0, "", nil, fmt.Errorf("creating netns dir: %w", err)
	}

	sleepBin, err := findSleep()
	if err != nil {
		return 0, "", nil, err
	}

	holder := exec.Command(sleepBin, "infinity")
	holder.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
	if err := holder.Start(); err != nil {
		return 0, "", nil, fmt.Errorf("starting netns holder: %w", err)
	}

	stopHolder := func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	}

	nsPath := p.nsPath(handle)
	f, err := os.OpenFile(nsPath, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0400)
	if err != nil {
		stopHolder()
		return 0, "", nil, fmt.Errorf("creating netns bind mount target: %w", err)
	}
	_ = f.Close()

	holderNetns := fmt.Sprintf("/proc/%d/ns/net", holder.Process.Pid)
	if err := unix.Mount(holderNetns, nsPath, "", unix.MS_BIND, ""); err != nil {
		stopHolder()
		_ = os.Remove(nsPath)
		return 0, "", nil, fmt.Errorf("bind mounting netns: %w", err)
	}

	log.Info("prepared", lager.Data{"holder-pid": holder.Process.Pid, "netns-path": nsPath})
	return holder.Process.Pid, nsPath, stopHolder, nil
}

// AdjustForRuntime makes peer routes visible to the VM runtime.
//
// CNI plugins such as silk configure the container interface with a
// point-to-point address (e.g. 10.255.0.2/32 peer 169.254.0.1) and a default
// route via the peer. The route to the peer is created by the kernel
// (proto kernel). Kata Containers does not copy kernel routes into the guest,
// expecting the guest kernel to recreate them from the interface addresses,
// but its address model has no peer. Without the peer route the guest cannot
// add the default route ("Network is unreachable"). Re-tagging these peer
// routes as "proto boot" keeps them identical on the host and makes Kata
// replicate them in the guest.
func (p *BindMountPreparer) AdjustForRuntime(log lager.Logger, handle string) error {
	log = log.Session("adjust-netns", lager.Data{"handle": handle})

	nsHandle, err := netns.GetFromPath(p.nsPath(handle))
	if err != nil {
		return fmt.Errorf("opening netns: %w", err)
	}
	defer nsHandle.Close()

	nl, err := netlink.NewHandleAt(nsHandle)
	if err != nil {
		return fmt.Errorf("netlink handle in netns: %w", err)
	}
	defer nl.Close()

	links, err := nl.LinkList()
	if err != nil {
		return fmt.Errorf("listing links: %w", err)
	}

	for _, link := range links {
		if link.Attrs().Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := nl.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return fmt.Errorf("listing addresses of %s: %w", link.Attrs().Name, err)
		}

		routes, err := nl.RouteList(link, netlink.FAMILY_ALL)
		if err != nil {
			return fmt.Errorf("listing routes of %s: %w", link.Attrs().Name, err)
		}

		for _, route := range routes {
			if route.Protocol != unix.RTPROT_KERNEL || route.Dst == nil || !isPeer(route.Dst, addrs) {
				continue
			}

			adjusted := netlink.Route{
				LinkIndex: route.LinkIndex,
				Dst:       route.Dst,
				Src:       route.Src,
				Scope:     route.Scope,
				Table:     route.Table,
				Protocol:  unix.RTPROT_BOOT,
			}
			if err := nl.RouteReplace(&adjusted); err != nil {
				return fmt.Errorf("re-tagging peer route %s on %s: %w", route.Dst, link.Attrs().Name, err)
			}
			log.Info("retagged-peer-route", lager.Data{"link": link.Attrs().Name, "dst": route.Dst.String()})
		}
	}

	return nil
}

func isPeer(dst *net.IPNet, addrs []netlink.Addr) bool {
	for _, addr := range addrs {
		if addr.Peer != nil && addr.Peer.IP.Equal(dst.IP) {
			return true
		}
	}
	return false
}

func (p *BindMountPreparer) Release(log lager.Logger, handle string) error {
	nsPath := p.nsPath(handle)

	if err := unix.Unmount(nsPath, unix.MNT_DETACH); err != nil &&
		!errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("unmounting netns %s: %w", nsPath, err)
	}

	if err := os.Remove(nsPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing netns %s: %w", nsPath, err)
	}

	return nil
}

func findSleep() (string, error) {
	for _, candidate := range sleepBinaryCandidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("could not find a sleep binary for the netns holder process")
}
