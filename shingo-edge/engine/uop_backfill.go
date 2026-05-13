// uop_backfill.go — bucket backfill for fresh Core deployments.
//
// One-shot backfill that seeds Core's authoritative lineside_buckets
// table from Edge's local node_lineside_bucket state on first boot
// against a fresh Core. Item 3 wires the auto-fire path (probe at
// startup, run on Core-empty + Edge-has-rows) and an admin endpoint
// for re-runs.
//
// Why only buckets? bins.uop_remaining on Core has been authoritative
// for as long as bins have existed — every release / delivery /
// cycle-count path writes it via BinManifestService. Buckets are
// different: lineside_buckets is the authoritative table for the
// per-station per-part counts and starts empty on a fresh Core;
// backfill seeds it from Edge's local state so the delta-apply path
// has real state to do capture/drain math against.
//
// Online-safe: the reporter's flush path serializes via outbox;
// concurrent ticks continue emitting deltas during backfill.
// Sequence-id allocation per-scope means a tick that races a backfill
// row gets its own SequenceID and Core's dedup table picks the higher
// value. Net effect: at-least-once delivery of the seed value,
// idempotent because the seed is always "the qty as observed at this
// moment."
//
// Re-running is safe (dedup catches the replay) but pointless once
// Core has the seed; the auto-fire probe (BucketBackfillNeeded)
// returns false in the populated case so re-boots are no-ops.
package engine

import "fmt"

// BackfillBucketsForStation walks every local node_lineside_bucket
// row this station knows about and emits a
// LinesideBucketDelta(capture_fill, +qty) per row. Returns the count
// of deltas recorded.
//
// The deltas land in the reporter's accumulator and ship via the
// next flush. Caller can pass force=true to flush synchronously
// before returning — useful for the typical "flip the flag right
// after backfill" sequence so Core's authoritative table has the
// seed before delta apply switches target.
//
// Errors during the per-node bucket fetch are logged and the
// backfill continues with the remaining nodes (best-effort
// completeness over fail-fast).
func (e *Engine) BackfillBucketsForStation(force bool) (int, error) {
	if e.inventoryDelta == nil {
		return 0, fmt.Errorf("inventory delta sink not configured; backfill requires a reporter")
	}
	return e.inventoryDelta.Backfill(force)
}

// BucketBackfillNeeded returns true when Core reports zero buckets for
// this station AND Edge has at least one local bucket row to seed.
// Used by the startup auto-fire path: a fresh Core deployment booting
// against an Edge that already has lineside state should backfill once
// without operator intervention. Idempotent — re-runs return false
// after the first backfill lands and Core's table is non-empty.
//
// Returns (false, nil) when:
//   - Core's HTTP is unreachable (FetchUOPState returns nil) — caller
//     just retries on the next startup
//   - Core has any buckets for this station (already populated)
//   - Edge has no local buckets to seed
//
// Returns (true, nil) when both Core is empty AND Edge has rows.
func (e *Engine) BucketBackfillNeeded() (bool, error) {
	if e.coreClient == nil || !e.coreClient.Available() {
		return false, nil
	}
	station := ""
	if e.cfg != nil {
		station = e.cfg.StationID()
	}
	nodes, err := e.db.ListProcessNodes()
	if err != nil {
		return false, fmt.Errorf("list process nodes: %w", err)
	}
	nodeNames := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.CoreNodeName != "" {
			nodeNames = append(nodeNames, n.CoreNodeName)
		}
	}
	if len(nodeNames) == 0 {
		return false, nil
	}

	snapshot, err := e.coreClient.FetchUOPState(station, nodeNames)
	if err != nil || snapshot == nil {
		// Unreachable / partial data — defer the backfill, caller
		// retries on next startup.
		return false, nil
	}
	if len(snapshot.Buckets) > 0 {
		// Core already has data for this station's nodes — backfill
		// already ran (or wasn't needed).
		return false, nil
	}

	// Core empty for this station; check Edge has buckets to seed.
	for _, n := range nodes {
		buckets, err := e.db.ListLinesideBuckets(n.ID)
		if err != nil {
			continue
		}
		for _, b := range buckets {
			if b.Qty > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}
