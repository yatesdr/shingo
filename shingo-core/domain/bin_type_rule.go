package domain

// BinTypeRule is payload_bin_types for ONE payload, resolved to a set and
// carried as a value: "which carrier types may hold this part".
//
// ── WHY IT IS A VALUE AND NOT A DB CALL ───────────────────────────────────
//
// The rule binds predicates that run PER CANDIDATE BIN — binresolver's
// BinUnavailableReason over every bin at a node, binsource.RejectReason over
// every bin in a loader's pool. A predicate that read payload_bin_types itself
// would issue one query per candidate, turning a single-query door into an N+1
// on the dispatch hot path, and would put I/O inside binsource, which is pure
// by construction and exhaustively table-tested because of it.
//
// So the rule is LOADED ONCE PER CALL by the door — the payload is fixed for a
// call, so the rule is too — and passed down as a value. The cost is one extra
// read per door call, not one per bin.
//
// ── THE TWO HALVES, AND WHY nil IS NOT len()==0 ───────────────────────────
//
// STRICT WHERE ROWS EXIST. Where payload_bin_types names types for a payload,
// only those types may carry it. Hard at shingo, plant and customer level;
// carriers are not flexed.
//
// PERMISSIVE WHERE NONE DO. A payload with no rows constrains nothing. The
// table is sparsely populated, and a pre-2026-04-27 hard INNER JOIN starved
// orders for payloads nobody had gotten around to describing. Coverage is a gap
// in the DATA, not a licence, which is why the fallback is "no rule" rather
// than "no bins" — the same reading helpers.PayloadBinTypeRuleArm renders as
// `OR NOT EXISTS`.
//
// Those are DIFFERENT ANSWERS for a nil set and an empty one, so the two cannot
// share a representation. A nil `allowed` is "no rows: no rule"; a non-nil
// empty `allowed` is "described as carryable by nothing", which refuses every
// type. A len()==0 check would collapse them and silently turn the second into
// the first — reading a configuration gap as permission, which is exactly the
// direction the ruling forbids.
//
// The zero value is therefore UNRESTRICTED, which is what a caller with no
// payload to key on must pass: a removal leg clearing whatever is resident, an
// empty-carrier pickup that dropped its payload context. Those doors ask for a
// bin without naming a part, and a part is what the rule is about.
type BinTypeRule struct {
	// allowed is nil when the payload has no payload_bin_types rows at all.
	// Unexported so the nil-vs-empty distinction cannot be flattened by a
	// caller building the struct literally.
	allowed map[int64]bool
}

// NewBinTypeRule builds the rule for one payload from the bin-type ids
// payload_bin_types names for it. A nil slice means the payload has no rows and
// constrains nothing; a non-nil empty slice means it is described as carryable
// by nothing and refuses every type.
func NewBinTypeRule(binTypeIDs []int64) BinTypeRule {
	if binTypeIDs == nil {
		return BinTypeRule{}
	}
	allowed := make(map[int64]bool, len(binTypeIDs))
	for _, id := range binTypeIDs {
		allowed[id] = true
	}
	return BinTypeRule{allowed: allowed}
}

// Permits reports whether a carrier of binTypeID may hold the payload this rule
// was built for. Unrestricted (the zero value) permits everything.
func (r BinTypeRule) Permits(binTypeID int64) bool {
	if r.allowed == nil {
		return true
	}
	return r.allowed[binTypeID]
}

// Restricts reports whether the rule constrains anything at all — true when the
// payload has rows. Callers use it to keep a refusal message honest rather than
// to decide the verdict; Permits is the decision.
func (r BinTypeRule) Restricts() bool { return r.allowed != nil }
