// Package bins holds bin-aggregate persistence for shingo-core.
//
// Stage 2D of the architecture plan moved bin CRUD, bin types, bin
// manifest operations, and node↔bin-type bindings out of the flat
// store/ package and into this sub-package. The outer store/ keeps
// type aliases (`store.Bin = bins.Bin`, etc.) and one-line delegate
// methods on *store.DB so callers see no public API change.
// Cross-aggregate methods (those whose return type or mutations span
// multiple aggregates, e.g. SetBinManifestFromTemplate) stay at the
// outer store/ level as composition methods.
package bins

import (
	"database/sql"
	"errors"
	"fmt"
	"shingo/protocol/clock"
	"strings"
	"time"

	"shingocore/domain"
	"shingocore/store/internal/helpers"
	"shingocore/store/internal/nodetree"
	"shingocore/store/reservations"
)

// Bin is the bin domain entity. The struct lives in shingocore/domain
// (Stage 2A); this alias keeps the bins.Bin name that every read
// helper, scan function, and Create/Update call in this package uses.
// store.Bin aliases onto this in turn, so call sites across the
// codebase compile unchanged.
type Bin = domain.Bin

// binJoinQuery is the SELECT prefix used by every bin-reading query.
// Export as BinJoinQuery so cross-aggregate readers at the outer store/
// level (which need to add their own WHERE clauses) can reuse it.
// BinJoinQuery is the SELECT prefix used by every bin-reading query.
// The 27th column (has_pending_reservation) is populated from the
// reservations table so BinUnavailableReason can filter reserved bins
// without a separate round-trip. ScanBin reads it into HasPendingReservation.
// Pending-ONLY is sufficient: a confirmed reservation coincides with a hard
// claimed_by (structural, since the one-tx claim+confirm moves them together),
// which b.claimed_by already covers — so this projector never needs to see 'confirmed'.
const BinJoinQuery = `SELECT b.id, b.bin_type_id, b.label, b.description, b.node_id, b.status, b.claimed_by, b.staged_at, b.staged_expires_at,
	COALESCE(b.payload_code, ''), b.manifest, b.uop_remaining, b.delta_epoch, b.manifest_confirmed,
	b.locked, b.locked_by, b.locked_at, b.last_counted_at, b.last_counted_by,
	b.loaded_at, b.anomaly_at, COALESCE(b.anomaly_note, ''), b.created_at, b.updated_at,
	bt.code, COALESCE(n.name, ''), COALESCE(p.uop_capacity, 0),
	EXISTS(SELECT 1 FROM reservations r WHERE r.bin_id = b.id AND r.state = 'pending') AS has_pending_reservation
	` + BinFromClause

// BinFromClause is the FROM/JOIN half of every bin-reading query, split out of
// BinJoinQuery so a COUNT over the same population can reach it.
//
// A count that repeated these joins by hand would be a second definition of
// "which bins exist and what is a bin's type and node" — and the aliases b, bt,
// n, p are what every shared WHERE fragment in this file is written against, so
// a divergence would not be a compile error, it would be a different answer.
const BinFromClause = `FROM bins b
	JOIN bin_types bt ON bt.id = b.bin_type_id
	LEFT JOIN nodes n ON n.id = b.node_id
	LEFT JOIN payloads p ON p.code = b.payload_code`

// ── THE DIG EXCLUSION ───────────────────────────────────────────────────────

// NotForeignDugArm hides candidates standing in a lane a FOREIGN dig holds.
//
// ── WHY EMPTY SELECTION NEEDED THIS ─────────────────────────────────────────
//
// Empty selection was dig-blind: no asker, no dig predicate, in any of the four
// finders. AccessibleEmptyOrder ranks reachable candidates first, so a buried
// empty only wins when EVERY compatible empty is buried — and at that point
// tier 6 turns the pick into a dig rather than sending a robot to a slot it
// cannot reach. If the chosen lane is already dig-held, planBuriedReshuffle
// refuses it at IsLocked and the order parks under CauseLaneLocked.
//
// What happens next depends on THE KIND OF HOLD, which is the distinction round
// 1 spent four reviews finding:
//
//   - EXCAVATION (compound-backed): the dig claims its own target inside the
//     compound transaction, and every empty finder already excludes claimed
//     bins. The next tick cannot see that carrier and diverts by itself — the
//     park self-heals in one tick, and this arm changes nothing.
//   - §R.101 SOURCE LOCK: a mouth row held by a demand. No compound, no bin
//     claims, nothing hidden. The parked order re-picks the SAME buried empty
//     every tick and re-parks, indefinitely, while a diggable free lane sits
//     unconsidered. It is not bounded by any dig's duration because the hold is
//     not a dig that finishes.
//
// THE SOURCE LOCK IS WHAT THIS BUYS. Anyone evaluating the arm against the
// excavation case will conclude it was pointless, because there it is.
//
// ── SEVERITY, STATED ────────────────────────────────────────────────────────
//
// No plant runs lane locks yet, so the collision cannot occur in production
// today (owner, 2026-08-17). This is sim-proven future-proofing that lands with
// MG3 because the queries are open here, not because anything on a floor is
// waiting on it.
//
// ── NARROW, AND FIND-SIDE ONLY ──────────────────────────────────────────────
//
// It hides candidates in the DUG LANE and nothing else — never the whole group.
// A sibling lane in the same group stays eligible, which is the entire point:
// diverting to it is what the order should have done in the first place.
//
// The predicate is RENDERED by DigExclusionSQL, never hand-spelled, so the
// empty finders join the three existing readers of the dig-lock question rather
// than becoming a fourth answer to it. That file's account of what happens when
// the readers disagree is why this is an import and not a copy.
//
// AND IT NEVER ENTERS A COUNT. See EmptyOfTypeInGroupWhere: a count that hid a
// dug-lane resident produces a real extra order that nothing cancels, and a
// per-asker count would make the level flap with every dig, manufacturing
// phantom shortfalls that fight the dig that caused them.
//
// The hold sits on the LANE, and a candidate's lane is its node's parent — the
// same join ListChildNodesUnlocked makes from the other direction.
func NotForeignDugArm(modeParam, askerParam, laneOwnerParam int) string {
	return fmt.Sprintf(`
	  AND NOT EXISTS (
		SELECT 1 FROM reservations dig_hold
		 WHERE dig_hold.resource_kind = 'mouth'
		   AND dig_hold.node_id = n.parent_id
		   AND dig_hold.state IN ('pending','confirmed')
		   AND dig_hold.mode = $%d
		   AND %s
	  )`, modeParam, reservations.DigExclusionSQL("dig_hold.order_id", askerParam, laneOwnerParam))
}

// emptyQueryArgs accumulates bind values and hands out their positions.
//
// The empty finders compose optional arms — a zone preference, a maintained-
// group fence, a dig exclusion — and each one that is absent shifts every
// placeholder after it. Hand-numbering that across two query variants per
// finder is arithmetic nobody can review, and getting it wrong binds the right
// value to the wrong clause: a query that runs, returns rows, and answers a
// different question.
//
// add returns the 1-based position of the value it just appended, which is what
// every arm renderer takes.
type emptyQueryArgs struct{ vals []any }

func (a *emptyQueryArgs) add(v any) int {
	a.vals = append(a.vals, v)
	return len(a.vals)
}

// ── THE FENCE ───────────────────────────────────────────────────────────────

// EmptyFence is what a plant-wide empty search needs to know about maintained
// groups: who is asking, and on whose behalf.
//
// SHARING IS THE PLANT DEFAULT. Derek's plant-wide empty sharing stays exactly
// as it is for everyone; the ONLY fenced zones are maintained groups with
// strict_sourcing on. A blank EmptyFence fences nothing, which is what every
// caller that has no order in hand keeps getting.
type EmptyFence struct {
	// ProcessNode is the asker's process node NAME — the identity the supports
	// table is keyed on. Blank means "supported nowhere", which is the correct
	// reading for an ask that names no process: it is an outsider at every
	// strict group, which is the safe direction.
	ProcessNode string
	// OriginGroup is the maintained group this ask exists to FILL, by name.
	// Blank for everything that is not a level keeper's top-off.
	OriginGroup string
}

