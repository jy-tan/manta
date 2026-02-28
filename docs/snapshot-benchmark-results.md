# Snapshot Benchmark Results

## User snapshot restore TTI

- Fixture setup (untimed):
  - boot from baseline
  - apply deterministic mutation (`/opt/manta/state.txt = restored-ok`)
  - create user snapshot (`snapshot_id=us-1`)
  - destroy fixture sandbox
- Measured loop:
  - `POST /snapshot/restore`
  - `POST /exec` sanity command (`cat /opt/manta/state.txt`)
  - `POST /destroy` (not timed)
- 50 samples
- `restore_only` min: `105.671ms`, p50: `111.468ms`, p95: `117.760ms`, p99: `118.985ms`, max: `119.310ms`
- `restore_tti` min: `108.616ms`, p50: `114.177ms`, p95: `120.454ms`, p99: `121.843ms`, max: `121.932ms`
- failures: `restore_api=0`, `sanity_exec=0`, `destroy=0`, `other=0`

