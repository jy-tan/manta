package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type snapshotPaths struct {
	Dir       string
	BaseDir   string
	BaseDisk  string
	StateFile string
	MemFile   string
	MetaFile  string
}

type snapshotMeta struct {
	Version        int    `json:"version"`
	LineageID      string `json:"lineage_id"`
	BaseRootfsPath string `json:"base_rootfs_path"`
	CreatedAt      string `json:"created_at"`
}

func snapshotLayout(workDir string) snapshotPaths {
	dir := filepath.Join(workDir, "snapshot")
	base := filepath.Join(dir, "base")
	return snapshotPaths{
		Dir:       dir,
		BaseDir:   base,
		BaseDisk:  filepath.Join(base, "rootfs.ext4"),
		StateFile: filepath.Join(dir, "state.snap"),
		MemFile:   filepath.Join(dir, "mem.snap"),
		MetaFile:  filepath.Join(dir, "meta.json"),
	}
}

func ensureSnapshot(cfg config) (snapshotPaths, error) {
	sp := snapshotLayout(cfg.WorkDir)

	// If snapshot files exist, validate lineage metadata to ensure restore
	// compatibility with the currently configured base rootfs.
	if fileExists(sp.StateFile) && fileExists(sp.MemFile) && fileExists(sp.BaseDisk) {
		if err := validateSnapshotMeta(sp, cfg); err == nil {
			return sp, nil
		} else {
			log.Printf("snapshot metadata mismatch; rebuilding snapshot: %v", err)
		}
		if err := resetSnapshotDir(sp); err != nil {
			return sp, err
		}
	}

	if err := os.MkdirAll(sp.BaseDir, 0o755); err != nil {
		return sp, fmt.Errorf("create snapshot dir: %w", err)
	}

	// Prepare base disk which will be used for the snapshot and for per-sandbox
	// reflink clones. This disk must remain immutable after snapshot creation.
	if err := materializeSandboxRootfs(cfg, cfg.BaseRootfsPath, sp.BaseDisk); err != nil {
		return sp, fmt.Errorf("copy base disk for snapshot: %w", err)
	}

	// Boot a golden VM using stable resource names/paths so the snapshot state
	// can be restored inside per-sandbox netns+jail directories.
	const snapID = "snapshot"
	const snapSubnet = 250
	nc, err := setupSandboxNetnsAndRouting(snapID, snapSubnet)
	if err != nil {
		return sp, fmt.Errorf("setup snapshot netns: %w", err)
	}
	defer func() {
		_ = cleanupSandboxNetnsAndRouting(cfg, nc)
	}()

	// Create a minimal Firecracker config that uses relative paths and stable
	// device names.
	configPath := filepath.Join(sp.BaseDir, "vm-config.json")
	if err := writeVMConfig(configPath, cfg, nc.TapName, "rootfs.ext4", snapSubnet, "vsock.sock", 3); err != nil {
		return sp, fmt.Errorf("write snapshot vm config: %w", err)
	}

	// Remove stale sockets if a previous attempt crashed.
	_ = os.Remove(filepath.Join(sp.BaseDir, "firecracker.sock"))
	_ = os.Remove(filepath.Join(sp.BaseDir, "vsock.sock"))

	logPath := filepath.Join(sp.BaseDir, "firecracker.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return sp, fmt.Errorf("open snapshot firecracker log: %w", err)
	}
	defer logFile.Close()

	fcCmd := exec.Command("ip", "netns", "exec", nc.NetnsName, cfg.FirecrackerBin, "--api-sock", "firecracker.sock", "--config-file", "vm-config.json")
	fcCmd.Dir = sp.BaseDir
	fcCmd.Stdout = logFile
	fcCmd.Stderr = logFile
	fcCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := fcCmd.Start(); err != nil {
		return sp, fmt.Errorf("start snapshot firecracker: %w", err)
	}
	defer func() {
		_ = killProcessGroup(fcCmd)
		_, _ = fcCmd.Process.Wait()
	}()

	vsockPath := filepath.Join(sp.BaseDir, "vsock.sock")
	ac, err := waitForAgentReady(vsockPath, cfg.AgentPort, cfg.AgentWaitTimeout, cfg.AgentDialTimeout)
	if err != nil {
		return sp, fmt.Errorf("wait for snapshot agent: %w", err)
	}
	_ = ac.Close()

	apiSock := filepath.Join(sp.BaseDir, "firecracker.sock")
	fc := newFCClient(apiSock, 5*time.Second)

	// Pause before snapshotting.
	if err := fc.pauseVM(); err != nil {
		return sp, fmt.Errorf("pause snapshot vm: %w", err)
	}

	// Create a full snapshot. Note: these files are shared read-only across all
	// restored VMs, and must remain immutable.
	_ = os.Remove(sp.StateFile)
	_ = os.Remove(sp.MemFile)
	if err := fc.createFullSnapshot(sp.StateFile, sp.MemFile); err != nil {
		return sp, fmt.Errorf("create snapshot: %w", err)
	}

	// Kill the golden VM. We keep base disk + snapshot files.
	_ = killProcessGroup(fcCmd)
	_, _ = fcCmd.Process.Wait()

	if err := writeSnapshotMeta(sp, cfg); err != nil {
		return sp, err
	}

	log.Printf("snapshot ready: state=%s mem=%s base_disk=%s", sp.StateFile, sp.MemFile, sp.BaseDisk)
	return sp, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (s *server) createSandboxFromSnapshot(id string) (*sandbox, error) {
	createStart := time.Now()
	sp, err := ensureSnapshot(s.cfg)
	if err != nil {
		return nil, err
	}
	sb, timings, err := s.restoreSandboxFromArtifacts(
		id,
		createStart,
		sp.BaseDisk,
		sp.StateFile,
		sp.MemFile,
		"clone snapshot base disk",
		s.cfg.KeepFailedSandboxes,
		true,
	)
	if err != nil {
		return nil, err
	}
	if s.cfg.EnableStageTimingLogs {
		log.Printf("create snapshot timing: sandbox_id=%s disk_materialize=%s netns_acquire=%s prep_overlap=%s socket_ready=%s snapshot_load=%s agent_ready=%s guest_net=%s total=%s", id, timings.DiskMaterialize, timings.NetnsAcquire, timings.PrepOverlap, timings.SocketReady, timings.SnapshotLoad, timings.AgentReady, timings.GuestNet, timings.Total)
	}
	return sb, nil
}

func waitForUnixSocketReady(socketPath string, timeout time.Duration) error {
	if strings.TrimSpace(socketPath) == "" {
		return fmt.Errorf("socket path is empty")
	}
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err != nil {
			lastErr = err
		} else {
			c, err := net.DialTimeout("unix", socketPath, 50*time.Millisecond)
			if err == nil {
				_ = c.Close()
				return nil
			}
			lastErr = err
		}
		time.Sleep(2 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("%q not ready after %s: %w", socketPath, timeout, lastErr)
	}
	return fmt.Errorf("%q not ready after %s", socketPath, timeout)
}

func loadSnapshotWithRetry(fc *fcClient, statePath, memPath string, resume bool, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}

	deadline := time.Now().Add(timeout)
	for {
		err := fc.loadSnapshot(statePath, memPath, resume)
		if err == nil {
			return nil
		}
		if !isTransientUnixSocketErr(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isTransientUnixSocketErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connect: no such file or directory") ||
		strings.Contains(msg, "connect: connection refused")
}

func materializeSandboxRootfs(cfg config, srcPath, dstPath string) error {
	reflinkMode := "--reflink=auto"
	if cfg.RootfsCloneMode == "reflink-required" {
		reflinkMode = "--reflink=always"
	}
	_, _, err := runCmd("cp", reflinkMode, srcPath, dstPath)
	if err != nil && cfg.RootfsCloneMode == "reflink-required" {
		return fmt.Errorf("%w; reflink-required mode prevents full-copy fallback", err)
	}
	return err
}

func validateSnapshotMeta(sp snapshotPaths, cfg config) error {
	raw, err := os.ReadFile(sp.MetaFile)
	if err != nil {
		return fmt.Errorf("read snapshot meta: %w", err)
	}
	var meta snapshotMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return fmt.Errorf("decode snapshot meta: %w", err)
	}
	if meta.Version != 1 {
		return fmt.Errorf("unsupported snapshot meta version %d", meta.Version)
	}
	if strings.TrimSpace(cfg.BaseRootfsLineageID) == "" {
		return nil
	}
	if strings.TrimSpace(meta.LineageID) == "" {
		return fmt.Errorf("snapshot meta missing lineage id")
	}
	if meta.LineageID != cfg.BaseRootfsLineageID {
		return fmt.Errorf("snapshot lineage mismatch (meta=%s current=%s)", meta.LineageID, cfg.BaseRootfsLineageID)
	}
	return nil
}

func writeSnapshotMeta(sp snapshotPaths, cfg config) error {
	meta := snapshotMeta{
		Version:        1,
		LineageID:      cfg.BaseRootfsLineageID,
		BaseRootfsPath: cfg.BaseRootfsPath,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode snapshot meta: %w", err)
	}
	raw = append(raw, '\n')
	tmp := sp.MetaFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write snapshot meta: %w", err)
	}
	if err := os.Rename(tmp, sp.MetaFile); err != nil {
		return fmt.Errorf("persist snapshot meta: %w", err)
	}
	return nil
}

func resetSnapshotDir(sp snapshotPaths) error {
	if err := os.RemoveAll(sp.Dir); err != nil {
		return fmt.Errorf("remove old snapshot dir: %w", err)
	}
	if err := os.MkdirAll(sp.BaseDir, 0o755); err != nil {
		return fmt.Errorf("recreate snapshot dir: %w", err)
	}
	return nil
}

func computeFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open file for sha256 %q: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash file %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
