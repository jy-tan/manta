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

## User snapshot restore TTI (post agent-connection snapshot fix)

- Change context:
  - closed persistent in-guest agent connection before snapshot capture to avoid restoring with stale active vsock stream state
  - kept same benchmark fixture and sanity validation
- 50 samples
- `restore_only` min: `43.016ms`, p50: `63.132ms`, p95: `69.228ms`, p99: `72.141ms`, max: `73.431ms`
- `restore_tti` min: `44.581ms`, p50: `65.881ms`, p95: `72.338ms`, p99: `75.323ms`, max: `76.583ms`
- failures: `restore_api=0`, `sanity_exec=0`, `destroy=0`, `other=0`

## User snapshot restore TTI (post polling interval tuning)

- Change context:
  - reduced unix socket readiness polling interval (`20ms` -> `2ms`)
  - reduced agent readiness polling interval (`50ms` -> `10ms`)
- 50 samples
- `restore_only` min: `34.765ms`, p50: `48.670ms`, p95: `58.910ms`, p99: `61.053ms`, max: `81.153ms`
- `restore_tti` min: `38.319ms`, p50: `50.696ms`, p95: `60.934ms`, p99: `67.959ms`, max: `85.371ms`
- failures: `restore_api=0`, `sanity_exec=0`, `destroy=0`, `other=0`

