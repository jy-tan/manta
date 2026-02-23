package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type netnsConfig struct {
	NetnsName string

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

func runCmdNetns(ns string, name string, args ...string) (string, string, error) {
	all := append([]string{"netns", "exec", ns, name}, args...)
	return runCmd("ip", all...)
}

func createNetns(ns string) error {
	_, _, err := runCmd("ip", "netns", "add", ns)
	return err
}

func deleteNetns(ns string) error {
	_, _, err := runCmd("ip", "netns", "del", ns)
	return err
}

func setupSandboxNetnsAndRouting(id string, subnet int, hostIface string) (*netnsConfig, error) {
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

	if err := createNetns(ns); err != nil {
		return nil, fmt.Errorf("create netns %q: %w", ns, err)
	}
	createdNS := true
	defer func() {
		if createdNS {
			_ = deleteNetns(ns)
		}
	}()

	// Create veth pair, move one end into the sandbox netns.
	if _, _, err := runCmd("ip", "link", "add", vethHost, "type", "veth", "peer", "name", vethNS); err != nil {
		return nil, fmt.Errorf("create veth pair: %w", err)
	}
	createdVeth := true
	defer func() {
		if createdVeth {
			_, _, _ = runCmd("ip", "link", "del", vethHost)
		}
	}()

	if _, _, err := runCmd("ip", "link", "set", vethNS, "netns", ns); err != nil {
		return nil, fmt.Errorf("move veth into netns: %w", err)
	}

	// Root side.
	if _, _, err := runCmd("ip", "addr", "add", vethHostIP+"/30", "dev", vethHost); err != nil {
		return nil, fmt.Errorf("assign veth host ip: %w", err)
	}
	if _, _, err := runCmd("ip", "link", "set", vethHost, "up"); err != nil {
		return nil, fmt.Errorf("set veth host up: %w", err)
	}

	// Netns side.
	if _, _, err := runCmdNetns(ns, "ip", "addr", "add", vethNSIP+"/30", "dev", vethNS); err != nil {
		return nil, fmt.Errorf("assign veth ns ip: %w", err)
	}
	if _, _, err := runCmdNetns(ns, "ip", "link", "set", vethNS, "up"); err != nil {
		return nil, fmt.Errorf("set veth ns up: %w", err)
	}
	if _, _, err := runCmdNetns(ns, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return nil, fmt.Errorf("enable ip_forward in netns: %w", err)
	}
	if _, _, err := runCmdNetns(ns, "ip", "route", "replace", "default", "via", vethHostIP, "dev", vethNS); err != nil {
		return nil, fmt.Errorf("set netns default route: %w", err)
	}

	// Route guest /30 subnet to the sandbox netns.
	if _, _, err := runCmd("ip", "route", "replace", subnetCIDR, "via", vethNSIP, "dev", vethHost); err != nil {
		return nil, fmt.Errorf("add route to guest subnet: %w", err)
	}

	// NAT guest subnet out of the host interface.
	if _, _, err := runCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", subnetCIDR, "-o", hostIface, "-j", "MASQUERADE"); err != nil {
		return nil, fmt.Errorf("add NAT rule: %w", err)
	}
	natAdded := true
	defer func() {
		if natAdded {
			_, _, _ = runCmd("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", subnetCIDR, "-o", hostIface, "-j", "MASQUERADE")
		}
	}()

	// Create sandbox netns tap and host endpoint IP for guest traffic.
	if _, _, err := runCmdNetns(ns, "ip", "tuntap", "add", tap, "mode", "tap"); err != nil {
		return nil, fmt.Errorf("create tap: %w", err)
	}
	if _, _, err := runCmdNetns(ns, "ip", "addr", "add", hostIP+"/30", "dev", tap); err != nil {
		return nil, fmt.Errorf("assign tap ip: %w", err)
	}
	if _, _, err := runCmdNetns(ns, "ip", "link", "set", tap, "up"); err != nil {
		return nil, fmt.Errorf("set tap up: %w", err)
	}

	createdNS = false
	createdVeth = false
	natAdded = false

	return &netnsConfig{
		NetnsName:  ns,
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
	// - Remove NAT + route in root netns
	// - Delete veth host (removes peer)
	// - Delete netns
	if _, _, err := runCmd("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", nc.SubnetCIDR, "-o", cfg.HostNATIface, "-j", "MASQUERADE"); err != nil {
		errs = append(errs, fmt.Sprintf("remove NAT rule: %v", err))
	}

	if _, _, err := runCmd("ip", "route", "del", nc.SubnetCIDR); err != nil {
		// Route may already be gone; ignore "No such process".
		if !strings.Contains(err.Error(), "No such process") && !strings.Contains(err.Error(), "No such file") {
			errs = append(errs, fmt.Sprintf("remove route: %v", err))
		}
	}

	if _, _, err := runCmd("ip", "link", "del", nc.VethHost); err != nil {
		// If the netns is already gone, the veth might have disappeared too.
		if !strings.Contains(err.Error(), "Cannot find device") {
			errs = append(errs, fmt.Sprintf("remove veth: %v", err))
		}
	}

	// netns deletion removes any remaining in-netns links.
	if err := deleteNetns(nc.NetnsName); err != nil {
		if !strings.Contains(err.Error(), "No such file") && !strings.Contains(err.Error(), "Cannot remove namespace") {
			errs = append(errs, fmt.Sprintf("remove netns: %v", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func sandboxPathJoin(dir, name string) string {
	// Helper for naming files inside a per-sandbox jail directory.
	return filepath.Join(dir, name)
}

func shortSleep() {
	time.Sleep(25 * time.Millisecond)
}
