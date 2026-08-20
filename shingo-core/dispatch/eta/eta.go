package eta

import (
	"time"

	"shingo/protocol/clock"
)

// ── THESE STAMPS SIT BESIDE SIM-CLOCK ROWS AND WERE MADE OF WALL TIME ─────
//
// Both functions used `time.Now()` while every order timestamp the resulting
// pill is rendered next to comes from `clock.Now()` (§R.98 stage D). On a sim
// clock running ahead of wall, a transit of thirty in-sim seconds produced an
// ETA the better part of half an in-sim hour out, and Edge's late/early buckets
// mis-bucketed for the whole run.
//
// Nothing decides anything on an ETA — it is display, and that is why this is
// last in the ranked clock repairs rather than first. It is also why it matters
// enough to fix: the dashboards a wedge investigation reads are exactly these.

// Stamp returns an ISO 8601 (RFC 3339) timestamp for when this order is
// expected to be delivered, given its source/delivery pair. The pair
// lookup uses the medians Cache; cold-start and unknown-route paths
// fall back to the global p70 and then to the static default, so the
// returned string is always populated.
//
// Callers should attach the result to OrderUpdate.ETA when the order
// transitions into in_transit. The Edge HMI parses the string back to
// a Date and renders the pill bucket from there.
func Stamp(cache *Cache, source, delivery string) string {
	if cache == nil {
		return ""
	}
	d, _ := cache.Lookup(source, delivery)
	if d <= 0 {
		return ""
	}
	return clock.Now().Add(d).UTC().Format(time.RFC3339)
}

// StampFrom is the boot-scan variant. It builds an ETA from a known
// in_transit timestamp rather than `now`, so an order that has been
// in_transit for 30 s gets an ETA 30 s closer than a freshly-stamped
// one. Returns empty if the order is already overdue past `grace` —
// no point in shipping a backwards-pointing ETA that Edge will
// immediately mark as running late on first render.
func StampFrom(cache *Cache, source, delivery string, inTransitAt time.Time, grace time.Duration) string {
	if cache == nil {
		return ""
	}
	d, _ := cache.Lookup(source, delivery)
	if d <= 0 {
		return ""
	}
	// inTransitAt comes off an order row, which is sim-stamped, so the "how
	// overdue is this" comparison has to be made on the same clock.
	eta := inTransitAt.Add(d)
	if clock.Now().Sub(eta) > grace {
		return ""
	}
	return eta.UTC().Format(time.RFC3339)
}
