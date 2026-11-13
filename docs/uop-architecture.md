# UOP Architecture

Verified against: 51489c78

UOP (Unit of Production) is the count of consumable parts in a physical bin and the count of parts on a lineside
bench. This document describes how UOP is tracked, mutated, audited, and propagated across `shingo-core` and
`shingo-edge`. What a count triggers (thresholds and cell autoreorder) is
[uop-threshold-replenishment.md](uop-threshold-replenishment.md)'s.

## Ownership: Edge-authoritative, Core-aggregating

Bins are the unit of inventory. A bin's UOP count belongs to the physical container — not to the slot it sits in, not
to the order that's moving it, not to any cached projection. Movement does not change the count. Storage→storage
moves, supermarket→line, line→supermarket, complex two-bin orders: none of them touch the count. The count changes
only when parts physically enter or leave the bin.

The Edge is authoritative for the count of any bin physically at one of its nodes: its runtime row,
`process_node_runtime_states.remaining_uop_cached` for the bin bound as `active_bin_id`, is the source of truth for
at-node bins. Core mirrors it through the delta stream. Nothing heals the Edge from Core: a divergence shows as a
`report_divergence` episode from the Edge's 60-s lineside report, and a dropped message's count is carried by the
next message's running `Net`. Correctness rests on Kafka delivery and Core's `inventory_delta_dedup`; consumer lag is
the operational signal.

**What Core is authoritative for:** cross-station inventory aggregation, the manifest on a bin, the audit trail.
Edge owns the per-tick count; Core owns the inventory picture across all stations.

## The authoritative rows

Edge's runtime row is authoritative for at-node bins; Core's `bins.uop_remaining` aggregates across stations. Piles
have one writer, the Edge, and a mirror on Core.

| Row | Meaning | Sign | Authoritative for |
|---|---|---|---|
| `process_node_runtime_states.remaining_uop_cached` (Edge SQLite) | Count of parts in the bin currently at this Edge node | Signed; can go negative | At-node bins, this Edge |
| `bins.uop_remaining` (Core Postgres) | Count of parts in a physical bin, mirrored from Edge's delta stream | Signed; can go negative | Bins in transit / at supermarket / cross-station |
| `node_lineside_bucket.qty` (Edge SQLite) | Count of parts on a lineside bench (a pile), per (node, payload, state) | Non-negative | Piles; the Edge is the only writer |
| `lineside_buckets.qty` (Core Postgres) | Core's mirror of the Edge's piles, per (core node, payload, state), set from the Edge's levels | Non-negative | Nothing: a mirror |

Bins can go negative because real-world bins overpack and underpack. A nominal-1000 bin might physically hold 1005;
the next nominal-1000 might hold 995. The system tracks reality, not the nominal. Over time, the discrepancies wash
out at the inventory aggregate level.

Piles stay non-negative because they're counts of parts on a bench — you can't have a negative number of parts in
front of you. A tick drains the active pile first and never below zero; the rest goes to the bin.

A pile is **active** from the pull until its process's next cutover (every active-style flip: the changeover cutover
and the admin style flip). At the cutover every active pile at the process's nodes becomes **stranded**
(`engine/lineside_strand.go`): a permanent record, labelled a count anomaly at cutover, that never drains, never
counts on either side and never revives when the style comes back. Stranded is a state on the row, set by the Edge.
Operators run out what they pull, so a leftover at cutover is most likely the size of a declaration error. The next
pull of that part makes a new active pile.

The plant-wide inventory invariant is approximate:

```
total physical UOP ≈ sum(bins.uop_remaining) + sum(lineside_buckets.qty WHERE state = 'active')
```

The bin-sum term drifts signed in either direction over time as overpack/underpack accumulates and washes out.
`GET /api/inventory/invariant` reports both terms and the signed total; trends matter more than instantaneous values.

## Package structure

UOP state mutations route through dedicated packages on each side. The packages own the chokepoint; everything else
(engine, www, messaging, dispatch) depends on the narrow interfaces.

### `shingo-edge/uop/`

The Edge-side mutator. Holds:

- `Mutator` — the public type carrying the intent verbs, grouped by concern into the sub-interfaces of
  `interfaces.go` (below), composed by `Sink`, which the engine holds.
- `accumulator` (unexported, `accumulator.go`) — per-bin signed-delta accumulation, and per-pile dirty marks: each
  flush sends one level per changed pile row, read from the table at flush. Periodic flush to the outbox; an entry
  is cleared only after its enqueue succeeds. `pending.go` gives the lineside report a snapshot of what is not yet
  flushed (`Sink.WithPending`), so the report states each seat's count as of the carrier's flushed sequence.
- Narrow store interfaces (`store_iface.go`: `runtimeWriter`, `bucketStore`) so the package never imports engine.
  `*store.DB` satisfies both at the composition root.
- `archtest_test.go` — CI tests: no production file outside `uop/` records a delta directly (every emission routes
  through a named verb), and only the known files write `node_lineside_bucket` (the pile store, process delete and
  the migrations), so every pile write marks its level.

### `shingo-core/uop/`

The Core-side applier, `InventoryDeltaService` (`applier.go`; `service.InventoryDeltaService` aliases it):

- `ApplyBinUOPDelta` receives `BinUOPDelta` envelopes from the Edge, dedups against `inventory_delta_dedup`, applies
  the message's running net minus what it has already applied for the scope to `bins.uop_remaining` (the signed
  delta only for an Edge that sends no net), writes the audit row, and fires the manifest clear (below).
- `ApplyLinesideBucketLevel` receives `LinesideBucketLevel` envelopes and sets the `lineside_buckets` mirror row to
  the level under a seq guard (one UPSERT, or a DELETE at 0), writing a `lineside_drain_ledger` row when the level
  carries drains. Core never writes a pile.
- `ManifestClearer` — the narrow interface whose `ClearForReuseTx` fires `BinManifestService`'s clear inside the
  delta-apply transaction. Atomicity is load-bearing; a crash between the bin update and the manifest clear would
  leave a bin with `uop_remaining=0` but a stale manifest.

Audit table writes (`bin_uop_ledger`) live in `shingo-core/store/audit/`, shared infrastructure: the applier writes
it, and so does `BinManifestService` (manifest imprint, manifest clear, partial-back sync, release override).

### Intent verbs

Engine and other callers route every UOP state mutation through a named verb on `uop.Mutator` (Edge) or
`uop.InventoryDeltaService` (Core). The Edge surface, by sub-interface (`interfaces.go` is the roster):

| Sub-interface | Covers | Verbs |
|---|---|---|
| **Ticker** | the PLC tick path's delta emission (`tick.go`), called from `wiring_counter_delta.go` | `Consumed` (active pile drained first, then a `consume_tick` bin delta), `Produced` (`produce_tick`, bin only), `Fallthrough` (pile drains, then `ab_fallthrough` when no active-pull consume node is visible) |
| **SlotWriter** | `process_node_runtime_states` writes, one store statement each | `Land` (bind the carrier Core placed, refusing an older placement: `processes.BindLanding`), `BindActiveBin`, `BindFromCore` (Core's word on carrier, count and generation), `BindStagedUnlessDeparted`, `ClearActiveBin` (pointer and count together; claim and `active_order_id` left alone), `LeaveAtPickup`, `SetClaim` (the claim only, never the count), `SetClaimCountAndEpoch` (the clear shape), `ManualLoad` (a LOAD, with Core's count) |
| **Capturer** | the release's capture (`capture.go`) | `CaptureToLineside`: add each part's qty to its active pile (never a stranded one), mark its level, and emit the paired `capture_reduction` bin delta |
| **Piles** | pile writes made outside the capture and the tick (`piles.go`) | `PilesChanged` (the caller wrote the rows — the cutover's strand, the admin Clear, a process delete), `ResendLevels` (boot: every row's level, unconditionally) |
| **Pickup** | the robot's lift | `TakeAfterLift` (take back windows counted after the lift), `OnBinPickedUp` (flush so in-flight ticks attribute to the lifted bin) |
| **Boundary** | non-UOP orchestration boundaries | `MarkAttributionBoundary` (flush before the A/B active-pull flip, `commitFlip`; engine owns the flip, UOP owns the flush) |

