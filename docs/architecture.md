# Manta Architecture

This document explains the architecture and key components of Manta, a minimal cloud sandbox provider built on Firecracker microVMs.

## Cloud Sandbox Provider

A cloud sandbox provider gives users on-demand isolated Linux environments via an API. The user calls "create," gets a sandbox, runs commands in it, and destroys it when done. The sandbox runs on the provider's infrastructure, not the user's machine.

A sandbox must:

- **Be isolated.** One user's sandbox cannot see or affect another's, or the host machine.
- **Be general purpose.** Users can run arbitrary commands, scripts, and programs. Install packages. Make network requests. It behaves like a real Linux machine.
- **Be remote.** A real network separates the user from the sandbox.
- **Be programmatic.** Create, use, and destroy via API calls.

## Goal

Manta provides a very small remote API for sandbox lifecycle:

- `POST /create` -> boot a sandbox VM and return `sandbox_id`
- `POST /exec` -> run a command inside that VM
- `POST /destroy` -> tear down VM and host resources

## High-Level Architecture

At a high level, this is a single-host control plane plus one microVM per sandbox:

- **Control plane:** Go HTTP server in `cmd/server/main.go`
- **Isolation runtime:** Firecracker process per sandbox
- **Guest OS artifacts:** shared kernel + base rootfs image
- **Per-sandbox writable disk:** cloned rootfs per VM
- **Command transport:** vsock RPC to an in-guest agent (default); SSH is kept for debugging
- **Network path:** per-sandbox host tap device + tiny `/30` subnet + host NAT (`iptables MASQUERADE`) for outbound internet access

### Architecture Diagram

```mermaid
graph TB
  Client[Client]

  subgraph Host["Host machine"]
    APIServer["Go API server\n(cmd/server)"]
    Net["Host networking (tap + /30 + iptables NAT)"]
    Artifacts["Guest artifacts (vmlinux + base rootfs.ext4)"]
    PerSandboxDisk["Per-sandbox rootfs clone (work dir)"]
    FC["Firecracker process (one per sandbox)"]
  end

  subgraph Guest["microVM (guest)"]
    Kernel["Linux kernel (vmlinux)"]
    Rootfs["Alpine root filesystem (ext4)"]
    Agent["manta-agent (vsock RPC)"]
    SSHD["sshd (debug)"]
    Workload["User workload(command execution)"]
  end

  Client -->|HTTP JSON| APIServer

  APIServer -->|ip + iptables| Net
  APIServer -->|cp --reflink=auto| PerSandboxDisk
  APIServer -->|write vm-config.json| FC
  APIServer -->|vsock RPC| Agent
  APIServer -.->|SSH (debug)| SSHD

  Artifacts --> FC
  Artifacts --> PerSandboxDisk
  Net <--> FC
  PerSandboxDisk --> FC

  FC -->|boots| Kernel
  Kernel --> Rootfs
  Rootfs --> Agent
  Rootfs -.-> SSHD
  Agent --> Workload
```

### Data Flow Diagram

```mermaid
sequenceDiagram
  participant C as Client
  participant S as API server
  participant H as Host OS (net/fs)
  participant F as Firecracker
  participant G as Guest (manta-agent)

  C->>S: POST /create
  S->>H: Create tap + assign IP
  S->>H: Add NAT rule (iptables MASQUERADE)
  S->>H: Clone rootfs
  S->>H: Write Firecracker config JSON
  S->>F: Start Firecracker process
  S->>G: Wait for agent readiness (vsock ping)
  S->>G: Configure guest network (ip addr/route + DNS)
  S-->>C: 200 {sandbox_id}

  C->>S: POST /exec {sandbox_id, cmd}
  S->>G: RPC exec request (timeout enforced)
  S-->>C: 200 {stdout, stderr, exit_code}

  C->>S: POST /destroy {sandbox_id}
  S->>F: Kill process + wait
  S->>H: Remove NAT + delete tap + rm sandbox dir
  S-->>C: 200 {status:"ok"}
```

### Request/Execution Path