// Empty reports whether this fence excludes nothing, so a caller can skip
// rendering the CTE entirely rather than run a walk over an empty root set.
func (f EmptyFence) Empty() bool { return f.ProcessNode == "" && f.OriginGroup == "" }

// Args returns the two bind values FencedNodesCTE's placeholders take, in the
// order the placeholders were named. Beside the renderer, on DigAsker.Args's
// precedent, so a caller cannot pass them in the wrong order or forget one.
func (f EmptyFence) Args() []any { return []any{f.ProcessNode, f.OriginGroup} }

// FencedNodesCTE renders the set of nodes this asker may not source an empty
// from, as a recursive walk over the two rules that hide a carrier.
//
// ── RULE (i): THE FENCE ─────────────────────────────────────────────────────
//
// A strict maintained group's empties are RESERVED for the processes it
// supports. An outsider's plant-wide scan cannot see them. That is the whole
// point of the feature: nothing may steal from the press empty zones, and
// everyone else keeps sharing.
//
// Supported-ness is read from node_maintain_supports by process node NAME,
// which is what the ask carries. A group that supports nobody fences everybody,
// and that is right rather than a degenerate case — it is a group in the middle
// of being configured, and the safe reading of "I have not said who this is
// for" is "not for you".
//
// RECIPROCITY FALLS OUT, unasked for: a keeper topping up group A is not in
// group B's supports list either, so it is an outsider at B by the same rule
// that makes a press an outsider at A. Two maintained groups cannot drain each
// other, and nothing had to be written to arrange it.
//
// ── RULE (ii): NOT FROM THE GROUP YOU ARE FILLING ───────────────────────────
//
// A top-off ask may not source a carrier already standing in the group it is
// filling. That is MG2-11, absorbed here so there is ONE spelling of "not from
// a maintained group" rather than two that can drift.
//
// It is a SEPARATE RULE and not a special case of the fence, and the difference
// matters: the fence asks "are you an outsider here?" — a keeper is not, at its
// own group — while this asks "are you filling this group?". The keeper is
// exempt from rule (i) at its own group and caught by rule (ii) there anyway.
// Net effect: the keeper sources from the market and the cells, never from any
// maintained group, and a supported press reaches its own group through the
// supports list.
//
// The measured consequence of not having rule (ii): a six-position group
// standing at 2 of a level of 4 dispatched both its top-off asks against its
// OWN remaining carriers, moving them from one of its positions to another. The
// claims then dropped `resident`, which re-opened the gap, which asked again —
// the group shuffled itself and never reached its level.
//
// APPLIED WITHOUT REGARD TO strict_sourcing, unlike rule (i). Filling a group
// from itself is a null trip whether or not anybody has fenced it.
//
// ── WHY A NODE SET AND NOT A PER-ROW TEST ───────────────────────────────────
//
// The question is "does this carrier sit under a fenced group", which is an
// ancestor walk from each candidate — a correlated recursion per row. Inverting
// it into one descendant walk from the fenced ROOTS computes the same set once,
// and closes NESTING by construction: a group inside a fenced group is in the
// subtree, so membership-in-any-maintained-ancestor is what the walk already
// answers.
//
// processParam and originParam are the 1-based positional parameters that will
// carry EmptyFence.Args().
func FencedNodesCTE(processParam, originParam int) string {
	return fmt.Sprintf(`WITH RECURSIVE fenced_roots(id) AS (
		SELECT np.node_id FROM node_properties np
		 WHERE np.key = 'strict_sourcing' AND np.value = 'on'
		   AND NOT EXISTS (
			 SELECT 1 FROM node_maintain_supports s
			 JOIN nodes pn ON pn.id = s.process_node_id
			 WHERE s.group_node_id = np.node_id AND pn.name = $%d
		   )
		UNION
		SELECT g.id FROM nodes g WHERE $%d <> '' AND g.name = $%d
	),
	fenced(id) AS (
		SELECT id FROM fenced_roots
		UNION ALL
		SELECT n2.id FROM nodes n2 JOIN fenced f ON n2.parent_id = f.id
	) `, processParam, originParam, originParam)
}

// NotFencedArm keeps a candidate out of the fenced set. Assumes a `fenced(id)`
// CTE is in scope — compose FencedNodesCTE for it.
//
// FIND-SIDE ONLY, and that is a standing ruling rather than an oversight. See
// EmptyOfTypeInGroupWhere for why no fence, no dig arm and no asker may ever
// enter a count.
func NotFencedArm() string {
	return `
	  AND b.node_id NOT IN (SELECT id FROM fenced)`
}

// ── THE EMPTY-CARRIER FRAGMENT FAMILY ───────────────────────────────────────
//
// Four empty finders carried four hand-written copies of the same predicate,
// differing only in which arms they added. Round 1's census kept every TIER —
// they differ in kind, and a single parameterized finder could express at most
// two of six — but named the one consolidation that IS earned as sitting a
// level down: the WHERE bodies, not the tiers.
//
// WHAT MAKES A CARRIER AN EMPTY, in one place. Every clause below is in every
// one of the four queries today, character for character; the copies were
// identical, which is exactly why nobody noticed they were copies.
//
// THE ARMS ARE FUNCTIONS OF A PARAMETER INDEX, not strings a caller splices.
// Each finder numbers its placeholders differently, so an arm has to be told
// which position it occupies — and taking an int rather than a string means a
// caller cannot put anything into the SQL but a positional placeholder. That is
// nodetree's rule, for nodetree's reason.
//
// WHAT IS NOT HERE, DELIBERATELY: the ordering. AccessibleEmptyOrder stays a
// separate trailing fragment each finder appends for itself, because it is a
// different kind of thing — the WHERE says which carriers are eligible, the
// ORDER BY says which eligible one costs least to grab. Consolidating them
// together would let a future arm silently change the ranking.

// EmptyCarrierWhere is the core: an unclaimed, unlocked, unstaged, payload-less
// carrier standing at an enabled physical node, with nothing pending against it.
//
// Each exclusion is here because sourcing excludes it, and any count over the
// same population must agree:
//   - staged and pending-reservation carriers are spoken for;
//   - claimed and locked ones likewise;
//   - synthetic and disabled nodes hold nothing anybody can act on;
//   - anything carrying a payload has left the empty population entirely.
//
// It opens the WHERE. Arms append to it; nothing composes in front of it.
const EmptyCarrierWhere = `
	WHERE ` + SourceableStatusSQL + ` AND b.status <> 'staged'
	  AND b.claimed_by IS NULL
	  AND b.locked = false
	  AND b.node_id IS NOT NULL
	  AND n.enabled = true
	  AND n.is_synthetic = false
	  AND COALESCE(b.payload_code, '') = ''
	  AND NOT EXISTS (SELECT 1 FROM reservations r WHERE r.bin_id = b.id AND r.state = 'pending')`

// OfTypeArm narrows to ONE carrier type, matched on CODE.
//
// On code and not on bin_type_id, because that is what keeps the readers
// honest: the level keeper holds a code (it comes out of the episode key, which
// carries the code so a log line and a restore are both readable) and the
// finders have always matched on code. An id-keyed arm would be equivalent and
// would be a SECOND SPELLING of "of this type".
func OfTypeArm(typeParam int) string {
	return fmt.Sprintf(`
	  AND bt.code = $%d`, typeParam)
}

// InGroupArm narrows to carriers standing inside a group's subtree.
//
// Assumes a `descendants(id)` CTE is in scope — compose nodetree.DescendantsOf
// for it, which is SELF-EXCLUDED: a group node is synthetic and holds no
// carriers, so its own id in the set changes nothing today and would mean
// something different the day one does.
func InGroupArm() string {
	return `
	  AND b.node_id IN (SELECT id FROM descendants)`
}

