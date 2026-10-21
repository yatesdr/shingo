package domain

// tote_draw.go — what a press cell's build drew from the cart outside it.
//
// A tote press fills totes on the FRONT cart (on the press's own position) and
// draws empty totes from the OUTSIDE cart (on its paired position). When the
// outside cart is lifted to take the front's place, the count of the front's
// build says how many totes were filled, and so how many the outside cart gave.
//
// A nil pointer is unknown, the one spelling of it here and in the column it is
// read from (bins.totes_on_cart IS NULL); 0 is a real count, a base.

// ToteDrawInput is what the derivation reads when the outside cart is lifted.
type ToteDrawInput struct {
	// Cycles is the front cart's count (its uop_remaining), in UOP. nil when
	// the count is not one to trust yet.
	Cycles *int
	// UOPPerTote is the front payload's payloads.uop_per_tote: the cycles that
	// fill one tote.
	UOPPerTote *int
	// FrontTotes is the front cart's totes before the build: the totes it was
	// carrying when it was set on the press.
	FrontTotes *int
	// OutsideTotes is the lifted cart's totes: what stood on it while the front
	// built.
	OutsideTotes *int
}

// ToteDraw is the derivation's answer: the counts to write on the two carts.
type ToteDraw struct {
	// Outside is the lifted cart's totes after the draw, all empty.
	Outside *int
	// Front is every tote now on the front cart, filled or empty.
	Front *int
	// Overdrawn: the count says more totes were filled than both carts held.
	// Outside is then 0, and the ledger detail says so.
	Overdrawn bool
}

// DeriveToteDraw is the one rule:
//
//	filled  = ceil(cycles / uop_per_tote)
//	k       = the front cart's totes, unknown counted as 0
//	drawn   = max(0, filled − k)
//	outside = max(0, H − drawn)      H (the outside cart's totes) unknown stays unknown
//	front   = max(k, filled)
//
// With no UOP per tote, or no count to trust (nil, or below 0), both are
// unknown.
//
// UNKNOWN k COUNTS AS 0, AND THAT ONLY EVER UNDER-STATES. If the front carried
// k > 0 totes nobody counted, the operators drew at most filled − k from the
// outside, so the outside truly holds at least H − filled, which is what this
// writes; and the front truly holds max(k, filled) ≥ filled. Neither number
// claims a tote that is not there. Pinned by TestDeriveToteDraw's "unknown
// front" row.
func DeriveToteDraw(in ToteDrawInput) ToteDraw {
	if in.Cycles == nil || *in.Cycles < 0 || in.UOPPerTote == nil || *in.UOPPerTote < 1 {
		return ToteDraw{}
	}
	filled := (*in.Cycles + *in.UOPPerTote - 1) / *in.UOPPerTote
	k := 0
	if in.FrontTotes != nil {
		k = *in.FrontTotes
	}
	front := max(k, filled)
	out := ToteDraw{Front: &front}
	if in.OutsideTotes == nil {
		return out
	}
	outside := *in.OutsideTotes - max(0, filled-k)
	if outside < 0 {
		outside, out.Overdrawn = 0, true
	}
	out.Outside = &outside
	return out
}