1. Client calls `POST /create`
2. Server allocates host networking (tap + `/30` subnet + NAT)
3. Server clones rootfs for sandbox
4. Server writes Firecracker config JSON (including vsock device)
5. Server starts Firecracker process
6. Server waits for agent readiness in guest (vsock ping)
7. Server configures per-sandbox guest networking via agent
8. Server returns `sandbox_id`
9. Client calls `POST /exec` with command
10. Server sends exec request to agent and returns stdout/stderr/exit code
11. Client calls `POST /destroy`
12. Server kills VM process and cleans networking + files

## Key Components

### API Server (`cmd/server/main.go`)

This is the control plane and orchestration layer.

Responsibilities:

- Validates environment and prerequisites (`/dev/kvm`, artifacts, Firecracker binary)
- Exposes HTTP API (`/create`, `/exec`, `/destroy`, `/healthz`)
- Maintains in-memory sandbox map and IDs
- Runs host commands for network setup and cleanup
- Starts/stops Firecracker processes
- Handles agent readiness (vsock ping) and command execution (vsock RPC)

### Firecracker Runtime

Each sandbox is backed by one Firecracker process. This provides stronger isolation than containers (separate guest kernel per sandbox).

Responsibilities:

- Boots a microVM from supplied kernel + rootfs config
- Attaches virtual network device to host tap
- Runs isolated guest kernel/userspace

### Guest Kernel (`guest/build-kernel.sh`)

Single reusable `vmlinux` artifact, built with Firecracker-compatible config.

Why required:

- Firecracker needs a Linux kernel image to boot every VM.
- Reusing one kernel artifact keeps the system simple.

### Guest Rootfs (`guest/build-rootfs.sh`)

Builds Alpine-based `rootfs.ext4` and SSH key artifacts. This provides the root filesystem that contains the userspace programs and config the kernel will run after it finishes booting.

What it includes:

- OpenRC init setup
- `manta-agent` (vsock RPC server) enabled on boot
- `openssh-server` (debug access)
- Base utilities + optional tooling (`python3`, `nodejs`, `npm`, etc.)
- `iproute2` for runtime network configuration

### Per-Sandbox Rootfs Clone

On each create, Manta copies the base rootfs to a sandbox-specific file using:

- `cp --reflink=auto ...`

Why required:

- Each VM needs writable disk state isolated from other VMs.

### Agent Command Channel (vsock RPC)

`/exec` sends an RPC request over Firecracker vsock to an in-guest agent which runs the command and returns stdout/stderr/exit code.

Why required:

- It avoids SSH handshake overhead and makes readiness deterministic.
- It enables post-boot configuration (like per-sandbox networking) without mutating the rootfs image on disk.

Trade-off:

- Requires a small amount of custom code inside the guest (the agent binary and init service).

### Host Networking Layer

Per sandbox:

- tap device (`tap-sb-*`)
- private `/30` subnet (`172.16.X.0/30` pattern)
- host IP (`.1`) and guest IP (`.2`)
- NAT rule via iptables `MASQUERADE` on host egress interface

What this means:

- **tap device (per-sandbox):** a host-side virtual Ethernet interface created for a single microVM. Firecracker attaches the VM's virtual NIC to this tap, giving the host a direct L2 link used for guest egress traffic (and optional debug SSH access).
- **`/30` subnet:** CIDR mask `255.255.255.252` (4 IPs total, 2 usable). Manta uses it like a point-to-point link:
  - subnet: `172.16.X.0/30`
  - host (tap) IP: `172.16.X.1`
  - guest IP: `172.16.X.2`
- **`iptables MASQUERADE`:** source NAT for outbound traffic. When the guest sends packets to the internet from `172.16.X.2`, the host rewrites the source IP to the host's egress IP so replies can return and be mapped back to the guest connection.

Host vs guest configuration:

- **On the host:** create the tap, assign the host IP, enable IPv4 forwarding, and add/remove the NAT rule.
- **In the guest (microVM):** the network interface still needs configuration (IP address, netmask, default gateway, DNS). Manta configures this post-boot via vsock RPC by running `ip addr/route` commands in the guest through the agent.

Why required:

- Guest workloads need outbound network access.
