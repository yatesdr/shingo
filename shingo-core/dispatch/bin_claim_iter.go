package dispatch

import (
	"fmt"

	"shingocore/dispatch/binresolver"
	"shingocore/store/bins"
)

// claimFirstAvailable iterates a node's bin candidates and attempts to
// claim the first one that passes both the availability check
// (BinUnavailableReason) and the caller-supplied tryClaim closure.
// Returns the claimed bin or nil with the per-bin reject reasons in
// iteration order.
//
// Two callers — planMove (concrete-source-node branch) and
// claimComplexBins (per pickup step) — used to share this loop body
// inline. Both need the same retry-on-race semantics: a bin can pass
// BinUnavailableReason (read of payload/status/claimed_by) and still
// fail ClaimForDispatch (write under the SQL claimed_by IS NULL guard
// when another order grabbed it first). The retry walks remaining
// candidates so a transient race doesn't fail the whole order.
//
// Single-shot lookup paths (planRetrieve via FindSourceBinFIFO,
// planRetrieveEmpty via FindEmptyCompatibleBin, planMove NGRP via the
// resolver) don't use this — they get back exactly one bin and a claim
// failure is terminal.
//
// tryClaim owns the side-effect (the ClaimForDispatch call with the
// caller's UOP and order context). Returning a non-nil error appends a
// "ClaimForDispatch failed: …" reject and continues; returning nil
// counts as success and the loop returns.
//
// Reject strings are diff-stable with the inline loops they replaced so
// existing log greps and incident postmortems still find the same
// reason text.
func claimFirstAvailable(candidates []*bins.Bin, payloadCode string, tryClaim func(b *bins.Bin) error) (*bins.Bin, []string) {
	var rejects []string
	for _, b := range candidates {
		if reason := binresolver.BinUnavailableReason(b, payloadCode); reason != "" {
			rejects = append(rejects, fmt.Sprintf("bin=%d (%s): %s", b.ID, b.Label, reason))
			continue
		}
		if err := tryClaim(b); err != nil {
			rejects = append(rejects, fmt.Sprintf("bin=%d (%s): ClaimForDispatch failed: %v", b.ID, b.Label, err))
			continue
		}
		return b, rejects
	}
	return nil, rejects
}