## Wire envelopes

Three envelope types carry inventory-state changes between Edge and Core, plus Core's count announcement. Their
fields are generated in [wire-protocol.md](wire-protocol.md) (`BinUOPDelta`, `LinesideBucketLevel`, `BinPickedUp`,
`UOPAdjustment`).

### `BinUOPDelta` — Edge to Core

Subject `inventory.bin_uop_delta`. The Edge emits one envelope per accumulated delta window
(`protocol.InventoryDeltaFlushWindow`, 5s): per-bin accumulation under a mutex, periodic flush via the outbox,
send-then-sweep — the entry is not touched until the enqueue succeeds, so a failed flush is retried whole by the
next. Each envelope carries the signed `Delta`, the `Reason` (`consume_tick`, `produce_tick`, `capture_reduction`,
`ab_fallthrough`, `operator_correction`), a `SequenceID` monotonic per scope, the carrier's generation (`Epoch`), and
the scope's running total (`Net`).

`PayloadCode` carries the bin's actual current payload at the moment of emission, not the target style's template
payload. Core's `ApplyBinUOPDelta` validates it against the bin row and rejects a mismatch.

### `LinesideBucketLevel` — Edge to Core

Subject `inventory.lineside_bucket_level`. The level, not the change: `Qty` is the sum over every Edge process node
carrying that core node name. Core sets its row to it, so a lost or late message cannot drift the mirror; a seq that
went backward with a later window (a restored Edge) is applied and re-anchors. A pull (bin to bench) and a strand
(a count correction) are not consumption and carry `Drained` 0.

Manual-swap nodes have no PLC and never have piles.

### `BinPickedUp` — Core to Edge

Subject `transit.bin_picked_up`. When a pickup block finishes, Core lifts the bin (`BinService.Lift`): one
transaction moves it to `_TRANSIT`, ends a streamed carrier's generation (`delta_epoch`+1, `lifted_at`), and writes
this message to the lifting order's station, carrying the new `Epoch` and `PickedUpAt` = `lifted_at`.

The Edge's handler (`engine/handler_bin_picked_up.go`) takes back the windows it counted after the lift
(`TakeAfterLift`), flushes (`OnBinPickedUp`), and leaves the seat (`LeaveAtPickup`), holding those post-lift ticks
for the next carrier of the part. A window that reaches Core started at or after the lift is refused
(`refuseAfterLift`), consuming neither the seq nor the net: it is the rule working, and the Edge moves those ticks
itself when the pickup reaches it.

### Dedup

Every delta envelope carries a monotonic `SequenceID` per scope. Core's `inventory_delta_dedup` table records
`last_seq` per `(station, scope_kind, scope_key, epoch)`. A message at or below it is skipped, and its count is
carried by the next applied message's running `Net`. A count correction writes `bins.uop_remaining` directly and
reaches the counting station as a `UOPAdjustment` fenced by the net it was taken at (`AsOfNet`/`AsOfSeq`); a reset
bumps `delta_epoch`, which retires the old generation's in-flight deltas.

## Process semantics

These are SME-locked and treated as ground truth across the codebase.

**UOP is assembly-normalized.** UOP counts production-cycle increments per bin's claim, not raw physical parts. A bin
holding 100 UOP of a part that's used twice per assembly physically contains 200 parts; a bin holding 100 UOP of a
part used once per assembly contains 100. The "unit of production" is the cycle of the consuming assembly,
normalized to what each bin is tracking. When a multi-cavity die produces parts that fan out to separate bins (e.g.,
LH and RH halves into different containers), each receiving bin gets its own UOP delta per cycle — one physical
cycle, multiple bin-level increments, each normalized to that bin's claim. Per-part details live in the manifest.

