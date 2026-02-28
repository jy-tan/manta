package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"manta/internal/agentrpc"
)

type snapshotCreateRequest struct {
	SandboxID string `json:"sandbox_id"`
	Name      string `json:"name,omitempty"`
}

type snapshotCreateResponse struct {
	SnapshotID string `json:"snapshot_id"`
}

type snapshotRestoreRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

type snapshotRestoreResponse struct {
	SandboxID string `json:"sandbox_id"`
}

type snapshotDeleteRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

type snapshotDeleteResponse struct {
	Status string `json:"status"`
}

type userSnapshotMeta struct {
	SnapshotID       string `json:"snapshot_id"`
	Name             string `json:"name,omitempty"`
	CreatedAt        string `json:"created_at"`
	StateFile        string `json:"state_file"`
	MemFile          string `json:"mem_file"`
	DiskFile         string `json:"disk_file"`
	LineageID        string `json:"lineage_id"`
	SourceSandboxID  string `json:"source_sandbox_id"`
	SourceRootfsPath string `json:"source_rootfs_path"`
}

func userSnapshotsDir(workDir string) string {
	return filepath.Join(workDir, "user-snapshots")
}

func userSnapshotRootDir(workDir, snapshotID string) string {
	return filepath.Join(userSnapshotsDir(workDir), snapshotID)
}

func userSnapshotMetaPath(workDir, snapshotID string) string {
	return filepath.Join(userSnapshotRootDir(workDir, snapshotID), "meta.json")
}

func (s *server) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	var req snapshotCreateRequest
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

	snapshotID := fmt.Sprintf("us-%d", atomic.AddUint64(&s.nextSnapshotID, 1))
	meta, err := s.createUserSnapshotFromSandbox(sb, snapshotID, req.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, snapshotCreateResponse{SnapshotID: meta.SnapshotID})
}

func (s *server) handleSnapshotRestore(w http.ResponseWriter, r *http.Request) {
	var req snapshotRestoreRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	req.SnapshotID = strings.TrimSpace(req.SnapshotID)
	if req.SnapshotID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "snapshot_id is required"})
		return
	}

	meta, err := s.loadUserSnapshotMeta(req.SnapshotID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(s.cfg.BaseRootfsLineageID) != "" && strings.TrimSpace(meta.LineageID) != "" && meta.LineageID != s.cfg.BaseRootfsLineageID {
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("snapshot lineage mismatch (snapshot=%s current=%s)", meta.LineageID, s.cfg.BaseRootfsLineageID)})
		return
	}

	id := fmt.Sprintf("sb-%d", atomic.AddUint64(&s.nextSandboxID, 1))
	sb, err := s.createSandboxFromUserSnapshot(id, meta)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	s.mu.Lock()
	s.sandboxes[sb.ID] = sb
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, snapshotRestoreResponse{SandboxID: sb.ID})
}

func (s *server) handleSnapshotList(w http.ResponseWriter, _ *http.Request) {
	items, err := s.listUserSnapshots()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": items})
}

func (s *server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	var req snapshotDeleteRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	req.SnapshotID = strings.TrimSpace(req.SnapshotID)
	if req.SnapshotID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "snapshot_id is required"})
		return
	}
	if err := os.RemoveAll(userSnapshotRootDir(s.cfg.WorkDir, req.SnapshotID)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("delete snapshot: %v", err)})
		return
	}
	writeJSON(w, http.StatusOK, snapshotDeleteResponse{Status: "ok"})
}

func (s *server) createUserSnapshotFromSandbox(sb *sandbox, snapshotID, name string) (userSnapshotMeta, error) {
	if sb == nil {
		return userSnapshotMeta{}, fmt.Errorf("sandbox is nil")
	}
	// Avoid snapshotting an active host<->guest agent stream. A stale captured
	// vsock session can delay agent re-readiness after restore.
	sb.agentMu.Lock()
	if sb.Agent != nil {
		_ = sb.Agent.Close()
		sb.Agent = nil
	}
	sb.agentMu.Unlock()

	rootDir := userSnapshotRootDir(s.cfg.WorkDir, snapshotID)
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return userSnapshotMeta{}, fmt.Errorf("create snapshot dir: %w", err)
	}
	stateFile := filepath.Join(rootDir, "state.snap")
	memFile := filepath.Join(rootDir, "mem.snap")
	diskFile := filepath.Join(rootDir, "disk.ext4")

	fc := newFCClient(sb.SocketPath, 10*time.Second)
	if err := fc.pauseVM(); err != nil {
		return userSnapshotMeta{}, fmt.Errorf("pause vm: %w", err)
	}
	resumeNeeded := true
	defer func() {
		if resumeNeeded {
			_ = fc.resumeVM()
		}
	}()

	_ = os.Remove(stateFile)
	_ = os.Remove(memFile)
	_ = os.Remove(diskFile)

	if err := fc.createFullSnapshot(stateFile, memFile); err != nil {
		return userSnapshotMeta{}, fmt.Errorf("create user snapshot: %w", err)
	}
	if err := materializeSandboxRootfs(s.cfg, sb.RootfsPath, diskFile); err != nil {
		return userSnapshotMeta{}, fmt.Errorf("persist snapshot disk: %w", err)
	}

	meta := userSnapshotMeta{
		SnapshotID:       snapshotID,
		Name:             strings.TrimSpace(name),
		CreatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
		StateFile:        stateFile,
		MemFile:          memFile,
		DiskFile:         diskFile,
		LineageID:        s.cfg.BaseRootfsLineageID,
		SourceSandboxID:  sb.ID,
		SourceRootfsPath: sb.RootfsPath,
	}
	if err := s.writeUserSnapshotMeta(meta); err != nil {
		return userSnapshotMeta{}, err
	}
	if err := fc.resumeVM(); err != nil {
		return userSnapshotMeta{}, fmt.Errorf("resume vm after snapshot: %w", err)
	}
	resumeNeeded = false
	return meta, nil
}

