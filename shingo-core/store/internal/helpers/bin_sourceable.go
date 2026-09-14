package helpers

import "shingocore/store/reservations"

// ── THE SOURCING PREDICATE, AND WHY THERE IS ONLY ONE OF IT ─────────────────
//
// "May this bin be sourced for this job?" — right part, attested, not held, at
// a node automation is allowed to drive to, in a carrier the part is allowed to
// travel in.
//
// It used to be answered by six hand-written WHERE clauses that did not agree.
// A golden photograph of them (dispatch/binresolver) found four divergences,
// and the owner ruled on each on 2026-09-14:
//
//   - STATUS IS THE ALLOW-LIST, ONCE. SourceableStatusSQL is the one source of
//     truth; the hand-spelled reject-lists are deleted. A reject-list answers
//     TRUE for any status it forgot to name, and the column carries no CHECK
//     constraint, so off-spec values were sourceable by default in three
//     readers and refused in the rest.
//   - THE BIN-TYPE RULE BINDS EVERY READER. It is hard at shingo, plant and
//     customer level; carriers are not flexed. One reader enforced it.
//   - A DISABLED NODE IS DEAD TO AUTOMATION. No sourcing, no digs. Only the
//     by-hand door — an engineer moving a bin deliberately — may touch a bin
//     standing there.
//   - THE DIG READERS ARE NOT SOURCING READERS. "Which buried bin do I dig" is
//     a different question and keeps its own spelling; it is reservation-blind
//     on purpose, because a reservation on a buried bin is the reason to dig
//     it. They take the disabled-node rule and nothing else from here.
//
// IT LIVES HERE AND NOT IN store/bins BECAUSE TWO RULES INTERSECT. depguard's
// store-sub-pkg-isolation stops store/sourceability importing store/bins, and
// Go's internal rule stops service/ importing store/internal. store/internal/
// helpers is the one place every store aggregate may reach — it is already the
// home of the shared lane SQL and already imports store/reservations — so the
// text lives here and store/bins re-exports the names for callers outside
// store/. One definition, several doors.
//
// THE THREE PARTS ARE NAMED SEPARATELY BECAUSE TWO READERS NEED THEM APART, not
// as configuration. PoolBreakdownByPayload reports free-vs-held over one pool,
// so it needs membership without the holds; EmptyCarrierWhere asks the same
// question of an empty carrier, which has no stock to attest. Every reader that
// wants the whole question calls BinSourceableSQL and gets all of it.
//
// Every fragment assumes the bins table is aliased `b` and nodes `n`, as
// BinJoinQuery establishes.

// SourceableStatusSQL is the allow-list half of the status rule. Its Go twin is
// domain.BinStatus.Sourceable; TestSourceableStatus_GoSQLAgree fails if the two
// disagree about any status, off-spec values included.
const SourceableStatusSQL = `b.status IN ('available','staged')`

// NodeEnabledSQL — a disabled node is dead to automation. Nothing automated
// sources from one, digs one, or counts stock on one; only the by-hand door
// (an engineer moving a bin deliberately) may touch a bin standing there.
// Split out because one reader needs it without the synthetic half: the
// changeover preflight's presence count deliberately sees bins riding a robot.
const NodeEnabledSQL = `n.enabled = true`

// BinAtLiveNodeSQL — the node is one automation may drive to. A synthetic node
// is a robot deck or _TRANSIT, not a place.
const BinAtLiveNodeSQL = NodeEnabledSQL + ` AND COALESCE(n.is_synthetic, false) = false`

// BinUnheldSQL — nobody else has this bin: no hard claim, no operator lock, no
// pending reservation.
const BinUnheldSQL = `b.claimed_by IS NULL AND b.locked = false AND NOT ` + reservations.BinSpokenForSQL

// BinCarriesSourceableStockSQL — the carrier holds attested stock in a state
// that may leave. 'staged' is excluded on top of the allow-list: a staged bin
// is one an operator is working at.
const BinCarriesSourceableStockSQL = `b.manifest_confirmed = true AND ` + SourceableStatusSQL + ` AND b.status <> 'staged'`

// PayloadBinTypeRuleArm renders the payload_bin_types rule for the payload named
// by payloadExpr — a placeholder ("$1") or a column ("b.payload_code") so a
// grouped reader can correlate instead of parameterising. It replaces a const
// that hardcoded $1 and made callers arrange their parameters around it.
//
// THE RULE IS HARD WHERE IT IS DECLARED. It was named "advisory", which read as
// permission to flex a carrier, and it is not: where payload_bin_types has rows
// for a payload, only those types may carry it. What is advisory is the table's
// COVERAGE — a payload with no rows constrains nothing, because the table is
// sparsely populated and a pre-2026-04-27 hard INNER JOIN starved orders for
// payloads nobody had gotten around to describing. That is a gap in the data,
// not a licence, and it is why the fallback is "no rule" rather than "no bins".
func PayloadBinTypeRuleArm(payloadExpr string) string {
	return `
	  AND (
	    b.bin_type_id IN (
	      SELECT pbt.bin_type_id FROM payload_bin_types pbt
	      JOIN payloads p ON p.id = pbt.payload_id WHERE p.code = ` + payloadExpr + `
	    )
	    OR NOT EXISTS (
	      SELECT 1 FROM payload_bin_types pbt
	      JOIN payloads p ON p.id = pbt.payload_id WHERE p.code = ` + payloadExpr + `
	    )
	  )`
}

// BinSourceableSQL is the whole question, for the payload named by payloadExpr.
// Every sourcing reader composes this and adds only its own scope — a lane, a
// node to avoid, a payload list.
func BinSourceableSQL(payloadExpr string) string {
	return BinCarriesSourceableStockSQL + ` AND ` + BinAtLiveNodeSQL +
		` AND ` + BinUnheldSQL + PayloadBinTypeRuleArm(payloadExpr)
}
