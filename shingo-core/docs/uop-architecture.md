# UOP Architecture

UOP (Unit of Production) is the count of consumable parts in a physical bin and the count of parts on a lineside bench. This document describes how UOP is tracked, mutated, audited, and propagated across `shingo-core` and `shingo-edge`.

## Ownership: Edge-authoritative, Core-aggregating

Bins are the unit of inventory. A bin's UOP count belongs to the physical container — not to the slot it sits in, not to the order that's moving it, not to any cached projection. Movement does not change the count. Storage→storage moves, supermarket→line, line→supermarket, complex two-bin orders: none of them touch the count. The count changes only when parts physically enter or leave the bin.

**Post-2026-05-04 (commit `6d226d1`):** Edge is authoritative for the count of any bin physically at one of its nodes. The bin's runtime cache on Edge — `process_node_runtime_states.remaining_uop_cached`, indexed by `active_bin_id` — is the source of truth for at-node bins. Core mirrors via the delta stream. There is no reconciler healing Edge from Core; if the delta stream drops a message and Edge's local truth diverges from Core's mirror, `FlushFailures` surfaces the problem.

This is a flip from the prior architecture, where Core was authoritative and a reconciler healed Edge's local cache from Core every ~60s. The flip happened because plant logs showed the reconciler was healing *correct* Edge decrements with *frozen* Core values from a broken delta pipeline — masking the underlying bug. The new "trust the bus, surface failures, no heal" model relies on Kafka delivery + Core's `inventory_delta_dedup` for correctness and dashboards (`FlushFailures`, consumer lag) for ops visibility.

**What Core remains authoritative for:** cross-station inventory aggregation, the manifest on a bin, the audit trail. Edge owns the per-tick count; Core owns the inventory picture across all stations.

**What the old reconciler did:** if you read older code comments referring to `uop_reconciler.go`, `IsPendingBinDelta`, `SetReconcileFn`, or "heal/healing/self-heal," they pre-date the flip. The relevant code is gone; the comments are stale (Phase 0 of the refactor swept the worst offenders).

## The two authoritative rows

Two tables carry the entire physical-inventory picture. Edge's runtime cache is authoritative for at-node bins; Core's `bins.uop_remaining` aggregates across stations.

| Row | Meaning | Sign | Authoritative for |
|---|---|---|---|
| `process_node_runtime_states.remaining_uop_cached` (Edge SQLite) | Count of parts in the bin currently at this Edge node | Signed; can go negative | At-node bins, this Edge |
| `bins.uop_remaining` (Core Postgres) | Count of parts in a physical bin, mirrored from Edge's delta stream | Signed; can go negative | Bins in transit / at supermarket / cross-station |
| `lineside_buckets.qty` (Core Postgres) | Count of parts on a lineside bench, captured during release | Non-negative | Plant-wide |

Bins can go negative because real-world bins overpack and underpack. A nominal-1000 bin might physically hold 1005; the next nominal-1000 might hold 995. The system tracks reality, not the nominal. Over time, the discrepancies wash out at the inventory aggregate level.

Buckets stay non-negative because they're real-time counts of physical parts on a bench — you can't have a negative number of parts in front of you. `drainLinesideFirst` clamps bucket draws at zero by construction.

The plant-wide inventory invariant is approximate:

```
total physical UOP ≈ sum(bins.uop_remaining) + sum(lineside_buckets.qty)
```

The bin-sum term drifts signed in either direction over time as overpack/underpack accumulates and washes out. `GET /api/inventory/invariant` reports both terms and the signed total; trends matter more than instantaneous values.

## Package structure

UOP state mutations route through dedicated packages on each side. The packages own the chokepoint; everything else (engine, www, messaging, dispatch) depends on the narrow interfaces.

### `shingo-edge/uop/`

The Edge-side mutator. Holds:

