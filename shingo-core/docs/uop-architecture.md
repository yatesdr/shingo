# UOP Architecture

UOP (Unit of Production) is the count of consumable parts in a physical bin and the count of parts on a lineside bench. This document describes how UOP is tracked, mutated, audited, and reconciled across `shingo-core` and `shingo-edge`.

## Bin-as-truth

Bins are the unit of inventory. A bin's UOP count belongs to the physical container — not to the slot it sits in, not to the order that's moving it, not to any cached projection on Edge. Movement does not change the count. Storage→storage moves, supermarket→line, line→supermarket, complex two-bin orders: none of them touch the count. The count changes only when parts physically enter or leave the bin.

Core owns the truth. Edge mirrors via deltas plus periodic reconciliation. When Edge's local cache disagrees with Core's row, Core wins.

**Note on ownership scope.** Core's authority is a function of the current operational topology, not a permanent design choice. Today, manual-swap loaders and unloaders imprint and remove bin manifests via Core's HTTP API — that's why Core is the natural central source of truth for bin state. As AMR systems take over those manual-swap roles and end-to-end production automation lands, the natural home for the authoritative bin row shifts to Edge: bins physically live at Edge, every count change originates from PLC events at Edge, and the Core-as-authority round-trip becomes overhead. When AMR controls all facets of production, the loader/unloader pattern dissolves and the ownership can flip to Edge with Core as the aggregating mirror. The current architecture is correct for the current reality; it is not the end state. Cleanup is straightforward when that transition is ready.

## The two authoritative rows

Two Postgres tables on `shingo-core` carry the entire physical-inventory picture:

| Row | Meaning | Sign |
|---|---|---|
| `bins.uop_remaining` | Count of parts in a physical bin | Signed; can go negative |
| `lineside_buckets.qty` | Count of parts on a lineside bench, captured from a bin during release | Non-negative |

Bins can go negative because real-world bins overpack and underpack. A nominal-1000 bin might physically hold 1005; the next nominal-1000 might hold 995. The system tracks reality, not the nominal. Over time, the discrepancies wash out at the inventory aggregate level.

Buckets stay non-negative because they're real-time counts of physical parts on a bench — you can't have a negative number of parts sitting in front of you. `drainLinesideFirst` clamps bucket draws at zero by construction.

The plant-wide inventory invariant is approximate, not exact:

```
total physical UOP ≈ sum(bins.uop_remaining) + sum(lineside_buckets.qty)
```

The `bin_sum` term can drift signed in either direction over time as overpack/underpack accumulates and washes out. The endpoint at `GET /api/inventory/invariant` reports both terms and the signed total; trends matter more than instantaneous values.

## Wire envelopes

Three envelope types carry inventory-state changes between Edge and Core. All three route through the existing `HandleData` channel (`protocol/ingestor.go`) with a subject discriminator. They do not add new methods to the `MessageHandler` interface.

### BinUOPDelta — Edge to Core

```go
type BinUOPDelta struct {
    Station     string
    BinID       int64
    PayloadCode string    // bin's actual current payload, NOT the claim's template
    Delta       int       // signed
    Reason      string    // consume_tick / produce_tick / capture_reduction / ab_fallthrough
    SequenceID  int64     // monotonic per (station, bin_id)
    WindowStart time.Time
    WindowEnd   time.Time
}
```

Subject: `inventory.bin_uop_delta`. Edge emits one envelope per accumulated delta window (default 5s, configurable via `uop.delta_flush_interval`). The accumulator at `inventory_delta_reporter.go` mirrors the existing `production_reporter.go` pattern: per-bin accumulation under a mutex, periodic flush via the outbox, restore on enqueue failure.

`PayloadCode` carries the bin's actual current payload at the moment of delta emission, not the target style's template payload. Core's `inventory_delta_service.go.ApplyBinUOPDelta` validates this against the bin row and rejects mismatches.

