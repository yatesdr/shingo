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

// ── THE OTHER SIDE OF THE SAME RULE: THE FINDING ────────────────────────────
//
// PayloadBinTypeRuleArm admits a bin. These two name the bins it REFUSES, and
// they exist because the produce door stopped refusing them.
//
// The write end used to be symmetric with the read end: a finalize onto a
// carrier payload_bin_types excludes was refused outright. That records a lie —
// by the time a cell reports a finalize the parts are physically in the bin, so
// a refusal says "this did not happen" about something that did, and leaves the
// count wrong in the direction nobody can see. Owner, 2026-09-20: the produce
// door ACCEPTS and records a finding; the operator's Load Payload, which is a
// person typing an assignment before anything physical has happened, keeps
// refusing. Sourcing is untouched either way — the bin is still unfetchable,
// which is exactly what the finding is for.

// UndeclaredCarrierRuleSQL is the strict complement of PayloadBinTypeRuleArm:
// true for a bin whose payload HAS declared carriers and whose own type is not
// among them.
//
// IT IS NOT `NOT PayloadBinTypeRuleArm(...)`, and the difference is the sparse
// table. The admission arm is `IN (...) OR NOT EXISTS (...)`; negating it gives
// `NOT IN (...) AND EXISTS (...)`, which is this — but written out, because the
// `EXISTS` half is the whole ruling and a reader has to be able to see it. A
// payload with NO rows constrains nothing and therefore flags nothing: at
// Springfield one payload of 128 has rows, and a rule that flagged the other
// 127 would bury the one case worth walking to.
//
// binTypeExpr names the carrier — a column (`b.bin_type_id`) or a placeholder,
// so a recompute that writes a new type can judge the NEW value rather than the
// row's old one.
func UndeclaredCarrierRuleSQL(payloadExpr, binTypeExpr string) string {
	return `(
	    EXISTS (
	      SELECT 1 FROM payload_bin_types pbt
	      JOIN payloads p ON p.id = pbt.payload_id WHERE p.code = ` + payloadExpr + `
	    )
	    AND ` + binTypeExpr + ` NOT IN (
	      SELECT pbt.bin_type_id FROM payload_bin_types pbt
	      JOIN payloads p ON p.id = pbt.payload_id WHERE p.code = ` + payloadExpr + `
	    )
	  )`
}

// BinInUndeclaredCarrierSQL reads the STAMP, not the rule. Every surface that
// counts or lists findings composes this one fragment, so the inventory page's
// count, the /material-flags list and the sourcing reason cannot disagree about
// how many there are.
//
// It reads the stamp rather than re-deriving the rule because the two must not
// be able to differ: the stamp is what the write door decided, and a surface
// that re-derived it would report a bin as fine the instant a rule changed,
// before the recompute that clears the flag had run. The recompute is in the
// same transaction as the rule change (payloads.SetBinTypes), so the stamp is
// never stale — and if it ever were, the stamp is the number an operator was
// shown and the one to reconcile against.
const BinInUndeclaredCarrierSQL = `b.undeclared_carrier_at IS NOT NULL`

// BinSourceableSQL is the whole question, for the payload named by payloadExpr.
// Every sourcing reader composes this and adds only its own scope — a lane, a
// node to avoid, a payload list.
//
// THE FINDING IS NOT IN HERE AND MUST NOT BE. A flagged bin is refused by the
// bin-type arm already; adding the stamp would be a second spelling of one
// rule, and the day the stamp lagged a rule change the two halves would
// disagree about the same bin.
func BinSourceableSQL(payloadExpr string) string {
	return BinCarriesSourceableStockSQL + ` AND ` + BinAtLiveNodeSQL +
		` AND ` + BinUnheldSQL + PayloadBinTypeRuleArm(payloadExpr)
}
