# Produce Nodes & Edge Wiring

## Overview

Produce nodes fill empty bins. When the operator finalizes (locks the UOP count), the system manifests the bin at Core via an ingest order, then dispatches swap choreography based on the claim's swap mode: simple (bare ingest), sequential (ingest + complex removal), single_robot (10-step all-in-one swap), or two_robot (two coordinated complex orders). Edge wiring handles UOP tracking — counter deltas increment produce UOP (counting UP) and decrement consume UOP (counting DOWN, floored at zero). Completion events reset UOP based on node role and order type.

Edge tests use SQLite in a temp directory — no Docker required.

## Test files

- `engine/produce_swap_test.go` — swap mode finalization (TC-66a-f, 7 tests)
- `engine/wiring_test.go` — edge event handlers (TC-67a-g, TC-70, 8+ tests)

Run this domain's tests:

```bash
cd shingo-edge
go test -v -run "TestProduce|TestWiring|TestHandlePayloadCatalog" ./engine/ -timeout 60s
```

## Index

| TC | Description | Status |
|----|-------------|--------|
| TC-66a | Produce simple — FinalizeProduceNode creates ingest order, resets UOP to 0 | PASS |
| TC-66b | Produce sequential — ingest + complex removal order created | PASS |
| TC-66c | Produce single_robot — 10-step complex swap order created | PASS |
| TC-66d | Produce two_robot — two coordinated complex orders, both tracked in runtime | PASS |
| TC-66e | Produce finalize rejects zero UOP (nothing to finalize) | PASS |
| TC-66f | Produce finalize rejects consume-role node | PASS |
| TC-67a | Ingest completion resets produce UOP to 0 and clears order tracking | PASS |
| TC-67b | Retrieve completion resets produce UOP to 0 (empty bin received) | PASS |
| TC-67c | Retrieve completion resets consume UOP to capacity (full bin received) | PASS |
| TC-67d | Counter delta increments produce UOP (counting UP) | PASS |
| TC-67e | Counter delta decrements consume UOP (counting DOWN) | PASS |
| TC-67f | Counter delta floors consume UOP at zero (never negative) | PASS |
| TC-67g | Bin loader move completion resets runtime state | PASS |
| TC-70 | Payload catalog sync prunes deleted entries | FIXED |

## Bugs found and fixed

### TC-70: Payload catalog sync does not prune deleted entries

**Scenario:** Core sends a payload catalog sync to Edge. Edge upserts the received entries into its local SQLite database. Later, an admin deletes a payload from Core's catalog. On the next sync, Core sends the updated catalog (without the deleted entry). Edge upserts the entries it receives but never removes entries that Core no longer includes. The deleted payload remains in Edge's local catalog forever.

**Expected behavior:** After a catalog sync, Edge's local catalog should exactly match Core's catalog. Entries that were deleted from Core should be removed from Edge's local database.

**Result:** BUG FOUND. `HandlePayloadCatalog` in `shingo-edge/engine/engine.go` only called `UpsertPayloadCatalog` for each received entry. It never removed entries that were no longer in Core's response. Stale deleted payloads accumulated in Edge's local catalog indefinitely.

**Root cause:** Missing prune step after upsert. The handler collected entries from Core and upserted them, but had no mechanism to detect and remove entries that Core had deleted since the last sync.

```go
// Before (broken): only upserts, stale entries persist forever
func (e *Engine) HandlePayloadCatalog(entries []protocol.CatalogPayloadInfo) {
    for _, b := range entries {
        entry := &store.PayloadCatalogEntry{...}
        e.db.UpsertPayloadCatalog(entry)
    }
    // no prune step -- deleted entries stay in local DB
}

// After (fixed): upsert then prune stale entries
func (e *Engine) HandlePayloadCatalog(entries []protocol.CatalogPayloadInfo) {
    ids := make([]int64, 0, len(entries))
    for _, b := range entries {
        entry := &store.PayloadCatalogEntry{...}
        e.db.UpsertPayloadCatalog(entry)
        ids = append(ids, b.ID)
    }
    // prune entries not in core's active set
    if err := e.db.DeleteStalePayloadCatalogEntries(ids); err != nil {
        log.Printf("engine: prune stale payload catalog: %v", err)
    }
}
```

**New method:** `DeleteStalePayloadCatalogEntries(activeIDs []int64)` in `store/payload_catalog.go` deletes entries whose IDs are not in the active set. Safety guard: if `activeIDs` is empty, no entries are removed (prevents accidental full wipe on empty sync).

