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

## Netns pool + netlink

- With `MANTA_ENABLE_SNAPSHOTS=1`
- With `MANTA_NETNS_POOL_SIZE=64` (default)
- 50 samples
- min: `63.899ms`, p50: `83.897ms`, p95: `105.149ms`, p99: `111.205ms`, max: `124.351ms`