### LinesideBucketDelta — Edge to Core

Same shape as BinUOPDelta but keyed on `(node_id, pair_key, style_id, part_number)` instead of bin ID. Subject `inventory.lineside_bucket_delta`. Reasons: `capture_fill` (operator pulled parts to lineside on release), `consume_drain` (PLC tick drained the bucket).

Manual-swap nodes have no PLC and never emit bucket deltas. Their state changes are operator-driven and audited via direct insert.

### BinPickedUp — Core to Edge

```go
type BinPickedUp struct {
    OrderUUID  string
    BinID      int64
    Location   string
    PickedUpAt time.Time
}
```

Subject: `transit.bin_picked_up`. Core fires this when the RDS poller's `diffBlockStates` detects a pickup block transitioning to FINISHED — the same trigger that drives `BinService.MoveToTransit` in the bin-transit-state mechanism. Edge's `handler_bin_picked_up.go` receives the event, flushes the released bin's accumulator, and advances the active-claim transition.

The envelope exists to close the SEND PARTIAL BACK timing gap. When the operator releases a partial bin and the cell keeps cycling, ticks need to attribute to the released bin until the robot physically picks it up — not to the next claim. The pickup transition is the boundary signal.

Acceptable bias: if Edge crashes between Core sending BinPickedUp and Edge processing it, the active-claim transition is lost. Subsequent ticks attribute to the released bin instead of the new one. The reconciler corrects within the heartbeat interval (~60s). Rare scenario; bounded inaccuracy.

### Dedup

Every delta envelope carries a monotonic `SequenceID` per scope. Core's `inventory_delta_dedup` table records the last-applied sequence per `(station, scope_kind, scope_key)`. Replays with `seq <= last_applied` are silently dropped. Cycle counts and other operator actions that bypass the delta channel write directly to the bins table and bump the dedup sequence to invalidate any in-flight deltas with stale sequences.

## The reconciler

The reconciler at `uop_reconciler.go` keeps Edge's runtime cache aligned with Core's authoritative row. It is always-on; there is no toggle. Healing is unconditional — that is the mechanism's reason to exist.

It fires on the heartbeat path. Every successful heartbeat tick (default 60s, configurable via `uop.reconcile_interval` in `shingoedge.yaml`) calls `Engine.Reconcile(false)`. The since-last-pass gate at the reconciler coalesces if the heartbeat ever fires faster than the configured interval. If the heartbeat fails entirely (Core unreachable, goroutine death after `recover()`), reconciliation pauses too — accepted graceful degradation, since reconciliation against an unreachable Core has no work to do anyway.

Each pass fetches state from Core via `core_client.go.FetchUOPState`, walks bins and buckets, and overwrites Edge's local cache for any row where Edge disagrees with Core. For nodes Edge tracks but Core's response omits, the reconciler explicit-zeros the runtime — confirmed-empty is observation, not correction, and fires regardless of any other state.

### Pending-delta guard

The naive heal sequence has a race: Edge has emitted deltas that Core hasn't applied yet; Edge's cache is correct; reconciler comparison says Edge is wrong; heal stomps Edge with stale Core values. The guard prevents this.

`InventoryDeltaReporter` maintains `pendingBinIDs map[int64]struct{}` and `pendingBucketKeys map[bucketKey]struct{}`, guarded by the existing reporter mutex. On `RecordBin`/`RecordBucket`, the scope key is added. On successful `Flush` (after outbox enqueue confirms), it's removed. On Edge startup, the maps are populated from outbox-unflushed entries so post-crash state is correct.

Before healing a scope, the reconciler checks `IsPendingBinDelta`/`IsPendingBucketDelta`. If the scope has unflushed deltas, the heal is skipped this pass — the next pass after the flush succeeds will pick it up.

The same guard protects the operator's release click. `operator_release.go.ReleaseOrderWithLineside` checks `IsPendingBinDelta` after flushing and before shipping the `OrderRelease` envelope; if pending, it returns HTTP 409 Conflict so the UI shows a retry hint.

