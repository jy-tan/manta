package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

func logStartupDiagnostics(cfg config) {
	reflinkOK, reflinkErr := probeReflinkSupport(cfg.WorkDir)
	log.Printf("startup diagnostics:")
	log.Printf("- runtime: listen_addr=%s host_iface=%s work_dir=%s", cfg.ListenAddr, cfg.HostNATIface, cfg.WorkDir)
	log.Printf("- features: snapshots_enabled=%t netns_pool_size=%d cgroups_enabled=%t", cfg.EnableSnapshots, cfg.NetnsPoolSize, cfg.EnableCgroups)
	log.Printf("- storage: rootfs_clone_mode=%s", cfg.RootfsCloneMode)
	log.Printf("- diagnostics: stage_timing_logs=%t", cfg.EnableStageTimingLogs)
	if reflinkErr != nil {
		log.Printf("- storage: reflink_probe_error=%v", reflinkErr)
		return
	}
	log.Printf("- storage: reflink_supported=%t", reflinkOK)
	if cfg.EnableSnapshots && !reflinkOK {
		log.Printf("- warning: reflink probe failed for work dir; snapshot disk materialization may fall back to full copy unless MANTA_ROOTFS_CLONE_MODE=reflink-required")
	}
}

func probeReflinkSupport(workDir string) (bool, error) {
	probeDir := filepath.Join(workDir, ".reflink-probe")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		return false, fmt.Errorf("create probe dir: %w", err)
	}
	defer os.RemoveAll(probeDir)

	src := filepath.Join(probeDir, "src")
	dst := filepath.Join(probeDir, "dst")
	if err := os.WriteFile(src, []byte("probe\n"), 0o644); err != nil {
		return false, fmt.Errorf("write probe src: %w", err)
	}
	_, _, err := runCmd("cp", "--reflink=always", src, dst)
	if err != nil {
		return false, nil
	}
	return true, nil
}