**The PLC is the truth for a tick.** The Edge computes each delta from the last-seen counter value
(`plc.CalculateDelta`). A 16- or 32-bit rollover below the jump threshold counts as the wrap; a backward count is a
counter reset, whose delta is the new count; a jump past the threshold is counted whole. The anomaly label
(`reset`, `jump`) is a record only: every consumer counts the delta.

**Bin transition for tick attribution is the bin physically leaving the slot, not the operator's release click.**
On the consume side the boundary is the lift: every tick charged before it is the carrier's, and the window after
it is not (`BinPickedUp`, above). The release commits the bin's final-state intent — its declared count, or a
capture to lineside — but does not stop tick attribution: cells routinely finish in-flight cycles between the
release and the physical pickup, and those ticks belong to the released bin. If the operator pulled parts to a
lineside container, those parts are captured into the node's active pile by the release, and subsequent ticks
decrement the pile first before touching any bin's count. The pile is location-bound (it lives at the slot, not the
bin), so it continues to drain across the bin transition, and through a changeover window, until the process's
cutover strands it. On the produce/manual_swap loader side, the operator's LOAD is the boundary — there is no
release/pickup gap there, because the loader physically loads the bin at that moment. For A/B cycling pairs, the
boundary is the active-pull flip, preceded by `MarkAttributionBoundary` to ship pending deltas under the outgoing
attribution context.

**Manual swap nodes have no PLC.** All state changes on manual-swap nodes (loaders, unloaders) are operator-driven
asks to Core (`bin.load`, `bin.clear`), audited by Core in the same transaction. They have no lineside piles. This is
the legitimate exception to "deltas are the only mutation path": operator declarations are not PLC events.

**A count correction is a declaration.** A person corrects a count on Core's Bins page or at the line (the
`bin.count` ask, `Engine.RecordBinCount`). Core writes the new value to `bins.uop_remaining` directly, records it in
the audit trail, and announces it to the counting station as a fenced `UOPAdjustment` (`BinService.RecordCount`),
which rebases its seat. No dedup sequence is touched.

**Operator-trusted measurements.** A release's declared count is ground truth — it overrides the runtime cache.
Produce-ingest count uses the operator-measured runtime value at finalize time, not the template's `UOPCapacity`.