## Process semantics

These are SME-locked and treated as ground truth across the codebase. They are described here because they shape every decision below.

**UOP is assembly-normalized.** UOP counts production-cycle increments per bin's claim, not raw physical parts. A bin holding 100 UOP of a part that's used twice per assembly physically contains 200 parts; a bin holding 100 UOP of a part used once per assembly contains 100. The "unit of production" is the cycle of the consuming assembly, normalized to what each bin is tracking. When a multi-cavity die produces parts that fan out to separate bins (e.g., LH and RH halves into different containers), each receiving bin gets its own UOP delta per cycle — one physical cycle, multiple bin-level increments, each normalized to that bin's claim. Per-part details live in the manifest.

**The PLC counter is infinite.** Edge calculates per-tick deltas from the last-seen counter value. There is no "lost ticks on counter reset" failure mode.

**Bin transition for tick attribution is the bin physically leaving the slot, not the operator's release click.** On the consume side, the boundary is the `BinPickedUp` envelope (fired by Core's bin-transit-state mechanism when the RDS pickup block transitions to FINISHED). The operator's release click commits the bin's final-state intent — PARTIAL count, RELEASE EMPTY, or capture-to-lineside — but does not stop tick attribution. Cells routinely finish in-flight cycles between release click and physical pickup; those ticks legitimately belong to the released bin. If the operator pulled parts to a lineside container, those parts are captured into a bucket via the release disposition, and subsequent ticks decrement the bucket first via `drainLinesideFirst` before touching any bin's count. The bucket is location-bound (lives at the slot, not the bin), so it correctly continues to drain across the bin transition while the cell keeps running. On the produce/manual_swap loader side, the bin-loader confirm IS the boundary — there is no release/pickup gap on that side because the loader physically loads the bin at confirm time. For A/B cycling pairs, the boundary is a runtime state flip (active-pull change), wrapped in a single SQLite transaction at `operator_ab_cycling.go.FlipABNode` to eliminate the race window.

**Manual swap nodes have no PLC.** All state changes on manual-swap nodes (loaders, unloaders) are operator-driven. They write through Core's HTTP API (e.g., `LoadBin`) and audit via direct insert. They do not emit `LinesideBucketDelta` envelopes. This is a legitimate exception to "deltas are the only mutation path" — operator actions are conceptually different from automated PLC events.

**Cycle count is admin-only.** Operators do not cycle-count bins during production; SCO uses the Bins admin page on Core. Cycle count writes the new value to `bins.uop_remaining` directly and bumps the dedup sequence to invalidate in-flight deltas.

**Operator-trusted measurements.** SEND PARTIAL BACK count is ground truth — overrides the runtime cache. Produce-ingest count uses the operator-measured runtime value at finalize time, not the template's `UOPCapacity`.

## Release UI dispositions

The operator station's release modal exposes three buttons (per `commit 0becc04`, 2026-04-30):

| Button | When shown | Bin / bucket effect |
|---|---|---|
| `PULL PARTS LINESIDE, RELEASE` | Always (primary) | Bin reduced by `sum(captures)` via delta; lineside bucket increased; bin returns to supermarket |
| `RELEASE PARTIAL` | When `runtime.RemainingUOPCached > 0` | Bin returns to supermarket as-is; manifest preserved; count synced via `OrderRelease.RemainingUOP=&N` |
| `RELEASE EMPTY` | When `runtime.RemainingUOPCached == 0` | Bin returns empty; manifest cleared; count synced via `OrderRelease.RemainingUOP=&0` |

The capture path emits `BinUOPDelta(reason=capture_reduction, delta=-sum(captures))` only — no companion `RemainingUOP=&0` send. (This was the dual-write removed by the bin-as-truth refactor.) For partial-back and explicit-empty paths, the `OrderRelease` envelope carries the operator's count directly to Core's `BinManifestService.SyncOrClearForReleased`.

