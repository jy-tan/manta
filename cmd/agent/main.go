package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"
	"github.com/vishvananda/netlink"

	"manta/internal/agentrpc"
)

const agentVersion = "v0.2.0"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	ln, err := vsock.Listen(uint32(agentrpc.DefaultPort), nil)
	if err != nil {
		log.Fatalf("vsock listen: %v", err)
	}
	log.Printf("manta-agent listening: port=%d version=%s", agentrpc.DefaultPort, agentVersion)

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go serveConn(c)
	}
}

func serveConn(c net.Conn) {
	defer c.Close()

	br := bufio.NewReader(c)
	for {
		var req agentrpc.Request
		if err := agentrpc.ReadMessage(br, &req); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			// Connection-level protocol error; close.
			log.Printf("read request: %v", err)
			return
		}

		resp := handle(req)
		if err := agentrpc.WriteMessage(c, resp); err != nil {
			log.Printf("write response: %v", err)
			return
		}
	}
}

func handle(req agentrpc.Request) agentrpc.Response {
	switch req.Type {
	case "ping":
		return agentrpc.Response{
			OK: true,
			Ping: &agentrpc.PingResponse{
				AgentVersion: agentVersion,
				NowUnixMs:    time.Now().UnixMilli(),
			},
		}
	case "exec":
		if req.Exec == nil {
			return agentrpc.Response{OK: false, Error: "missing exec payload"}
		}
		out := runExec(*req.Exec)
		return agentrpc.Response{OK: out.err == nil, Error: errString(out.err), Exec: out.resp}
	case "net":
		if req.Net == nil {
			return agentrpc.Response{OK: false, Error: "missing net payload"}
		}
		if err := configureNetwork(*req.Net); err != nil {
			return agentrpc.Response{OK: false, Error: err.Error(), Net: &agentrpc.NetResponse{Configured: false}}
		}
		return agentrpc.Response{OK: true, Net: &agentrpc.NetResponse{Configured: true}}
	default:
		return agentrpc.Response{OK: false, Error: fmt.Sprintf("unknown request type %q", req.Type)}
	}
}

type execResult struct {
	resp *agentrpc.ExecResponse
	err  error
}

func runExec(req agentrpc.ExecRequest) execResult {
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 20 * time.Second
	}

	maxOut := req.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = 1 << 20 // 1 MiB per stream
	}

	argv, err := normalizeArgv(req)
	if err != nil {
		return execResult{resp: &agentrpc.ExecResponse{ExitCode: 2}, err: err}
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	if strings.TrimSpace(req.Cwd) != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Env = append(os.Environ(), req.Env...)

	// Put the command in its own process group so we can SIGKILL the whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return execResult{resp: &agentrpc.ExecResponse{ExitCode: 1}, err: err}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return execResult{resp: &agentrpc.ExecResponse{ExitCode: 1}, err: err}
	}

	if err := cmd.Start(); err != nil {
		return execResult{resp: &agentrpc.ExecResponse{ExitCode: 127}, err: err}
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutDone := make(chan struct{}, 1)
	stderrDone := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(&stdoutBuf, io.LimitReader(stdoutPipe, maxOut))
		stdoutDone <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(&stderrBuf, io.LimitReader(stderrPipe, maxOut))
		stderrDone <- struct{}{}
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var timedOut bool
	var waitErr error
	select {
	case waitErr = <-waitCh:
		// ok
	case <-time.After(timeout):
		timedOut = true
		killProcessGroup(cmd)
		_ = <-waitCh
		// On timeout we intentionally treat the kill/wait outcome as success and
		// return a synthetic exit code.
		waitErr = nil
	}

	<-stdoutDone
	<-stderrDone

	exitCode := 0
	if timedOut {
		exitCode = 124
	} else if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ProcessState.ExitCode()
			waitErr = nil
		} else {
			exitCode = 1
		}
	}

	return execResult{
		resp: &agentrpc.ExecResponse{
			ExitCode: exitCode,
			Stdout:   stdoutBuf.String(),
			Stderr:   stderrBuf.String(),
			TimedOut: timedOut,
		},
		err: waitErr,
	}
}

func normalizeArgv(req agentrpc.ExecRequest) ([]string, error) {
	cmd := strings.TrimSpace(req.Cmd)
	if req.UseShell {
		if cmd == "" {
			return nil, fmt.Errorf("use_shell set but cmd is empty")
		}
		return []string{"/bin/sh", "-lc", cmd}, nil
	}
	if len(req.Argv) == 0 {
		if cmd != "" {
			return nil, fmt.Errorf("cmd provided without use_shell; provide argv or set use_shell")
		}
		return nil, fmt.Errorf("argv is required when not using shell")
	}
	return req.Argv, nil
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// With Setpgid=true, pgid == pid for the child. A negative pid targets the group.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}

func configureNetwork(req agentrpc.NetRequest) error {
	iface := strings.TrimSpace(req.Interface)
	if iface == "" {
		iface = "eth0"
	}
	addr := strings.TrimSpace(req.Address)
	gw := strings.TrimSpace(req.Gateway)
	if addr == "" || gw == "" {
		return fmt.Errorf("address and gateway are required")
	}
	gateway := net.ParseIP(gw)
	if gateway == nil {
		return fmt.Errorf("invalid gateway ip %q", gw)
	}

	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("lookup interface %q: %w", iface, err)
	}
	// Bring link up and overwrite any prior config from the base image.
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("set interface %q up: %w", iface, err)
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list addresses on %q: %w", iface, err)
	}
	for _, existing := range addrs {
		if err := netlink.AddrDel(link, &existing); err != nil {
			return fmt.Errorf("remove address %q on %q: %w", existing.String(), iface, err)
		}
	}

	parsedAddr, err := netlink.ParseAddr(addr)
	if err != nil {
		return fmt.Errorf("parse interface address %q: %w", addr, err)
	}
	if err := netlink.AddrAdd(link, parsedAddr); err != nil {
		return fmt.Errorf("assign address %q to %q: %w", addr, iface, err)
	}
	if err := netlink.RouteReplace(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       nil, // default route
		Gw:        gateway,
	}); err != nil {
		return fmt.Errorf("set default route via %q dev %q: %w", gw, iface, err)
	}

	if dns := strings.TrimSpace(req.DNS); dns != "" {
		_ = os.WriteFile("/etc/resolv.conf", []byte("nameserver "+dns+"\n"), 0o644)
	}

	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