- `Mutator` — the public type with sixteen intent verbs across six concerns (Ticker, SlotWriter, Capturer, Pickup, Boundary, Backfiller).
- `accumulator` (unexported) — per-bin and per-bucket signed-delta accumulation, periodic flush to outbox, restore-on-failure.
- Narrow store interfaces (`runtimeWriter`, `bucketStore`, `nodeStore`) so the package never imports engine. `*store.DB` satisfies all three at the composition root.
- Verb implementations split by concern across `tick.go` (Consumed/Produced/Fallthrough), `slot.go` (Bind*/Clear*/Prepare*/SetClaim*/OnDelivered/ManualLoad), `capture.go` (CaptureToLineside), `pickup.go` (OnBinPickedUp), `boundary.go` (MarkAttributionBoundary), `backfill.go` (Backfill), `admin.go` (AdjustBucket), `release.go` (ReleaseDisposition + pure functions).
- `archtest_test.go` — CI test that fails if any production file outside `uop/` calls `RecordBin` or `RecordBucket` directly. Every delta emission must route through a named verb.

### `shingo-core/uop/`

The Core-side applier. Holds:

- `InventoryDeltaService` — receives `BinUOPDelta` and `LinesideBucketDelta` envelopes from Edge, dedups against `inventory_delta_dedup`, applies the signed delta to `bins.uop_remaining` / `lineside_buckets`, writes the audit row, fires `ClearForReuse` when a `capture_reduction` drives `uop_remaining` to zero.
- `ManifestClearer` narrow interface — the `*sql.Tx`-taking method used to fire `BinManifestService.ClearForReuse` atomically inside the delta-apply transaction. Atomicity is load-bearing; a crash between the bin update and manifest clear would leave a bin with `uop_remaining=0` but a stale manifest.

Audit table writes (`bin_uop_audit`) live in `shingo-core/store/audit/`. The audit package is shared infrastructure: the uop applier writes it, but so does `BinManifestService` (manifest imprint, manifest clear, partial-back sync, release override). Audit consolidation is a deferred follow-up; see `SHINGO_TODO.md`.

### Intent verbs

Engine and other callers route every UOP state mutation through a named verb on `uop.Mutator` (Edge) or `uop.InventoryDeltaService` (Core). The Edge surface, organised by sub-interface:

**Ticker** — PLC tick path delta emission.

| Verb | Reasons emitted | Call site |
|---|---|---|
| `Consumed` | `consume_drain` (bucket) + `consume_tick` (bin) | `wiring_counter_delta.go` |
| `Produced` | `produce_tick` (bin only) | `wiring_counter_delta.go` |
| `Fallthrough` | `consume_drain` (bucket) + `ab_fallthrough` (bin) | `wiring_counter_delta.go` |

**SlotWriter** — `process_node_runtime_states` mutations.

| Verb | Effect |
|---|---|
| `BindActiveBin` | `active_bin_id := bin` (L1 retrieve confirm) |
| `ClearActiveBin` | `active_bin_id := nil` (pickup clear) |
| `SetClaimAndCount` | `active_claim_id := claim`, `remaining_uop_cached := uop` (no pointer changes) |
| `ClearActiveAndReset` | `active_claim_id := claim`, `active_bin_id := nil`, `remaining_uop_cached := 0` (atomic Order B completion at supermarket) |
| `OnDelivered` | `active_claim_id` + `active_bin_id` + `active_bin_epoch` + `remaining_uop_cached` atomic (delivery binds the arrived bin from its OrderDelivered envelope) |
| `ManualLoad` | claim + active_bin + count atomic (operator imprint via loader fallback) |

**Capturer** — release-click capture + admin bucket adjust.

| Verb | Effect |
|---|---|
| `CaptureToLineside` | Loop over captures: bucket writes + `capture_fill` deltas + paired `capture_reduction` bin delta. Atomic. |
| `AdjustBucket` | Set bucket qty to exact value + emit operator-correction delta + flush. |

**Pickup** — bin-pickup boundary.

| Verb | Effect |
|---|---|
| `OnBinPickedUp` | Flush accumulator at the pickup boundary so in-flight ticks attribute to the released bin. |

**Boundary** — non-UOP orchestration boundaries.

| Verb | Effect |
|---|---|
| `MarkAttributionBoundary` | Flush before a SetActivePull swap (A/B flip). Engine owns the swap; UOP owns the flush. |