## Manifest-clearing trigger

When a bin's `uop_remaining` reaches zero, the manifest may need to clear so the bin can be reassigned. The trigger is conservative: it fires on operator-declared zero only, never on PLC-delta alone.

| Path to zero | Manifest auto-clears? |
|---|---|
| Operator hits RELEASE EMPTY | Yes |
| Operator captures everything via PULL PARTS LINESIDE (delta drives count to 0 with `reason=capture_reduction`) | Yes |
| Operator enters cycle count = 0 | No (SCO uses two-step: cycle to 0, then explicit clear) |
| Admin clears bin from Bins page | Yes (explicit operator action) |
| PLC consume tick drives count to 0 with `reason=consume_tick` | No (overpack scenario could leave bin physically non-empty) |
| RELEASE PARTIAL with operator count > 0 | No (operator says there are still parts) |

The trigger lives in `inventory_delta_service.go.ApplyBinUOPDelta`, post-update: when `valueBefore + d.Delta == 0` and the delta's reason is `capture_reduction`, it calls `BinManifestService.ClearForReuse` in the same transaction. Atomic, idempotent. The dedup table protects against replay re-firing the trigger.

The conservative behavior protects the overpack scenario. A bin nominal 100, physical 105: 100 ticks fire, count reaches 0, but the bin still has 5 parts. If the trigger fired on consume-tick zero, the manifest would clear and the operator's PARTIAL release of those 5 parts would land on a manifest-less bin — orphan state. Restricting the trigger to operator-declared paths means the manifest stays until the operator confirms intent.

## Audit log

Every mutation of `bins.uop_remaining` writes a row to `bin_uop_audit` (`shingo-core/store/migrations.go`):

