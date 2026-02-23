package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"manta/internal/agentrpc"
)

type config struct {
	ListenAddr     string
	KernelPath     string
	BaseRootfsPath string
	SSHPrivateKey  string
	FirecrackerBin string
	HostNATIface   string
	WorkDir        string
	CgroupRoot     string
	EnableCgroups  bool

	// EnableSnapshots switches /create from "boot fresh VM" to "restore from a
	// golden snapshot". Snapshotting requires Firecracker snapshot support.
	EnableSnapshots bool

	// ExecTransport controls how /exec runs commands inside the guest.
	// Supported: "agent" (vsock RPC), "ssh" (debug fallback).
	ExecTransport string

	// Agent controls readiness + RPC dialing for the in-guest agent.
	AgentPort        int
	AgentWaitTimeout time.Duration
	AgentDialTimeout time.Duration
	AgentCallTimeout time.Duration
	AgentMaxOutputB  int64
	SSHWaitTimeout   time.Duration
	SSHDialTimeout   time.Duration
	SSHExecWait      time.Duration
	ExecTimeout      time.Duration
	BootArgs         string
	DefaultMemMiB    int
	DefaultVCPU      int
}

type sandbox struct {
	ID         string
	Subnet     int
	TapDevice  string
	HostIP     string
	GuestIP    string
	GuestCID   uint32
	Netns      *netnsConfig
	Dir        string
	SocketPath string
	VsockPath  string
	ConfigPath string
	RootfsPath string
	LogPath    string
	CgroupPath string
	Process    *exec.Cmd
	SSHClient  *ssh.Client // debug-only; exec path no longer depends on SSH
	Agent      *agentConn
	agentMu    sync.Mutex
}

type server struct {
	cfg           config
	mu            sync.Mutex
	nextSandboxID uint64
	nextSubnet    uint32
	sandboxes     map[string]*sandbox
}

type createResponse struct {
	SandboxID string `json:"sandbox_id"`
}

type execRequest struct {
	SandboxID string `json:"sandbox_id"`
	// Shell mode (default for backward compatibility): run /bin/sh -lc <cmd>.
	Cmd string `json:"cmd,omitempty"`

	// No-shell mode: run argv directly (execve-style).
	Argv []string `json:"argv,omitempty"`

	// Optional explicit switch. If omitted:
	// - cmd => use_shell=true
	// - argv => use_shell=false
	UseShell *bool `json:"use_shell,omitempty"`

	// Optional per-request timeout override. 0 uses server default.
	TimeoutMs int64 `json:"timeout_ms,omitempty"`
}

type execResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

type destroyRequest struct {
	SandboxID string `json:"sandbox_id"`
}

