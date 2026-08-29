package reservations

import (
	"database/sql"
	"fmt"
	"slices"
)

// Mode is a mouth reservation's work direction — the reservations.mode column
// (v51). Only mouth rows carry a mode; bin and slot rows leave it NULL.
//
//	inbound  — the owner drops into the lane (its in-lane blocks are dropoffs)
//	outbound — the owner picks from the lane (its in-lane blocks are pickups)
//	dig      — a reshuffle compound working the lane; ALWAYS exclusive (it does
//	           both directions at once), so a dig admits no other holder and no
//	           other holder admits a dig.
type Mode string

const (
	ModeInbound  Mode = "inbound"
	ModeOutbound Mode = "outbound"
	ModeDig      Mode = "dig"
)

// ── WHAT A mode='dig' ROW IS: ONE SPELLING, TWO QUESTIONS (§R.101, law 3) ──
//
// ModeDig answers "is this lane held exclusively". Until §R.101 that was the
// same question as "is an EXCAVATION running here", because a dig was the only
// thing that ever took the mode. §R.101 generalized every demand's SOURCE hold
// from outbound to dig — a demand owns the lane it resolves onto until the bin
// leaves by its mover — and the two questions came apart without the readers
// being told.
//
// Nothing about exclusivity changed and nothing here changes it: admitMouth's
// rule is still that a dig excludes every other owner and is excluded by every
// other owner, whichever kind it is. What splits is only what the row is CALLED
// when something reports it — and the readers that report it name an excavation:
// the lane-hold cause classifier, and admission's refusal. A wait that says an
// excavation is running when a plain retrieve is sourcing sends the next
// engineer to look for a dig that was never planned. It is the §17.5/§17.8
// family — not an alarm that fails to fire, an alarm with the wrong name on it.
//
// So the kind is read off reserved_by, which every writer already stamps and
// which had ZERO readers before this. Law 15 on our own terms: the fact is
// recorded, so it is read rather than re-derived.
const (
	// ByExcavation — a reshuffle compound working this lane. LaneLock.TryLockFor,
	// which is the one door every production dig comes through.
	ByExcavation = "lanelock"
	// BySourceLock — §R.101's source hold: an ordinary demand owns the lane it
	// resolved onto. Exclusive, and not an excavation. Written by the lane gate.
	BySourceLock = "lanegate"
	// ByDigHandoff — the outbound row a finished dig hands to the bin's collector
	// (HandOffLaneToPicker). Never a dig row, so never a kind this predicate has
	// to rule on; named here so the three tags are in one place.
	ByDigHandoff = "dighandoff"
)

// IsExcavation reports whether a mouth row's reserved_by tag says a reshuffle
// owns the lane, as opposed to §R.101's source lock.
//
// UNTAGGED ROWS ARE NOT EXCAVATIONS, and that is the safe direction for every
// caller this has: they all decide what to CALL a refusal that has already been
// made elsewhere. Guessing "excavation" on an unknown tag is the wrong-name
// failure this exists to stop; guessing "source lock" costs a reader one extra
// question and never moves a robot.
func IsExcavation(reservedBy string) bool { return reservedBy == ByExcavation }

// MouthHold is one active mouth row on a lane: the owning order, its mode, and
// the tag saying which kind of hold it is (see IsExcavation).
type MouthHold struct {
	OrderID    int64
	Mode       Mode
	ReservedBy string
}

// AcquireLanes takes a mode-tagged mouth hold on each lane for owner, all-or-
// nothing. It is the only writer of mouth rows. DORMANT in P0 — exercised by
// tests only; the dispatch chokepoints wire it in at P4.
//
// The whole call runs in ONE transaction. Lanes are sorted by id and each is
// guarded by a transaction-scoped advisory lock BEFORE its rows are read — two
// acquirers contending for the same lane therefore serialize on the lock, and
// because every lane a call touches is locked in id order, the multi-lane case
// cannot deadlock against another acquirer. Sorting the ids is precisely why the
// design asks for it: the locks are held simultaneously inside the one tx.
//
// Admission, per lane, against the OTHER owners' active mouth rows:
//
//	free (no other rows)                     → admit (insert a row)
//	held, all same mode, neither side dig    → admit, share (insert a row)
//	held different-mode, or dig on either side → ErrReservationConflict
//
// Idempotent: a lane this owner already holds in this mode is skipped (no second
// row). All-or-nothing is structural — a conflict on any lane rolls the whole tx
// back, so there are no partial takes and no advisory lock survives (the durable
// state is the ROWS; the advisory lock is only the serializer and never outlives
// the tx).
//
// ── EVERY LANE YIELDS A ROW, INCLUDING SINGLE-SLOT ONES ───────────────────
//
// It used to read, here and at LaneLock: "Depth-1 lanes are EXEMPT: a
// single-slot lane is already serialized by its slot reservation, so no mouth
// row is taken for it", justified at the lock by "Digs never touch depth-1
// lanes: nothing can be buried in a lane with one slot."
//
// Both sentences are quoted rather than deleted because the second one is
// FALSE, and it is false in a way that reads true. It is true of the lane a dig
// EXCAVATES — nothing can be buried behind one slot — and false of the lane a
// dig PARKS A BLOCKER IN, which is the other end of every leg it writes. The
// lane-stress seed says so in its own words: LS_S1, LS_S2 and LS_S3 are depth 1
// and the file calls them "the cheapest shuffle destinations in the group".
//
// The first sentence is not false, it is narrow. A slot reservation serializes
// who may have THAT SLOT; it says nothing about who is driving in the corridor
// to reach it, which is what a mouth row is for. Those are the same fact only
// while a lane has exactly one thing in it to contend over, and a robot parking
// a blocker meets a robot coming out with a bin regardless of how many slots
// there are.
//
// The exemption was inert while nothing acquired mouth rows on unmarked lanes.
// It stops being inert the moment acquisition universalizes, and a single-slot
// lane full of shuffle traffic is the last place to leave a hole.
//
// A mouth row is inserted 'confirmed': it confirms at fleet-create ("robot
// committed"), with no pending phase — a mouth hold has no paired hard-claim step
// the way a bin or slot reservation does.
//
// On ErrReservationConflict the caller requeues (the queue-and-replay disposition
// every dispatch path already implements); any other error is a transient DB
// failure.
func AcquireLanes(db *sql.DB, owner int64, mode Mode, reservedBy string, laneIDs ...int64) error {
	return AcquireLanesFor(db, owner, mode, Anyone, reservedBy, laneIDs...)
}