**Backfiller** — one-shot seeding for fresh Core deployments.

| Verb | Effect |
|---|---|
| `Backfill` | Walk every node's lineside buckets, emit `capture_fill` deltas to seed Core's table. |

## Wire envelopes

Three envelope types carry inventory-state changes between Edge and Core. All three route through the existing `HandleData` channel (`protocol/ingestor.go`) with a subject discriminator.

### `BinUOPDelta` — Edge to Core

```go
type BinUOPDelta struct {
    Station     string
    BinID       int64
    PayloadCode string    // bin's actual current payload, NOT the claim's template
    Delta       int       // signed
    Reason      string    // consume_tick / produce_tick / capture_reduction /
                          // ab_fallthrough / operator_correction
    SequenceID  int64     // monotonic per (station, bin_id)
    WindowStart time.Time
    WindowEnd   time.Time
}
```

Subject: `inventory.bin_uop_delta`. Edge emits one envelope per accumulated delta window (default 5s, configurable via `uop.delta_flush_interval`). The accumulator at `shingo-edge/uop/accumulator.go` mirrors the existing `production_reporter.go` pattern: per-bin accumulation under a mutex, periodic flush via the outbox, restore on enqueue failure.

`PayloadCode` carries the bin's actual current payload at the moment of delta emission, not the target style's template payload. Core's `ApplyBinUOPDelta` validates this against the bin row and rejects mismatches.

### `LinesideBucketDelta` — Edge to Core

Same shape as `BinUOPDelta` but keyed on `(node_id, pair_key, style_id, part_number)` instead of bin ID. Subject `inventory.lineside_bucket_delta`. Reasons: `capture_fill` (operator pulled parts to lineside on release), `consume_drain` (PLC tick drained the bucket), `operator_correction_bucket` (admin engineer override).

Manual-swap nodes have no PLC and never emit bucket deltas. Their state changes are operator-driven and audited via direct insert.

### `BinPickedUp` — Core to Edge

```go
type BinPickedUp struct {
    OrderUUID  string
    BinID      int64
    Location   string
    PickedUpAt time.Time
}
```

