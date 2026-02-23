package main

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

type netnsConfig struct {
	NetnsName string
	Subnet    int
	Pooled    bool

	// Link between root netns and per-sandbox netns.
	VethHost   string
	VethNS     string
	VethCIDR   string
	VethHostIP string
	VethNSIP   string

	// Guest subnet (tap<->guest) inside the sandbox netns.
	TapName    string
	SubnetCIDR string
	HostIP     string
	GuestIP    string
}

func netnsNameForSandbox(id string) string {
	// ip netns names are also used as filenames in /var/run/netns.
	// Keep it simple and reasonably short.
	id = strings.TrimSpace(id)
	if id == "" {
		return "manta-unknown"
	}
	name := "manta-" + id
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

func setupSandboxNetnsAndRouting(id string, subnet int) (*netnsConfig, error) {
	ns := netnsNameForSandbox(id)

	// Use stable interface names inside the sandbox netns so the Firecracker
	// snapshot can refer to them.
	// Use the subnet as the unique key to avoid name collisions from truncation.
	vethHost := fmt.Sprintf("veth%03d", subnet)
	vethNS := "veth0"
	tap := "tap0"

	// Dedicated point-to-point link between root netns and sandbox netns.
	// Keep it disjoint from the 172.16/12 guest subnets.
	vethHostIP := fmt.Sprintf("10.200.%d.1", subnet)
	vethNSIP := fmt.Sprintf("10.200.%d.2", subnet)
	vethCIDR := fmt.Sprintf("10.200.%d.0/30", subnet)

	hostIP := fmt.Sprintf("172.16.%d.1", subnet)
	guestIP := fmt.Sprintf("172.16.%d.2", subnet)
	subnetCIDR := fmt.Sprintf("172.16.%d.0/30", subnet)

	// netns.NewNamed changes the current thread's network namespace. Make sure
	// we restore the original namespace before doing any "root" netlink work.
	runtime.LockOSThread()
	origNS, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("get current netns: %w", err)
	}
	nsHandle, err := netns.NewNamed(ns)
	if err != nil {
		_ = origNS.Close()
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("create netns %q: %w", ns, err)
	}
	// Restore the original netns for this thread before unlocking.
	if err := netns.Set(origNS); err != nil {
		_ = nsHandle.Close()
		_ = origNS.Close()
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("restore original netns after create: %w", err)
	}
	_ = origNS.Close()
	runtime.UnlockOSThread()
	defer nsHandle.Close()

	rootHandle, err := netlink.NewHandle()
	if err != nil {
		_ = netns.DeleteNamed(ns)
		return nil, fmt.Errorf("netlink root handle: %w", err)
	}
	defer rootHandle.Delete()

	// Create veth pair in root namespace.
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: vethHost},
		PeerName:  vethNS,
	}
	if err := rootHandle.LinkAdd(veth); err != nil {
		_ = netns.DeleteNamed(ns)
		return nil, fmt.Errorf("create veth pair: %w", err)
	}
	cleanupVeth := true
	defer func() {
		if cleanupVeth {
			if l, lerr := rootHandle.LinkByName(vethHost); lerr == nil {
				_ = rootHandle.LinkDel(l)
			}
		}
	}()

	// Move the peer into the sandbox netns.
	peer, err := rootHandle.LinkByName(vethNS)
	if err != nil {
		_ = netns.DeleteNamed(ns)
		return nil, fmt.Errorf("lookup veth peer: %w", err)
	}
	if err := rootHandle.LinkSetNsFd(peer, int(nsHandle)); err != nil {
		_ = netns.DeleteNamed(ns)
		return nil, fmt.Errorf("move veth peer into netns: %w", err)
	}

	// Configure root side.
	rootVeth, err := rootHandle.LinkByName(vethHost)
	if err != nil {
		_ = netns.DeleteNamed(ns)
		return nil, fmt.Errorf("lookup veth host: %w", err)
	}
	addrHost, err := netlink.ParseAddr(vethHostIP + "/30")
	if err != nil {
		_ = netns.DeleteNamed(ns)
		return nil, err
	}
	if err := rootHandle.AddrAdd(rootVeth, addrHost); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("assign veth host ip: %w", err)
	}
	if err := rootHandle.LinkSetUp(rootVeth); err != nil {
		return nil, fmt.Errorf("set veth host up: %w", err)
	}

	// Configure netns side (veth + routes + tap). TUN/TAP creation uses ioctls
	// on /dev/net/tun, which operate in the *current thread's* network namespace,
	// not the netlink handle's namespace. Do this work inside withNetns.
	if err := withNetns(nsHandle, func() error {
		nsHandleNL, herr := netlink.NewHandle()
		if herr != nil {
			return fmt.Errorf("netlink netns handle: %w", herr)
		}
		defer nsHandleNL.Delete()

		nsVeth, herr := nsHandleNL.LinkByName(vethNS)
		if herr != nil {
			return fmt.Errorf("lookup veth in netns: %w", herr)
		}

		addrNS, herr := netlink.ParseAddr(vethNSIP + "/30")
		if herr != nil {
			return herr
		}
		if herr := nsHandleNL.AddrAdd(nsVeth, addrNS); herr != nil && !os.IsExist(herr) {
			return fmt.Errorf("assign veth ns ip: %w", herr)
		}
		if herr := nsHandleNL.LinkSetUp(nsVeth); herr != nil {
			return fmt.Errorf("set veth ns up: %w", herr)
		}

		if herr := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); herr != nil {
			return fmt.Errorf("enable ip_forward in netns: %w", herr)
		}
		gw := net.ParseIP(vethHostIP)
		if gw == nil {
			return fmt.Errorf("invalid gw ip: %q", vethHostIP)
		}
		if herr := nsHandleNL.RouteReplace(&netlink.Route{LinkIndex: nsVeth.Attrs().Index, Gw: gw}); herr != nil {
			return fmt.Errorf("set netns default route: %w", herr)
		}

		// Create sandbox netns tap and host endpoint IP for guest traffic.
		tapLink := &netlink.Tuntap{
			LinkAttrs: netlink.LinkAttrs{Name: tap},
			Mode:      netlink.TUNTAP_MODE_TAP,
			// Firecracker expects a TAP backend with NO_PI and typically VNET_HDR.
			// Keep it single-queue; Firecracker config controls queueing separately.
			Flags:  netlink.TUNTAP_NO_PI | netlink.TUNTAP_VNET_HDR | netlink.TUNTAP_ONE_QUEUE,
			Queues: 0, // legacy branch (single queue) with our explicit Flags
		}
		if herr := nsHandleNL.LinkAdd(tapLink); herr != nil {
			return fmt.Errorf("create tap: %w", herr)
		}
		nsTap, herr := nsHandleNL.LinkByName(tap)
		if herr != nil {
			return fmt.Errorf("lookup tap: %w", herr)
		}
		tapAddr, herr := netlink.ParseAddr(hostIP + "/30")
		if herr != nil {
			return herr
		}
		if herr := nsHandleNL.AddrAdd(nsTap, tapAddr); herr != nil && !os.IsExist(herr) {
			return fmt.Errorf("assign tap ip: %w", herr)
		}
		if herr := nsHandleNL.LinkSetUp(nsTap); herr != nil {
			return fmt.Errorf("set tap up: %w", herr)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// Route guest /30 subnet to the sandbox netns.
	_, dst, err := net.ParseCIDR(subnetCIDR)
	if err != nil {
		return nil, err
	}
	via := net.ParseIP(vethNSIP)
	if via == nil {
		return nil, fmt.Errorf("invalid veth ns ip: %q", vethNSIP)
	}
	if err := rootHandle.RouteReplace(&netlink.Route{
		LinkIndex: rootVeth.Attrs().Index,
		Dst:       dst,
		Gw:        via,
	}); err != nil {
		return nil, fmt.Errorf("add route to guest subnet: %w", err)
	}

	cleanupVeth = false

	return &netnsConfig{
		NetnsName:  ns,
		Subnet:     subnet,
		VethHost:   vethHost,
		VethNS:     vethNS,
		VethCIDR:   vethCIDR,
		VethHostIP: vethHostIP,
		VethNSIP:   vethNSIP,
		TapName:    tap,
		SubnetCIDR: subnetCIDR,
		HostIP:     hostIP,
		GuestIP:    guestIP,
	}, nil
}

func cleanupSandboxNetnsAndRouting(cfg config, nc *netnsConfig) error {
	if nc == nil {
		return nil
	}

	var errs []string

	// Best-effort cleanup. Order matters a bit:
	// - Remove route in root netns
	// - Delete veth host (removes peer)
	// - Delete netns

	rootHandle, err := netlink.NewHandle()
	if err == nil {
		if _, dst, perr := net.ParseCIDR(nc.SubnetCIDR); perr == nil {
			_ = rootHandle.RouteDel(&netlink.Route{Dst: dst})
		}
		if l, lerr := rootHandle.LinkByName(nc.VethHost); lerr == nil {
			_ = rootHandle.LinkDel(l)
		}
		rootHandle.Delete()
	} else {
		errs = append(errs, fmt.Sprintf("netlink root handle: %v", err))
	}

	// netns deletion removes any remaining in-netns links.
	if err := netns.DeleteNamed(nc.NetnsName); err != nil {
		errs = append(errs, fmt.Sprintf("remove netns: %v", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
