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
	SSHWaitTimeout time.Duration
	SSHDialTimeout time.Duration
	SSHExecWait    time.Duration
	ExecTimeout    time.Duration
	BootArgs       string
	DefaultMemMiB  int
	DefaultVCPU    int
}

type sandbox struct {
	ID         string
	Subnet     int
	TapDevice  string
	HostIP     string
	GuestIP    string
	Dir        string
	SocketPath string
	ConfigPath string
	RootfsPath string
	LogPath    string
	CgroupPath string
	Process    *exec.Cmd
	SSHClient  *ssh.Client
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
	Cmd       string `json:"cmd"`
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
		ListenAddr:     envOr("MANTA_LISTEN_ADDR", ":8080"),
		KernelPath:     envOr("MANTA_KERNEL_PATH", "./guest-artifacts/vmlinux"),
		BaseRootfsPath: envOr("MANTA_ROOTFS_PATH", "./guest-artifacts/rootfs.ext4"),
		SSHPrivateKey:  envOr("MANTA_SSH_KEY_PATH", "./guest-artifacts/sandbox_key"),
		FirecrackerBin: envOr("MANTA_FIRECRACKER_BIN", "firecracker"),
		WorkDir:        envOr("MANTA_WORK_DIR", "/tmp/manta"),
		CgroupRoot:     envOr("MANTA_CGROUP_ROOT", "/sys/fs/cgroup/manta"),
		EnableCgroups:  intOr("MANTA_ENABLE_CGROUPS", 1) != 0,
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
	if strings.TrimSpace(req.SandboxID) == "" || strings.TrimSpace(req.Cmd) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sandbox_id and cmd are required"})
		return
	}

	s.mu.Lock()
	sb := s.sandboxes[req.SandboxID]
	s.mu.Unlock()

	if sb == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "sandbox not found"})
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
	exitCode, err := runSSHCommand(session, req.Cmd, &stdout, &stderr, s.cfg.ExecTimeout)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("exec failed: %v", err)})
		return
	}

	writeJSON(w, http.StatusOK, execResponse{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
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
	sbDir := filepath.Join(s.cfg.WorkDir, "sandboxes", id)
	if err := os.MkdirAll(sbDir, 0o755); err != nil {
		return nil, fmt.Errorf("create sandbox dir: %w", err)
	}

	tap := "tap-" + id
	hostIP := fmt.Sprintf("172.16.%d.1", subnet)
	guestIP := fmt.Sprintf("172.16.%d.2", subnet)
	subnetCIDR := fmt.Sprintf("172.16.%d.0/30", subnet)

	if _, _, err := runCmd("ip", "tuntap", "add", tap, "mode", "tap"); err != nil {
		return nil, fmt.Errorf("create tap: %w", err)
	}
	cleanupTap := true
	defer func() {
		if cleanupTap {
			_, _, _ = runCmd("ip", "link", "del", tap)
		}
	}()

	if _, _, err := runCmd("ip", "addr", "add", hostIP+"/30", "dev", tap); err != nil {
		return nil, fmt.Errorf("assign tap ip: %w", err)
	}
	if _, _, err := runCmd("ip", "link", "set", tap, "up"); err != nil {
		return nil, fmt.Errorf("set tap up: %w", err)
	}
	if _, _, err := runCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", subnetCIDR, "-o", s.cfg.HostNATIface, "-j", "MASQUERADE"); err != nil {
		return nil, fmt.Errorf("add NAT rule: %w", err)
	}
	natAdded := true
	defer func() {
		if natAdded {
			_, _, _ = runCmd("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", subnetCIDR, "-o", s.cfg.HostNATIface, "-j", "MASQUERADE")
		}
	}()

	rootfsCopy := filepath.Join(sbDir, "rootfs.ext4")
	if _, _, err := runCmd("cp", "--reflink=auto", s.cfg.BaseRootfsPath, rootfsCopy); err != nil {
		return nil, fmt.Errorf("copy rootfs: %w", err)
	}

	if err := updateNetworkConfig(rootfsCopy, guestIP, hostIP); err != nil {
		return nil, fmt.Errorf("update rootfs network config: %w", err)
	}

	configPath := filepath.Join(sbDir, "vm-config.json")
	if err := writeVMConfig(configPath, s.cfg, tap, rootfsCopy, subnet); err != nil {
		return nil, fmt.Errorf("write vm config: %w", err)
	}
	socketPath := filepath.Join(sbDir, "firecracker.sock")
	_ = os.Remove(socketPath)

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

	fcCmd := exec.Command(s.cfg.FirecrackerBin, "--api-sock", socketPath, "--config-file", configPath)
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

	sshClient, err := waitForSSH(guestIP, s.cfg.SSHPrivateKey, s.cfg.SSHWaitTimeout, s.cfg.SSHDialTimeout)
	if err != nil {
		_ = killProcessGroup(fcCmd)
		_ = killCgroup(cgroupPath)
		_, _, _ = runCmd("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", subnetCIDR, "-o", s.cfg.HostNATIface, "-j", "MASQUERADE")
		natAdded = false
		_, _, _ = runCmd("ip", "link", "del", tap)
		cleanupTap = false
		_ = logFile.Close()
		return nil, fmt.Errorf("wait for ssh: %w", err)
	}

	_ = logFile.Close()
	cleanupTap = false
	natAdded = false

	return &sandbox{
		ID:         id,
		Subnet:     subnet,
		TapDevice:  tap,
		HostIP:     hostIP,
		GuestIP:    guestIP,
		Dir:        sbDir,
		SocketPath: socketPath,
		ConfigPath: configPath,
		RootfsPath: rootfsCopy,
		LogPath:    logPath,
		CgroupPath: cgroupPath,
		Process:    fcCmd,
		SSHClient:  sshClient,
	}, nil
}

func (s *server) cleanupSandbox(sb *sandbox) error {
	var errs []string

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

	subnetCIDR := fmt.Sprintf("172.16.%d.0/30", sb.Subnet)
	if _, _, err := runCmd("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", subnetCIDR, "-o", s.cfg.HostNATIface, "-j", "MASQUERADE"); err != nil {
		errs = append(errs, fmt.Sprintf("remove NAT rule: %v", err))
	}

	if _, _, err := runCmd("ip", "link", "del", sb.TapDevice); err != nil {
		errs = append(errs, fmt.Sprintf("remove tap: %v", err))
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

func writeVMConfig(configPath string, cfg config, tapDevice, rootfsPath string, subnet int) error {
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
