package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type fcClient struct {
	socketPath string
	http       *http.Client
}

func newFCClient(socketPath string, timeout time.Duration) *fcClient {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	tr := &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.DialTimeout("unix", socketPath, timeout)
		},
	}
	return &fcClient{
		socketPath: socketPath,
		http:       &http.Client{Transport: tr, Timeout: timeout},
	}
}

func (c *fcClient) doJSON(method, path string, payload any) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	} else {
		body = http.NoBody
	}

	req, err := http.NewRequest(method, "http://unix"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := bytes.TrimSpace(raw)
		if len(msg) == 0 {
			return fmt.Errorf("firecracker %s %s: status %d", method, path, resp.StatusCode)
		}
		return fmt.Errorf("firecracker %s %s: status %d body=%q", method, path, resp.StatusCode, string(msg))
	}
	return nil
}

func (c *fcClient) pauseVM() error {
	return c.doJSON(http.MethodPatch, "/vm", map[string]string{"state": "Paused"})
}

func (c *fcClient) resumeVM() error {
	return c.doJSON(http.MethodPatch, "/vm", map[string]string{"state": "Resumed"})
}

func (c *fcClient) createFullSnapshot(statePath, memPath string) error {
	return c.doJSON(http.MethodPut, "/snapshot/create", map[string]string{
		"snapshot_type": "Full",
		"snapshot_path": statePath,
		"mem_file_path": memPath,
	})
}

func (c *fcClient) loadSnapshot(statePath, memPath string, resume bool) error {
	return c.doJSON(http.MethodPut, "/snapshot/load", map[string]any{
		"snapshot_path": statePath,
		"mem_backend": map[string]any{
			"backend_type": "File",
			"backend_path": memPath,
		},
		"resume_vm": resume,
	})
}