**Hold-and-replay gap handling.** The Edge runtime row carries a single bin pointer, `active_bin_id`. The release
finalizes the *outgoing* bin per its disposition and does **not** pre-load or stamp the incoming supply bin — the
new bin binds when Core reports it landed (`Land`). Between physical pickup of the old bin and delivery of the new
one, no bin is bound: PLC ticks during this gap still drain lineside as usual, but the bin portion of each tick is
accumulated into `pending_uop_delta` (a durable column, with its part in `pending_uop_payload`) rather than charged
to a departed bin or lost. When the next bin of that part binds, the first tick applies `current + pending` and
resets the hold to zero. The active pile — location-bound at the slot — covers the parts the operator runs during the
gap. A produce seat never sets aside more than one pack (the claim's `UOPCapacity`), whether a carrier is coming or
not: the stroke that would pass it throws the whole pile away (a `hold_dropped` `hold_cap` row) and the seat sets
nothing aside until a cart binds there; those strokes reach Core as one `no_bin` row per no-cart stretch (SPR
ALN_011, 2026-11-12). The hold replays only onto a cart delivered by an order the Edge holds that places a bin at the
node; any other bind (a cart placed or recorded by hand, a hand load, a set-down naming an order the Edge does not
have) drops the standing hold in the bind's own transaction with one `no_carrier` row, and ends the no-cart stretch.

## Release dispositions

The operator's release and its count, capture and finalize are one commit, `engine/release_commit.go`: the
operator station offers one release (or RELEASE SHORT where a strict claim ships short), and the release carries
the declared count to Core (`OrderRelease.RemainingUOP`, applied by `BinManifestService.SyncOrClearForReleased`)
or, for a capture, the paired `capture_reduction` delta from `CaptureToLineside`.

## Manifest-clearing trigger

When a bin's `uop_remaining` reaches zero, the manifest may need to clear so the bin can be reassigned. The trigger is
conservative: it fires on an operator's declaration, never on PLC deltas alone.

| Path to zero | Manifest auto-clears? |
|---|---|
| A release declaring 0 (`SyncOrClearForReleased`) | Yes |
| A capture that drives the count to 0 or below (`reason=capture_reduction`) | Yes |
| A count correction to 0 | No (a correction to 0, then an explicit clear) |
| Admin clears the bin from the Bins page | Yes (explicit operator action) |
| A PLC consume tick drives the count to 0 (`reason=consume_tick`) | No (an overpacked bin can still hold parts) |
| A release declaring a count above 0 | No (the operator says there are still parts) |

The capture trigger lives in `ApplyBinUOPDelta` post-update: when a `capture_reduction` delta leaves
`uop_remaining` at or below zero, it calls `ClearForReuseTx` (through `ManifestClearer`) in the same transaction —
the `<= 0` boundary covers an operator who pulled more than the tracked count showed. Atomic, idempotent; the dedup
high-water check protects against replay re-firing it.

The `*sql.Tx` in `ManifestClearer`'s signature is load-bearing: the manifest write must share the same database
transaction as the bin row update, so a crash between them rolls both back. An implementation that opened a
separate connection would silently break this invariant.

The conservative behavior protects the overpack scenario. A bin nominal 100, physical 105: 100 ticks fire, count
reaches 0, but the bin still has 5 parts. If the trigger fired on consume-tick zero, the manifest would clear and the
operator's partial release of those 5 parts would land on a manifest-less bin — orphan state. Restricting the trigger
to operator-declared paths means the manifest stays until the operator confirms intent.

## Audit log

Every mutation of `bins.uop_remaining` writes a row to `bin_uop_ledger`
([data-model.md](data-model.md#core-bin_uop_ledger) has the columns; retention and the two permanent tables beside it
are [data-model.md](data-model.md#bin)'s). Notes the schema does not say:

- `source` is the writer's file and function; `actor` is the station, operator or system.
- `before_uop` is the plain prior value on an ordinary row (`audit.AppendBinUOP`). On an operator override row
  (`AppendBinUOPOverride`) it carries the system's **suggested** value and `after_uop` the operator's entry; there
  is no column of its own for the suggestion. The `op` tag is what tells the two apart, and what separates
  operator-driven mutations from automated ones for forensics ("stations where the system is consistently wrong").
- `metadata` carries per-part diff context for a multi-part capture, written by the override path
  (`BinManifestService.AuditReleaseOverride`); `detail` is the context the ordinary append path writes.

The writers are `ApplyBinUOPDelta` (one row per applied delta) and `shingo-core/service/bin_manifest.go`
(manifest-imprint, manifest-clear, partial-back UOP sync, release override), each calling `audit.AppendBinUOP`
inside its own transaction.

## Operational characteristics

**Negative bin values are normal.** Overpack means a bin reads negative briefly until the operator catches it with a
release declaring the actual count. Underpack means the bin reads positive briefly and the operator releases it
declaring 0 when it is actually empty. Either way, the inventory tracks physical reality. An unbounded growth of
negative bins would indicate systematic miscalibration of nominal capacity.

**No reconciler.** Edge's runtime cache is durable across restarts (SQLite-backed) and Core mirrors via the delta
stream. A dropped message's count is carried by the next applied message's running `Net`; a disagreement that
persists shows as a `report_divergence` episode from the Edge's 60-s lineside report, compared against Core's
replica on every ingest. Nothing writes a count from it. Consumer lag (Core) and the report episodes are the signals
for delta-pipeline health.

## Known integrity gap

**A LOAD's follow-up runs in the process that asked.** A LOAD is an ask (`bin.load`); Core commits the imprint and
answers, and the Edge seats Core's count on the node when the answer comes (`afterLoad`). An Edge restart between
Core's commit and the answer's arrival leaves the seat without Core's count. The window is one ask round-trip.

## What is not in scope

- **Feature toggles** for the architecture itself. The architecture ships unconditionally.
- **Shadow stores or shadow columns.** One authoritative row per concept.
- **Scrap accounting as an invariant problem.** Scrap is zeroed through shingo before physical scrap; the explicit
  zero preserves the invariant. There is no automated scrap event.
- **Backup-restore handling.** If Core is restored from a backup, behaviour is undefined relative to
  lost-since-backup state. Operational concern: stop Edge processes before restoring Core if needed.
- **Reverse reconciliation.** There is no automated heal from Core to Edge. A divergence manifests as a
  `report_divergence` episode or an operator-reported anomaly and requires manual investigation.

## Quick reference

### Files

| Path | Purpose |
|---|---|
| `shingo-edge/uop/mutator.go` | Public `Mutator` type + verb methods (slot-lifecycle, pickup, boundary) |
| `shingo-edge/uop/interfaces.go` | Segregated sub-interfaces (Ticker / SlotWriter / Capturer / Piles / Pickup / Boundary) + `Sink` umbrella |
| `shingo-edge/uop/accumulator.go` | Per-bin signed-delta accumulator and per-pile level marks + outbox flush (internal) |
| `shingo-edge/uop/pending.go` | The unflushed snapshot the lineside report subtracts |
| `shingo-edge/uop/tick.go` | Consumed / Produced / Fallthrough verb implementations + TickEvent |
| `shingo-edge/uop/capture.go` | CaptureToLineside verb (pile gains + bin reduction) + CaptureEvent |
| `shingo-edge/uop/piles.go` | PilesChanged / ResendLevels: pile writes outside the capture and the tick, and the boot resend |
| `shingo-edge/uop/release.go` | ReleaseDisposition + ComputeReleaseRemainingUOP + BuildProtocolDisposition (pure functions) |
| `shingo-edge/uop/store_iface.go` | Narrow store interfaces (runtimeWriter / bucketStore) |
| `shingo-edge/uop/archtest_test.go` | CI invariants: no direct delta recording outside uop/; only known files write node_lineside_bucket |
| `shingo-edge/engine/wiring_counter_delta.go` | PLC tick path; resolves context + calls Consumed/Produced/Fallthrough |
| `shingo-edge/engine/release_commit.go` | The release commit: dispositions, the A/B flip (`commitFlip`, which calls MarkAttributionBoundary before the active-pull swap), the outgoing bin's count and finalize, CaptureToLineside with its ledger row (does not pre-load the incoming bin) |
| `shingo-edge/engine/lineside_strand.go` | The cutover's strand: every active pile at the process's nodes becomes stranded; called from completeCutover and SetProcessActiveStyle |
| `shingo-edge/engine/handler_bin_picked_up.go` | Edge handler for BinPickedUp: TakeAfterLift, OnBinPickedUp, the seat's leave |
| `shingo-core/uop/applier.go` | InventoryDeltaService — apply bin deltas and pile levels; manifest-clearing trigger |
| `shingo-core/store/audit/bin_uop.go` | Audit insert helpers (shared with manifest service) |
| `shingo-core/engine/wiring_block_completed.go` | Pickup block completed → `BinService.Lift` → BinPickedUp |
| `protocol/payloads.go` | Wire envelope definitions |

### Subject constants

```go
const (
    SubjectBinUOPDelta         = "inventory.bin_uop_delta"
    SubjectLinesideBucketLevel = "inventory.lineside_bucket_level"
    SubjectBinPickedUp         = "transit.bin_picked_up"
)
```

### Config

None. The flush cadence is the constant `protocol.InventoryDeltaFlushWindow` (5s), shared by both modules; there is
no YAML key for it.

### HTTP endpoints

| Side | Method | Path | Purpose |
|---|---|---|---|
| Core | GET | `/api/inventory/invariant` | Plant-wide invariant probe (signed bin sum + active pile sum) |
| Core | GET | `/api/audit/bin/{id}` | Per-bin audit timeline |
| Core | GET | `/api/audit/discrepancies` | Discrepancy report |

Routes: `shingo-core/www/router.go`. The Edge has no pile backfill endpoint: it re-sends every pile's level at boot,
and Core's seq guard makes that idempotent.
