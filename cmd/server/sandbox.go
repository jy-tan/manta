package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"manta/internal/agentrpc"
)

func (s *server) createSandbox(id string) (*sandbox, error) {
	if s.cfg.EnableSnapshots {
		return s.createSandboxFromSnapshot(id)
	}

	sbDir := filepath.Join(s.cfg.WorkDir, "sandboxes", id)
	if err := os.MkdirAll(sbDir, 0o755); err != nil {
		return nil, fmt.Errorf("create sandbox dir: %w", err)
	}
	cleanupDir := true
	defer func() {
		if cleanupDir {
			_ = os.RemoveAll(sbDir)
		}
	}()

	// Acquire netns and clone rootfs in parallel to reduce /create critical path.
	rootfsCopy := filepath.Join(sbDir, "rootfs.ext4")
	copyErrCh := make(chan error, 1)
	netnsCh := make(chan struct {
		nc  *netnsConfig
		err error
	}, 1)
	go func() {
		if err := materializeSandboxRootfs(s.cfg, s.cfg.BaseRootfsPath, rootfsCopy); err != nil {
			copyErrCh <- fmt.Errorf("copy rootfs: %w", err)
			return
		}
		copyErrCh <- nil
	}()
	go func() {
		nc, err := s.acquireNetns(id)
		netnsCh <- struct {
			nc  *netnsConfig
			err error
		}{nc: nc, err: err}
	}()

	copyErr := <-copyErrCh
	netnsRes := <-netnsCh
	if copyErr != nil || netnsRes.err != nil {
		if netnsRes.nc != nil {
			s.releaseNetns(netnsRes.nc)
		}
		if copyErr != nil {
			return nil, copyErr
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

	configPath := filepath.Join(sbDir, "vm-config.json")
	// Use stable, relative paths inside the per-sandbox jail dir.
	if err := writeVMConfig(configPath, s.cfg, nc.TapName, "rootfs.ext4", nc.Subnet, "vsock.sock", uint32(1000+nc.Subnet)); err != nil {
		return nil, fmt.Errorf("write vm config: %w", err)
	}
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

	fcCmd := exec.Command("ip", "netns", "exec", nc.NetnsName, s.cfg.FirecrackerBin, "--api-sock", "firecracker.sock", "--config-file", "vm-config.json")
	fcCmd.Dir = sbDir
	fcCmd.Stdout = logFile
	fcCmd.Stderr = logFile
	// Start Firecracker in its own process group so cleanup can SIGKILL the group.
	fcCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := fcCmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start firecracker: %w", err)
	}

	if cgroupPath != "" {
		if err := movePidToCgroup(cgroupPath, fcCmd.Process.Pid); err != nil {
			log.Printf("move firecracker pid to cgroup failed (pid=%d cgroup=%q): %v", fcCmd.Process.Pid, cgroupPath, err)
			_ = os.Remove(cgroupPath)
			cgroupPath = ""
		}
	}

	vsockPath := filepath.Join(sbDir, "vsock.sock")
	ac, err := waitForAgentReady(vsockPath, s.cfg.AgentPort, s.cfg.AgentWaitTimeout, s.cfg.AgentDialTimeout)
	if err != nil {
		_ = killProcessGroup(fcCmd)
		_ = killCgroup(cgroupPath)
		_ = cleanupSandboxNetnsAndRouting(s.cfg, nc)
		cleanupNet = false
		_ = logFile.Close()
		return nil, fmt.Errorf("wait for agent: %w", err)
	}

	// Configure per-sandbox networking inside the guest via vsock so /create
	// doesn't depend on SSHD or disk mutation of /etc/network/interfaces.
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
		_ = cleanupSandboxNetnsAndRouting(s.cfg, nc)
		cleanupNet = false
		_ = logFile.Close()
		return nil, fmt.Errorf("agent network config failed: %w", err)
	}

	_ = logFile.Close()
	cleanupNet = false
	cleanupDir = false

	sb := &sandbox{
		ID:         id,
		Subnet:     nc.Subnet,
		TapDevice:  nc.TapName,
		HostIP:     nc.HostIP,
		GuestIP:    nc.GuestIP,
		GuestCID:   uint32(1000 + nc.Subnet),
		Netns:      nc,
		Dir:        sbDir,
		SocketPath: socketPath,
		VsockPath:  vsockPath,
		ConfigPath: configPath,
		RootfsPath: rootfsCopy,
		LogPath:    logPath,
		CgroupPath: cgroupPath,
		Process:    fcCmd,
		Agent:      ac,
	}
	return sb, nil
}

func (s *server) cleanupSandbox(sb *sandbox) error {
	var errs []string

	sb.agentMu.Lock()
	if sb.Agent != nil {
		_ = sb.Agent.Close()
		sb.Agent = nil
	}
	sb.agentMu.Unlock()

	if sb.SSHClient != nil {
		_ = sb.SSHClient.Close()
	}

	// Best-effort: kill everything in the sandbox cgroup first. Note that the
	// cgroup dir often can't be removed until after processes fully exit.
	if sb.CgroupPath != "" {
		if err := killCgroup(sb.CgroupPath); err != nil {
			errs = append(errs, fmt.Sprintf("kill cgroup: %v", err))
		}
	}

	if sb.Process != nil && sb.Process.Process != nil {
		_ = killProcessGroup(sb.Process)
		done := make(chan error, 1)
		go func() { done <- sb.Process.Wait() }()
		select {
		case <-time.After(5 * time.Second):
			errs = append(errs, "timed out waiting for firecracker process exit")
		case err := <-done:
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					errs = append(errs, fmt.Sprintf("wait firecracker: %v", err))
				}
			}
		}
	}

	if sb.CgroupPath != "" {
		// Removing cgroup dirs is racy immediately after kill; retry briefly.
		if err := removeCgroupDir(sb.CgroupPath, 1500*time.Millisecond); err != nil {
			// Non-fatal: leaving an empty cgroup dir behind is acceptable. We also
			// scavenge leftover cgroups at server startup.
			log.Printf("remove cgroup %q failed (non-fatal): %v", sb.CgroupPath, err)
		}
	}

	s.releaseNetns(sb.Netns)
	sb.Netns = nil

	if err := os.RemoveAll(sb.Dir); err != nil {
		errs = append(errs, fmt.Sprintf("remove sandbox dir: %v", err))
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (s *server) destroyAll() {
	s.mu.Lock()
	all := make([]*sandbox, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		all = append(all, sb)
	}
	s.sandboxes = make(map[string]*sandbox)
	s.mu.Unlock()

	for _, sb := range all {
		if err := s.cleanupSandbox(sb); err != nil {
			log.Printf("cleanup %s error: %v", sb.ID, err)
		}
	}
}