// OutsideGroupArm is InGroupArm's inverse: everywhere EXCEPT a subtree.
//
// It takes the SUBTREE walk (nodetree.SubtreeOf), not the descendants one, and
// the difference is load-bearing in the direction of exclusion. Excluding only
// the descendants would leave the root itself eligible; the root is synthetic
// and holds no carriers today, so the two are equivalent now and stop being
// equivalent the moment a group node can hold one. An exclusion that is
// accidentally correct is the kind that stops being correct silently.
//
// Both walks name their CTE `descendants` — deliberately, so they are drop-in
// for one another and the FUNCTION names carry the difference. That naming is
// also how MG2-11 first shipped broken: the query said `FROM subtree`, threw on
// every call, and every caller read the throw as "no empty found".
func OutsideGroupArm() string {
	return `
	  AND b.node_id NOT IN (SELECT id FROM descendants)`
}

// InZoneArm narrows to one zone. See the note on FindEmptyOfType for why a zone
// PREFERENCE exists at all.
func InZoneArm(zoneParam int) string {
	return fmt.Sprintf(`
	  AND n.zone = $%d`, zoneParam)
}

// ExcludeNodeArm drops one node — the destination, so a retrieve cannot source
// from the place it is delivering to. Zero excludes nothing.
func ExcludeNodeArm(nodeParam int) string {
	return fmt.Sprintf(`
	  AND ($%d = 0 OR b.node_id != $%d)`, nodeParam, nodeParam)
}

// EmptyOfTypeInGroupWhere is the predicate for "an unclaimed empty carrier of
// ONE type, standing at an enabled physical node inside a group".
//
// ONE SPELLING, TWO READERS, BY CONSTRUCTION — FindEmptyOfTypeInGroup and
// CountEmptyOfTypeInGroup both interpolate this exact string, so the level
// keeper cannot count six carriers the press finder cannot see. That failure is
// not hypothetical: it is the "buffer slots not sourced" shape at a new grain,
// and it arrives silently because both halves look correct in isolation. Round 1
// asked for the count to be built from the finder's own WHERE for exactly this
// reason; sharing the text is the only version of that promise a reader can
// check.
//
// WHAT IT DELIBERATELY EXCLUDES, each because the finder excludes it and the
// count must agree:
//   - staged bins and pending-reservation bins — spoken for, invisible to
//     sourcing, and therefore not part of a level that exists to be sourced FROM;
//   - claimed and locked bins, for the same reason;
//   - synthetic and disabled nodes — a carrier parked on a disabled position is
//     not on hand in any sense the keeper can act on;
//   - anything carrying a payload. A maintained level counts EMPTIES. A carrier
//     that gained a payload while resident has left the level, which is stated
//     policy (design §2.3: strict hides empties only) rather than an oversight.
//
// Placeholders: $1 = bin type CODE, $2 = group node id, $3 = node id to exclude
// (0 = exclude nothing). It assumes a `descendants(id)` CTE is in scope —
// compose nodetree.DescendantsOf($2), which is SELF-EXCLUDED: a group node is
// synthetic and holds no carriers, and its own id in the set would change
// nothing today and mean something different the day one does.
//
// COMPOSED FROM THE FAMILY AS OF MG3-1, and the identity is unchanged: same
// name, same clauses, same semantics, still interpolated verbatim by BOTH
// readers. Only the definition moved — from a hand-written body to
// EmptyCarrierWhere plus three arms — so the finder and the count still share
// one string by construction rather than by agreement.
//
// NO STRICT ARM AND NO DIG ARM EVER ENTER IT. That is a standing ruling, and
// the reasons are asymmetric in duration. A find/count divergence under a live
// dig is transient and self-heals; a COUNT that hides a dug-lane resident
// produces a real extra order that nothing ever cancels — permanent overfill,
// the 241 shape arriving through the count. And a per-asker count would make
// the level bounce with every dig, manufacturing phantom shortfalls that fight
// the dig that caused them. The level is PHYSICAL: how many carriers are
// standing there, not how many this particular asker may take.
//
// A var rather than a const now, since it is composed at init.
// AccessibleEmptyOrder set that precedent for the same reason.
//
// THE TYPE IS MATCHED ON CODE, not on bin_type_id, and that is what keeps the
// two readers honest. The keeper holds a code (it comes out of the episode key,
// which carries the code so a log line and a restore are both readable); the
// finder has always matched on code. An id-keyed count would be equivalent and
// would be a SECOND SPELLING of "of this type" — precisely the thing this
// fragment exists to prevent.
var EmptyOfTypeInGroupWhere = EmptyCarrierWhere +
	OfTypeArm(1) + InGroupArm() + ExcludeNodeArm(3)

// SourceableStatusSQL is the SQL twin of domain.BinStatus.Sourceable: the set of
// statuses a bin may be picked up from. One rule in two languages —
// TestSourceableStatus_GoSQLAgree evaluates every enum constant plus an off-spec
// value against both and fails if they part, so adding a status forces both sides
// to be updated together.
//
// ALLOW-LIST for the same reason the Go side is: the status column carries no
// CHECK constraint, so a value outside the enum is representable and must not be
// sourceable by default.
//
// A reader that must be STRICTER composes on top of this rather than restating the
// whole rule — the full-source and empty-source queries additionally exclude
// 'staged' so a plant-wide scan cannot take a bin an operator is working at:
//
//	SourceableStatusSQL + ` AND b.status <> 'staged'`
//
// Assumes the bins table is aliased `b`, as BinJoinQuery establishes.
const SourceableStatusSQL = `b.status IN ('available','staged')`

// PayloadBinTypeAdvisoryClause enforces payload_bin_types as an advisory
// allow-list: when the table has rules for the payload, only matching bin
// types are eligible; when no rules exist for the payload, any bin type
// is eligible. Used by FindEmptyCompatible (empty-bin retrieve) and
// FindSourceFIFO (full-bin retrieve) so the two readers stay coherent.
//
// Both branches reference $1 (payloadCode) — callers must place
// payloadCode at parameter position $1.
//
// Rationale: the allow-list table is sparsely populated in practice. A
// pre-2026-04-27 hard INNER JOIN on this table starved orders for
// payloads with no rules even when compatible empty bins existed. Every
// other reader (FindSourceFIFO, SetManifest writes) ignores the
// table entirely. Advisory enforcement matches that prior practice while
// preserving the constraint for plants that DO populate the table.
const PayloadBinTypeAdvisoryClause = `
	  AND (
	    b.bin_type_id IN (
	      SELECT pbt.bin_type_id FROM payload_bin_types pbt
	      JOIN payloads p ON p.id = pbt.payload_id WHERE p.code = $1
	    )
	    OR NOT EXISTS (
	      SELECT 1 FROM payload_bin_types pbt
	      JOIN payloads p ON p.id = pbt.payload_id WHERE p.code = $1
	    )
	  )`

// AccessibleEmptyOrder ranks compatible empty-bin candidates by least-work-to-
// grab and is the trailing ORDER BY / LIMIT for every empty-source query.
//
// Empties are fungible — which physical empty fills an order doesn't matter, so
// the planner should grab the one that costs the least to extract:
//  1. accessible slots first — nodes.ReachableSQL, the one definition of "no
//     occupied slot sits strictly shallower in the same lane". This used to be
//     an inline copy annotated "mirrors nodes.IsSlotAccessible exactly", which
//     is the kind of claim a comment cannot keep;
//  2. then shallowest depth — a lane-mouth empty beats one a row deeper;
//  3. then bin id — a stable tiebreak.
//
// Before 2026-06-13 these queries ordered by bin id alone (lane-blind FIFO), so
// the planner routinely picked a buried empty and then reactively reshuffled the
// bins on top of it — the post-find buried check in source_finder.go's tier 6,
// which routes to planBuriedReshuffleAtIntake. (This cited planning_service.go
// until 2026-08-04; the check moved onto the finder and the citation did not.)
// Ordering accessibility first means an accessible empty is always preferred and
// a reshuffle happens only when EVERY compatible empty is buried — the lane mouth
// is emptied before anything gets dug out. The reshuffle path stays as the
// last-resort fallback; it is no longer the common case.
//
// The accessibility subquery is uncorrelated to query params (it references the
// candidate's own node columns), so it does not shift caller placeholder numbers.
//
// The two escape hatches ahead of it are kept verbatim. ReachableSQL already
// answers true for a slot with no parent (the correlation yields no rows) and
// for one with no depth (its own IS NOT NULL guard), so they are redundant —
// but this is the only spelling that carried them in SQL rather than in Go, and
// deleting them here would make a reader hunt for where the null cases went.
//
// A var rather than a const now, since it is composed at init. Every caller
// interpolates it with fmt.Sprintf, so nothing needed a constant.
var AccessibleEmptyOrder = `
	ORDER BY (n.parent_id IS NULL OR n.depth IS NULL OR ` + helpers.ReachableSQL("n") + `) DESC,
	         COALESCE(n.depth, 0) ASC,
	         b.id ASC
	LIMIT 1`

