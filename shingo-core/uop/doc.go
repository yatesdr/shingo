// Package uop holds the Core-side chokepoint for Unit-of-Production
// state mutations.
//
// Today's surface is the Applier — receives BinUOPDelta and
// LinesideBucketDelta envelopes from Edge, dedups against
// inventory_delta_dedup, applies the signed delta to
// bins.uop_remaining / lineside_buckets, writes the audit row, and
// fires the post-update ClearForReuse hook when a capture_reduction
// drives the bin count to zero.
//
// Edge has the symmetric package at shingoedge/uop (post-May-4 Edge
// is authoritative for at-node bin counts; Core's row is the
// aggregating mirror for cross-station inventory).
//
// Audit append (bin_uop_audit row writes) lives in store/audit and is
// shared with bin_manifest_service — kept there because manifest sync
// is the other major path that audits UOP changes alongside this one.
// Future cleanup may consolidate, but the audit boundary is its own
// design question.
//
// See shingo-uop-refactor-plan.md (GitHub root) for the full plan.
package uop