// AcquireLanesFor is AcquireLanes for a hold taken ON BEHALF OF another order:
// beneficiary's own mouth rows do not refuse it.
//
// ── WHY A HOLD HAS A BENEFICIARY AT ALL ───────────────────────────────────
//
// A dig is not work anyone wants for its own sake. It is raised to rescue a
// demand that cannot move, and that demand is frequently holding the very lane
// the dig has to take: a gate-staged order keeps its outbound row until it
// places, and the wall it is staged behind is exactly what the dig is for.
//
// Owner-blind, that is a two-cycle with itself. X waits for a dig; the dig is
// refused because X waits. Measured on the lane-stress rig 2026-08-10: LS_C5
// held one outbound row belonging to a gate-staged order, 16,947 heal parents
// were created and cancelled against it, and no dig ever started. The pre-check
// (DigAdmissible) stopped the order churn; it could not start the dig, because
// the acquire was answering the same owner-blind question one layer down.
//
// So the rescue is told who it is rescuing, and the rescued order's own hold
// stops being an obstacle to it. Nobody ELSE's does — a dig still excludes
// every other owner, which is the whole content of ModeDig.
//
// ── THE TWO SHAPES, AND WHY ONE OF THEM UPGRADES ──────────────────────────
//
// The beneficiary is either a DIFFERENT order from the hold's owner or the SAME
// one, and both occur today:
//
//	SERVICE DIG (owner ≠ beneficiary) — the gate-dweller heal mints a synthetic
//	parent, because {staged → reshuffling} is not a legal transition. The
//	dweller's outbound row is skipped and the parent's dig row is inserted
//	beside it. Two rows, two owners, and the end of the dig already expects
//	exactly this: HandOffLaneToPicker's "THE PICKER MAY ALREADY HOLD THIS LANE"
//	arm is written for the same pairing, from the other side.
//
//	PLAIN DIG (owner == beneficiary) — planBuriedReshuffle re-parents the demand
//	onto its own excavation, so the order asking for the dig row is the order
//	already holding an outbound one. Inserting a second row would put one owner
//	on one lane twice, which is the incoherent state admitMouth exists to
//	refuse. Its row is UPGRADED to dig instead: one owner, one lane, one row,
//	and the strongest mode wins. That is the mirror of HandOffLaneToPicker,
//	which walks the same row back down to outbound when the excavation ends.
//
// ── THE ASYMMETRY THIS DOES NOT CLOSE ─────────────────────────────────────
//
// The row does not RECORD its beneficiary, so the exemption only runs in one
// direction: the dig may ignore the dweller, but the dweller re-asking for its
// own outbound hold would still be refused by the dig raised to rescue it.
//
// IT IS REACHABLE NOW, and this paragraph used to say the opposite: "Not
// reachable today, and named rather than left to be discovered: the mouth holds
// are taken just before the fleet commit, a gate-staged order has already made
// that commit, and resolveOrderLaneHolds yields nothing at all on an unmarked
// lane. It becomes reachable the moment mouth acquisition universalizes (§R.96
// stage 2)."
//
// That moment has been and gone — §R.96 stage 2 landed, and resolveOrderLaneHolds
// now yields holds on every lane, marked or not (see its own header, which
// retracts the same claim). So the asymmetry is live rather than pending: a
// dweller re-asking for its own outbound hold is refused by the dig raised to
// rescue it. The durable form of the fix is still a beneficiary column on the
// row, not a second exemption written somewhere else.
func AcquireLanesFor(db *sql.DB, owner int64, mode Mode, beneficiary DigAsker, reservedBy string, laneIDs ...int64) error {
	lanes := sortedUniqueLanes(laneIDs)
	if len(lanes) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("reservations acquire-lanes: begin: %w", err)
	}
	defer tx.Rollback() // no-op once committed; the rollback path is what makes it all-or-nothing

	for _, lane := range lanes {
		// Transaction-scoped advisory lock on the lane id: serializes concurrent
		// acquirers of THIS lane. xact-scoped, so a crash cannot strand it.
		if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, lane); err != nil {
			return fmt.Errorf("reservations acquire-lanes: lock lane %d: %w", lane, err)
		}
		verdict, err := admitMouth(tx, lane, owner, mode, beneficiary)
		if err != nil {
			return err
		}
		switch verdict {
		case admitIdempotent:
			// ── THE TAG IS PROMOTED, NEVER DEMOTED ────────────────────────
			//
			// The row is already this owner's in this mode, so there is nothing
			// to do about the HOLD. There can still be something to do about
			// what it is CALLED, and before §R.101 there never was: a demand
			// that dug the lane it was holding arrived here holding an OUTBOUND
			// row, so the verdict was admitUpgrade and the tag was rewritten by
			// that arm's UPDATE. §R.101 made the source hold a dig too, which
			// turned that upgrade into this no-op and froze the tag at whatever
			// wrote first.
			//
			// MEASURED, not reasoned: a demand takes its source hold (lanegate),
			// planBuriedReshuffle re-parents it onto its own excavation and
			// TryLockFor acquires with lanelock — and the row still read
			// lanegate. A genuine excavation wearing the source-lock tag is
			// exactly the wrong-name failure IsExcavation exists to prevent,
			// pointing the wrong way.
			//
			// Promotion only. A dig row that has been an excavation stays one
			// until it is released or handed off, so a later gate acquire by the
			// same owner cannot talk it back down — the compound legs of a live
			// dig do re-enter through the lane gate, and letting them demote the
			// parent's tag would be the same bug with the clock turned round.
			if IsExcavation(reservedBy) {
				if _, err := tx.Exec(
					`UPDATE reservations SET reserved_by=$1
					  WHERE order_id=$2 AND resource_kind='mouth' AND node_id=$3
					    AND state IN ('pending','confirmed')
					    AND COALESCE(reserved_by, '') <> $1`,
					reservedBy, owner, lane,
				); err != nil {
					return fmt.Errorf("reservations acquire-lanes: promote lane %d: %w", lane, err)
				}
			}
			continue // owner already holds this lane in this mode
		case admitConflict:
			return ErrReservationConflict
		case admitUpgrade:
			// The owner holds this lane in a weaker mode and is now digging it for
			// itself. UPDATE rather than INSERT: one owner on one lane is one row.
			if _, err := tx.Exec(
				`UPDATE reservations SET mode=$1, reserved_by=$2
				  WHERE order_id=$3 AND resource_kind='mouth' AND node_id=$4
				    AND state IN ('pending','confirmed')`,
				string(mode), reservedBy, owner, lane,
			); err != nil {
				return fmt.Errorf("reservations acquire-lanes: upgrade lane %d: %w", lane, err)
			}
		case admitFresh:
			if _, err := tx.Exec(
				`INSERT INTO reservations (order_id, resource_kind, node_id, state, reserved_by, mode)
				 VALUES ($1, 'mouth', $2, 'confirmed', $3, $4)`,
				owner, lane, reservedBy, string(mode),
			); err != nil {
				return fmt.Errorf("reservations acquire-lanes: insert lane %d: %w", lane, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reservations acquire-lanes: commit: %w", err)
	}
	return nil
}

type mouthVerdict int

const (
	admitFresh mouthVerdict = iota
	admitIdempotent
	admitUpgrade
	admitConflict
)

// admitMouth reads the lane's active mouth rows and applies the admission rule
// for (owner, mode), on behalf of beneficiary. It must be called with the lane's
// advisory lock held. Pass Anyone for a hold taken for nobody in particular,
// which is the owner-blind rule this had before beneficiaries existed.
func admitMouth(tx *sql.Tx, laneID, owner int64, mode Mode, beneficiary DigAsker) (mouthVerdict, error) {
	holders, err := activeMouthRows(tx, laneID)
	if err != nil {
		return admitConflict, err
	}
	ownerHolds := false
	ownerHoldsOtherMode := Mode("")
	for _, h := range holders {
		if h.OrderID == owner {
			if h.Mode == mode {
				ownerHolds = true
				continue // our own row — idempotent
			}
			// An order is one mode per lane (§2). DECIDED AFTER THE LOOP, not
			// here: a foreign conflict is the stronger refusal and must win
			// regardless of which row activeMouthRows happens to return first.
			// Returning from inside the loop made the answer depend on the id
			// ordering of unrelated orders.
			ownerHoldsOtherMode = h.Mode
			continue
		}
		if beneficiary.Owns(h.OrderID) {
			// The order this hold is being taken FOR. Its own row is not an
			// obstacle to its own rescue — see AcquireLanesFor's header for the
			// two-cycle this arm exists to break.
			continue
		}
		// A different owner holds the lane. dig excludes everyone (either side);
		// otherwise only an exact same-mode share is admitted.
		if mode == ModeDig || h.Mode == ModeDig || h.Mode != mode {
			return admitConflict, nil
		}
	}
	if ownerHoldsOtherMode != "" {
		if mode == ModeDig && beneficiary.Owns(owner) {
			// The owner is digging the lane FOR ITSELF while holding it in a
			// weaker mode — the plain re-parented shape. Upgrade the row it has.
			return admitUpgrade, nil
		}
		// Still a caller bug — surface it loudly rather than insert an
		// incoherent second row.
		return admitConflict, fmt.Errorf(
			"reservations acquire-lanes: order %d already holds lane %d as %s, cannot also hold as %s",
			owner, laneID, ownerHoldsOtherMode, mode)
	}
	if ownerHolds {
		return admitIdempotent, nil
	}
	return admitFresh, nil
}

// DigAdmissible reports whether a dig could take laneID right now — the
// CHEAP PRE-CHECK for the same question admitMouth answers authoritatively.
//
// ── WHY IT EXISTS: TWO READERS ASKED DIFFERENT QUESTIONS ──────────────────
//
// The heal-dig path pre-checked with LaneLock.IsLocked, which asks "does a DIG
// hold this lane" (DigOwner, mode='dig' only). It then created the dig's parent
// order and called TryLock, which is AcquireLanes(ModeDig) and refuses on ANY
// other owner's mouth row — as TryLock's own doc says: "already held — by
// another dig, OR BY AN ORDINARY ORDER'S MOUTH HOLD."
//
// So on a lane held by an ordinary order the pre-check said GO and the acquire
// said NO, every single time, and between them sat a durable order INSERT. The
// loser was cancelled and the next event tried again.
//
// MEASURED, lane-stress rig 2026-08-10: LS_C5 held exactly one row — an
// `outbound` mouth hold belonging to a gate-staged order that legitimately keeps
// it until it places. 16,947 heal parents were created and cancelled, ZERO digs
// ever started, and the plant stopped doing anything else. The cancellation
// reason said "another dig took lane LS_C5 first", naming a dig that did not
// exist — the misdiagnosis was the tell.
//
// THE RULE HERE IS admitMouth's, NOT A SECOND COPY OF IT: a dig excludes every
// other owner, so any active mouth row from anyone else refuses it. Read off
// activeMouthRows, the same rows admitMouth reads.
//
// IT IS A PRE-CHECK AND NOTHING MORE. The arbiter is still AcquireLanes, under
// the lane's advisory lock; this only stops the caller paying for an order row
// to be told something it could have asked first. A read error reports NOT
// admissible — fail closed, same direction as IsLocked.
//
// ── AND IT ASKS ON BEHALF OF SOMEBODY ─────────────────────────────────────
//
// It used to be `len(holders) == 0`, which made the rescued order's own hold
// refuse its rescue — the other half of the 16,947 story, and the half that
// survived the pre-check being added. The exemption is AcquireLanesFor's, read
// off the same rows, so the cheap question and the authoritative one still
// answer alike; see that header for what a beneficiary is and why.
//
// Pass Anyone for a dig serving nobody in particular: Owns matches no row, so
// the rule collapses to the len()==0 it was.
func DigAdmissible(q Queryer, laneID int64, beneficiary DigAsker) (bool, error) {
	holders, err := activeMouthRows(q, laneID)
	if err != nil {
		return false, err
	}
	for _, h := range holders {
		if !beneficiary.Owns(h.OrderID) {
			return false, nil
		}
	}
	return true, nil
}

// activeMouthRows returns the active (pending or confirmed) mouth holds on laneID.
func activeMouthRows(q Queryer, laneID int64) ([]MouthHold, error) {
	rows, err := q.Query(
		`SELECT order_id, mode, COALESCE(reserved_by, '') FROM reservations
		 WHERE resource_kind='mouth' AND node_id=$1 AND state IN ('pending','confirmed')
		 ORDER BY order_id`,
		laneID,
	)
	if err != nil {
		return nil, fmt.Errorf("reservations active-mouth-rows: %w", err)
	}
	defer rows.Close()
	var out []MouthHold
	for rows.Next() {
		var h MouthHold
		var mode sql.NullString
		if err := rows.Scan(&h.OrderID, &mode, &h.ReservedBy); err != nil {
			return nil, fmt.Errorf("reservations active-mouth-rows scan: %w", err)
		}
		h.Mode = Mode(mode.String)
		out = append(out, h)
	}
	return out, rows.Err()
}

// ActiveMouthRows returns the active mouth holds on laneID — the friction-surface
// read (who holds the lane, in what mode). Safe to call outside the acquire tx.
func ActiveMouthRows(q Queryer, laneID int64) ([]MouthHold, error) {
	return activeMouthRows(q, laneID)
}

// ReleaseLane deletes owner's mouth row on laneID — the per-visit release (§4).
// Owner-scoped by construction: the order_id predicate means a release aimed with
// the wrong owner deletes nothing, so the G3 foreign-release class is structurally
// dead for mouth rows. Idempotent (no row → no-op).
func ReleaseLane(db Execer, owner, laneID int64) error {
	_, err := db.Exec(
		`DELETE FROM reservations WHERE order_id=$1 AND resource_kind='mouth' AND node_id=$2`,
		owner, laneID,
	)
	if err != nil {
		return fmt.Errorf("reservations release-lane: %w", err)
	}
	return nil
}

// ReleaseLaneHandoff is ReleaseLane for the PER-BLOCK early handoff (§4) — it
// deletes owner's mouth row on laneID unless that row is a dig claim.
//
// The handoff and a dig claim have different lifetimes, and conflating them cost
// the dig its row on every reshuffle. A plain order's hold is per-visit: an
// outbound hold exists so nothing else enters while the bin is coming out, and
// once the bin has cleared the lane the hold has done its job. A dig's hold
// spans the WHOLE reshuffle — several legs, several pickups and dropoffs — and
// the first of those pickups is not the end of anything.
//
// It was one call. Because laneOwnerFor routes a child's block progress to the
// compound PARENT (deliberately — children never own rows), the first unbury
// leg's pickup arrived here owned by the parent, matched the parent's dig row,
// and deleted it. Every later leg then ran with no durable claim on the lane.
// Nothing caught it: memory was still the grant authority, the resolver skips
// dig-locked lanes from memory, and CheckDivergence had no production caller.
//
// The mode predicate is in the SQL rather than a read-then-delete in Go, so
// there is no window between deciding and deleting.
//
// LaneLock.Unlock keeps using ReleaseLane, which has no mode predicate: ending
// the dig IS its job, and that is the one caller allowed to drop the claim.
func ReleaseLaneHandoff(db Execer, owner, laneID int64) error {
	_, err := db.Exec(
		`DELETE FROM reservations
		 WHERE order_id=$1 AND resource_kind='mouth' AND node_id=$2
		   AND COALESCE(mode, '') <> $3`,
		owner, laneID, string(ModeDig),
	)
	if err != nil {
		return fmt.Errorf("reservations release-lane-handoff: %w", err)
	}
	return nil
}

// ReleaseLanesByOwner deletes all of owner's mouth rows (any mode, any lane) —
// the row dual of LaneLock.UnlockByOwner's cleanup path. Idempotent.
//
// IT RETURNS THE LANES IT FREED, and that is the whole reason it is a query
// rather than an Exec. Releasing a lane is a lane-CLEARING event: an order
// dwelling at that lane's mark is waiting on exactly this, and the release is
// the last thing that happens, so every event that could have re-asked has
// already fired while the lane was still held. A caller that cannot name what it
// just freed cannot wake anybody, and the dwellers wait for unrelated traffic —
// which on a quiet lane never comes.
//
// DELETE ... RETURNING rather than a read followed by a delete, on the same
// reasoning ConsumePendingLaneExtensionByBin states: one statement means the set
// released and the set reported cannot come apart.
//
// The ids are node ids of LANES (mouth rows are keyed on the lane node), in no
// particular order. A caller with nothing to wake ignores them.
func ReleaseLanesByOwner(db Queryer, owner int64) ([]int64, error) {
	rows, err := db.Query(
		`DELETE FROM reservations WHERE order_id=$1 AND resource_kind='mouth' RETURNING node_id`, owner)
	if err != nil {
		return nil, fmt.Errorf("reservations release-lanes-by-owner: %w", err)
	}
	defer rows.Close()

	var freed []int64
	for rows.Next() {
		var laneID int64
		if err := rows.Scan(&laneID); err != nil {
			return nil, fmt.Errorf("reservations release-lanes-by-owner scan: %w", err)
		}
		freed = append(freed, laneID)
	}
	return freed, rows.Err()
}

// LanesHeldByOwner returns the lane node ids this order currently holds a mouth
// row on, in no particular order.
//
// It is the SNAPSHOT half of a teardown, and it exists because the release is
// not the only thing that drops these rows: TerminalizeOrder deletes an order's
// reservations in the same transaction as its status write, so a compound whose
// parent has already been failed or confirmed owns nothing by the time its
// unlock runs. A caller that needs to know which lanes it just freed — in order
// to wake whoever was waiting behind them — must ask BEFORE the disposition,
// not after.
func LanesHeldByOwner(q Queryer, owner int64) ([]int64, error) {
	rows, err := q.Query(
		`SELECT node_id FROM reservations WHERE order_id=$1 AND resource_kind='mouth'`, owner)
	if err != nil {
		return nil, fmt.Errorf("reservations lanes-held-by-owner: %w", err)
	}
	defer rows.Close()

	var held []int64
	for rows.Next() {
		var laneID int64
		if err := rows.Scan(&laneID); err != nil {
			return nil, fmt.Errorf("reservations lanes-held-by-owner scan: %w", err)
		}
		held = append(held, laneID)
	}
	return held, rows.Err()
}

// DigHold is one active dig mouth row: the lane node and its owning order.
type DigHold struct {
	LaneID  int64
	OrderID int64
}

// ListDigHolds returns every active dig mouth hold across all lanes — the bulk
// read the LaneLock repopulates its in-memory cache from at boot (making the rows
// the restart-durable authority), and the read the divergence check compares
// against.
func ListDigHolds(q Queryer) ([]DigHold, error) {
	rows, err := q.Query(
		`SELECT node_id, order_id FROM reservations
		 WHERE resource_kind='mouth' AND mode=$1 AND state IN ('pending','confirmed')
		 ORDER BY node_id`, string(ModeDig))
	if err != nil {
		return nil, fmt.Errorf("reservations list-dig-holds: %w", err)
	}
	defer rows.Close()
	var out []DigHold
	for rows.Next() {
		var h DigHold
		if err := rows.Scan(&h.LaneID, &h.OrderID); err != nil {
			return nil, fmt.Errorf("reservations list-dig-holds scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// LanesHeldByHandoff returns the distinct lane nodes carrying an active mouth
// row tagged ByDigHandoff — the population the lane liveness floor visits.
//
// KEYED ON THE ROW, NOT ON WHO IS WAITING, and that is the whole reason it
// exists rather than the floor reusing its own waiter set. The wedge a stranded
// handoff produces is a corridor that refuses every inbound comer: the orders
// queued behind it are `sourcing` demands the resolver turned away, not gate
// dwellers or held legs, so a waiter-derived lane set does not contain the lane
// and the floor never looks at it. A row-derived one always does.
//
// On a healthy plant this returns zero rows and the sweep costs one indexed
// query (idx_reservations_kind_node).
func LanesHeldByHandoff(q Queryer) ([]int64, error) {
	rows, err := q.Query(
		`SELECT DISTINCT node_id FROM reservations
		 WHERE resource_kind='mouth' AND reserved_by=$1 AND state IN ('pending','confirmed')
		 ORDER BY node_id`, ByDigHandoff)
	if err != nil {
		return nil, fmt.Errorf("reservations lanes-held-by-handoff: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var laneID int64
		if err := rows.Scan(&laneID); err != nil {
			return nil, fmt.Errorf("reservations lanes-held-by-handoff scan: %w", err)
		}
		out = append(out, laneID)
	}
	return out, rows.Err()
}

// HandOff is what HandOffLaneToPicker did. Three outcomes, and only ONE of them
// leaves the lane free for anybody else to act on.
//
// ── WHY THIS IS NOT A BOOL, AND WHAT THE BOOL COST ────────────────────────
//
// It was a bool, and false meant two incompatible things: "the picker is not the
// collector" and "there was no dig row to take". The one caller reads false as
// permission to RELEASE, and the release it performs — LaneLock.Unlock →
// ReleaseLane — matches on owner and resource_kind only. It is MODE-BLIND
// (deliberately: ending the dig is its job), so it deletes whatever mouth row
// that owner has on that lane.
//
// Put those together with a caller that runs OUTSIDE the lane evaluator's mutex
// — which maybeReleaseDigOnLastBlockerOut does by design, because waking a lane
// from inside it is a self-deadlock — and two blocker-out events on one lane can
// both read the same dig owner and both walk. The first converts. The second's
// DELETE matches zero rows, reports "not handed", and the caller releases: the
// outbound row the first one just created, for the same owner, is deleted. The
// bin the excavation uncovered is then standing at an open mouth with nothing
// holding the corridor, which is the precise window this whole exception exists
// to cover.
//
// The rescue is not more locking. It is that "there was nothing to convert" is
// not the same answer as "this lane is yours to release", and the type now says
// so — decided inside the advisory-locked transaction, so it reports a settled
// fact and not a snapshot.
type HandOff int

const (
	// HandOffNoDigRow: no dig row was there to convert. Somebody else — a
	// concurrent conversion, or a concurrent release — already resolved this
	// lane, under this same advisory lock, and did their own waking. The caller
	// must do nothing at all: not convert, not release, not wake.
	//
	// It is the zero value on purpose, so an error return carries the answer that
	// touches nothing.
	HandOffNoDigRow HandOff = iota
	// HandOffConverted: the corridor is now the picker's OUTBOUND hold, and the
	// picker is a live order whose per-visit release and terminalization both end
	// it. The lane is not the caller's to release.
	HandOffConverted
	// HandOffPickerNotCollector: the dig row is gone, but the picker holds this
	// lane INBOUND — it is dropping into the lane, not picking from it, so it is
	// not the bin's collector and this was never its hold to take. Nothing was
	// converted and nothing must be released: that inbound row is the picker's
	// own, and a mode-blind release would take it.
	//
	// §R.101 makes this unreachable in principle — one owner, one lane, ONE row,
	// and an acquire upgrades rather than doubles — so it is a defensive arm. The
	// wake it does not perform costs latency, not correctness: every lane wait has
	// a periodic floor behind its event releaser.
	HandOffPickerNotCollector
)

// HandOffLaneToPicker converts the dig hold on laneID into picker's OUTBOUND
// hold, in one transaction under the lane's advisory lock. It reports which of
// the three HandOff outcomes happened.
//
// ── WHY THE MODE CHANGES, AND WHY THAT IS THE WHOLE MECHANISM ─────────────
//
// An excavation ends with a bin standing at an open lane mouth and the demand it
// was dug for not yet dispatched. The dig's own need for the corridor is over —
// everything after this is somebody else's pickup — but the SLOTS IN FRONT of
// that bin are now the cheapest shuffle candidates in the group, and the next
// order to want one re-buries the bin the excavation was run to expose.
//
// What has to be excluded is therefore precisely a DROP into that lane. That is
// what an outbound hold says, in the vocabulary that already exists: outbound
// excludes inbound and dig, and shares with other outbound holders — who can
// only take bins OUT, and so cannot re-bury anything.
//
// It also means the picker's own dispatch needs no special case. AcquireLanes
// for its source lane asks for outbound, finds its own row, and is idempotent.
// A dig-mode row would have made that call an ERROR — admitMouth refuses one
// owner two modes on one lane — so handing the corridor over as a dig would have
// blocked the very order it was held for.
//
// ── AND WHY IT ENDS ON ITS OWN ────────────────────────────────────────────
//
// Because the new owner is a LIVE ORDER. Its per-visit release drops the row
// when its bin clears the lane, and its terminalization drops the row whatever
// happens to it. There is no state left behind that outlives an order, which is
// what a hold parked on a finished dig was.
//
// ── AND THAT DEPENDS ENTIRELY ON THE CALLER'S GATE ────────────────────────
//
// The per-visit release fires "when its bin clears the lane", so it is worth
// nothing to a picker whose bin cleared the lane BEFORE this row existed. Such a
// row has one releaser left, terminalization — and a holder waiting at its
// station for an evac partner that needs to drop into this very lane never
// reaches it. That was the leak: a corridor shut with nothing inside it, both
// sides of a swap waiting on each other.
//
// So this function is safe because handOffDugLane calls it only for a holder its
// gate 3 rules NOT COMMITTED — pre-dispatch or mid-dig, minus `faulted`, which
// releases. Do not widen the caller without re-reading this paragraph: the
// sentence above is a conclusion about the caller, not a property of the row.
//
// (It said "a holder that has NOT yet dispatched", which was never what the gate
// tested: `faulted` and the terminal statuses all arrive here post-dispatch. Same
// defect class as the three comments 9f8ea225 corrected — a sentence describing a
// gate that does not exist.)
//
// ── THE THREE ANSWERS ARE THREE DIFFERENT INSTRUCTIONS ────────────────────
//
// This returned a bool, and the bool conflated the two ways of not converting.
// "The dig row was already gone" came back as plain false, and the one caller
// reads false as PERMISSION TO RELEASE — which is wrong in a way that undoes the
// work of whoever got here first. See HandOff below.
func HandOffLaneToPicker(db *sql.DB, laneID, digOwner, picker int64, reservedBy string) (HandOff, error) {
	tx, err := db.Begin()
	if err != nil {
		return HandOffNoDigRow, fmt.Errorf("reservations hand-off-lane: begin: %w", err)
	}
	defer tx.Rollback() // no-op once committed

	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, laneID); err != nil {
		return HandOffNoDigRow, fmt.Errorf("reservations hand-off-lane: lock lane %d: %w", laneID, err)
	}
	res, err := tx.Exec(
		`DELETE FROM reservations
		  WHERE order_id=$1 AND resource_kind='mouth' AND node_id=$2 AND mode=$3`,
		digOwner, laneID, string(ModeDig))
	if err != nil {
		return HandOffNoDigRow, fmt.Errorf("reservations hand-off-lane: drop dig row: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// SOMEBODY ELSE ALREADY RESOLVED THIS LANE, and this is decided UNDER THE
		// ADVISORY LOCK, so it is a settled fact rather than a snapshot: whoever
		// held the lock before us has committed. Not converted, and emphatically
		// not the caller's to release.
		return HandOffNoDigRow, tx.Commit()
	}

	// THE PICKER MAY ALREADY HOLD THIS LANE — it can be gate-staged at the mouth
	// with its own outbound row while the dig works behind it. Inserting a second
	// row would put one owner on one lane twice, which is the incoherent state
	// admitMouth exists to refuse. Reuse whatever is there.
	holders, err := activeMouthRows(tx, laneID)
	if err != nil {
		return HandOffNoDigRow, err
	}
	for _, h := range holders {
		if h.OrderID != picker {
			continue
		}
		if h.Mode == ModeOutbound {
			return HandOffConverted, tx.Commit() // already holds it the right way round
		}
		// It holds the lane INBOUND: it is dropping into this lane, not picking
		// from it, so it is not the bin's collector and this is not its hold to
		// take. The dig row is gone; the inbound row is the picker's own and stays.
		return HandOffPickerNotCollector, tx.Commit()
	}
	if _, err := tx.Exec(
		`INSERT INTO reservations (order_id, resource_kind, node_id, state, reserved_by, mode)
		 VALUES ($1, 'mouth', $2, 'confirmed', $3, $4)`,
		picker, laneID, reservedBy, string(ModeOutbound)); err != nil {
		return HandOffNoDigRow, fmt.Errorf("reservations hand-off-lane: insert outbound row: %w", err)
	}
	return HandOffConverted, tx.Commit()
}

// ── Hold B: who is INSIDE the lane ────────────────────────────────────────
//
// A reshuffle's CLAIM on a lane and a robot's PRESENCE in it are different facts
// with different lifetimes, and they used to share one row.
//
// The claim is a mouth row, mode='dig', owned by the compound PARENT, and it
// spans the whole reshuffle — many legs, many pickups and dropoffs. Presence is
// owned by ONE CHILD: it starts when Core dispatches that child into the lane
// and ends when the child places its bin and leaves. Because there was only one
// row for both, the only way to guarantee a single robot inside a lane was to
// let a single child exist at a time — which is what capped a reshuffle at one
// robot no matter how much work it had.
//
// PRESENCE IS DERIVED FROM CORE'S OWN DISPATCH DECISION, not observed. Core
// cannot see a robot enter a lane: RobotStatus has no dispatch consumer, and
// every available signal is lagging — the earliest is a block completing AT a
// slot, which is already inside. It does not need to see it. Nothing enters a
// lane that Core did not send there, so "dispatched into L" is the entry moment
// and it is knowable at the instant it becomes true. This is the RDS axiom
// applied: Core owns prevention, not observation.
//
// The row is keyed on the node the occupancy is OF, which is the lane today.
// Under a block shape that unit may be an aisle or a column instead; that is a
// change of which node id goes in the row, not a change of anything here.

// AcquireOccupancy records that orderID is inside nodeID. Idempotent: an order
// already inside is not recorded twice.
//
// THIS ROW NOW ARBITRATES — read carefully, because the mechanism did not change
// and the sentence it replaces did not become false by itself. What changed is
// the caller: the compound scheduler's sibling-in-flight guard is gone, and
// dispatch.admit reads these rows to decide whether a leg may enter. So a lane's at-most-one-inside now RESTS on this table, where it used
// to rest on there being one child at a time and this being a witness to it.
//
// The SQL is unchanged and still permissive: the NOT EXISTS is keyed on
// (order_id, node_id), so it de-dupes ONE order's repeat takes and says nothing
// about a DIFFERENT order on the same node. Arbitration is the caller's read,
// not this write — which is exactly why the write's error now has to reach the
// caller (dispatch.TakeLaneOccupancy). A take that fails silently leaves the
// next leg's read seeing an empty lane.
//
// Making this the arbiter itself would mean a partial unique index on
// (node_id) WHERE resource_kind='occupancy' and an INSERT that reports the
// conflict. That is a real option and not what is here today; do not read the
// idempotent-insert shape as a decision against it.
//
// ── THE TAKE REPORTS WHETHER IT TOOK ─────────────────────────────────────
//
// The return says which of the two idempotent outcomes happened: took=true,
// this call INSERTED the row; took=false, the row was already there and the
// INSERT matched nothing. The gated append's failure rollback is the consumer:
// it may give back a row THIS CALL took and no other, because a dweller's
// release re-takes the source-lane row its own dispatch took — a no-op here —
// and dropping that one on an append failure declares an occupied corridor
// empty to the next entrant (the phantom-absence twin of §R.54's phantom row).
//
// The insert's own RowsAffected is the ONLY non-racy spelling of that answer.
// A caller that reads the table first and decides from what it saw races every
// other writer: "absent" read, row inserted by someone in between, insert here
// no-ops — and the caller releases a row it did not take. The INSERT .. WHERE
// NOT EXISTS decides and reports in one statement, so the answer is a property
// of the row rather than of the interleaving.
func AcquireOccupancy(db Execer, owner, nodeID int64) (bool, error) {
	res, err := db.Exec(
		`INSERT INTO reservations (order_id, resource_kind, node_id, state, reserved_by)
		 SELECT $1, `+OccupancyKindSQL()+`, $2, 'confirmed', $3
		 WHERE NOT EXISTS (
		   SELECT 1 FROM reservations
		   WHERE order_id=$1 AND resource_kind=`+OccupancyKindSQL()+` AND node_id=$2
		     AND state IN ('pending','confirmed')
		 )`,
		owner, nodeID, "lane-occupancy",
	)
	if err != nil {
		return false, fmt.Errorf("reservations acquire-occupancy: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reservations acquire-occupancy rows: %w", err)
	}
	return n > 0, nil
}

// ReleaseOccupancy records that orderID has left nodeID. Owner-scoped and
// idempotent, so a release that arrives twice — or for an order that never
// entered — is a no-op.
func ReleaseOccupancy(db Execer, owner, nodeID int64) error {
	_, err := db.Exec(
		`DELETE FROM reservations WHERE order_id=$1 AND resource_kind=`+OccupancyKindSQL()+` AND node_id=$2`,
		owner, nodeID,
	)
	if err != nil {
		return fmt.Errorf("reservations release-occupancy: %w", err)
	}
	return nil
}

// ReleaseAllOccupancy drops every occupancy row an order holds — the
// terminalization path. A child that fails, is cancelled, or is skipped is not
// inside any lane, however it got there.
func ReleaseAllOccupancy(db Execer, owner int64) error {
	_, err := db.Exec(
		`DELETE FROM reservations WHERE order_id=$1 AND resource_kind=`+OccupancyKindSQL(), owner)
	if err != nil {
		return fmt.Errorf("reservations release-all-occupancy: %w", err)
	}
	return nil
}

// ReleaseOccupancyForLane drops owner's occupancy on ONE lane — the row dual of
// leaving that corridor, as opposed to ReleaseAllOccupancy's "this order is
// finished with every lane it was in".
//
// IT EXISTS BECAUSE PRESENCE IS PER-LANE AND THE RELEASE WAS NOT. An order can
// legitimately be inside two corridors across its plan, and exiting one says
// nothing about the other; releasing both because a robot drove out of one is
// the same class of error as holding both because it drove into one. The mouth
// hold has had a per-lane release since §4 (ReleaseLane); occupancy only had the
// order-wide one, which is why the exit path could not use it.
func ReleaseOccupancyForLane(db Execer, owner, laneID int64) error {
	_, err := db.Exec(
		`DELETE FROM reservations
		 WHERE order_id=$1 AND resource_kind=`+OccupancyKindSQL()+` AND node_id=$2`, owner, laneID)
	if err != nil {
		return fmt.Errorf("reservations release-occupancy-for-lane: %w", err)
	}
	return nil
}

// OccupantsOf returns the orders currently inside nodeID, ascending. The read
// behind "is anyone in this lane" and behind the at-most-one-inside assertion.
func OccupantsOf(q Queryer, nodeID int64) ([]int64, error) {
	rows, err := q.Query(
		`SELECT order_id FROM reservations
		 WHERE resource_kind=`+OccupancyKindSQL()+` AND node_id=$1 AND state IN ('pending','confirmed')
		 ORDER BY order_id`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("reservations occupants-of: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("reservations occupants-of scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ExcavationOwner returns the order whose EXCAVATION holds laneID, or 0 if the
// lane's dig rows are all §R.101 source locks (or there are none).
//
// THE SIBLING OF DigHoldOwner, NOT A REPLACEMENT FOR IT, and the pairing is the
// whole of the split. DigHoldOwner answers the EXCLUSIVITY question — may
// anything else be in this corridor — and every keep-out decision keeps asking
// it, unchanged, because §R.101's source lock excludes exactly as hard as a
// reshuffle does. This answers the NAMING question — is what holds it an
// excavation — and only the sites that put a word in front of a human ask it.
//
// Deciding admission on this instead would let a second order into a lane a
// demand owns, which is §R.101 reversed. The two are one row read two ways, and
// the difference between them is a sentence, never a robot.
func ExcavationOwner(q Queryer, laneID int64) (int64, error) {
	holders, err := activeMouthRows(q, laneID)
	if err != nil {
		return 0, err
	}
	for _, h := range holders {
		if h.Mode == ModeDig && IsExcavation(h.ReservedBy) {
			return h.OrderID, nil
		}
	}
	return 0, nil
}

// DigHoldOwner returns the order holding a dig on laneID, or 0 if none — the
// single-lane read behind LaneLock.IsLocked / LockedBy.
//
// One row, not a loop: the callers that use it (the lane gate's park decision,
// the expose-mode extension's ownership check) each ask about ONE lane they
// already have in hand. The scanning callers do not come through here at all —
// they filter dig-held lanes out of their candidate query instead.
func DigHoldOwner(q Queryer, laneID int64) (int64, error) {
	rows, err := q.Query(
		`SELECT order_id FROM reservations
		 WHERE resource_kind='mouth' AND node_id=$1 AND mode=$2 AND state IN ('pending','confirmed')
		 ORDER BY order_id
		 LIMIT 1`, laneID, string(ModeDig))
	if err != nil {
		return 0, fmt.Errorf("reservations dig-hold-owner: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, rows.Err() // no dig on this lane
	}
	var owner int64
	if err := rows.Scan(&owner); err != nil {
		return 0, fmt.Errorf("reservations dig-hold-owner scan: %w", err)
	}
	return owner, rows.Err()
}

// laneDepth1Exempt WAS HERE. It counted a lane's real child slots and returned
// true for 0 or 1, and AcquireLanes skipped those lanes entirely.
//
// Its own words: "a lane with 0 or 1 slot cannot trap a robot, so it needs no
// mouth gate." Trapping was never what the mouth row was for — it arbitrates
// the CORRIDOR, and a single-slot lane has one. See the note on AcquireLanes for
// the sentence that made this look safe and why it is not.
//
// Deleted rather than left unused: an exemption with no caller is an exemption
// waiting for one.

// sortedUniqueLanes returns the lane ids ascending with duplicates removed — the
// deadlock-free lock order AcquireLanes takes them in.
func sortedUniqueLanes(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	cp := append([]int64(nil), ids...)
	slices.Sort(cp)
	return slices.Compact(cp)
}