type destroyResponse struct {
	Status string `json:"status"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if os.Geteuid() != 0 {
		log.Fatalf("this server must run as root (try: sudo go run ./cmd/server)")
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	if err := ensurePreflight(cfg); err != nil {
		log.Fatalf("preflight failed: %v", err)
	}

	srv := &server{
		cfg:       cfg,
		sandboxes: make(map[string]*sandbox),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /create", srv.handleCreate)
	mux.HandleFunc("POST /exec", srv.handleExec)
	mux.HandleFunc("POST /destroy", srv.handleDestroy)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("server listening on %s", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Printf("shutdown signal received, cleaning up")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("http shutdown error: %v", err)
	}
	srv.destroyAll()
}

func loadConfig() (config, error) {
	cfg := config{
		ListenAddr:      envOr("MANTA_LISTEN_ADDR", ":8080"),
		KernelPath:      envOr("MANTA_KERNEL_PATH", "./guest-artifacts/vmlinux"),
		BaseRootfsPath:  envOr("MANTA_ROOTFS_PATH", "./guest-artifacts/rootfs.ext4"),
		SSHPrivateKey:   envOr("MANTA_SSH_KEY_PATH", "./guest-artifacts/sandbox_key"),
		FirecrackerBin:  envOr("MANTA_FIRECRACKER_BIN", "firecracker"),
		WorkDir:         envOr("MANTA_WORK_DIR", "/tmp/manta"),
		CgroupRoot:      envOr("MANTA_CGROUP_ROOT", "/sys/fs/cgroup/manta"),
		EnableCgroups:   intOr("MANTA_ENABLE_CGROUPS", 1) != 0,
		EnableSnapshots: intOr("MANTA_ENABLE_SNAPSHOTS", 0) != 0,
		ExecTransport:   strings.ToLower(strings.TrimSpace(envOr("MANTA_EXEC_TRANSPORT", "agent"))),

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
	for _, p := range []*string{&cfg.KernelPath, &cfg.BaseRootfsPath, &cfg.SSHPrivateKey} {
		abs, err := filepath.Abs(*p)
		if err != nil {
			return cfg, fmt.Errorf("resolve path %q: %w", *p, err)
		}
		*p = abs
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

func (s *server) handleCreate(w http.ResponseWriter, _ *http.Request) {
	id := fmt.Sprintf("sb-%d", atomic.AddUint64(&s.nextSandboxID, 1))
	subnet := int(atomic.AddUint32(&s.nextSubnet, 1))

	sb, err := s.createSandbox(id, subnet)
	if err != nil {
		log.Printf("create %s failed: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	s.mu.Lock()
	s.sandboxes[sb.ID] = sb
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, createResponse{SandboxID: sb.ID})
}

func (s *server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req execRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if strings.TrimSpace(req.SandboxID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sandbox_id is required"})
		return
	}

	s.mu.Lock()
	sb := s.sandboxes[req.SandboxID]
	s.mu.Unlock()

	if sb == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "sandbox not found"})
		return
	}

	timeout := s.cfg.ExecTimeout
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}

	cmd := strings.TrimSpace(req.Cmd)
	useShell := false
	switch {
	case len(req.Argv) > 0:
		if cmd != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provide either cmd or argv, not both"})
			return
		}
		useShell = false
		if req.UseShell != nil && *req.UseShell {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "use_shell=true is not valid with argv"})
			return
		}
	case cmd != "":
		useShell = true
		if req.UseShell != nil && !*req.UseShell {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "use_shell=false is not valid with cmd; provide argv instead"})
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cmd or argv is required"})
		return
	}

	switch s.cfg.ExecTransport {
	case "ssh":
		if !useShell {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ssh transport only supports cmd (shell mode)"})
			return
		}

		sshClient, err := waitForExecSSH(sb.GuestIP, s.cfg.SSHPrivateKey, s.cfg.SSHExecWait, s.cfg.SSHDialTimeout)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("ssh dial failed: %v", err)})
			return
		}
		defer sshClient.Close()

		session, err := sshClient.NewSession()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("new ssh session: %v", err)})
			return
		}
		defer session.Close()

		var stdout, stderr bytes.Buffer
		exitCode, err := runSSHCommand(session, cmd, &stdout, &stderr, timeout)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("exec failed: %v", err)})
			return
		}
		writeJSON(w, http.StatusOK, execResponse{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode})
		return

	case "agent", "":
		// ok
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unknown exec transport %q", s.cfg.ExecTransport)})
		return
	}

	sb.agentMu.Lock()
	defer sb.agentMu.Unlock()

	// Prefer a persistent agent connection, but transparently redial if needed.
	ac := sb.Agent
	if ac == nil {
		newAC, err := dialAgent(sb.VsockPath, s.cfg.AgentPort, s.cfg.AgentDialTimeout)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("agent dial failed: %v", err)})
			return
		}
		sb.Agent = newAC
		ac = newAC
	}

	resp, err := ac.Call(agentrpc.Request{
		Type: "exec",
		Exec: &agentrpc.ExecRequest{
			UseShell:       useShell,
			Cmd:            cmd,
			Argv:           req.Argv,
			TimeoutMs:      timeout.Milliseconds(),
			MaxOutputBytes: s.cfg.AgentMaxOutputB,
		},
	}, s.cfg.AgentCallTimeout)
	if err != nil {
		// Retry once on likely broken connection.
		_ = ac.Close()
		sb.Agent = nil

		newAC, derr := dialAgent(sb.VsockPath, s.cfg.AgentPort, s.cfg.AgentDialTimeout)
		if derr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("agent dial failed: %v (original error: %v)", derr, err)})
			return
		}
		sb.Agent = newAC

		resp, err = newAC.Call(agentrpc.Request{
			Type: "exec",
			Exec: &agentrpc.ExecRequest{
				UseShell:       useShell,
				Cmd:            cmd,
				Argv:           req.Argv,
				TimeoutMs:      timeout.Milliseconds(),
				MaxOutputBytes: s.cfg.AgentMaxOutputB,
			},
		}, s.cfg.AgentCallTimeout)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("agent exec failed: %v", err)})
			return
		}
	}

	writeJSON(w, http.StatusOK, execResponse{
		Stdout:   resp.Exec.Stdout,
		Stderr:   resp.Exec.Stderr,
		ExitCode: resp.Exec.ExitCode,
	})
}

func (s *server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	var req destroyRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if strings.TrimSpace(req.SandboxID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sandbox_id is required"})
		return
	}

	s.mu.Lock()
	sb := s.sandboxes[req.SandboxID]
	delete(s.sandboxes, req.SandboxID)
	s.mu.Unlock()

	if sb == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "sandbox not found"})
		return
	}

	if err := s.cleanupSandbox(sb); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, destroyResponse{Status: "ok"})
}

func (s *server) createSandbox(id string, subnet int) (*sandbox, error) {
	if s.cfg.EnableSnapshots {
		return s.createSandboxFromSnapshot(id, subnet)
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

	nc, err := setupSandboxNetnsAndRouting(id, subnet, s.cfg.HostNATIface)
	if err != nil {
		return nil, err
	}
	cleanupNet := true
	defer func() {
		if cleanupNet {
			_ = cleanupSandboxNetnsAndRouting(s.cfg, nc)
		}
	}()

	rootfsCopy := filepath.Join(sbDir, "rootfs.ext4")
	if _, _, err := runCmd("cp", "--reflink=auto", s.cfg.BaseRootfsPath, rootfsCopy); err != nil {
		return nil, fmt.Errorf("copy rootfs: %w", err)
	}

	configPath := filepath.Join(sbDir, "vm-config.json")
	// Use stable, relative paths inside the per-sandbox jail dir.
	if err := writeVMConfig(configPath, s.cfg, nc.TapName, "rootfs.ext4", subnet, "vsock.sock", uint32(1000+subnet)); err != nil {
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

	return &sandbox{
		ID:         id,
		Subnet:     subnet,
		TapDevice:  nc.TapName,
		HostIP:     nc.HostIP,
		GuestIP:    nc.GuestIP,
		GuestCID:   uint32(1000 + subnet),
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
	}, nil
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

	if err := cleanupSandboxNetnsAndRouting(s.cfg, sb.Netns); err != nil {
		errs = append(errs, fmt.Sprintf("cleanup netns: %v", err))
	}

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

func ensureCgroupRoot(root string) error {
	// Simple cgroup v2 check.
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return fmt.Errorf("cgroup v2 not available at /sys/fs/cgroup: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create cgroup root %q: %w", root, err)
	}
	return nil
}

func scavengeCgroups(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		log.Printf("scavenge cgroups: read %q: %v", root, err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cg := filepath.Join(root, e.Name())
		_ = killCgroup(cg)
		if err := removeCgroupDir(cg, 1500*time.Millisecond); err != nil {
			log.Printf("scavenge cgroups: remove %q: %v", cg, err)
		}
	}
}

func killCgroup(cgroupPath string) error {
	if strings.TrimSpace(cgroupPath) == "" {
		return nil
	}
	killFile := filepath.Join(cgroupPath, "cgroup.kill")
	if _, err := os.Stat(killFile); err != nil {
		return fmt.Errorf("cgroup.kill missing for %q: %w", cgroupPath, err)
	}
	if err := os.WriteFile(killFile, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("write cgroup.kill for %q: %w", cgroupPath, err)
	}
	return nil
}

func removeCgroupDir(cgroupPath string, timeout time.Duration) error {
	if strings.TrimSpace(cgroupPath) == "" {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		err := os.Remove(cgroupPath)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		// Typical transient errors while the kernel is still tearing down tasks.
		if errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.ENOTEMPTY) {
			if time.Now().After(deadline) {
				return err
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return err
	}
}

func movePidToCgroup(cgroupPath string, pid int) error {
	procsFile := filepath.Join(cgroupPath, "cgroup.procs")
	if _, err := os.Stat(procsFile); err != nil {
		return fmt.Errorf("cgroup.procs missing for %q: %w", cgroupPath, err)
	}
	if err := os.WriteFile(procsFile, []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		return fmt.Errorf("write cgroup.procs for %q: %w", cgroupPath, err)
	}
	return nil
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// With Setpgid=true, pgid == pid for the child. A negative pid targets the
	// entire process group.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		// Fallback: try killing just the main process.
		_ = cmd.Process.Kill()
		return err
	}
	return nil
}

func waitForSSH(ip, keyPath string, timeout, dialTimeout time.Duration) (*ssh.Client, error) {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort(ip, "22")
	for time.Now().Before(deadline) {
		client, err := dialSSH(ip, keyPath, dialTimeout)
		if err == nil {
			// Dial success alone can be too early; verify command execution.
			session, serr := client.NewSession()
			if serr == nil {
				if rerr := session.Run("true"); rerr == nil {
					_ = session.Close()
					return client, nil
				}
				_ = session.Close()
			}
			_ = client.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}

	return nil, fmt.Errorf("ssh not reachable at %s after %s", addr, timeout)
}

func waitForExecSSH(ip, keyPath string, timeout, dialTimeout time.Duration) (*ssh.Client, error) {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort(ip, "22")
	var lastErr error
	for time.Now().Before(deadline) {
		client, err := dialSSH(ip, keyPath, dialTimeout)
		if err == nil {
			return client, nil
		}
		lastErr = err
		time.Sleep(150 * time.Millisecond)
	}
	if lastErr != nil {
		return nil, fmt.Errorf("dial %s failed for %s: %w", addr, timeout, lastErr)
	}
	return nil, fmt.Errorf("dial %s failed for %s", addr, timeout)
}

func dialSSH(ip, keyPath string, dialTimeout time.Duration) (*ssh.Client, error) {
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read ssh key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse ssh key: %w", err)
	}

	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         dialTimeout,
	}

	addr := net.JoinHostPort(ip, "22")
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func runSSHCommand(session *ssh.Session, cmd string, stdout, stderr *bytes.Buffer, timeout time.Duration) (int, error) {
	session.Stdout = stdout
	session.Stderr = stderr

	if err := session.Start(cmd); err != nil {
		return 0, err
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- session.Wait()
	}()

	select {
	case err := <-waitCh:
		if err == nil {
			return 0, nil
		}
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitStatus(), nil
		}
		return 0, err
	case <-time.After(timeout):
		_ = session.Signal(ssh.SIGKILL)
		return 124, fmt.Errorf("command timed out after %s", timeout)
	}
}

func updateNetworkConfig(rootfsPath, guestIP, hostIP string) error {
	mountDir, err := os.MkdirTemp("", "manta-rootfs-mount-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mountDir)

	if _, _, err := runCmd("mount", "-o", "loop", rootfsPath, mountDir); err != nil {
		return fmt.Errorf("mount rootfs: %w", err)
	}
	defer func() {
		_, _, _ = runCmd("umount", mountDir)
	}()

	if err := os.MkdirAll(filepath.Join(mountDir, "etc", "network"), 0o755); err != nil {
		return err
	}

	interfaces := fmt.Sprintf(`auto lo
iface lo inet loopback

auto eth0
iface eth0 inet static
  address %s
  netmask 255.255.255.252
  gateway %s
`, guestIP, hostIP)
	if err := os.WriteFile(filepath.Join(mountDir, "etc", "network", "interfaces"), []byte(interfaces), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(mountDir, "etc", "resolv.conf"), []byte("nameserver 1.1.1.1\n"), 0o644); err != nil {
		return err
	}

	return nil
}

func writeVMConfig(configPath string, cfg config, tapDevice, rootfsPath string, subnet int, vsockPath string, guestCID uint32) error {
	type bootSource struct {
		KernelImagePath string `json:"kernel_image_path"`
		BootArgs        string `json:"boot_args"`
	}
	type drive struct {
		DriveID      string `json:"drive_id"`
		PathOnHost   string `json:"path_on_host"`
		IsRootDevice bool   `json:"is_root_device"`
		IsReadOnly   bool   `json:"is_read_only"`
	}
	type netIf struct {
		IfaceID     string `json:"iface_id"`
		GuestMAC    string `json:"guest_mac"`
		HostDevName string `json:"host_dev_name"`
	}
	type machineConfig struct {
		VCPUCount  int `json:"vcpu_count"`
		MemSizeMiB int `json:"mem_size_mib"`
	}
	type vsockConfig struct {
		GuestCID uint32 `json:"guest_cid"`
		UDSPath  string `json:"uds_path"`
	}

	guestMAC := fmt.Sprintf("06:00:AC:10:%02X:%02X", (subnet>>8)&0xFF, subnet&0xFF)

	cfgObj := map[string]any{
		"boot-source": bootSource{
			KernelImagePath: cfg.KernelPath,
			BootArgs:        cfg.BootArgs,
		},
		"drives": []drive{
			{
				DriveID:      "rootfs",
				PathOnHost:   rootfsPath,
				IsRootDevice: true,
				IsReadOnly:   false,
			},
		},
		"network-interfaces": []netIf{
			{
				IfaceID:     "eth0",
				GuestMAC:    guestMAC,
				HostDevName: tapDevice,
			},
		},
		"machine-config": machineConfig{
			VCPUCount:  cfg.DefaultVCPU,
			MemSizeMiB: cfg.DefaultMemMiB,
		},
		"vsock": vsockConfig{
			GuestCID: guestCID,
			UDSPath:  vsockPath,
		},
	}

	raw, err := json.MarshalIndent(cfgObj, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(configPath, raw, 0o644)
}

func decodeJSON(r io.Reader, dst any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("write json error: %v", err)
	}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func runCmd(name string, args ...string) (string, string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("%s %v: %w (stderr: %s)", name, args, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stderr.String(), nil
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
