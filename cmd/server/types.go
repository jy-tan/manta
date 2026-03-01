package main

import (
	"os/exec"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type config struct {
	ListenAddr          string
	KernelPath          string
	BaseRootfsPath      string
	BaseRootfsLineageID string
	RootfsCloneMode     string
	SSHPrivateKey       string
	FirecrackerBin      string
	HostNATIface        string
	WorkDir             string
	CgroupRoot          string
	EnableCgroups       bool

	// NetnsPoolSize controls how many pre-created netns+tap+veth "slots" we keep
	// around. When >0, /create acquires a slot instead of building netns/veth/tap
	// from scratch.
	NetnsPoolSize int

	// EnableSnapshots switches /create from "boot fresh VM" to "restore from a
	// golden snapshot". Snapshotting requires Firecracker snapshot support.
	EnableSnapshots bool

	// KeepFailedSandboxes keeps sandbox dirs/logs on create failure for easier
	// debugging of Firecracker startup/snapshot issues.
	KeepFailedSandboxes   bool
	EnableStageTimingLogs bool

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

	lifecycleMu  sync.Mutex
	state        sandboxState
	inFlightExec int
}

type server struct {
	cfg            config
	mu             sync.Mutex
	nextSandboxID  uint64
	nextSnapshotID uint64
	nextSubnet     uint32
	sandboxes      map[string]*sandbox
	netnsPool      *netnsPool
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
