// Edge-side preflight inventory gate. Builds the to-style's required
// payload list from style_node_claims, calls Core's preflight endpoint,
// and returns the missing payload list so StartProcessChangeover can
// refuse to begin a changeover when bins are absent.

package service

import (
	"context"
	"fmt"

	"shingoedge/store"
	"shingoedge/store/processes"
)

// PreflightCorePoster is the narrow interface PreflightChecker requires
// from the Core HTTP client. Held as an interface (not the concrete
// *engine.CoreClient) to break the engine→service→engine import cycle:
// service is consumed by engine, and engine wires the concrete client
// into PreflightChecker at construction time.
type PreflightCorePoster interface {
	Available() bool
	PreflightInventory(station string, payloads []string) (*PreflightCoreResult, error)
}

// PreflightCoreResult mirrors the wire shape of engine.PreflightResult.
// Defined here so the interface above doesn't drag the engine package
// back into service.
type PreflightCoreResult struct {
	Missing []string
	// Absent is the payloads Core has no bin of at all — not free, not reserved,
	// not claimed. Missing ("none free right now") is congestion; Absent is the
	// only answer with nothing to wait for. AbsentKnown is false when the Core is
	// too old to send it, and a caller must read that as "not known".
	Absent      []string
	AbsentKnown bool
	Available   []PreflightCoreAvailability
}

// PreflightCoreAvailability mirrors engine.PreflightAvailability.
type PreflightCoreAvailability struct {
	PayloadCode string
	BinCount    int
}

// PreflightChecker holds the dependencies needed to gate a changeover on
// upstream bin availability.
type PreflightChecker struct {
	db         *store.DB
	coreClient PreflightCorePoster
	station    string
}

// NewPreflightChecker constructs a PreflightChecker.
func NewPreflightChecker(db *store.DB, coreClient PreflightCorePoster, station string) *PreflightChecker {
	return &PreflightChecker{db: db, coreClient: coreClient, station: station}
}

// PreflightInventoryCheck collects the to-style's required payload codes
// (skipping the empty-bin sentinel "__empty__") and asks Core whether each
// has at least one available bin in the supermarket. Returns the missing
// subset.
//
// nil missing slice + nil error means everything is available. A non-empty
// missing list is the operator-visible diagnostic.
//
// If Core is unavailable the call returns an error rather than degrading
// to "all available" — a preflight that silently passes when the source
// of truth is unreachable defeats the gate.
//
// "Collect the style's codes, then PreflightPayloads": the Core half is on its
// own below so a draft flow — claims that exist only in a request body — can be
// checked without a style row. The two must agree on a saved style, and
// TestChangeoverPreflight_PayloadsAgreeWithStyleCheck says so.
func (p *PreflightChecker) PreflightInventoryCheck(ctx context.Context, toStyleID int64) ([]string, error) {
	if p.coreClient == nil || !p.coreClient.Available() {
		return nil, fmt.Errorf("preflight: core API not configured")
	}
	claims, err := p.db.ListStyleNodeClaims(toStyleID)
	if err != nil {
		return nil, fmt.Errorf("preflight: list claims: %w", err)
	}
	return p.PreflightPayloads(ctx, PayloadCodesOf(claims))
}

// PayloadCodesOf is the payload list a set of claims needs checked: each
// claim's PayloadCode, in claim order, without the empty-bin sentinel and
// without repeats. Pure; the dedup lives here so both gates apply it once.
func PayloadCodesOf(claims []processes.NodeClaim) []string {
	codes := make([]string, 0, len(claims))
	for _, c := range claims {
		codes = append(codes, c.PayloadCode)
	}
	return dedupPayloadCodes(codes)
}

// dedupPayloadCodes drops blanks, the "__empty__" sentinel and repeats,
// keeping first-seen order.
func dedupPayloadCodes(codes []string) []string {
	seen := make(map[string]struct{}, len(codes))
	out := make([]string, 0, len(codes))
	for _, code := range codes {
		if code == "" || code == "__empty__" {
			continue
		}
		if _, dup := seen[code]; dup {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	return out
}

// PreflightPayloads asks Core whether each payload code has at least one
// available bin in the supermarket, and returns the missing subset. The
// sentinel and duplicates are skipped exactly as PreflightInventoryCheck skips
// them, and an empty list asks Core nothing.
//
// Core unavailable is an ERROR, never "all available" — the caller decides
// what an unchecked answer looks like (the composer's preview says
// "unchecked"; a start refuses), the gate does not pretend it looked.
func (p *PreflightChecker) PreflightPayloads(ctx context.Context, codes []string) ([]string, error) {
	if p.coreClient == nil || !p.coreClient.Available() {
		return nil, fmt.Errorf("preflight: core API not configured")
	}
	payloads := dedupPayloadCodes(codes)
	if len(payloads) == 0 {
		return nil, nil
	}
	result, err := p.coreClient.PreflightInventory(p.station, payloads)
	if err != nil {
		return nil, fmt.Errorf("preflight: core call: %w", err)
	}
	return result.Missing, nil
}
