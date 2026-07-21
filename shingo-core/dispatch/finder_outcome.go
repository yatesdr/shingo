package dispatch

import "log"

// finder_outcome.go — the ONE place a SourceResult's outcome is admitted into a
// consumer's switch.
//
// Three consumers switch on finder outcomes: intake planning (resolveSource),
// the fulfillment scanner's replay, and the complex supply widening
// (widenSupplyPickups). Before this existed, each carried its own switch with
// its own default arm — and a default arm is where a NEWLY ADDED Outcome goes
// to die silently: planning's default treated anything unknown as Wait, which
// would park an order forever under an outcome nobody wrote handling for.
//
// MapFinderOutcome validates membership and fails LOUDLY on an unknown value —
// it logs the bug and degrades to OutcomeStructural, which terminal-fails the
// order with a visible code instead of silently mis-filing it. Consumers
// switch on the sanitized value with all four arms explicit and no
// behavior-bearing default.
func MapFinderOutcome(res SourceResult) Outcome {
	switch res.Outcome {
	case OutcomeFound, OutcomeWait, OutcomeReshuffle, OutcomeStructural:
		return res.Outcome
	default:
		log.Printf("BUG dispatch: unknown finder outcome %d — a new Outcome was added without teaching MapFinderOutcome; failing structural rather than mis-filing", res.Outcome)
		return OutcomeStructural
	}
}
