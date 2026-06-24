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
