# User Snapshots Design

This document describes the current user snapshot architecture in Manta after initial implementation of Phase A and Phase B work.

## Overview

Manta now supports:

- Golden snapshot restore for `/create` (baseline fast-path)
- User snapshot lifecycle APIs:
  - `POST /snapshot/create`
  - `POST /snapshot/restore`
  - `GET /snapshot/list`
  - `POST /snapshot/delete`

User snapshots are represented as snapshot bundles (state + memory + disk + metadata) stored under the server work directory.

## Goals

- Allow users to persist and restore sandbox state reliably.
- Keep restore correctness explicit through lineage checks.
- Preserve isolation with per-sandbox writable disks.
- Maintain predictable performance for restore-driven workflows.

## Non-goals (current implementation)

- Cross-host snapshot portability
- Multi-tenant authz enforcement for snapshot APIs
- Snapshot storage quotas and retention policies
- Differential/incremental snapshot compaction

## Current State

### Storage layout

Manta uses two snapshot stores:

- Golden snapshot store:
  - `${MANTA_WORK_DIR}/snapshot`
  - Contains golden `state.snap`, `mem.snap`, base disk, and metadata.
- User snapshot store:
  - `${MANTA_WORK_DIR}/user-snapshots/<snapshot_id>/`
  - Contains:
    - `state.snap`
    - `mem.snap`
    - `disk.ext4`
    - `meta.json`

### Snapshot metadata and lineage

The runtime computes a base rootfs lineage ID (`sha256` of configured rootfs) and records lineage in snapshot metadata.

Restore preflight validates lineage compatibility before loading snapshot artifacts. Incompatible lineage restores are rejected with an explicit error.

### Rootfs materialization strategy

Disk materialization remains per-sandbox and currently uses copy/reflink semantics:

- default: `cp --reflink=auto`
- strict mode: `MANTA_ROOTFS_CLONE_MODE=reflink-required` (`cp --reflink=always`)

Strict mode is a guardrail to avoid silent full-copy fallback on non-reflink filesystems.

### API behavior

`POST /snapshot/create`:

1. Locate running sandbox
2. Pause VM
3. Create Firecracker full snapshot (`state`, `mem`)
4. Persist disk artifact and metadata
5. Resume VM

`POST /snapshot/restore`:

1. Load snapshot metadata
2. Validate lineage
3. Create new sandbox workdir
4. Materialize per-sandbox disk from snapshot disk
5. Start Firecracker and load snapshot
6. Wait for agent readiness
7. Apply per-sandbox network config
8. Return new `sandbox_id`

## Benchmarks (Current)

User snapshot restore benchmark (`cmd/bench_restore`) currently reports roughly:

- restore-only p50: ~111 ms
- restore TTI (restore + first exec) p50: ~114 ms
- 50/50 successful runs in current test environment

See `docs/snapshot-benchmark-results.md` for tracked results.

## Design Trade-offs

- Persisting a per-snapshot `disk.ext4` keeps restore correctness simple and explicit, but adds storage overhead.
- Per-restore disk materialization preserves isolation and compatibility but keeps disk work in the critical path.
- Synchronous network configuration during restore keeps "ready" semantics strong, at the cost of some TTI.

## Limitations

- Snapshot store is local to one host/workdir.
- No built-in access control model for snapshot ownership yet.
- No quota/retention enforcement for user snapshot growth.
- Restore path still includes per-sandbox disk materialization (not yet shared-base writable-layer model).

## Next Steps

1. Improve restore-path observability with per-stage timing metrics.
2. Add optional fast-restore mode (defer guest network config) while keeping strict-ready mode default.
3. Move from per-restore disk materialization to shared immutable base + writable layer design.
4. Add snapshot policy controls (quotas, retention, cleanup tooling).
5. Add authz model and owner scoping for snapshot APIs.
