package agentrpc

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// This package implements a tiny framed JSON RPC used between the host control
// plane and the in-guest agent. The framing is:
//
//   uint32_be payload_len
//   payload_len bytes of UTF-8 JSON
//
// A single connection can carry multiple request/response pairs.

const (
	// DefaultPort is the AF_VSOCK port the agent listens on in the guest.
	DefaultPort = 7777

	// MaxMessageBytes caps a single framed JSON payload to avoid OOM.
	MaxMessageBytes = 8 << 20 // 8 MiB
)

type Request struct {
	Type string `json:"type"` // "ping", "exec", "net"

	Exec *ExecRequest `json:"exec,omitempty"`
	Net  *NetRequest  `json:"net,omitempty"`
}

type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`

	Ping *PingResponse `json:"ping,omitempty"`
	Exec *ExecResponse `json:"exec,omitempty"`
	Net  *NetResponse  `json:"net,omitempty"`
}

type PingResponse struct {
	AgentVersion string `json:"agent_version"`
	NowUnixMs    int64  `json:"now_unix_ms"`
}

// ExecRequest supports both a shell command and an argv form. Exactly one of
// Cmd or Argv should be provided.
type ExecRequest struct {
	UseShell bool     `json:"use_shell"`
	Cmd      string   `json:"cmd,omitempty"`
	Argv     []string `json:"argv,omitempty"`
	Cwd      string   `json:"cwd,omitempty"`
	Env      []string `json:"env,omitempty"` // "KEY=value"

	TimeoutMs      int64 `json:"timeout_ms,omitempty"`       // 0 => server default
	MaxOutputBytes int64 `json:"max_output_bytes,omitempty"` // 0 => agent default
}

type ExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	TimedOut bool   `json:"timed_out"`
}

type NetRequest struct {
	Interface string `json:"interface,omitempty"` // default "eth0"
	Address   string `json:"address"`             // e.g. "172.16.5.2/30"
	Gateway   string `json:"gateway"`             // e.g. "172.16.5.1"
	DNS       string `json:"dns,omitempty"`       // e.g. "1.1.1.1"
}

type NetResponse struct {
	Configured bool `json:"configured"`
}

func WriteMessage(w io.Writer, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(raw) > MaxMessageBytes {
		return fmt.Errorf("agentrpc: message too large: %d bytes", len(raw))
	}

	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(raw)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(raw)
	return err
}

func ReadMessage(r *bufio.Reader, dst any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxMessageBytes {
		return fmt.Errorf("agentrpc: invalid message length: %d", n)
	}

	buf := make([]byte, int(n))
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}

	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

// ReadLine is used for Firecracker's host-side vsock-over-UDS handshake.
func ReadLine(r *bufio.Reader, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	type lineResult struct {
		s   string
		err error
	}
	ch := make(chan lineResult, 1)
	go func() {
		s, err := r.ReadString('\n')
		ch <- lineResult{s: s, err: err}
	}()

	select {
	case res := <-ch:
		return res.s, res.err
	case <-time.After(timeout):
		return "", fmt.Errorf("agentrpc: timed out reading line after %s", timeout)
	}
}
