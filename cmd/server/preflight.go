package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

func ensurePreflight(cfg config) error {
	if _, err := exec.LookPath(cfg.FirecrackerBin); err != nil {
		return fmt.Errorf("firecracker binary not found: %w", err)
	}

	for _, p := range []string{cfg.KernelPath, cfg.BaseRootfsPath, cfg.SSHPrivateKey} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("required file %q missing: %w", p, err)
		}
	}

	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("/dev/kvm unavailable: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(cfg.WorkDir, "sandboxes"), 0o755); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}

	if _, _, err := runCmd("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}

	// Ensure NAT is configured once so sandbox creation doesn't churn iptables.
	// This is intentionally a broad rule covering all guest subnets.
	if err := ensureGlobalMasquerade(cfg.HostNATIface); err != nil {
		return fmt.Errorf("ensure global MASQUERADE: %w", err)
	}

	if cfg.EnableCgroups {
		if err := ensureCgroupRoot(cfg.CgroupRoot); err != nil {
			log.Printf("cgroups disabled (falling back to process groups only): %v", err)
		} else {
			scavengeCgroups(cfg.CgroupRoot)
		}
	}

	if cfg.EnableSnapshots {
		if _, err := ensureSnapshot(cfg); err != nil {
			return fmt.Errorf("ensure snapshot: %w", err)
		}
	}

	return nil
}
