# Manta

Simple sandbox orchestrator for Firecracker microVMs.

## What it does

- `POST /create`: boots a new microVM and waits for SSH.
- `POST /exec`: runs a command in the VM via SSH.
- `POST /destroy`: tears down the VM and host networking state.

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
- `guest-artifacts/sandbox_key`
- `guest-artifacts/sandbox_key.pub`

## Run server

Server needs root privileges for tap devices, NAT rules, and rootfs mount operations.

```bash
sudo go run ./cmd/server
```

Environment variables:

- `MANTA_LISTEN_ADDR` (default `:8080`)
- `MANTA_KERNEL_PATH` (default `./guest-artifacts/vmlinux`)
- `MANTA_ROOTFS_PATH` (default `./guest-artifacts/rootfs.ext4`)
- `MANTA_SSH_KEY_PATH` (default `./guest-artifacts/sandbox_key`)
- `MANTA_FIRECRACKER_BIN` (default `firecracker`)
- `MANTA_HOST_IFACE` (default auto-detected from default route)
- `MANTA_WORK_DIR` (default `/tmp/manta`)

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

## Benchmark

Start server in one terminal:

```bash
sudo go run ./cmd/server
```

Run benchmark in another:

```bash
mkdir -p results
go run ./cmd/bench --iterations 50 > results/stage1-naive.json
```

The benchmark reports `min`, `p50`, `p95`, `p99`, `max` to `stderr` and writes machine-readable JSON summary to `stdout`.