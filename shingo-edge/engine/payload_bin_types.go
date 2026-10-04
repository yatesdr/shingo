package engine

import "shingo/protocol"

// SetPayloadBinTypes caches the payload→dunnage mapping delivered by Core on
// each NodeListResponse. Called from the node-list-response handler alongside
// SetCoreNodes. In-memory only: the catalog is tiny (~2–10 rows) and is
// re-delivered on every node-list sync, so no SQLite backing is needed.
func (e *Engine) SetPayloadBinTypes(entries []protocol.PayloadBinTypeInfo) {
	e.payloadBinTypesMu.Lock()
	e.payloadBinTypes = entries
	e.payloadBinTypesMu.Unlock()
}

// PayloadBinTypes returns the cached payload→dunnage mapping. Used by the
// operator-station view handler to include the catalog in the JSON response
// so the dunnage picker can derive its button list from the node's allowed
// payloads without a round-trip.
func (e *Engine) PayloadBinTypes() []protocol.PayloadBinTypeInfo {
	e.payloadBinTypesMu.RLock()
	defer e.payloadBinTypesMu.RUnlock()
	return e.payloadBinTypes
}

// BinTypeForPayload resolves one payload code to its dunnage code from the
// cached catalog, or "" when the catalog has no rule for it.
//
// "" IS "UNKNOWN", NOT "NONE", and every caller has to read it that way. The
// catalog arrives with each node-list sync, so an Edge that has not heard from
// Core yet answers "" for everything — and a consumer that treats that as a
// fact rather than an absence is inventing plant configuration.
func (e *Engine) BinTypeForPayload(payloadCode string) string {
	if payloadCode == "" {
		return ""
	}
	for _, row := range e.PayloadBinTypes() {
		if row.PayloadCode == payloadCode {
			return row.BinTypeCode
		}
	}
	return ""
}

// partPermitsCarrier is Core's carrier rule for a part, asked over the catalog
// Core sends with the node list: may a carrier of this type hold the part. It is
// the question Core's pickup asks (domain.BinTypeRule.Permits), asked the same
// way:
//
//   - a part with carriers listed may ride only those, and a part may list
//     several (payload_bin_types is many to many);
//   - a part with none listed may ride anything. That is Core's reading, not
//     the Edge's: the table is sparsely filled, and an unlisted part is not
//     refused anywhere.
//
// No catalog, and a carrier the read did not name, answer true: no judgement.
// The first is an Edge that has not heard from Core since it started, or a
// node list Core sent without the catalog because its own read failed; the
// second is a bin Core did not name the type of. Either way Core still judges
// the pickup. The direction is the safe one: an empty kept that Core will not
// lift waits at the pickup, where a person sees it; an empty sent back that
// Core would have taken is a robot trip the Edge invented.
func partPermitsCarrier(catalog []protocol.PayloadBinTypeInfo, part, carrier string) bool {
	if carrier == "" {
		return true
	}
	listed := false
	for _, row := range catalog {
		if row.PayloadCode != part {
			continue
		}
		if row.BinTypeCode == carrier {
			return true
		}
		listed = true
	}
	return !listed
}

// payloadDunnageCodes returns the distinct bin_type_codes that appear in
// catalog for the given payloadCodes. If payloadCodes is empty, all distinct
// bin_type_codes from the catalog are returned (no-restriction fallback).
// Exported for tests only; the JS side inlines the same logic.
func payloadDunnageCodes(catalog []protocol.PayloadBinTypeInfo, payloadCodes []string) []string {
	allowed := make(map[string]bool, len(payloadCodes))
	for _, c := range payloadCodes {
		allowed[c] = true
	}
	seen := make(map[string]bool)
	var codes []string
	for _, e := range catalog {
		if !seen[e.BinTypeCode] && (len(allowed) == 0 || allowed[e.PayloadCode]) {
			seen[e.BinTypeCode] = true
			codes = append(codes, e.BinTypeCode)
		}
	}
	return codes
}
