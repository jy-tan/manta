package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"manta/internal/agentrpc"
)

type agentConn struct {
	mu sync.Mutex
	c  net.Conn
	r  *bufio.Reader
}

func dialAgent(udsPath string, port int, timeout time.Duration) (*agentConn, error) {
	if strings.TrimSpace(udsPath) == "" {
		return nil, fmt.Errorf("agent uds path is empty")
	}
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid agent port: %d", port)
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	c, err := net.DialTimeout("unix", udsPath, timeout)
	if err != nil {
		return nil, err
	}

	ac := &agentConn{c: c, r: bufio.NewReader(c)}
	// Firecracker vsock device uses a simple line-based handshake:
	// CONNECT <port>\n -> OK <id>\n
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		_ = c.Close()
		return nil, err
	}
	line, err := agentrpc.ReadLine(ac.r, timeout)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if !strings.HasPrefix(line, "OK ") && strings.TrimSpace(line) != "OK" {
		_ = c.Close()
		return nil, fmt.Errorf("vsock CONNECT failed: %q", strings.TrimSpace(line))
	}
	_ = c.SetDeadline(time.Time{})
	return ac, nil
}

func (ac *agentConn) Close() error {
	if ac == nil || ac.c == nil {
		return nil
	}
	return ac.c.Close()
}

func (ac *agentConn) Call(req agentrpc.Request, timeout time.Duration) (agentrpc.Response, error) {
	ac.mu.Lock()
	defer ac.mu.Unlock()

	if ac.c == nil {
		return agentrpc.Response{}, errors.New("agent connection is nil")
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	_ = ac.c.SetDeadline(time.Now().Add(timeout))
	defer ac.c.SetDeadline(time.Time{})

	if err := agentrpc.WriteMessage(ac.c, req); err != nil {
		return agentrpc.Response{}, err
	}
	var resp agentrpc.Response
	if err := agentrpc.ReadMessage(ac.r, &resp); err != nil {
		return agentrpc.Response{}, err
	}
	if !resp.OK {
		if strings.TrimSpace(resp.Error) != "" {
			return resp, errors.New(resp.Error)
		}
		return resp, fmt.Errorf("agent returned ok=false")
	}
	return resp, nil
}

func waitForAgentReady(udsPath string, port int, timeout, dialTimeout time.Duration) (*agentConn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ac, err := dialAgent(udsPath, port, dialTimeout)
		if err == nil {
			// Ping verifies the agent has started and can service requests.
			_, perr := ac.Call(agentrpc.Request{Type: "ping"}, 2*time.Second)
			if perr == nil {
				return ac, nil
			}
			lastErr = perr
			_ = ac.Close()
		} else {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lastErr != nil {
		return nil, fmt.Errorf("agent not ready after %s: %w", timeout, lastErr)
	}
	return nil, fmt.Errorf("agent not ready after %s", timeout)
}
