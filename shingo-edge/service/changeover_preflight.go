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
	PreflightInventory(payloads []string) (*PreflightCoreResult, error)
}

// PreflightCoreResult is Core's answer (protocol.PreflightResponse). Defined
// here so the interface above doesn't drag the engine package back into
// service.
type PreflightCoreResult struct {
	// Missing is the payloads with no bin free right now: congestion, the bins
	// exist and will come free.
	Missing []string
	// Absent is the subset with no bin at all — not free, not reserved, not
	// claimed: the only answer with nothing to wait for.
	Absent []string
}

// PreflightChecker holds what the composer's draft check asks Core with.
type PreflightChecker struct {
	core PreflightCorePoster
}

// NewPreflightChecker constructs a PreflightChecker.
func NewPreflightChecker(core PreflightCorePoster) *PreflightChecker {
	return &PreflightChecker{core: core}
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
// available bin in the supermarket, and returns Core's answer: the missing
// subset and the absent one. The sentinel and duplicates are skipped
// (dedupPayloadCodes), and an empty list asks Core nothing (an empty answer).
//
// Core unavailable is an ERROR, never "all available": the composer's preview
// says "unchecked", and the check does not pretend it looked.
func (p *PreflightChecker) PreflightPayloads(ctx context.Context, codes []string) (PreflightCoreResult, error) {
	if p.core == nil || !p.core.Available() {
		// OWNER-WORDS
		return PreflightCoreResult{}, fmt.Errorf("preflight: Core does not answer the preflight ask")
	}
	payloads := dedupPayloadCodes(codes)
	if len(payloads) == 0 {
		return PreflightCoreResult{}, nil
	}
	result, err := p.core.PreflightInventory(payloads)
	if err != nil {
		return PreflightCoreResult{}, fmt.Errorf("preflight: ask Core: %w", err)
	}
	return *result, nil
}
