# Benchmark Results

## Naive (v0.1.0)

- 50 samples
- min: `1.685s`, p50: `2.343s`, p95: `2.725s`, p99: `2.742s`, max: `2.757s`

## Agent (vsock RPC) (v0.1.1)

- 50 samples
- min: `416.438ms`, p50: `455.725ms`, p95: `472.275ms`, p99: `479.685ms`, max: `485.256ms`

## Snapshot restore + netns (v0.1.2)

- With `MANTA_ENABLE_SNAPSHOTS=1`
- 50 samples
- min: `144.394ms`, p50: `171.834ms`, p95: `190.617ms`, p99: `192.687ms`, max: `204.762ms`

## Netns pool + netlink (v0.1.3)

- With `MANTA_ENABLE_SNAPSHOTS=1`
- With `MANTA_NETNS_POOL_SIZE=64` (default)
- 50 samples
- min: `63.899ms`, p50: `83.897ms`, p95: `105.149ms`, p99: `111.205ms`, max: `124.351ms`

## Parallelized create setup

- With `MANTA_ENABLE_SNAPSHOTS=1`
- With default `MANTA_WORK_DIR=.manta-work`
- With `MANTA_NETNS_POOL_SIZE=64` (default)
- 50 samples
- min: `60.321ms`, p50: `71.301ms`, p95: `78.160ms`, p99: `86.923ms`, max: `87.134ms`

## Clone-mode guardrail check

- With `MANTA_ENABLE_SNAPSHOTS=1`
- With `MANTA_ROOTFS_CLONE_MODE=auto` (default behavior)
- 50 samples
- min: `61.809ms`, p50: `75.033ms`, p95: `81.306ms`, p99: `83.919ms`, max: `84.606ms`
- Note: this change is primarily a correctness/scalability guardrail (optional fail-fast on non-reflink filesystems), so no major p50 gain is expected when reflink already succeeds.

## Polling interval tuning (v0.1.4)

- With snapshots enabled (`MANTA_ENABLE_SNAPSHOTS=1`)
- With reduced readiness poll sleeps (socket: `20ms` -> `2ms`, agent: `50ms` -> `10ms`)
- 50 samples
- min: `48.767ms`, p50: `66.405ms`, p95: `74.683ms`, p99: `75.777ms`, max: `76.745ms`

## In-guest net config via netlink

- With snapshots enabled (`MANTA_ENABLE_SNAPSHOTS=1`)
- With rebuilt rootfs/snapshot including updated `manta-agent` network config path
- 50 samples
- min: `43.839ms`, p50: `51.424ms`, p95: `59.030ms`, p99: `60.765ms`, max: `61.623ms`

