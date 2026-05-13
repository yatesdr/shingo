// Package uop holds the Edge-side chokepoint for Unit-of-Production
// state mutations.
//
// Phase 1 scope: the bin-delta / lineside-bucket-delta accumulator that
// used to live in shingo-edge/messaging. The public surface is a thin
// Mutator type satisfying the engine's InventoryDeltaSink interface; the
// implementation (accumulator) is unexported so later phases can grow
// the Mutator surface (intent verbs, slot lifecycle, capture, pickup)
// without expanding the accumulator's responsibilities.
//
// Architecture context: post-May-4 commit 6d226d1, Edge is authoritative
// for the count of any bin physically at one of its nodes. The deltas
// emitted from this package are how Core mirrors Edge's authoritative
// state. There is no reconciler healing back from Core; if a delta is
// rejected at Core (payload mismatch, dedup hit, etc.), FlushFailures
// surfaces the drift.
//
// See shingo-uop-refactor-plan.md (GitHub root) for the full plan.
package uop
