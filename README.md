# Manta

Simple sandbox orchestrator for Firecracker microVMs.

## What it does

- `POST /create`: boots a new microVM and waits for the in-guest agent (vsock RPC).
- `POST /exec`: runs a command in the VM via the agent (vsock RPC). SSH is kept as a debug fallback.
- `POST /destroy`: tears down the VM and host networking state.
- `POST /snapshot/create`: creates a user snapshot from a running sandbox.
- `POST /snapshot/restore`: restores a new sandbox from a user snapshot.
- `GET /snapshot/list`: lists user snapshots.
- `POST /snapshot/delete`: deletes a user snapshot.

## Prerequisites

- Linux host with KVM (`/dev/kvm`)
- Firecracker binary in `PATH` (or set `MANTA_FIRECRACKER_BIN`)
- Go `1.25+`
- `ip`, `iptables`, `mount`, `umount`, `cp`, `sysctl`
- `curl`, `tar`, `chroot`, `mkfs.ext4`, `resize2fs` (for rootfs build script)

## Build guest artifacts

Artifacts default to `./guest-artifacts`.

```bash
# Kernel build (long-running)
./guest/build-kernel.sh

# Rootfs build (requires root)
sudo ./guest/build-rootfs.sh
```

Kernel build script knobs:

- `JOBS=2 ./guest/build-kernel.sh` to reduce memory pressure
- `PREFER_CONTAINER_BUILD=always ./guest/build-kernel.sh` to build in podman/docker
- `AUTO_INSTALL_DEPS=0 ./guest/build-kernel.sh` to skip host package auto-install
- `KERNEL_VERSION=linux-6.1.y ./guest/build-kernel.sh` to pin kernel branch/ref
- `ALPINE_BRANCH=v3.20 sudo ./guest/build-rootfs.sh` to pin target Alpine branch
- `ALPINE_ARCH=x86_64 sudo ./guest/build-rootfs.sh` to select Alpine architecture
- `ROOTFS_SIZE_MB=1024 sudo ./guest/build-rootfs.sh` to control ext4 image size

You should end up with:

- `guest-artifacts/vmlinux`
- `guest-artifacts/rootfs.ext4`
- `guest-artifacts/manta-agent`
- `guest-artifacts/sandbox_key`
- `guest-artifacts/sandbox_key.pub`

## Run server

Server needs root privileges for tap devices and NAT rules.

```bash
sudo go run ./cmd/server
```

Environment variables:

