package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"manta/internal/agentrpc"
)

type snapshotPaths struct {
	Dir       string
	BaseDir   string
	BaseDisk  string
	StateFile string
	MemFile   string
	MetaFile  string
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

	// If snapshot files exist, assume valid.
	if fileExists(sp.StateFile) && fileExists(sp.MemFile) && fileExists(sp.BaseDisk) {
		return sp, nil
	}

	if err := os.MkdirAll(sp.BaseDir, 0o755); err != nil {
		return sp, fmt.Errorf("create snapshot dir: %w", err)
	}

	// Prepare base disk which will be used for the snapshot and for per-sandbox
	// reflink clones. This disk must remain immutable after snapshot creation.
	if _, _, err := runCmd("cp", "--reflink=auto", cfg.BaseRootfsPath, sp.BaseDisk); err != nil {
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

	log.Printf("snapshot ready: state=%s mem=%s base_disk=%s", sp.StateFile, sp.MemFile, sp.BaseDisk)
	return sp, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (s *server) createSandboxFromSnapshot(id string) (*sandbox, error) {
	sp, err := ensureSnapshot(s.cfg)
	if err != nil {
		return nil, err
	}

	sbDir := filepath.Join(s.cfg.WorkDir, "sandboxes", id)
	if err := os.MkdirAll(sbDir, 0o755); err != nil {
		return nil, fmt.Errorf("create sandbox dir: %w", err)
	}
	cleanupDir := true
	defer func() {
		if cleanupDir {
			if s.cfg.KeepFailedSandboxes {
				log.Printf("debug keep failed sandbox dir: %s", sbDir)
				return
			}
			_ = os.RemoveAll(sbDir)
		}
	}()

	// Clone disk and acquire netns in parallel; these are independent setup
	// steps and overlapping them shortens /create critical path.
	rootfsCopy := filepath.Join(sbDir, "rootfs.ext4")
	cloneErrCh := make(chan error, 1)
	netnsCh := make(chan struct {
		nc  *netnsConfig
		err error
	}, 1)
	go func() {
		if _, _, err := runCmd("cp", "--reflink=auto", sp.BaseDisk, rootfsCopy); err != nil {
			cloneErrCh <- fmt.Errorf("clone snapshot base disk: %w", err)
			return
		}
		cloneErrCh <- nil
	}()
	go func() {
		nc, err := s.acquireNetns(id)
		netnsCh <- struct {
			nc  *netnsConfig
			err error
		}{nc: nc, err: err}
	}()

	cloneErr := <-cloneErrCh
	netnsRes := <-netnsCh
	if cloneErr != nil || netnsRes.err != nil {
		if netnsRes.nc != nil {
			s.releaseNetns(netnsRes.nc)
		}
		if cloneErr != nil {
			return nil, cloneErr
		}
		return nil, netnsRes.err
	}
	nc := netnsRes.nc
	cleanupNet := true
	defer func() {
		if cleanupNet {
			s.releaseNetns(nc)
		}
	}()

	// Start Firecracker with API socket only; restore from snapshot via API.
	socketPath := filepath.Join(sbDir, "firecracker.sock")
	_ = os.Remove(socketPath)
	_ = os.Remove(filepath.Join(sbDir, "vsock.sock"))

	logPath := filepath.Join(sbDir, "firecracker.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open firecracker log file: %w", err)
	}

	var cgroupPath string
	if s.cfg.EnableCgroups {
		cg := filepath.Join(s.cfg.CgroupRoot, id)
		if err := os.Mkdir(cg, 0o755); err == nil {
			cgroupPath = cg
		} else {
			log.Printf("create cgroup %q failed, continuing without cgroups: %v", cg, err)
		}
	}

	fcCmd := exec.Command("ip", "netns", "exec", nc.NetnsName, s.cfg.FirecrackerBin, "--api-sock", "firecracker.sock")
	fcCmd.Dir = sbDir
	fcCmd.Stdout = logFile
	fcCmd.Stderr = logFile
	fcCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := fcCmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start firecracker: %w", err)
	}

	// Wait until Firecracker API socket is ready before hitting /snapshot/load.
	// Without this, short races can fail fast with ENOENT/ECONNREFUSED.
	if err := waitForUnixSocketReady(socketPath, 1500*time.Millisecond); err != nil {
		_ = killProcessGroup(fcCmd)
		_ = killCgroup(cgroupPath)
		_ = logFile.Close()
		return nil, fmt.Errorf("firecracker api socket not ready: %w", err)
	}

	// Best-effort: put process group in cgroup after spawn. (Children inherit.)
	if cgroupPath != "" {
		if err := movePidToCgroup(cgroupPath, fcCmd.Process.Pid); err != nil {
			log.Printf("move firecracker pid to cgroup failed (pid=%d cgroup=%q): %v", fcCmd.Process.Pid, cgroupPath, err)
			_ = os.Remove(cgroupPath)
			cgroupPath = ""
		}
	}

	// Load snapshot and resume.
	fc := newFCClient(socketPath, 10*time.Second)
	if err := loadSnapshotWithRetry(fc, sp.StateFile, sp.MemFile, true, 1500*time.Millisecond); err != nil {
		_ = killProcessGroup(fcCmd)
		_ = killCgroup(cgroupPath)
		_ = logFile.Close()
		return nil, fmt.Errorf("load snapshot: %w", err)
	}

	// Wait for the agent to accept new connections after resume.
	vsockPath := filepath.Join(sbDir, "vsock.sock")
	ac, err := waitForAgentReady(vsockPath, s.cfg.AgentPort, s.cfg.AgentWaitTimeout, s.cfg.AgentDialTimeout)
	if err != nil {
		_ = killProcessGroup(fcCmd)
		_ = killCgroup(cgroupPath)
		_ = logFile.Close()
		return nil, fmt.Errorf("wait for agent after snapshot: %w", err)
	}

	// Apply per-sandbox guest IP config post-restore.
	if _, err := ac.Call(agentrpc.Request{
		Type: "net",
		Net: &agentrpc.NetRequest{
			Interface: "eth0",
			Address:   nc.GuestIP + "/30",
			Gateway:   nc.HostIP,
			DNS:       "1.1.1.1",
		},
	}, 5*time.Second); err != nil {
		_ = ac.Close()
		_ = killProcessGroup(fcCmd)
		_ = killCgroup(cgroupPath)
		_ = logFile.Close()
		return nil, fmt.Errorf("agent network config failed: %w", err)
	}

	_ = logFile.Close()
	cleanupNet = false
	cleanupDir = false

	return &sandbox{
		ID:         id,
		Subnet:     nc.Subnet,
		TapDevice:  nc.TapName,
		HostIP:     nc.HostIP,
		GuestIP:    nc.GuestIP,
		GuestCID:   3,
		Netns:      nc,
		Dir:        sbDir,
		SocketPath: socketPath,
		VsockPath:  vsockPath,
		RootfsPath: rootfsCopy,
		LogPath:    logPath,
		CgroupPath: cgroupPath,
		Process:    fcCmd,
		Agent:      ac,
	}, nil
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
		time.Sleep(20 * time.Millisecond)
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