func (s *server) createSandboxFromUserSnapshot(id string, meta userSnapshotMeta) (*sandbox, error) {
	restoreStart := time.Now()
	for _, p := range []string{meta.StateFile, meta.MemFile, meta.DiskFile} {
		if !fileExists(p) {
			return nil, fmt.Errorf("snapshot artifact missing: %s", p)
		}
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
		start := time.Now()
		if err := materializeSandboxRootfs(s.cfg, meta.DiskFile, rootfsCopy); err != nil {
			cloneCh <- struct {
				err error
				dur time.Duration
			}{err: fmt.Errorf("clone user snapshot disk: %w", err), dur: time.Since(start)}
			return
		}
		cloneCh <- struct {
			err error
			dur time.Duration
		}{err: nil, dur: time.Since(start)}
	}()
	go func() {
		start := time.Now()
		nc, err := s.acquireNetns(id)
		netnsCh <- struct {
			nc  *netnsConfig
			err error
			dur time.Duration
		}{nc: nc, err: err, dur: time.Since(start)}
	}()

	cloneRes := <-cloneCh
	netnsRes := <-netnsCh
	prepOverlapDur := time.Since(restoreStart)
	if cloneRes.err != nil || netnsRes.err != nil {
		if netnsRes.nc != nil {
			s.releaseNetns(netnsRes.nc)
		}
		if cloneRes.err != nil {
			return nil, cloneRes.err
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
	socketWaitStart := time.Now()
	if err := waitForUnixSocketReady(socketPath, 1500*time.Millisecond); err != nil {
		_ = killProcessGroup(fcCmd)
		_ = killCgroup(cgroupPath)
		_ = logFile.Close()
		return nil, fmt.Errorf("firecracker api socket not ready: %w", err)
	}
	socketReadyDur := time.Since(socketWaitStart)
	if cgroupPath != "" {
		if err := movePidToCgroup(cgroupPath, fcCmd.Process.Pid); err != nil {
			_ = os.Remove(cgroupPath)
			cgroupPath = ""
		}
	}

	fc := newFCClient(socketPath, 10*time.Second)
	loadStart := time.Now()
	if err := loadSnapshotWithRetry(fc, meta.StateFile, meta.MemFile, true, 1500*time.Millisecond); err != nil {
		_ = killProcessGroup(fcCmd)
		_ = killCgroup(cgroupPath)
		_ = logFile.Close()
		return nil, fmt.Errorf("load snapshot: %w", err)
	}
	loadDur := time.Since(loadStart)

	vsockPath := filepath.Join(sbDir, "vsock.sock")
	agentWaitStart := time.Now()
	ac, err := waitForAgentReady(vsockPath, s.cfg.AgentPort, s.cfg.AgentWaitTimeout, s.cfg.AgentDialTimeout)
	if err != nil {
		_ = killProcessGroup(fcCmd)
		_ = killCgroup(cgroupPath)
		_ = logFile.Close()
		return nil, fmt.Errorf("wait for agent after snapshot: %w", err)
	}
	agentReadyDur := time.Since(agentWaitStart)
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
		return nil, fmt.Errorf("agent network config failed: %w", err)
	}
	guestNetDur := time.Since(guestNetStart)

	_ = logFile.Close()
	cleanupNet = false
	cleanupDir = false
	totalDur := time.Since(restoreStart)
	if s.cfg.EnableStageTimingLogs {
		log.Printf("snapshot restore timing: snapshot_id=%s sandbox_id=%s disk_materialize=%s netns_acquire=%s prep_overlap=%s socket_ready=%s snapshot_load=%s agent_ready=%s guest_net=%s total=%s", meta.SnapshotID, id, cloneRes.dur, netnsRes.dur, prepOverlapDur, socketReadyDur, loadDur, agentReadyDur, guestNetDur, totalDur)
	}
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

func (s *server) writeUserSnapshotMeta(meta userSnapshotMeta) error {
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode snapshot meta: %w", err)
	}
	raw = append(raw, '\n')
	metaPath := userSnapshotMetaPath(s.cfg.WorkDir, meta.SnapshotID)
	tmp := metaPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write snapshot meta: %w", err)
	}
	if err := os.Rename(tmp, metaPath); err != nil {
		return fmt.Errorf("persist snapshot meta: %w", err)
	}
	return nil
}

func (s *server) loadUserSnapshotMeta(snapshotID string) (userSnapshotMeta, error) {
	metaPath := userSnapshotMetaPath(s.cfg.WorkDir, snapshotID)
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return userSnapshotMeta{}, fmt.Errorf("read snapshot metadata: %w", err)
	}
	var meta userSnapshotMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return userSnapshotMeta{}, fmt.Errorf("decode snapshot metadata: %w", err)
	}
	if strings.TrimSpace(meta.SnapshotID) == "" {
		meta.SnapshotID = snapshotID
	}
	return meta, nil
}

func (s *server) listUserSnapshots() ([]userSnapshotMeta, error) {
	root := userSnapshotsDir(s.cfg.WorkDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []userSnapshotMeta{}, nil
		}
		return nil, fmt.Errorf("read snapshot directory: %w", err)
	}
	out := make([]userSnapshotMeta, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := s.loadUserSnapshotMeta(e.Name())
		if err != nil {
			continue
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}