<table>
  <thead>
    <tr>
      <th>Env Var</th>
      <th>Default Value</th>
      <th>Notes</th>
    </tr>
  </thead>
  <tbody>
    <tr>
      <td><code>MANTA_LISTEN_ADDR</code></td>
      <td><code>:8080</code></td>
      <td>HTTP listen address for the API server.</td>
    </tr>
    <tr>
      <td><code>MANTA_KERNEL_PATH</code></td>
      <td><code>./guest-artifacts/vmlinux</code></td>
      <td>Path to guest kernel image.</td>
    </tr>
    <tr>
      <td><code>MANTA_ROOTFS_PATH</code></td>
      <td><code>./guest-artifacts/rootfs.ext4</code></td>
      <td>Path to base guest rootfs image.</td>
    </tr>
    <tr>
      <td><code>MANTA_SSH_KEY_PATH</code></td>
      <td><code>./guest-artifacts/sandbox_key</code></td>
      <td>SSH private key path (debug fallback path).</td>
    </tr>
    <tr>
      <td><code>MANTA_FIRECRACKER_BIN</code></td>
      <td><code>firecracker</code></td>
      <td>Firecracker binary to execute.</td>
    </tr>
    <tr>
      <td><code>MANTA_HOST_IFACE</code></td>
      <td><em>auto-detected</em></td>
      <td>Host egress interface used for NAT.</td>
    </tr>
    <tr>
      <td><code>MANTA_WORK_DIR</code></td>
      <td><code>.manta-work</code></td>
      <td>Runtime state/work directory; canonical production path is <code>/var/lib/manta</code>.</td>
    </tr>
    <tr>
      <td><code>MANTA_NETNS_POOL_SIZE</code></td>
      <td><code>64</code></td>
      <td>Pre-created netns slots; on exhaustion, server falls back to on-demand netns creation.</td>
    </tr>
    <tr>
      <td><code>MANTA_ENABLE_SNAPSHOTS</code></td>
      <td><code>1</code></td>
      <td>Set to <code>0</code> to disable golden snapshot restore on <code>/create</code>.</td>
    </tr>
    <tr>
      <td><code>MANTA_ROOTFS_CLONE_MODE</code></td>
      <td><code>auto</code></td>
      <td>Rootfs materialization mode; use <code>reflink-required</code> to fail fast when reflink is unavailable.</td>
    </tr>
    <tr>
      <td><code>MANTA_EXEC_TRANSPORT</code></td>
      <td><code>agent</code></td>
      <td>Execution transport; set <code>ssh</code> for debug fallback.</td>
    </tr>
    <tr>
      <td><code>MANTA_AGENT_PORT</code></td>
      <td><code>7777</code></td>
      <td>Vsock port for the in-guest agent.</td>
    </tr>
    <tr>
      <td><code>MANTA_AGENT_WAIT_TIMEOUT</code></td>
      <td><code>30s</code></td>
      <td>Max wait for agent readiness.</td>
    </tr>
    <tr>
      <td><code>MANTA_AGENT_DIAL_TIMEOUT</code></td>
      <td><code>250ms</code></td>
      <td>Per-attempt agent dial timeout.</td>
    </tr>
    <tr>
      <td><code>MANTA_AGENT_CALL_TIMEOUT</code></td>
      <td><code>20s</code></td>
      <td>Agent RPC call timeout.</td>
    </tr>
    <tr>
      <td><code>MANTA_DEBUG_KEEP_FAILED_SANDBOX</code></td>
      <td><code>0</code></td>
      <td>Set to <code>1</code> to keep failed sandbox dirs/logs for debugging.</td>
    </tr>
  </tbody>
</table>

Disk model note:

- Manta uses per-sandbox writable rootfs materialization (default via reflink-capable clone semantics when available).
- In snapshot restore paths, user snapshot state/memory are restored first, and a per-sandbox writable disk is materialized from the snapshot disk artifact.

## API examples

```bash
curl -s -X POST http://localhost:8080/create

curl -s -X POST http://localhost:8080/exec \
  -H 'content-type: application/json' \
  -d '{"sandbox_id":"sb-1","cmd":"echo hello"}'

curl -s -X POST http://localhost:8080/destroy \
  -H 'content-type: application/json' \
  -d '{"sandbox_id":"sb-1"}'
```

User snapshot APIs:

```bash
# Create user snapshot from running sandbox
curl -s -X POST http://localhost:8080/snapshot/create \
  -H 'content-type: application/json' \
  -d '{"sandbox_id":"sb-1","name":"my-snapshot"}'

# Restore a new sandbox from snapshot
curl -s -X POST http://localhost:8080/snapshot/restore \
  -H 'content-type: application/json' \
  -d '{"snapshot_id":"us-1"}'

# List snapshots
curl -s http://localhost:8080/snapshot/list

# Delete snapshot
curl -s -X POST http://localhost:8080/snapshot/delete \
  -H 'content-type: application/json' \
  -d '{"snapshot_id":"us-1"}'
```

## Benchmark

Start server in one terminal:

```bash
sudo go run ./cmd/server
```

Run benchmark in another:

```bash
mkdir -p results
go run ./cmd/bench --iterations 50 > results/[filename].json
```

The benchmark reports `min`, `p50`, `p95`, `p99`, `max` to `stderr` and writes machine-readable JSON summary to `stdout`.

Restore-focused benchmark:

```bash
go run ./cmd/bench_restore --iterations 50 > results/[filename].json
```

This benchmark creates a fixture user snapshot once (untimed), then measures per-iteration restore latency:

- `restore_only`: `POST /snapshot/restore`
- `restore_tti`: restore + first sanity `/exec`
- `/destroy` is always called but excluded from timed interval