// ScanBin reads a single bin row (including joined bin_type code + node name).
// Exported for cross-aggregate readers at the outer store/ level.
func ScanBin(row interface{ Scan(...any) error }) (*Bin, error) {
	var b Bin
	var nodeID, claimedBy sql.NullInt64
	var manifest sql.NullString
	err := row.Scan(&b.ID, &b.BinTypeID, &b.Label, &b.Description, &nodeID, &b.Status, &claimedBy,
		&b.StagedAt, &b.StagedExpiresAt,
		&b.PayloadCode, &manifest, &b.UOPRemaining, &b.DeltaEpoch, &b.ManifestConfirmed,
		&b.Locked, &b.LockedBy, &b.LockedAt, &b.LastCountedAt, &b.LastCountedBy,
		&b.LoadedAt, &b.AnomalyAt, &b.AnomalyNote, &b.CreatedAt, &b.UpdatedAt, &b.BinTypeCode, &b.NodeName, &b.UOPCapacity,
		&b.HasPendingReservation)
	if err != nil {
		return nil, err
	}
	if nodeID.Valid {
		b.NodeID = &nodeID.Int64
	}
	if claimedBy.Valid {
		b.ClaimedBy = &claimedBy.Int64
	}
	if manifest.Valid {
		b.Manifest = &manifest.String
	}
	return &b, nil
}

func scanBins(rows *sql.Rows) ([]*Bin, error) {
	var bins []*Bin
	for rows.Next() {
		b, err := ScanBin(rows)
		if err != nil {
			return nil, err
		}
		bins = append(bins, b)
	}
	return bins, rows.Err()
}

// Create inserts a new bin row and sets b.ID on success.
func Create(db *sql.DB, b *Bin) error {
	id, err := helpers.InsertID(db, `INSERT INTO bins (bin_type_id, label, description, node_id, status) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		b.BinTypeID, b.Label, b.Description, helpers.NullableInt64(b.NodeID), b.Status)
	if err != nil {
		return fmt.Errorf("create bin: %w", err)
	}
	b.ID = id
	return nil
}

// Update writes the mutable columns on a bin (bin_type_id, label, description,
// node_id, status).
func Update(db *sql.DB, b *Bin) error {
	_, err := db.Exec(`UPDATE bins SET bin_type_id=$1, label=$2, description=$3, node_id=$4, status=$5, updated_at=$7 WHERE id=$6`,
		b.BinTypeID, b.Label, b.Description, helpers.NullableInt64(b.NodeID), b.Status, b.ID, clock.Now().UTC())
	return err
}

// Delete removes a bin row outright. Operational flows go through
// Retire instead — physical-row deletes break FK relationships to any
// table that points to bins.id (claims, order history, audit, transit
// tracking) and so are reserved for admin/DBA recovery paths where
// the caller has guaranteed those relationships are absent.
func Delete(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM bins WHERE id=$1`, id)
	return err
}

// Retire marks a bin retired and vacates its node assignment in a
// single atomic UPDATE. This is the operator-driven path that replaces
// the old "Delete Bin" admin action — pre-Round-3 the admin Delete
// button issued a DELETE which raised FK violations on any bin with
// history (claimed_by, order rows, audit entries), stranding operators
// who wanted to retire a physically out-of-service carrier.
//
// Setting node_id=NULL excludes the bin from operational readers
// (CountByAllNodes, NodeTileStates, ListByNode all filter
// status != 'retired' OR scope to node_id matches, and node_id=NULL
// satisfies neither). Audit/admin views that need retired bins can
// query via List + status filter.
//
// Idempotent: a second call on an already-retired bin is a successful
// no-op (the row stays status='retired', node_id=NULL).
func Retire(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE bins SET status='retired', node_id=NULL, updated_at=$2 WHERE id=$1`, id, clock.Now().UTC())
	return err
}

// Get fetches a bin by ID with its joined bin_type code and node name.
func Get(db *sql.DB, id int64) (*Bin, error) {
	row := db.QueryRow(fmt.Sprintf(`%s WHERE b.id=$1`, BinJoinQuery), id)
	return ScanBin(row)
}

// GetByLabel fetches a bin by its unique label.
func GetByLabel(db *sql.DB, label string) (*Bin, error) {
	row := db.QueryRow(fmt.Sprintf(`%s WHERE b.label=$1`, BinJoinQuery), label)
	return ScanBin(row)
}

// List returns every bin ordered by ID descending.
func List(db *sql.DB) ([]*Bin, error) {
	rows, err := db.Query(fmt.Sprintf(`%s ORDER BY b.id DESC`, BinJoinQuery))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}

// ListByNode returns all non-retired bins at a node ordered by ID descending.
// Retired bins are excluded so operational consumers (node-bins telemetry,
// occupancy checks, swap-vs-move decision) don't see a retired carrier as
// occupying its old node. Audit/admin views that need retired bins should
// query via List + status filter instead. This is a hedge until retirement
// vacates the operational node entirely (RETIRED_HOLD migration — see
// SHINGO_TODO.md).
func ListByNode(db *sql.DB, nodeID int64) ([]*Bin, error) {
	rows, err := db.Query(fmt.Sprintf(`%s WHERE b.node_id=$1 AND b.status != 'retired' ORDER BY b.id DESC`, BinJoinQuery), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}

// ListByNodes returns every non-retired bin across the given nodes in ONE query —
// the batched form of ListByNode for the dedicated-loader pool gather (one read
// instead of N per-member reads on the hot retrieve path). Same retired-bin
// exclusion + id-desc order. (pgx stdlib has no native []int64 array param, so the
// IN-list is built with explicit placeholders, as elsewhere in this file.)
func ListByNodes(db *sql.DB, nodeIDs []int64) ([]*Bin, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(nodeIDs))
	args := make([]any, len(nodeIDs))
	for i, id := range nodeIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	query := fmt.Sprintf(`%s WHERE b.node_id IN (%s) AND b.status != 'retired' ORDER BY b.id DESC`,
		BinJoinQuery, strings.Join(placeholders, ","))
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}

// CountByNode returns how many non-retired bins sit at the given node.
// Same retired-bin exclusion rationale as ListByNode.
func CountByNode(db *sql.DB, nodeID int64) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM bins WHERE node_id=$1 AND status != 'retired'`, nodeID).Scan(&count)
	return count, err
}

