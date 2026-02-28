# Benchmarking Manta

This document explains how to run Manta's benchmark harness and how to interpret the results.

The benchmark is designed to measure a single metric reliably: **TTI (Time to Interactive)** for a fresh sandbox.

Manta now includes two benchmark binaries:

- `cmd/bench`: baseline create-to-first-exec TTI
- `cmd/bench_restore`: user-snapshot restore benchmarks (`restore_only` and `restore_tti`)

## What We Measure (TTI)

**TTI** is the wall-clock time from:

1. Sending `POST /create` (boot a new microVM and wait for it to be ready)
2. Sending `POST /exec` (run the first command and get its output)

TTI intentionally excludes teardown. `/destroy` is always called, but it is not part of the timed interval.

This follows the [ComputeSDK benchmarks](https://github.com/computesdk/benchmarks).

For now, we'll optimize and benchmark locally until this project is in a deployable state.

### What "Ready" Means

In the current implementation, "ready" during `POST /create` means:

- Firecracker has booted the guest far enough that the in-guest agent (`manta-agent`) responds to a vsock `ping`.
- The host configures per-sandbox guest networking (IP address, default route, DNS) by sending a vsock RPC request to that agent.

This is a fast and deterministic readiness signal, but it is not the same as "everything the user might do is warmed/verified". For example, we do not attempt to validate outbound internet access (NAT + DNS + egress) beyond applying config, and we do not warm language runtimes or package caches.

## Prerequisites

You need the same prerequisites as running the server, plus a stable host environment:

- Linux host with KVM (`/dev/kvm`)
- Firecracker binary available (or set `MANTA_FIRECRACKER_BIN`)
- Root privileges to run the server (tap devices, NAT rules)
- Built guest artifacts: `guest-artifacts/vmlinux` and `guest-artifacts/rootfs.ext4`

## Running the Benchmark

Start the server (Terminal 1):

```bash
sudo go run ./cmd/server
```

Run the benchmark (Terminal 2):

```bash
mkdir -p results
go run ./cmd/bench --iterations 50 > results/stage1-naive.json
```

## Running User Snapshot Restore Benchmarks

Start the server with snapshots enabled (Terminal 1):

```bash
sudo env MANTA_ENABLE_SNAPSHOTS=1 go run ./cmd/server
```

Run restore benchmark (Terminal 2):

```bash
mkdir -p results
go run ./cmd/bench_restore --iterations 50 > results/stage-restore-user-snapshot.json
```

This benchmark has two phases:

1. Fixture setup (untimed): create sandbox, apply deterministic mutation, create user snapshot, destroy fixture sandbox.
2. Measured loop: restore sandbox from snapshot, run sanity `/exec`, stop timer, destroy sandbox (untimed).

It reports:

- `restore_only_*`: latency for `POST /snapshot/restore` only
- `restore_tti_*`: latency from restore request start to successful first sanity exec response
- failure classes (`restore_api`, `sanity_exec`, `destroy`, `other`)

## Benchmark Flags

The benchmark binary is `cmd/bench`. Common flags:

- `--endpoint` (default `http://localhost:8080`): server base URL
- `--iterations` (default `50`): number of measured runs
- `--warmup` (default `5`): warmup runs (not recorded)
- `--cmd` (default `echo "benchmark"`): command run during `/exec`
- `--timeout` (default `90s`): HTTP client timeout for API requests

Example: a smaller quick run:

```bash
go run ./cmd/bench --iterations 10 --warmup 2 --cmd 'uname -a'
```

## What One Iteration Does

Each iteration follows the same lifecycle:

1. `POST /create` -> get `sandbox_id`
2. `POST /exec` with `{sandbox_id, cmd}` -> ensure the sandbox is interactive
3. Measure elapsed time from iteration start to end of `/exec`
4. `POST /destroy` -> cleanup (not timed)

This means your results include:

- Host networking setup time (tap + iptables NAT)
- Rootfs clone time
- Firecracker startup time
- Guest boot time
- Time until the vsock agent is responding to `ping`
- Guest network configuration via vsock RPC
- First command execution via vsock RPC

## Output Format

The JSON output is a single object, with durations reported in nanoseconds:

- `iterations_requested`
- `iterations_success`
- `min_ns`
- `p50_ns`
- `p95_ns`
- `p99_ns`
- `max_ns`

The textual output prints the same percentiles as durations.

For `cmd/bench_restore`, output includes:

- `restore_only_min_ns`, `restore_only_p50_ns`, `restore_only_p95_ns`, `restore_only_p99_ns`, `restore_only_max_ns`
- `restore_tti_min_ns`, `restore_tti_p50_ns`, `restore_tti_p95_ns`, `restore_tti_p99_ns`, `restore_tti_max_ns`
- `failures` object with per-class failure counts

## Tips for Stable Measurements

If you care about comparing runs over time, try to keep these stable:

- Avoid heavy background disk activity (rootfs copy path is sensitive)
- Use the same filesystem (reflink support can drastically change results)
- Run on AC power / fixed CPU governor if applicable
- Keep Firecracker/kernel/rootfs artifacts unchanged between comparisons

If you change host settings (CPU pinning, kernel tunables), note them with the result file.

## Recording Results

Recommended result naming:

- `results/stage1-naive.json` for the baseline
- `results/stage2-<name>.json` and so on as we go.
- `results/stage-restore-user-snapshot.json` for user-snapshot restore runs

When you publish results, ideally include:

- host CPU model, RAM, storage type, filesystem type
- kernel version (host)
- Firecracker version
- Manta config knobs (e.g. VM mem/vcpu, cgroup enabled, host iface)

See:

- `docs/benchmark-results.md` for baseline create/exec benchmark history
- `docs/snapshot-benchmark-results.md` for user snapshot restore benchmark history

