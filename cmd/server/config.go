package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"manta/internal/agentrpc"
)

func loadConfig() (config, error) {
	cfg := config{
		ListenAddr:      envOr("MANTA_LISTEN_ADDR", ":8080"),
		KernelPath:      envOr("MANTA_KERNEL_PATH", "./guest-artifacts/vmlinux"),
		BaseRootfsPath:  envOr("MANTA_ROOTFS_PATH", "./guest-artifacts/rootfs.ext4"),
		RootfsCloneMode: strings.ToLower(strings.TrimSpace(envOr("MANTA_ROOTFS_CLONE_MODE", "auto"))),
		SSHPrivateKey:   envOr("MANTA_SSH_KEY_PATH", "./guest-artifacts/sandbox_key"),
		FirecrackerBin:  envOr("MANTA_FIRECRACKER_BIN", "firecracker"),
		// Dev default stays in-repo for reflink-friendly local benchmarking.
		// Canonical production location is /var/lib/manta.
		WorkDir:               envOr("MANTA_WORK_DIR", ".manta-work"),
		CgroupRoot:            envOr("MANTA_CGROUP_ROOT", "/sys/fs/cgroup/manta"),
		EnableCgroups:         intOr("MANTA_ENABLE_CGROUPS", 1) != 0,
		NetnsPoolSize:         intOr("MANTA_NETNS_POOL_SIZE", 64),
		EnableSnapshots:       intOr("MANTA_ENABLE_SNAPSHOTS", 1) != 0,
		KeepFailedSandboxes:   intOr("MANTA_DEBUG_KEEP_FAILED_SANDBOX", 0) != 0,
		EnableStageTimingLogs: intOr("MANTA_ENABLE_STAGE_TIMINGS", 0) != 0,
		ExecTransport:         strings.ToLower(strings.TrimSpace(envOr("MANTA_EXEC_TRANSPORT", "agent"))),

		AgentPort:        intOr("MANTA_AGENT_PORT", agentrpc.DefaultPort),
		AgentWaitTimeout: durationOr("MANTA_AGENT_WAIT_TIMEOUT", 30*time.Second),
		AgentDialTimeout: durationOr("MANTA_AGENT_DIAL_TIMEOUT", 250*time.Millisecond),
		AgentCallTimeout: durationOr("MANTA_AGENT_CALL_TIMEOUT", 20*time.Second),
		AgentMaxOutputB:  int64(intOr("MANTA_AGENT_MAX_OUTPUT_BYTES", 1<<20)),

		SSHWaitTimeout: durationOr("MANTA_SSH_WAIT_TIMEOUT", 30*time.Second),
		SSHDialTimeout: durationOr("MANTA_SSH_DIAL_TIMEOUT", 2*time.Second),
		SSHExecWait:    durationOr("MANTA_SSH_EXEC_WAIT_TIMEOUT", 20*time.Second),
		ExecTimeout:    durationOr("MANTA_EXEC_TIMEOUT", 20*time.Second),
		BootArgs: envOr(
			"MANTA_BOOT_ARGS",
			"console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/sbin/init",
		),
		DefaultMemMiB: intOr("MANTA_VM_MEM_MIB", 512),
		DefaultVCPU:   intOr("MANTA_VM_VCPU", 1),
	}

	// Firecracker is started with its working directory set to a per-sandbox
	// jail dir. Resolve any relative artifact paths now so they remain valid
	// regardless of cwd.
	for _, p := range []*string{&cfg.KernelPath, &cfg.BaseRootfsPath, &cfg.SSHPrivateKey, &cfg.WorkDir} {
		abs, err := filepath.Abs(*p)
		if err != nil {
			return cfg, fmt.Errorf("resolve path %q: %w", *p, err)
		}
		*p = abs
	}
	if cfg.EnableSnapshots {
		lineage, err := computeFileSHA256(cfg.BaseRootfsPath)
		if err != nil {
			return cfg, fmt.Errorf("compute base rootfs lineage: %w", err)
		}
		cfg.BaseRootfsLineageID = lineage
	}
	switch cfg.RootfsCloneMode {
	case "auto", "reflink-required":
		// ok
	default:
		return cfg, fmt.Errorf("invalid MANTA_ROOTFS_CLONE_MODE %q (expected auto or reflink-required)", cfg.RootfsCloneMode)
	}

	if cfg.HostNATIface = strings.TrimSpace(os.Getenv("MANTA_HOST_IFACE")); cfg.HostNATIface == "" {
		iface, err := detectDefaultInterface()
		if err != nil {
			return cfg, fmt.Errorf("detect default host interface: %w", err)
		}
		cfg.HostNATIface = iface
	}

	return cfg, nil
}

func detectDefaultInterface() (string, error) {
	out, _, err := runCmd("sh", "-c", "ip route show default | awk '{print $5; exit}'")
	if err != nil {
		return "", err
	}
	iface := strings.TrimSpace(out)
	if iface == "" {
		return "", fmt.Errorf("no default route interface found")
	}
	return iface, nil
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func intOr(name string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		parsed, err := strconv.Atoi(v)
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func durationOr(name string, fallback time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		parsed, err := time.ParseDuration(v)
		if err == nil {
			return parsed
		}
	}
	return fallback
}