Subject: `transit.bin_picked_up`. Core fires this when the RDS poller's `diffBlockStates` detects a pickup block transitioning to FINISHED. Edge's `handler_bin_picked_up.go` receives the event, calls `uop.Mutator.OnBinPickedUp` (which flushes the released bin's accumulator), then clears the slot pointers via `uop.Mutator.ClearActiveBin`.

The envelope exists to close the SEND PARTIAL BACK timing gap. When the operator releases a partial bin and the cell keeps cycling, ticks need to attribute to the released bin until the robot physically picks it up — not to the next claim. The pickup transition is the boundary signal.

**Acceptable bias:** if Edge crashes between Core sending `BinPickedUp` and Edge processing it, the active-claim transition is lost; subsequent ticks attribute to the released bin instead of the new one. The miscount is bounded by the pickup-to-restart window. No reconciler heals it post-flip; `FlushFailures` surfaces any Core-side delta rejections that result.

### Dedup

Every delta envelope carries a monotonic `SequenceID` per scope. Core's `inventory_delta_dedup` table records the last-applied sequence per `(station, scope_kind, scope_key)`. Replays with `seq <= last_applied` are silently dropped. Cycle counts and other admin actions that bypass the delta channel write directly to the bins table and bump the dedup sequence to invalidate any in-flight deltas with stale sequences.

## Process semantics

These are SME-locked and treated as ground truth across the codebase.

**UOP is assembly-normalized.** UOP counts production-cycle increments per bin's claim, not raw physical parts. A bin holding 100 UOP of a part that's used twice per assembly physically contains 200 parts; a bin holding 100 UOP of a part used once per assembly contains 100. The "unit of production" is the cycle of the consuming assembly, normalized to what each bin is tracking. When a multi-cavity die produces parts that fan out to separate bins (e.g., LH and RH halves into different containers), each receiving bin gets its own UOP delta per cycle — one physical cycle, multiple bin-level increments, each normalized to that bin's claim. Per-part details live in the manifest.

**The PLC counter is infinite.** Edge calculates per-tick deltas from the last-seen counter value. There is no "lost ticks on counter reset" failure mode.

**Bin transition for tick attribution is the bin physically leaving the slot, not the operator's release click.** On the consume side, the boundary is the `BinPickedUp` envelope. The operator's release click commits the bin's final-state intent — PARTIAL count, RELEASE EMPTY, or capture-to-lineside — but does not stop tick attribution. Cells routinely finish in-flight cycles between release click and physical pickup; those ticks legitimately belong to the released bin. If the operator pulled parts to a lineside container, those parts are captured into a bucket via the release disposition, and subsequent ticks decrement the bucket first via `drainLinesideFirst` before touching any bin's count. The bucket is location-bound (lives at the slot, not the bin), so it correctly continues to drain across the bin transition while the cell keeps running. On the produce/manual_swap loader side, the bin-loader confirm IS the boundary — there is no release/pickup gap on that side because the loader physically loads the bin at confirm time. For A/B cycling pairs, the boundary is a runtime state flip (active-pull change), preceded by `uop.Mutator.MarkAttributionBoundary` to ship pending deltas under the outgoing attribution context.

**Manual swap nodes have no PLC.** All state changes on manual-swap nodes (loaders, unloaders) are operator-driven. They write through Core's HTTP API (e.g., `LoadBin`) and audit via direct insert. They do not emit `LinesideBucketDelta` envelopes. This is a legitimate exception to "deltas are the only mutation path" — operator actions are conceptually different from automated PLC events.

**Cycle count is admin-only.** Operators do not cycle-count bins during production; SCO uses the Bins admin page on Core. Cycle count writes the new value to `bins.uop_remaining` directly and bumps the dedup sequence to invalidate in-flight deltas.

**Operator-trusted measurements.** SEND PARTIAL BACK count is ground truth — overrides the runtime cache. Produce-ingest count uses the operator-measured runtime value at finalize time, not the template's `UOPCapacity`.

**Hold-and-replay gap handling.** The Edge runtime row carries a single bin pointer, `active_bin_id`. Release click finalizes the *outgoing* bin per its disposition and does **not** pre-load or stamp the incoming supply bin — the new bin's count and epoch arrive later on its `OrderDelivered` envelope, which binds `active_bin_id`. Between physical pickup of the old bin and delivery of the new one, no bin is bound: PLC ticks during this gap still drain lineside as usual, but the bin portion of each tick is accumulated into `pending_uop_delta` (a durable column) rather than charged to a departed bin or lost. When the next bin binds, the first tick applies `current + pending` and resets the pending pile to zero, replaying the held consumption onto the new bin. The lineside bucket — location-bound at the slot — covers the parts the operator runs during the gap.

## Release UI dispositions

The operator station's release modal exposes three buttons (per commit `0becc04`, 2026-04-30):

| Button | When shown | Bin / bucket effect |
|---|---|---|
| `PULL PARTS LINESIDE, RELEASE` | Always (primary) | Bin reduced by `sum(captures)` via delta; lineside bucket increased; bin returns to supermarket |
| `RELEASE PARTIAL` | When `runtime.RemainingUOPCached > 0` | Bin returns to supermarket as-is; manifest preserved; count synced via `OrderRelease.RemainingUOP=&N` |
| `RELEASE EMPTY` | When `runtime.RemainingUOPCached == 0` | Bin returns empty; manifest cleared; count synced via `OrderRelease.RemainingUOP=&0` |

The capture path emits `BinUOPDelta(reason=capture_reduction, delta=-sum(captures))` via `uop.Mutator.CaptureToLineside` — atomic with the per-part `capture_fill` bucket emissions. For partial-back and explicit-empty paths, the `OrderRelease` envelope carries the operator's count directly to Core's `BinManifestService.SyncOrClearForReleased`. There is a known dual-write at the release path (legacy `RemainingUOP=&0` send alongside the `capture_reduction` delta) that's flagged for cleanup but produces correct results because both target the same `bins.uop_remaining` row.

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

The trigger lives in `shingo-core/uop/applier.go` post-update: when `valueBefore + d.Delta == 0` and the delta's reason is `capture_reduction`, it calls `BinManifestService.ClearForReuse` (through the `ManifestClearer` narrow interface) in the same transaction. Atomic, idempotent. The dedup table protects against replay re-firing the trigger.

The `*sql.Tx` in `ManifestClearer`'s signature is load-bearing: the manifest write must share the same database transaction as the bin row update, so a crash between them rolls both back. An implementation that opened a separate connection would silently break this invariant.

The conservative behavior protects the overpack scenario. A bin nominal 100, physical 105: 100 ticks fire, count reaches 0, but the bin still has 5 parts. If the trigger fired on consume-tick zero, the manifest would clear and the operator's PARTIAL release of those 5 parts would land on a manifest-less bin — orphan state. Restricting the trigger to operator-declared paths means the manifest stays until the operator confirms intent.

## Audit log

Every mutation of `bins.uop_remaining` writes a row to `bin_uop_audit`:

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

The audit table is written from two paths today: `shingo-core/uop/applier.go.ApplyBinUOPDelta` (one row per applied delta) and `shingo-core/service/bin_manifest.go` (manifest-imprint, manifest-clear, partial-back UOP sync, release override). The `audit` package (`shingo-core/store/audit/`) is shared infrastructure; both UOP delta apply and manifest operations call `audit.AppendBinUOP` directly inside their transactions. Consolidating ownership of the audit boundary is a deferred follow-up.

The `suggested_uop` column is the system's expected value at the time of an operator action. Populated for `release_partial`, `release_capture`, `cycle_count`, `manual_clear`. Null for automated paths (`delta_consume`, `delta_produce`, etc.) where there is no operator suggestion to compare against. Op tags distinguish operator-driven mutations from automated ones; the distinction enables forensics like "operators who frequently override the system" or "stations where the system is consistently wrong."

For multi-part captures, the aggregate lands in `suggested_uop`/`after_uop`; per-part diff context goes in `metadata` (preserved by `BinManifestService.AuditReleaseOverride`).

## Operational characteristics

**Negative bin values are normal.** Overpack means a bin reads negative briefly until the operator catches it via PARTIAL release with the actual count. Underpack means the bin reads positive briefly and the operator releases EMPTY when they see it's actually empty. Either way, the inventory tracks physical reality. An unbounded growth of negative bins would indicate systematic miscalibration of nominal capacity.

**No reconciler.** Edge's runtime cache is durable across restarts (sqlite-backed) and Core mirrors via the delta stream. If the delta stream drops a message, `FlushFailures` increments and the per-bin scope stays out of sync until manually addressed. There is no automated heal. Operationally this means dashboards on `FlushFailures` (Edge) and consumer lag (Core) are the primary signals for delta-pipeline health.

**Trust the bus.** Kafka delivery + Core's `inventory_delta_dedup` handle correctness. The accumulator's restore-on-failure preserves count changes through a transient outbox blip. The dedup sequence handles out-of-order delivery and replay. What the system does NOT do anymore is cross-check Edge and Core periodically; if those mechanisms fail simultaneously, you have a bug, not a routine drift.

## Known integrity gaps

Two gaps surfaced during the UOP package extraction; both predate the refactor and are tracked in `SHINGO_TODO.md`. They are recorded here so the architecture is accurately described.

**Cycle count doesn't notify Edge.** When Core's `RecordCount` (admin cycle-count path) corrects a bin physically at an Edge node, Edge's local runtime cache stays stale. The arithmetic of the delta system remains consistent (Edge's subsequent ticks emit signed deltas that Core applies to the corrected value, so Core stays right), but Edge's local truthfulness degrades: the UI displays the pre-correction value to the operator, auto-reorder threshold logic fires at the wrong moment, and manual-swap operator decisions driven by Edge UI go wrong.