```sql
CREATE TABLE bin_uop_audit (
  id              BIGSERIAL PRIMARY KEY,
  bin_id          BIGINT NOT NULL,
  before_uop      INT,
  suggested_uop   INT NULL,           -- system's suggested value at operator action; NULL for non-operator paths
  after_uop       INT NOT NULL,       -- can be negative (signed bin values)
  op              TEXT NOT NULL,
  source          TEXT NOT NULL,      -- file:line of caller
  order_id        BIGINT,
  payload_code    TEXT,
  actor           TEXT,               -- station / operator / system
  metadata        JSONB,              -- per-part diff context for multi-part overrides
  applied_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

The `suggested_uop` column is the system's expected value at the time of an operator action. Populated for `release_partial`, `release_capture`, `cycle_count`, `manual_clear`. Null for automated paths (`delta_consume`, `delta_produce`, etc.) where there is no operator suggestion to compare against.

Op tags distinguish operator-driven mutations from automated ones. Operator paths populate `suggested_uop`; automated paths do not. The distinction enables forensics like "operators who frequently override the system" or "stations where the system is consistently wrong."

For multi-part captures, the aggregate lands in `suggested_uop`/`after_uop`; per-part diff context goes in `metadata` (preserved by `BinManifestService.AuditReleaseOverride`).

## Operational characteristics

**Negative bin values are normal.** Overpack means a bin reads negative briefly until the operator catches it via PARTIAL release with the actual count. Underpack means the bin reads positive briefly and the operator releases EMPTY when they see it's actually empty. Either way, the inventory tracks physical reality. The `negativeBinCount` metric in the reconciler exposes this trend; an unbounded growth would indicate systematic miscalibration of nominal capacity.

**Transient inaccuracy on partial-back returns.** When the operator releases PARTIAL and the bin physically returns to supermarket, Edge's runtime cache for the slot may briefly show the new bin's full capacity until the reconciler heals (~60s). This is the cost of using the runtime cache directly instead of routing every completion through a Core read. Acceptable per the SME; reconciler always corrects.

**Reconciler ping-pong protection.** Without the pending-delta guard, the sequence "tick → cache decrements → reconciler runs before delta lands → heal stomps cache with stale Core value → next tick decrements again" would loop forever. The guard at `IsPendingBinDelta` skips the heal when deltas are pending, breaking the loop.

**Heartbeat coupling failure mode.** If the heartbeat goroutine ever exits (it has `recover()` around the loop, so this requires a real bug), reconciliation pauses too. Edge restart restores both. Heartbeat health is a useful proxy for reconciliation health.

## What is not in scope

The bin-as-truth architecture explicitly does not include:

- **Feature toggles** for the architecture itself. There is no `BIN_AS_TRUTH=on` flag, no `selfHeal` toggle, no `INVENTORY_DELTA_PUBLISH` switch. The architecture ships unconditionally.
- **Shadow stores or shadow columns.** One authoritative row per concept. No promotion-from-shadow lifecycle.
- **Phased deployment.** Items shipped in dependency order during the initial build; once landed, the system is the system.
- **Scrap accounting as an invariant problem.** SCO zeros bin UOP via shingo before physical scrap; the explicit zero preserves the invariant. There is no automated scrap event.
- **Backup-restore handling.** If Core is restored from a backup, the reconciler's behavior is undefined relative to the lost-since-backup state. Operational concern, not an architectural one. Stop Edge processes before restoring Core if needed.
- **Changeover refactor.** Plan/Apply test refactor and floor-level changeover bugs are tracked separately; not part of the UOP architecture.

## Quick reference

### Files

| Path | Purpose |
|---|---|
| `shingo-core/service/inventory_delta_service.go` | Apply bin/bucket deltas; manifest-clearing trigger |
| `shingo-core/store/audit/bin_uop.go` | Audit insert helpers |
| `shingo-core/rds/poller.go` | RDS poll → block-state diff → BinPickedUp |
| `shingo-core/engine/wiring_block_completed.go` | Bin-transit-state Phase 2 handler |
| `shingo-edge/messaging/inventory_delta_reporter.go` | Accumulator + pending-delta guard |
| `shingo-edge/messaging/handler_bin_picked_up.go` | BinPickedUp Edge handler |
| `shingo-edge/engine/uop_reconciler.go` | Reconciler |
| `shingo-edge/engine/wiring_counter_delta.go` | PLC tick → delta emission |
| `shingo-edge/engine/operator_release.go` | Release dispositions |
| `shingo-edge/engine/operator_ab_cycling.go.FlipABNode` | A/B flip with transactional wrap |
| `protocol/payloads.go` | Wire envelope definitions |

### Subject constants

```go
const (
    SubjectBinUOPDelta         = "inventory.bin_uop_delta"
    SubjectLinesideBucketDelta = "inventory.lineside_bucket_delta"
    SubjectBinPickedUp         = "transit.bin_picked_up"
)
```

### YAML config

```yaml
uop:
  reconcile_interval: 60s    # how often the reconciler runs (coupled to heartbeat tick)
  delta_flush_interval: 5s   # accumulator flush cadence
  tolerance:
    per_bin: 1               # acceptable drift per bin in UOP units
    sustained_drift: 0       # sustained per-station drift
    simultaneous_alarm: 5    # alert when N bins drift simultaneously
```

### HTTP endpoints

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/telemetry/uop-state?station=X&nodes=...` | Reconciler reads Core's authoritative state |
| GET | `/api/inventory/invariant` | Plant-wide invariant probe (signed bin sum + bucket sum) |
| GET | `/api/reconciliation/uop` | Reconciler metrics (passes, bins seen/healed, buckets seen/healed, flush failures, negative bin count) |
| GET | `/api/audit/bin/:id` | Per-bin audit timeline |
| GET | `/api/audit/operator/:name` | Per-operator activity |
| GET | `/api/audit/station/:station` | Per-station drift report |
| POST | `/api/admin/uop/backfill?station=X[&force=true]` | Manual bucket backfill trigger |
