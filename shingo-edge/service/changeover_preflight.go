// Edge-side preflight inventory ask, for the flow composer's draft only: the
// draft's payload list goes to Core (the preflight read-ask) and the missing
// subset comes back for the confirm sheet. A changeover start does not ask; it
// reads the sourcing feed's verdict (engine changeoverStockAdvisory).

package service

import (
	"context"
	"fmt"

	"shingoedge/store/processes"
)

// PreflightCorePoster is the narrow interface PreflightChecker requires to
// ask Core. Held as an interface to break the engine→service→engine import
// cycle: service is consumed by engine, and engine wires its adapter over the
// preflight read-ask (engine preflightAsker) into PreflightChecker at
// construction time. Available is whether Core answers the ask.
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

// PreflightChecker holds what the composer's draft check asks Core with.
type PreflightChecker struct {
	core    PreflightCorePoster
	station string
}

// NewPreflightChecker constructs a PreflightChecker.
func NewPreflightChecker(core PreflightCorePoster, station string) *PreflightChecker {
	return &PreflightChecker{core: core, station: station}
}

// PayloadCodesOf is the payload list a set of claims needs checked: each
// claim's PayloadCode, in claim order, without the empty-bin sentinel and
// without repeats. Pure.
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
// sentinel and duplicates are skipped (dedupPayloadCodes), and an empty list
// asks Core nothing.
//
// Core unavailable is an ERROR, never "all available": the composer's preview
// says "unchecked", and the check does not pretend it looked.
func (p *PreflightChecker) PreflightPayloads(ctx context.Context, codes []string) ([]string, error) {
	if p.core == nil || !p.core.Available() {
		// OWNER-WORDS
		return nil, fmt.Errorf("preflight: Core does not answer the preflight ask")
	}
	payloads := dedupPayloadCodes(codes)
	if len(payloads) == 0 {
		return nil, nil
	}
	result, err := p.core.PreflightInventory(p.station, payloads)
	if err != nil {
		return nil, fmt.Errorf("preflight: ask Core: %w", err)
	}
	return result.Missing, nil
}