```sql
DELETE FROM payload_catalog WHERE id NOT IN (<activeIDs>)
```

**Production risk:** Low. Stale entries remain visible in Edge's payload dropdown after deletion from Core. An operator could attempt to use a deleted payload code, but the order would fail at Core (payload doesn't exist). The real risk is operator confusion: seeing payloads in the dropdown that no longer exist in Core. Over time, the dropdown accumulates outdated entries that operators must mentally filter.

**Status:** Fixed. `HandlePayloadCatalog` now collects active IDs during upsert and calls `DeleteStalePayloadCatalogEntries` afterward. Entries deleted from Core are pruned from Edge's local catalog on the next sync.

**Test:** `shingo-edge/engine/wiring_test.go` -- `TestHandlePayloadCatalog_PruneDeletedEntries`

---

## Verified scenarios

### Produce swap mode finalization (TC-66)

**Scenario:** Produce nodes fill empty bins. When the operator finalizes a bin (locks the UOP count), the system must manifest the bin at Core via an ingest order, then dispatch the appropriate swap choreography based on the claim's swap mode.

**Expected behavior:** All four swap modes create an ingest order first, then:

- **Simple** — bare ingest, no swap. Runtime UOP resets to 0.
- **Sequential** — ingest + complex removal order. Backfill auto-created by wiring when removal goes in_transit (same as consume).
- **Single_robot** — ingest + 10-step all-in-one complex swap order.
- **Two_robot** — ingest + two coordinated complex orders (OrderA for fetch-and-stage, OrderB for remove-filled). Both tracked in runtime.

Finalization is rejected if UOP is zero (nothing to finalize) or the node's role is not `produce`.

**Result:** PASS. 7 tests in `engine/produce_swap_test.go`.

**Test:** `shingoedge/engine/produce_swap_test.go` -- `TestProduceSimple_FinalizeIngest`, `TestProduceSequential_RemovalThenBackfill`, `TestProduceSingleRobot_TenStepSwap`, `TestProduceTwoRobot_BothOrdersCreated`, `TestProduceFinalize_RejectsZeroUOP`, `TestProduceFinalize_RejectsConsumeNode`

---

### Edge wiring — event-driven state transitions (TC-67)

**Scenario:** The Edge engine's event handlers manage UOP tracking and order lifecycle state for process nodes. Different order types and node roles trigger different reset behavior.

**Expected behavior:**

- Ingest completion (produce): UOP → 0, order IDs cleared. No auto-request (bin still at node in simple mode; swap orders already in flight for other modes).
- Retrieve/complex completion (produce): UOP → 0 (empty bin received, starts counting from zero).
- Retrieve/complex completion (consume): UOP → capacity (full bin received).
- Counter delta (produce): UOP increments (counting UP toward capacity).
- Counter delta (consume): UOP decrements (counting DOWN from capacity), floored at zero.
- Bin loader move completion: UOP → 0, order tracking cleared, auto-request for next empty.

**Bug found and fixed:** Produce UOP reset was using `claim.UOPCapacity` for all roles. Produce nodes receiving an empty bin should reset to 0, not capacity. Fixed with `if claim.Role == "produce" { resetUOP = 0 }`.

**Result:** PASS. 7 tests in `engine/wiring_test.go`.

**Test:** `shingoedge/engine/wiring_test.go` -- `TestWiring_*`

---

### TC-70: Payload catalog sync prunes deleted entries (Edge)

**Scenario:** Edge receives a payload catalog sync from Core. Core's response includes entries A and B. Edge upserts both. Later, Core deletes entry B. On the next sync, Core sends only entry A. Edge should upsert A and prune B from its local catalog.

**Expected behavior:** After sync, Edge's local catalog contains exactly the entries Core sent. Entries deleted from Core are removed from Edge's local database.

**Result:** PASS. `HandlePayloadCatalog` in `shingo-edge/engine/engine.go` collects active IDs during upsert and calls `DeleteStalePayloadCatalogEntries` after all entries are processed. The `DeleteStalePayloadCatalogEntries` method deletes entries whose IDs are not in the active set, with a safety guard that skips pruning if the active set is empty (prevents accidental full wipe).

**Bug found and fixed:** Before the fix, `HandlePayloadCatalog` only upserted entries without pruning. Stale deleted payloads accumulated in Edge's local catalog indefinitely. See TC-70 in Bugs Found and Fixed for full details.

**Test:** `shingo-edge/engine/wiring_test.go` -- `TestHandlePayloadCatalog_PruneDeletedEntries`

---
