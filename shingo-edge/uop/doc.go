// Package uop holds the Edge-side chokepoint for Unit-of-Production
// state mutations.
//
// Phase 1 scope: the bin-delta accumulator that used to live in
// shingo-edge/messaging, and the lineside pile levels. The public surface is a thin
// Mutator type satisfying the engine's InventoryDeltaSink interface; the
// implementation (accumulator) is unexported so later phases can grow
// the Mutator surface (intent verbs, slot lifecycle, capture, pickup)
// without expanding the accumulator's responsibilities.
//
// Architecture context: post-May-4 commit 6d226d1, Edge is authoritative
// for the count of any bin physically at one of its nodes, and it is the
// only writer of a lineside pile. The messages emitted from this package are
// how Core mirrors Edge's authoritative state. There is no reconciler
// healing back from Core. Each bin delta carries its scope's running net, so
// a message lost, duplicated or reordered on the way is healed by the next
// one; a delta Core refuses (payload mismatch, stale epoch) is recorded at
// Core. A pile is mirrored by its level, the row after every change, which
// heals the same way without a net; the boot re-sends every pile's level.
//
// See shingo-uop-refactor-plan.md (GitHub root) for the full plan.
package uop