// ListByClaim returns all non-retired bins claimed by the given order.
// Used by HandleOrderRelease's fallback when order.BinID is nil — the
// claim is the canonical "this order's bin(s)" pointer regardless of
// where the bin physically sits, which is critical under transit
// semantics where node_id may be _TRANSIT instead of the original
// source. Multi-bin complex orders return multiple rows in step order.
func ListByClaim(db *sql.DB, orderID int64) ([]*Bin, error) {
	rows, err := db.Query(fmt.Sprintf(`%s WHERE b.claimed_by=$1 AND b.status != 'retired' ORDER BY b.id`, BinJoinQuery), orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}

// ListAnomalousTransitBins returns bins parked on a SYNTHETIC node with no live
// order claim. This is the binary anomaly signal under bin-transit-state Phase 5
// — a bin that is not anywhere physical and that no order owns, so it needs
// operator recovery to be reassigned to a real node.
//
// Filters claimed_by IS NULL because a healthy in-flight bin has
// claimed_by set to its order (only the failure path clears the
// claim).
//
// The name is now narrower than what it returns — _TRANSIT is the commonest of
// these, not the only one. It is left alone here rather than renamed across its
// callers in a batch about something else; the widening is the load-bearing part
// and the doc says what it does.
// ── WHY IT IS EVERY SYNTHETIC NODE, NOT JUST _TRANSIT ─────────────────────
// It filtered on the name `_TRANSIT` alone, which made a whole shape of stray
// invisible: a bin recorded on a DIFFERENT synthetic node — a node group or a
// lane root — unclaimed, with no anomaly stamp. Nothing lists it, no floor
// covers it, and no selector will hand it out, because a bin belongs in a
// concrete slot and a group is not somewhere a bin can physically be. Observed
// on the rig as bin 37 at SYN_COMP, sitting unowned and unseen for a whole run
// while the page beside it showed one row.
//
// The widening is small by measurement, not by hope: on a healthy lane-stress
// run the non-_TRANSIT synthetic population was ONE bin, so this surfaces a real
// stray rather than flooding the operator with legitimate rows.
//
// anomaly_at is NOT required. Requiring it would re-hide exactly this shape —
// the stamp is written by the paths that KNOW they stranded something, and a bin
// nobody stamped is the one nobody noticed. Ordering still puts stamped rows
// first (NULLS LAST) so the diagnosed ones read at the top.
//
// ── EXCEPT THE CARRIER NODES ──────────────────────────────────────────────
// `_ROBOT:<vehicle>` is synthetic and its bins are unclaimed, so they matched
// both predicates and this listed them — which put "a bin riding a robot" on the
// operator's needs-physical-recovery list. That is the precise error the carrier
// node exists to avoid: a bin whose location is known exactly is not lost. Worse
// than the noise, the recovery button was then live on it, and "I found it, it's
// at X" would have moved a bin off a robot still carrying it (RecoverTransitAnomaly
// now refuses that too).
//
// Excluded by prefix rather than by narrowing back to `_TRANSIT`, because the
// widening above is load-bearing: a bin stray at a node group or a lane root is
// still an anomaly nobody else lists.
func ListAnomalousTransitBins(db *sql.DB) ([]*Bin, error) {
	// Concatenated, not Sprintf'd: NotCarrierNodeSQL contains a LIKE pattern
	// ending `%'`, which Sprintf reads as a verb.
	rows, err := db.Query(BinJoinQuery + ` WHERE b.claimed_by IS NULL AND n.is_synthetic AND b.status != 'retired'
		AND ` + NotCarrierNodeSQL + ` ORDER BY b.anomaly_at NULLS LAST, b.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}

// CarrierNodePrefix names the per-robot synthetic nodes a bin rides on while it
// is still on that robot's deck: `_ROBOT:<vehicle>`. One spelling, here, because
// three layers match on it (this package's queries, the engine's sweep, the www
// bins page) and a second literal is how they drift apart.
const CarrierNodePrefix = "_ROBOT:"

// CarrierNodeSQL / NotCarrierNodeSQL match (or exclude) the carrier nodes by
// name.
//
// THE UNDERSCORE IS ESCAPED, and it has to be: `_` is LIKE's single-character
// wildcard, so the obvious `LIKE '_ROBOT:%'` means "any character, then ROBOT:".
// Nothing in the live node set collides today — `_TRANSIT` does not match — so
// this was latent rather than broken, which is exactly the kind of thing that
// stops being latent when someone adds a node type.
const (
	CarrierNodeSQL    = `n.name LIKE '\_ROBOT:%' ESCAPE '\'`
	NotCarrierNodeSQL = `n.name NOT LIKE '\_ROBOT:%' ESCAPE '\'`
)

// CountByAllNodes returns a map of node_id -> bin count for all nodes that have bins.
func CountByAllNodes(db *sql.DB) (map[int64]int, error) {
	rows, err := db.Query(`SELECT node_id, COUNT(*) FROM bins WHERE node_id IS NOT NULL GROUP BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[int64]int)
	for rows.Next() {
		var nodeID int64
		var count int
		if err := rows.Scan(&nodeID, &count); err != nil {
			return nil, err
		}
		counts[nodeID] = count
	}
	return counts, rows.Err()
}

// NodeTileState holds summary flags for rendering a node tile. The
// struct lives in shingocore/domain (Stage 2A.2); this alias keeps
// the bins.NodeTileState name that the NodeTileStates aggregator
// below and downstream callers (page-data builder, www handlers)
// reference.
type NodeTileState = domain.NodeTileState

// NodeTileStates returns per-node tile rendering state for all nodes that have bins.
func NodeTileStates(db *sql.DB) (map[int64]NodeTileState, error) {
	rows, err := db.Query(`SELECT b.node_id,
		MAX(CASE WHEN b.manifest IS NOT NULL AND b.manifest_confirmed = true THEN 1 ELSE 0 END),
		MAX(CASE WHEN b.manifest IS NULL OR b.manifest_confirmed = false THEN 1 ELSE 0 END),
		MAX(CASE WHEN b.claimed_by IS NOT NULL THEN 1 ELSE 0 END),
		MAX(CASE WHEN b.status = 'staged' THEN 1 ELSE 0 END),
		MAX(CASE WHEN b.status IN ('maintenance', 'flagged', 'quality_hold') THEN 1 ELSE 0 END)
		FROM bins b
		WHERE b.node_id IS NOT NULL AND b.status != 'retired'
		GROUP BY b.node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := make(map[int64]NodeTileState)
	for rows.Next() {
		var nodeID int64
		var hasPayload, hasEmptyBin, claimed, staged, maintenance int
		if err := rows.Scan(&nodeID, &hasPayload, &hasEmptyBin, &claimed, &staged, &maintenance); err != nil {
			return nil, err
		}
		states[nodeID] = NodeTileState{
			HasPayload:  hasPayload == 1,
			HasEmptyBin: hasEmptyBin == 1,
			Claimed:     claimed == 1,
			Staged:      staged == 1,
			Maintenance: maintenance == 1,
		}
	}
	return states, rows.Err()
}

// MoveAndClearStaging relocates a bin and, when clearStaging is set, drops a
// stale staged status — both in one transaction. The staging clear is
// guarded (WHERE status='staged'), so it is a no-op on a non-staged bin and
// never flips an unrelated status, unlike the unguarded ReleaseStaged.
//
// A manual Move bypasses the arrival paths (ApplyArrival / recovery) that
// re-derive staging, so a bin staged at a lineside node would otherwise stay
// staged after relocating to a storage slot. Callers pass clearStaging=true
// only when the bin was staged and the destination is a storage slot.
func MoveAndClearStaging(db *sql.DB, binID, toNodeID int64, clearStaging bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE bins SET node_id=$1, updated_at=$3 WHERE id=$2 AND (node_id IS NULL OR node_id != $1)`, toNodeID, binID, clock.Now().UTC())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("bin %d is already at node %d", binID, toNodeID)
	}

	if clearStaging {
		if _, err := tx.Exec(`UPDATE bins SET status='available', staged_at=NULL, staged_expires_at=NULL, updated_at=$2 WHERE id=$1 AND status='staged'`, binID, clock.Now().UTC()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListAvailable returns bins with no payload (empty, available for loading).
//
// Empty-bin definition: COALESCE(b.payload_code, ”) = ”. Same NULL-safe
// form FindEmptyCompatible uses post-2026-04-27. The previous filter
// `(manifest IS NULL OR payload_code = ”)` was the same bug class as
// the FindEmptyCompatible bug fixed in 7c274ac/4337344: a bin with
// payload_code=NULL evaluates `payload_code = ”` to NULL (falsy in
// WHERE), but the OR-clause `manifest IS NULL` could rescue it. The
// COALESCE form is unambiguous about the NULL case.
//
// SetManifest and ClearManifest always set payload_code and manifest
// together, so under normal operation the two columns are correlated
// and the simpler payload_code-only filter produces identical results.
// In partial-write/legacy states where manifest is NULL but payload_code
// is non-empty, this filter correctly treats the bin as NOT available
// (it has a payload, even without a manifest blob).
func ListAvailable(db *sql.DB) ([]*Bin, error) {
	// Exclude bins parked at synthetic nodes (notably _TRANSIT). An empty
	// bin in transit can match COALESCE(payload_code, '') = '' but it is
	// NOT available — its physical location is mid-flight. Without this
	// filter, a claim-finding caller could pick an in-flight bin and
	// double-claim it, the exact failure mode the synthetic node was
	// introduced to prevent.
	rows, err := db.Query(fmt.Sprintf(`%s WHERE COALESCE(b.payload_code, '') = ''
		  AND COALESCE(n.is_synthetic, false) = false
		ORDER BY b.id`, BinJoinQuery))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}

// binExecer is satisfied by both *sql.DB and *sql.Tx, so the claim primitive can
// run standalone (Claim) or inside a caller's transaction (ClaimTx) — the latter
// lets the hard claim commit atomically with the reservation confirm.
type binExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// Claim marks a bin as claimed by an order to prevent double-dispatch.
// Fails if the bin is locked or already claimed by another order.
//
// Demoted-CAS guard: additionally requires this order's pending
// reservation to exist (placed by ClaimForDispatch's Acquire step) so
// a concurrent claimer without a reservation cannot steal the bin even
// if it passes the claimed_by check. claimed_by IS NULL stays as
// defense-in-depth for the mixed-binary rollback window.
//
// Owner-idempotent: the CAS is (claimed_by IS NULL OR claimed_by=$1),
// mirroring nodes.ClaimSlot. A re-claim by the SAME order succeeds instead of
// hitting 0 rows — so a claim that committed but whose reservation confirm did
// NOT (a transient DB error / core restart between the two writes) heals on the
// next retry rather than wedging codeClaimFailed forever. The EXISTS(pending)
// seatbelt is untouched: a claim without a live reservation still affects 0 rows.
func Claim(db *sql.DB, binID, orderID int64) error {
	return claimBin(db, binID, orderID)
}

// ClaimTx is Claim inside a caller-provided transaction, so the hard claim can
// commit atomically with the reservation pending→confirmed flip — closing the
// claim/confirm non-atomicity wedge. Identical demoted-CAS + owner-idempotent
// seatbelt as Claim.
func ClaimTx(tx *sql.Tx, binID, orderID int64) error {
	return claimBin(tx, binID, orderID)
}

func claimBin(db binExecer, binID, orderID int64) error {
	res, err := db.Exec(`UPDATE bins SET claimed_by=$1, updated_at=$3
		WHERE id=$2 AND locked=false AND (claimed_by IS NULL OR claimed_by=$1)
		  AND EXISTS (SELECT 1 FROM reservations WHERE order_id=$1 AND bin_id=$2 AND state='pending')`,
		orderID, binID, clock.Now().UTC())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bin %d is locked, already claimed, or does not exist", binID)
	}
	return nil
}

// FindEmptyCompatible finds an unclaimed, available bin with no manifest that is
// compatible with the given payload code (via payload_bin_types) at an enabled
// physical node. Prefers bins in the given zone, then falls back to any zone.
// FindEmptyCompatible looks for an unclaimed empty bin matching payloadCode,
// preferring preferZone. excludeNodeID > 0 omits bins parked at that node —
// pass the order's destination node so the caller never receives a same-node
// bin (would produce a fleet order with src == dst, which the fleet cancels
// and the kanban demand re-fires, producing an order spam loop). Pass 0 to
// disable exclusion. See SHINGO_TODO.md "Same-node retrieve" entry.
//
// Empty-bin definition (post-2026-04-27 fix): COALESCE(payload_code, ”) = ”.
// A bin with no payload_code is empty by definition — manifest is a
// derived/stale field that's unreliable when bins go through arrival
// transitions. The previous filter `(manifest IS NULL OR payload_code = ”)`
// was brittle around NULL-vs-empty-string (a bin with manifest=” and
// payload_code=NULL evaluated to `false OR NULL`, treated as falsy in
// WHERE, silently rejecting genuinely-empty bins). An earlier attempt
// added `manifest_confirmed = false` but that's too strict — it requires
// the operator to have explicitly cleared the confirmation, which doesn't
// happen on every arrival path. Plant test 2026-04-27 (order #462 stuck
// on 'awaiting inventory' with empties at SMN_002 / SMN_003 visible).
//
// Compatibility enforcement (post-2026-04-27 v2 fix): advisory.
// payload_bin_types is treated as an allow-list — rows say "this payload IS
// allowed in this bin type." Absence of rows for a payload means "no
// restrictions configured" → any bin works. This matches how every other
// reader treats the table (FindSourceFIFO, SetManifest both ignore it) and
// how the admin UI populates it. The previous form used
// hard INNER JOINs to payload_bin_types/payloads which eliminated all
// candidates when no rules existed — the cause of the 2026-04-27 starvation.
// FindEmptyCompatibleInGroup is FindEmptyCompatible scoped to descendants of
// a synthetic group node (NGRP / LANE). Used by planRetrieveEmpty when the
// edge sends a source-group constraint, so an empty-bin retrieve picks from
// the configured supermarket instead of any compatible empty in the system.
//
// Mirrors FindEmptyCompatible's availability gates (status='available',
// claimed_by IS NULL, locked=false, n.enabled, non-synthetic node, empty
// payload_code, payload-bin-type advisory) but adds a recursive descendant
// filter rooted at groupNodeID. excludeNodeID > 0 skips bins at that node
// (typically the destination — same-node retrieve guard).
//
// Bug origin: pre-fix planRetrieveEmpty called the unscoped FindEmptyCompatible
// regardless of order.SourceNode, so a multi-supermarket plant (Hopkinsville,
// 2026-05-14) saw retrieve_empty pull empties from whichever supermarket had
// the lowest bins.id — typically the empty-tote return area, not the
// configured Inbound supermarket.
// FindEmptyOfTypeInGroup returns an empty carrier of ONE bin type from within a
// node group. The typed twin of FindEmptyCompatibleInGroup, for a loader that
// has declared a carrier mix.
//
// THE TYPE IS IN THE QUERY, not applied to the result, and that is the whole
// point. Asking the group for any empty and then rejecting the wrong type would
// let one carrier of the wrong type mask every right-typed one behind it — the
// group returns its best candidate, not a list.
//
// No payload-compatibility clause: this is an empty carrier and the type IS the
// requirement. The payload rules exist to stop a part going into a carrier that
// cannot hold it; here a person has said which carrier they want.
//
// ── RECURSES THE SUBTREE, AND THE PLANT HAS A SECOND ANSWER ──────────────────
//
// DescendantsOf walks the whole subtree, so a NESTED GROUP's slots are in scope
// here: an empty parked inside a group inside this group is a candidate.
//
// The retrieve resolver answers the same question differently.
// binresolver.GroupResolver.scanForBestBin iterates DIRECT CHILDREN only and
// silently skips a synthetic child that is not a LANE — so for a LOADED carrier,
// a nested group is invisible.
//
// Same question, two live answers, split by what is being sourced. Predates the
// maintained-groups program and is not caused by it. NESTING SEMANTICS FOR
// SOURCING IS AN OPEN OWNER RULING: maintained groups sidestep it (refused at
// save time unless flat), every other group still lives with it, and whoever
// needs it decided should get the ruling rather than quietly change one side.
//
// FindEmptyCompatibleInGroup below carries the same property for the same reason.
func FindEmptyOfTypeInGroup(db *sql.DB, binTypeCode string, groupNodeID, excludeNodeID int64,
	asker reservations.DigAsker) (*Bin, error) {

	if binTypeCode == "" {
		return nil, sql.ErrNoRows
	}
	// NO FENCE HERE. A group-scoped need names its group explicitly, so the
	// question "may this asker source from that group" is a disposition the
	// finder answers with a cause (MG3-2), not something the query hides. The
	// dig exclusion is different — it is about which LANE inside the group is
	// contended, which only the query can see.
	a := &emptyQueryArgs{vals: []any{binTypeCode, groupNodeID, excludeNodeID}}
	q := nodetree.DescendantsOf(2) + " " + BinJoinQuery + EmptyOfTypeInGroupWhere +
		NotForeignDugArm(a.add(string(reservations.ModeDig)),
			a.add(asker.OrderID), a.add(asker.LaneOwner)) + AccessibleEmptyOrder
	return ScanBin(db.QueryRow(q, a.vals...))
}

// CountEmptyOfTypeInGroup counts what FindEmptyOfTypeInGroup can see.
//
// THE SAME WHERE, LITERALLY — EmptyOfTypeInGroupWhere, interpolated by both. The
// level keeper subtracts this count from the group's declared level, and if the
// count could see a carrier the finder cannot, the keeper would decide the group
// was stocked while every press pull queued for want of one. "The keeper counts
// six, the press finds none" is the failure this construction makes unspellable,
// and TestEmptyOfTypeInGroup_CountAndFindAgree asserts the equivalence directly
// (find != nil ⟺ count > 0) rather than trusting the shared text.
//
// No exclude-node argument: the finder takes one so a caller can avoid sourcing
// from the node it is delivering to, which is a question about one ASK. A level
// is a property of the whole group, so the count passes 0 — exclude nothing.
func CountEmptyOfTypeInGroup(db *sql.DB, binTypeCode string, groupNodeID int64) (int, error) {
	if binTypeCode == "" {
		// The finder returns ErrNoRows for a blank code rather than matching
		// everything; the count agrees by returning zero rather than the whole
		// group.
		return 0, nil
	}
	var n int
	err := db.QueryRow(
		nodetree.DescendantsOf(2)+" SELECT COUNT(*) "+BinFromClause+EmptyOfTypeInGroupWhere,
		binTypeCode, groupNodeID, 0).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count empty %s in group %d: %w", binTypeCode, groupNodeID, err)
	}
	return n, nil
}

// FindEmptyOfType returns an empty carrier of ONE bin type from anywhere,
// preferring the destination's zone. The typed twin of FindEmptyCompatible.
//
// ── THE ZONE PREFERENCE IS DELIBERATE, AND IT IS DEREK'S ────────────────────
//
// Round 1's census found this arm carrying no written justification anywhere
// and flagged it "ask, do not remove". Asked and answered (owner, 2026-08-17):
// Derek added plant-wide empty sharing on purpose — prefer the destination's
// zone, then take from ANYWHERE — to keep lines running and share empties
// rather than run pure-strict. A line that has run out of carriers is a line
// that has stopped, and a nearby empty in the wrong zone is worth more than a
// correctly-zoned one nobody can reach.
//
// So: PREFERENCE, never restriction. The zone query is tried first and the
// any-zone query answers when it finds nothing, which is what makes this
// sharing rather than fencing. That ordering is the whole semantic.
//
// AND IT IS WHY THE FENCE HAD TO BE ADDITIVE. Phase 3 does not narrow this arm;
// it adds one exception to it — maintained groups with strict_sourcing on —
// and everything else keeps sharing exactly as before. A blank EmptyFence
// renders neither the CTE nor the arm, so an unfenced plant runs Derek's query
// unchanged, byte for byte.
//
// The level keeper is this preference's first deliberate user: its top-off asks
// pass the destination group's zone, so a carrier near the group it is filling
// is preferred over one across the plant. That is also what made the
// self-sourcing defect easy to hit — the group's own positions share its zone,
// so preferZone ranked its own carriers FIRST — which is now rule (ii)'s job to
// prevent rather than a reason to distrust the preference.
func FindEmptyOfType(db *sql.DB, binTypeCode, preferZone string, excludeNodeID int64,
	fence EmptyFence, asker reservations.DigAsker) (*Bin, error) {

	if binTypeCode == "" {
		return nil, sql.ErrNoRows
	}
	// TWO PARAMETERS, NOT ONE STRUCT. The fence is POLICY — config-born, changes
	// at save time, keyed on supports and origin. The dig exclusion is PHYSICAL
	// CONTENTION — reservation-born, changes per dig, keyed on order identity. A
	// DigAsker field on EmptyFence would teach every later reader that fences are
	// dig-aware policy, which is the two-questions-one-spelling drift this whole
	// family exists to prevent. One extra parameter is cheaper than one lie in a
	// type name.
	build := func(withZone bool) (string, []any) {
		a := &emptyQueryArgs{}
		where := EmptyCarrierWhere + OfTypeArm(a.add(binTypeCode))
		if withZone {
			where += InZoneArm(a.add(preferZone))
		}
		where += ExcludeNodeArm(a.add(excludeNodeID))
		cte := ""
		if !fence.Empty() {
			cte = FencedNodesCTE(a.add(fence.ProcessNode), a.add(fence.OriginGroup))
			where += NotFencedArm()
		}
		where += NotForeignDugArm(a.add(string(reservations.ModeDig)),
			a.add(asker.OrderID), a.add(asker.LaneOwner))
		return cte + BinJoinQuery + where + AccessibleEmptyOrder, a.vals
	}

	if preferZone != "" {
		q, args := build(true)
		b, err := ScanBin(db.QueryRow(q, args...))
		if err == nil {
			return b, nil
		}
		// A REAL ERROR PROPAGATES; only none-found falls through to any-zone.
		//
		// This arm read `if err == nil && b != nil` until MG3-1 — it swallowed
		// EVERY error, so a zone query that could not run was indistinguishable
		// from a zone with no carriers, and the fallback quietly answered for it.
		// Its untyped twin has always propagated; two copies of one query with
		// different error handling is exactly the drift the family ends.
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	q, args := build(false)
	return ScanBin(db.QueryRow(q, args...))
}

func FindEmptyCompatibleInGroup(db *sql.DB, payloadCode string, groupNodeID, excludeNodeID int64,
	asker reservations.DigAsker) (*Bin, error) {

	a := &emptyQueryArgs{vals: []any{payloadCode, groupNodeID, excludeNodeID}}
	q := nodetree.DescendantsOf(2) + BinJoinQuery +
		EmptyCarrierWhere + InGroupArm() + ExcludeNodeArm(3) +
		NotForeignDugArm(a.add(string(reservations.ModeDig)),
			a.add(asker.OrderID), a.add(asker.LaneOwner)) +
		PayloadBinTypeAdvisoryClause + AccessibleEmptyOrder
	return ScanBin(db.QueryRow(q, a.vals...))
}

func FindEmptyCompatible(db *sql.DB, payloadCode, preferZone string, excludeNodeID int64,
	fence EmptyFence, asker reservations.DigAsker) (*Bin, error) {

	build := func(withZone bool) (string, []any) {
		a := &emptyQueryArgs{}
		// $1 is the payload for PayloadBinTypeAdvisoryClause, which names it
		// explicitly — so it is added first whether or not the zone arm follows.
		payloadP := a.add(payloadCode)
		where := EmptyCarrierWhere
		if withZone {
			where += InZoneArm(a.add(preferZone))
		}
		where += ExcludeNodeArm(a.add(excludeNodeID))
		cte := ""
		if !fence.Empty() {
			cte = FencedNodesCTE(a.add(fence.ProcessNode), a.add(fence.OriginGroup))
			where += NotFencedArm()
		}
		where += NotForeignDugArm(a.add(string(reservations.ModeDig)),
			a.add(asker.OrderID), a.add(asker.LaneOwner))
		_ = payloadP
		return cte + BinJoinQuery + where + PayloadBinTypeAdvisoryClause + AccessibleEmptyOrder, a.vals
	}

	if preferZone != "" {
		q, args := build(true)
		b, err := ScanBin(db.QueryRow(q, args...))
		if err == nil {
			return b, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	q, args := build(false)
	return ScanBin(db.QueryRow(q, args...))
}

// UpdateStatus sets the status on a bin.
func UpdateStatus(db *sql.DB, binID int64, status domain.BinStatus) error {
	_, err := db.Exec(`UPDATE bins SET status=$1, updated_at=$3 WHERE id=$2`, status, binID, clock.Now().UTC())
	return err
}

// Stage marks a bin as staged with expiry tracking.
// If expiresAt is nil, the bin is staged permanently (no auto-release).
func Stage(db *sql.DB, binID int64, expiresAt *time.Time) error {
	_, err := db.Exec(`UPDATE bins SET status='staged', staged_at=$3, staged_expires_at=$1, updated_at=$3 WHERE id=$2`,
		helpers.NullableTime(expiresAt), binID, clock.Now().UTC())
	return err
}

// ReleaseStaged clears the staged status on a single bin, setting it back to available.
func ReleaseStaged(db *sql.DB, binID int64) error {
	_, err := db.Exec(`UPDATE bins SET status='available', staged_at=NULL, staged_expires_at=NULL, updated_at=$2 WHERE id=$1`, binID, clock.Now().UTC())
	return err
}

// ReleaseExpiredStaged releases staged bins whose expiry has passed.
// Returns the number of bins released.
func ReleaseExpiredStaged(db *sql.DB) (int, error) {
	result, err := db.Exec(`UPDATE bins SET status='available', staged_at=NULL, staged_expires_at=NULL, updated_at=$1 WHERE status='staged' AND claimed_by IS NULL AND staged_expires_at IS NOT NULL AND staged_expires_at < $1`, clock.Now().UTC())
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// Lock prevents automated claiming/movement of a bin.
func Lock(db *sql.DB, binID int64, actor string) error {
	res, err := db.Exec(`UPDATE bins SET locked=true, locked_by=$1, locked_at=$3, updated_at=$3 WHERE id=$2 AND locked=false`,
		actor, binID, clock.Now().UTC())
	if err != nil {
		return fmt.Errorf("lock bin: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bin %d is already locked", binID)
	}
	return nil
}

// Unlock clears the lock on a bin.
func Unlock(db *sql.DB, binID int64) error {
	_, err := db.Exec(`UPDATE bins SET locked=false, locked_by='', locked_at=NULL, updated_at=$2 WHERE id=$1`, binID, clock.Now().UTC())
	return err
}

// MoveToTransit moves a bin to the synthetic _TRANSIT node identified by
// transitNodeID. Idempotent: if the bin is already at transitNodeID, the
// row is left unchanged and nil is returned. Distinct from Move (which
// errors on a no-op same-node move) because vendor pickup events legitimately
// retry. Does not touch claimed_by or status — see BinService.MoveToTransit
// for the design rationale.
func MoveToTransit(db *sql.DB, binID, transitNodeID int64) error {
	_, err := db.Exec(
		`UPDATE bins SET node_id=$1, updated_at=$3 WHERE id=$2 AND (node_id IS NULL OR node_id != $1)`,
		transitNodeID, binID, clock.Now().UTC())
	return err
}

// ListOnCarrierNodes returns every bin parked on a per-robot carrier node
// (`_ROBOT:<vehicle>`), with the vehicle id its node names.
//
// The prefix IS the query. A carrier node is created lazily per robot and there
// is no separate table of them; the name carries the robot, and a LIKE on a
// short prefix over the node table is cheaper than the bookkeeping to avoid it.
func ListOnCarrierNodes(db *sql.DB) ([]*Bin, error) {
	rows, err := db.Query(BinJoinQuery + ` WHERE ` + CarrierNodeSQL + ` ORDER BY b.id`)
	if err != nil {
		return nil, fmt.Errorf("list bins on carrier nodes: %w", err)
	}
	defer rows.Close()
	return scanBins(rows)
}

// MarkAnomalyWithNote stamps the anomaly and records where the robot carrying
// the bin last was, so the operator gets a map pin instead of a search.
//
// COALESCE on anomaly_at preserves an earlier stamp — the anomaly state is
// "still unresolved", not "happened at exactly this moment" — but the NOTE is
// overwritten, because a later sweep has newer robot telemetry than the first
// one did and stale coordinates are worse than none.
func MarkAnomalyWithNote(db *sql.DB, binID int64, note string) error {
	now := clock.Now().UTC()
	_, err := db.Exec(`UPDATE bins SET anomaly_at=COALESCE(anomaly_at, $2), anomaly_note=$3, updated_at=$2
		WHERE id=$1`, binID, now, note)
	return err
}

// MarkAnomaly stamps bins.anomaly_at = NOW(). Idempotent — repeated calls
// just bump the timestamp, since the anomaly state is "still unresolved"
// rather than "happened at exactly this moment."
func MarkAnomaly(db *sql.DB, binID int64) error {
	_, err := db.Exec(`UPDATE bins SET anomaly_at=$2, updated_at=$2 WHERE id=$1`, binID, clock.Now().UTC())
	return err
}

// ClearAnomaly clears bins.anomaly_at.
func ClearAnomaly(db *sql.DB, binID int64) error {
	_, err := db.Exec(`UPDATE bins SET anomaly_at=NULL, updated_at=$2 WHERE id=$1`, binID, clock.Now().UTC())
	return err
}

// RecoverToNode moves a bin to toNodeID and clears its anomaly flag in a
// single UPDATE — the persistence side of the operator's transit-anomaly
// recovery action. Caller validates that the destination is physical and
// empty.
func RecoverToNode(db *sql.DB, binID, toNodeID int64) error {
	_, err := db.Exec(
		`UPDATE bins SET node_id=$1, anomaly_at=NULL, updated_at=$3 WHERE id=$2`,
		toNodeID, binID, clock.Now().UTC())
	return err
}

// RecordCount updates UOP and records the count timestamp. Accepts
// any Execer (*sql.DB or *sql.Tx) so the service layer can wrap the
// count + bin_uop_ledger insert in one transaction. Item 19: cycle
// counts now write a bin_uop_ledger row (OpCycleCount) — see
// BinService.RecordCount.
// A SUCCESSFUL COUNT CLEARS anomaly_at, and that is the point of the flag.
//
// anomaly_at means "this carrier has had counts refused — cycle count it". It
// used to survive the count. So doing exactly what the flag asked for did not
// clear it, the mark accumulated permanently, and every carrier ended up
// flagged forever: at Hopkinsville on 2026-08-02, all ten bins carried it, seven
// of them counted AFTER being flagged and still flagged, the oldest since May.
// A signal that is set on everything and never cleared says nothing, so the
// "which carrier do I go count" answer the inventory page exists to give had
// quietly become "all of them".
//
// Cleared HERE, in the same statement as the count, rather than in the service
// layer: the two cannot then disagree, and every caller of RecordCount gets it
// without having to remember.
//
// Deliberately NOT a delta_epoch bump. A count corrects the number in the
// carrier; it does not end the carrier's load lifecycle, and bumping here would
// open a stale-epoch drop window against an Edge that has no way to learn the
// new value (see the epoch-resync gap: the "next bin-state refresh" the drop
// path names does not exist).
func RecordCount(db RecordCountExecer, binID int64, actualUOP int, actor string) error {
	_, err := db.Exec(`UPDATE bins SET uop_remaining=$1, last_counted_at=$4, last_counted_by=$2,
		anomaly_at=NULL, updated_at=$4 WHERE id=$3`,
		actualUOP, actor, binID, clock.Now().UTC())
	return err
}

// RecordCountExecer is the minimal interface satisfied by *sql.DB
// and *sql.Tx — same shape as audit.BinUOPExecer, kept package-local
// so callers don't need to import the audit package just to get the
// interface name.
type RecordCountExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// UnconfirmManifest resets the manifest confirmation flag.
func UnconfirmManifest(db *sql.DB, binID int64) error {
	_, err := db.Exec(`UPDATE bins SET manifest_confirmed=false, updated_at=$2 WHERE id=$1`, binID, clock.Now().UTC())
	return err
}

// HasNotes returns a map indicating which bins have audit log entries.
func HasNotes(db *sql.DB, binIDs []int64) (map[int64]bool, error) {
	result := make(map[int64]bool)
	if len(binIDs) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(binIDs))
	args := make([]any, len(binIDs))
	for i, id := range binIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	query := fmt.Sprintf(`SELECT DISTINCT entity_id FROM audit_log WHERE entity_type='bin' AND entity_id IN (%s)`,
		strings.Join(placeholders, ","))
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			result[id] = true
		}
	}
	return result, rows.Err()
}
