# Benchmark Results

## Naive (v0.1.0)

- 50 samples
- min: `1.685s`, p50: `2.343s`, p95: `2.725s`, p99: `2.742s`, max: `2.757s`

## Agent (vsock RPC) (v0.1.1)

- 50 samples
- min: `416.438ms`, p50: `455.725ms`, p95: `472.275ms`, p99: `479.685ms`, max: `485.256ms`

## Snapshot restore + netns

- With `MANTA_ENABLE_SNAPSHOTS=1`
- 50 samples
- min: `144.394ms`, p50: `171.834ms`, p95: `190.617ms`, p99: `192.687ms`, max: `204.762ms`

