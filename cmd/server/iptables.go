package main

import (
	"fmt"
	"strings"
)

// ensureGlobalMasquerade installs one broad MASQUERADE rule so sandbox creation
// doesn't need to add/remove iptables rules on the hot path.
//
// This intentionally leaves the rule installed for the server lifetime.
func ensureGlobalMasquerade(hostIface string) error {
	if strings.TrimSpace(hostIface) == "" {
		return fmt.Errorf("host iface is empty")
	}

	// All guest subnets are within 172.16.0.0/16.
	const guestCIDR = "172.16.0.0/16"

	// iptables -C returns non-zero if rule is missing.
	_, _, err := runCmd("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", guestCIDR, "-o", hostIface, "-j", "MASQUERADE")
	if err == nil {
		return nil
	}

	// Best effort: add the rule once.
	if _, _, aerr := runCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", guestCIDR, "-o", hostIface, "-j", "MASQUERADE"); aerr != nil {
		return fmt.Errorf("add global MASQUERADE rule: %w", aerr)
	}
	return nil
}
