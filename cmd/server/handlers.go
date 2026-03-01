package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"manta/internal/agentrpc"
)

func (s *server) handleCreate(w http.ResponseWriter, _ *http.Request) {
	id := fmt.Sprintf("sb-%d", atomic.AddUint64(&s.nextSandboxID, 1))
	sb, err := s.createSandbox(id)
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
