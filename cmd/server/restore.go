package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"manta/internal/agentrpc"
)

type restoreTimings struct {
	DiskMaterialize time.Duration
	NetnsAcquire    time.Duration
	PrepOverlap     time.Duration
	SocketReady     time.Duration
	SnapshotLoad    time.Duration
	AgentReady      time.Duration
	GuestNet        time.Duration
	Total           time.Duration
}

func (s *server) restoreSandboxFromArtifacts(
	id string,
	start time.Time,
	diskSrcPath string,
	stateFile string,
	memFile string,
	cloneErrLabel string,
	keepFailedSandboxDir bool,
	logCgroupErrors bool,
) (*sandbox, restoreTimings, error) {
	var timings restoreTimings

	sbDir := filepath.Join(s.cfg.WorkDir, "sandboxes", id)
	if err := os.MkdirAll(sbDir, 0o755); err != nil {
		return nil, timings, fmt.Errorf("create sandbox dir: %w", err)
	}
	cleanupDir := true
	defer func() {
		if cleanupDir {
			if keepFailedSandboxDir {
				log.Printf("debug keep failed sandbox dir: %s", sbDir)
				return
			}
			_ = os.RemoveAll(sbDir)
		}
	}()

	// Clone disk and acquire netns in parallel; these are independent setup
	// steps and overlapping them shortens /create critical path.
	rootfsCopy := filepath.Join(sbDir, "rootfs.ext4")
	cloneCh := make(chan struct {
		err error
		dur time.Duration
	}, 1)
	netnsCh := make(chan struct {
		nc  *netnsConfig
		err error
		dur time.Duration
	}, 1)
	go func() {
		cstart := time.Now()
		if err := materializeSandboxRootfs(s.cfg, diskSrcPath, rootfsCopy); err != nil {
			cloneCh <- struct {
				err error
				dur time.Duration
			}{err: fmt.Errorf("%s: %w", cloneErrLabel, err), dur: time.Since(cstart)}
			return
		}
		cloneCh <- struct {
			err error
			dur time.Duration
		}{err: nil, dur: time.Since(cstart)}
	}()
	go func() {
		nstart := time.Now()
		nc, err := s.acquireNetns(id)
		netnsCh <- struct {
			nc  *netnsConfig
			err error
			dur time.Duration
		}{nc: nc, err: err, dur: time.Since(nstart)}
	}()

	cloneRes := <-cloneCh
	netnsRes := <-netnsCh
	timings.DiskMaterialize = cloneRes.dur
	timings.NetnsAcquire = netnsRes.dur
	timings.PrepOverlap = time.Since(start)
	if cloneRes.err != nil || netnsRes.err != nil {
		if netnsRes.nc != nil {
			s.releaseNetns(netnsRes.nc)
		}
		if cloneRes.err != nil {
			return nil, timings, cloneRes.err
		}
		return nil, timings, netnsRes.err
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
		return nil, timings, fmt.Errorf("open firecracker log file: %w", err)
	}

	var cgroupPath string
	if s.cfg.EnableCgroups {
		cg := filepath.Join(s.cfg.CgroupRoot, id)
		if err := os.Mkdir(cg, 0o755); err == nil {
			cgroupPath = cg
		} else if logCgroupErrors {
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
		return nil, timings, fmt.Errorf("start firecracker: %w", err)
	}

	// Wait until Firecracker API socket is ready before hitting /snapshot/load.
	// Without this, short races can fail fast with ENOENT/ECONNREFUSED.
	socketWaitStart := time.Now()
	if err := waitForUnixSocketReady(socketPath, 1500*time.Millisecond); err != nil {
		_ = killProcessGroup(fcCmd)
		_ = killCgroup(cgroupPath)
		_ = logFile.Close()
		return nil, timings, fmt.Errorf("firecracker api socket not ready: %w", err)
	}
	timings.SocketReady = time.Since(socketWaitStart)

	// Best-effort: put process group in cgroup after spawn. (Children inherit.)
	if cgroupPath != "" {
		if err := movePidToCgroup(cgroupPath, fcCmd.Process.Pid); err != nil {
			if logCgroupErrors {
				log.Printf("move firecracker pid to cgroup failed (pid=%d cgroup=%q): %v", fcCmd.Process.Pid, cgroupPath, err)
			}
			_ = os.Remove(cgroupPath)
			cgroupPath = ""
		}
	}

	// Load snapshot and resume.
	fc := newFCClient(socketPath, 10*time.Second)
	loadStart := time.Now()
	if err := loadSnapshotWithRetry(fc, stateFile, memFile, true, 1500*time.Millisecond); err != nil {
		_ = killProcessGroup(fcCmd)
		_ = killCgroup(cgroupPath)
		_ = logFile.Close()
		return nil, timings, fmt.Errorf("load snapshot: %w", err)
	}
	timings.SnapshotLoad = time.Since(loadStart)

	// Wait for the agent to accept new connections after resume.
	vsockPath := filepath.Join(sbDir, "vsock.sock")
	agentWaitStart := time.Now()
	ac, err := waitForAgentReady(vsockPath, s.cfg.AgentPort, s.cfg.AgentWaitTimeout, s.cfg.AgentDialTimeout)
	if err != nil {
		_ = killProcessGroup(fcCmd)
		_ = killCgroup(cgroupPath)
		_ = logFile.Close()
		return nil, timings, fmt.Errorf("wait for agent after snapshot: %w", err)
	}
	timings.AgentReady = time.Since(agentWaitStart)

	// Apply per-sandbox guest IP config post-restore.
	guestNetStart := time.Now()
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
		return nil, timings, fmt.Errorf("agent network config failed: %w", err)
	}
	timings.GuestNet = time.Since(guestNetStart)

	_ = logFile.Close()
	cleanupNet = false
	cleanupDir = false
	timings.Total = time.Since(start)

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
	}, timings, nil
}
