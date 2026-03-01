package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
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

var snapshotIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func normalizeSnapshotID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", fmt.Errorf("snapshot_id is required")
	}
	if !snapshotIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid snapshot_id")
	}
	return id, nil
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
	snapshotID, err := normalizeSnapshotID(req.SnapshotID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	meta, err := s.loadUserSnapshotMeta(snapshotID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	currentLineage := strings.TrimSpace(s.cfg.BaseRootfsLineageID)
	metaLineage := strings.TrimSpace(meta.LineageID)
	if currentLineage != "" {
		if metaLineage == "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "snapshot meta missing lineage id"})
			return
		}
		if metaLineage != currentLineage {
			writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("snapshot lineage mismatch (snapshot=%s current=%s)", metaLineage, currentLineage)})
			return
		}
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
	snapshotID, err := normalizeSnapshotID(req.SnapshotID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := os.RemoveAll(userSnapshotRootDir(s.cfg.WorkDir, snapshotID)); err != nil {
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
	snapshotID, err := normalizeSnapshotID(snapshotID)
	if err != nil {
		return userSnapshotMeta{}, err
	}
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
