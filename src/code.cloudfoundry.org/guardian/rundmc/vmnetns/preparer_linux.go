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

// AdjustForRuntime makes the container network configuration replicable by
// the VM runtime.
//
// CNI plugins such as silk configure the container interface with a
// point-to-point address (e.g. 10.255.0.2/32 peer 169.254.0.1) and a default
// route via the peer. Kata Containers copies addresses and routes into the
// guest, but:
//   - its address model has no peer, and Kata runtime-rs even reads the peer
//     (IFA_ADDRESS) instead of the local address (IFA_LOCAL), so the guest
//     got the gateway's IP;
//   - it does not copy kernel routes (proto kernel), such as the route to the
//     peer, so the guest cannot add the default route via the peer.
//
// Each point-to-point address is therefore replaced by the same plain /32
// address plus an explicit link route to the peer ("proto boot"), and the
// other routes of the link, which the kernel removes together with the
// address, are restored together with its permanent neighbour entries
// (silk's static ARP entry for the peer). The resulting forwarding behaviour
// on the host is unchanged.
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
		if err := adjustLink(log, nl, link); err != nil {
			return fmt.Errorf("adjusting %s: %w", link.Attrs().Name, err)
		}
	}

	return nil
}

func adjustLink(log lager.Logger, nl *netlink.Handle, link netlink.Link) error {
	addrs, err := nl.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("listing addresses: %w", err)
	}

	var peerAddrs []netlink.Addr
	for _, addr := range addrs {
		if addr.Peer != nil {
			peerAddrs = append(peerAddrs, addr)
		}
	}
	if len(peerAddrs) == 0 {
		return nil
	}

	routes, err := nl.RouteList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("listing routes: %w", err)
	}
	var restore []netlink.Route
	for _, route := range routes {
		if route.Protocol == unix.RTPROT_KERNEL {
			continue
		}
		restore = append(restore, netlink.Route{
			LinkIndex: route.LinkIndex,
			Dst:       route.Dst,
			Gw:        route.Gw,
			Src:       route.Src,
			Scope:     route.Scope,
			Table:     route.Table,
			Priority:  route.Priority,
			MTU:       route.MTU,
			Protocol:  route.Protocol,
		})
	}

	// Removing the last address flushes the link's neighbours, including the
	// permanent entry for the peer that CNI plugins add on NOARP links.
	neighs, err := nl.NeighList(link.Attrs().Index, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("listing neighbours: %w", err)
	}

	for _, addr := range peerAddrs {
		plain := &netlink.Addr{
			IPNet: &net.IPNet{IP: addr.IP, Mask: addr.Mask},
			Scope: addr.Scope,
			Label: addr.Label,
		}
		if err := nl.AddrDel(link, &addr); err != nil {
			return fmt.Errorf("removing point-to-point address %s: %w", addr.IPNet, err)
		}
		if err := nl.AddrAdd(link, plain); err != nil {
			return fmt.Errorf("adding address %s: %w", plain.IPNet, err)
		}
		peerRoute := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       addr.Peer,
			Src:       addr.IP,
			Scope:     netlink.SCOPE_LINK,
			Protocol:  unix.RTPROT_BOOT,
		}
		if err := nl.RouteReplace(peerRoute); err != nil {
			return fmt.Errorf("adding route to peer %s: %w", addr.Peer, err)
		}
		log.Info("converted-point-to-point-address", lager.Data{"address": plain.IPNet.String(), "peer": addr.Peer.String()})
	}

	for i := range restore {
		if err := nl.RouteReplace(&restore[i]); err != nil {
			return fmt.Errorf("restoring route %s: %w", restore[i].String(), err)
		}
	}

	for _, neigh := range neighs {
		if neigh.State&(netlink.NUD_PERMANENT|netlink.NUD_NOARP) == 0 || neigh.HardwareAddr == nil {
			continue
		}
		restored := netlink.Neigh{
			LinkIndex:    neigh.LinkIndex,
			Family:       neigh.Family,
			State:        neigh.State,
			IP:           neigh.IP,
			HardwareAddr: neigh.HardwareAddr,
		}
		if err := nl.NeighSet(&restored); err != nil {
			return fmt.Errorf("restoring neighbour %s: %w", neigh.IP, err)
		}
	}

	return nil
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
