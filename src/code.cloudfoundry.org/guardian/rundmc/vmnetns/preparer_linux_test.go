//go:build linux

package vmnetns_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"code.cloudfoundry.org/guardian/rundmc/vmnetns"
	"code.cloudfoundry.org/lager/v3/lagertest"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// Requires root (CAP_SYS_ADMIN + CAP_NET_ADMIN), e.g. a privileged container.
func TestPrepareAdjustRelease(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	log := lagertest.NewTestLogger("vmnetns")
	dir := t.TempDir()
	preparer := vmnetns.New(dir)

	pid, nsPath, done, err := preparer.Prepare(log, "handle-1")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if nsPath != filepath.Join(dir, "handle-1") || pid == 0 {
		t.Fatalf("unexpected prepare result: pid=%d path=%s", pid, nsPath)
	}

	// configure the namespace the way silk does: veth with a /32 address and
	// a point-to-point peer, plus a default route via the peer
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "vmnetns-h"}, PeerName: "vmnetns-c"}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("veth: %v", err)
	}
	defer func() { _ = netlink.LinkDel(veth) }()

	container, err := netlink.LinkByName("vmnetns-c")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	nsHandle, err := netns.GetFromPath(nsPath)
	if err != nil {
		t.Fatalf("netns: %v", err)
	}
	defer nsHandle.Close()
	if err := netlink.LinkSetNsFd(container, int(nsHandle)); err != nil {
		t.Fatalf("move link: %v", err)
	}

	nl, err := netlink.NewHandleAt(nsHandle)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	defer nl.Close()

	container, _ = nl.LinkByName("vmnetns-c")
	if err := nl.LinkSetUp(container); err != nil {
		t.Fatalf("up: %v", err)
	}
	addr := &netlink.Addr{
		IPNet: &net.IPNet{IP: net.ParseIP("10.255.0.2"), Mask: net.CIDRMask(32, 32)},
		Peer:  &net.IPNet{IP: net.ParseIP("169.254.0.1"), Mask: net.CIDRMask(32, 32)},
		Scope: unix.RT_SCOPE_LINK,
	}
	if err := nl.AddrAdd(container, addr); err != nil {
		t.Fatalf("addr: %v", err)
	}
	_, defaultDst, _ := net.ParseCIDR("0.0.0.0/0")
	if err := nl.RouteAdd(&netlink.Route{Dst: defaultDst, Gw: net.ParseIP("169.254.0.1"), Src: net.ParseIP("10.255.0.2")}); err != nil {
		t.Fatalf("default route: %v", err)
	}

	peerMAC, _ := net.ParseMAC("aa:aa:0a:ff:00:02")
	if err := nl.NeighAdd(&netlink.Neigh{
		LinkIndex:    container.Attrs().Index,
		Family:       netlink.FAMILY_V4,
		State:        netlink.NUD_PERMANENT,
		IP:           net.ParseIP("169.254.0.1"),
		HardwareAddr: peerMAC,
	}); err != nil {
		t.Fatalf("neigh: %v", err)
	}

	done()

	if err := preparer.AdjustForRuntime(log, "handle-1"); err != nil {
		t.Fatalf("adjust: %v", err)
	}

	routes, err := nl.RouteList(container, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	foundPeer, foundDefault := false, false
	for _, r := range routes {
		if r.Dst != nil && r.Dst.IP.Equal(net.ParseIP("169.254.0.1")) {
			foundPeer = true
			if r.Protocol != unix.RTPROT_BOOT {
				t.Fatalf("peer route has protocol %d, want boot", r.Protocol)
			}
			if !r.Src.Equal(net.ParseIP("10.255.0.2")) {
				t.Fatalf("peer route src %s", r.Src)
			}
		}
		if (r.Dst == nil || r.Dst.String() == "0.0.0.0/0") && r.Gw.Equal(net.ParseIP("169.254.0.1")) {
			foundDefault = true
		}
	}
	if !foundPeer || !foundDefault {
		t.Fatalf("peer route (%v) or default route (%v) missing: %+v", foundPeer, foundDefault, routes)
	}

	// Kata runtime-rs takes IFA_ADDRESS, which is the peer for point-to-point
	// addresses: the address must now be a plain /32 without peer
	addrs, err := nl.AddrList(container, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("addrs: %v", err)
	}
	if len(addrs) != 1 || !addrs[0].IP.Equal(net.ParseIP("10.255.0.2")) || addrs[0].Peer != nil {
		t.Fatalf("expected plain address 10.255.0.2/32, got %+v", addrs)
	}

	// silk's static ARP entry for the peer must survive the address change
	neighs, err := nl.NeighList(container.Attrs().Index, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("neighs: %v", err)
	}
	foundNeigh := false
	for _, n := range neighs {
		if n.IP.Equal(net.ParseIP("169.254.0.1")) && n.HardwareAddr.String() == peerMAC.String() && n.State&netlink.NUD_PERMANENT != 0 {
			foundNeigh = true
		}
	}
	if !foundNeigh {
		t.Fatalf("permanent neighbour for the peer missing: %+v", neighs)
	}

	// idempotent (no point-to-point address left)
	if err := preparer.AdjustForRuntime(log, "handle-1"); err != nil {
		t.Fatalf("second adjust: %v", err)
	}

	// the netns must survive the holder process (done() was called)
	if _, err := netns.GetFromPath(nsPath); err != nil {
		t.Fatalf("netns gone after holder exit: %v", err)
	}

	if err := preparer.Release(log, "handle-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(nsPath); !os.IsNotExist(err) {
		t.Fatalf("netns path not removed: %v", err)
	}
	if err := preparer.Release(log, "handle-1"); err != nil {
		t.Fatalf("release must be idempotent: %v", err)
	}
}
