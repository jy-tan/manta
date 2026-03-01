package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
)

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