Pre-flip, this was healed by the reconciler. Post-flip, no heal mechanism exists. Fix requires a Core→Edge correction envelope or audit-table CDC subscription. **Active correctness gap.**

**Manual load crash-window race.** Between Core's HTTP commit of the imprint and Edge's local cache write from the response, a crash leaves Edge with a stale cache. The window is narrow (single HTTP round-trip); fix is local (make the cache write transactional with the HTTP call's commit, or compensate on restart). **Narrow but real.**

## What is not in scope

- **Feature toggles** for the architecture itself. There is no `BIN_AS_TRUTH=on` flag, no `selfHeal` toggle, no `INVENTORY_DELTA_PUBLISH` switch. The architecture ships unconditionally.
- **Shadow stores or shadow columns.** One authoritative row per concept. No promotion-from-shadow lifecycle.
- **Scrap accounting as an invariant problem.** SCO zeros bin UOP via shingo before physical scrap; the explicit zero preserves the invariant. There is no automated scrap event.
- **Backup-restore handling.** If Core is restored from a backup, behaviour is undefined relative to lost-since-backup state. Operational concern: stop Edge processes before restoring Core if needed.
- **Reverse reconciliation.** The old `uop_reconciler.go` is gone. There is no automated heal from Core to Edge. If divergence occurs, it manifests via `FlushFailures` or operator-reported count anomalies and requires manual investigation.

## Quick reference

### Files

| Path | Purpose |
|---|---|
| `shingo-edge/uop/mutator.go` | Public `Mutator` type + verb methods (slot-lifecycle + boundary + admin live here) |
| `shingo-edge/uop/interfaces.go` | Segregated sub-interfaces (Ticker / SlotWriter / Capturer / Pickup / Boundary / Backfiller) + Sink umbrella |
| `shingo-edge/uop/accumulator.go` | Per-bin / per-bucket signed-delta accumulator + outbox flush (internal) |
| `shingo-edge/uop/tick.go` | Consumed / Produced / Fallthrough verb implementations + TickEvent |
| `shingo-edge/uop/capture.go` | CaptureToLineside verb (atomic bucket fills + bin reduction) + CaptureEvent |
| `shingo-edge/uop/release.go` | ReleaseDisposition + ComputeReleaseRemainingUOP + BuildProtocolDisposition (pure functions; Phase 2 lift) |
| `shingo-edge/uop/backfill.go` | One-shot bucket seeding for fresh Core deployments |
| `shingo-edge/uop/store_iface.go` | Narrow store interfaces (runtimeWriter / bucketStore / nodeStore) |
| `shingo-edge/uop/archtest_test.go` | CI invariant: no direct RecordBin/RecordBucket outside uop/ |
| `shingo-edge/engine/wiring_counter_delta.go` | PLC tick path; resolves context + calls Consumed/Produced/Fallthrough |
| `shingo-edge/engine/operator_release.go` | Release dispositions; finalizes the outgoing bin and calls CaptureToLineside (does not pre-load the incoming bin) |
| `shingo-edge/engine/operator_ab_cycling.go` | A/B flip; calls MarkAttributionBoundary before SetActivePull swap |
| `shingo-edge/engine/handler_bin_picked_up.go` | Edge handler for BinPickedUp envelope; calls OnBinPickedUp + ClearActiveBin |
| `shingo-core/uop/applier.go` | InventoryDeltaService — apply bin/bucket deltas; manifest-clearing trigger |
| `shingo-core/store/audit/bin_uop.go` | Audit insert helpers (shared with manifest service) |
| `shingo-core/rds/poller.go` | RDS poll → block-state diff → BinPickedUp |
| `shingo-core/engine/wiring_block_completed.go` | Bin-transit-state handler |
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
  delta_flush_interval: 5s   # accumulator flush cadence
```

Reconciler-related config (`reconcile_interval`, `tolerance.*`) was removed in commit `6d226d1`.

### HTTP endpoints

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/inventory/invariant` | Plant-wide invariant probe (signed bin sum + bucket sum) |
| GET | `/api/audit/bin/:id` | Per-bin audit timeline |
| GET | `/api/audit/operator/:name` | Per-operator activity |
| GET | `/api/audit/station/:station` | Per-station drift report |
| POST | `/api/admin/uop/backfill?station=X[&force=true]` | Manual bucket backfill trigger |

`/api/reconciliation/uop` and `/api/telemetry/uop-state` were removed alongside the reconciler.
