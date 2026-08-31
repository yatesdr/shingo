// Package reservations manages Phase-1 bin reservations for the
// plan/apply → reservation-sourcing refactor.
//
// Acquire → Confirm → Release is the per-dispatch lifecycle.
// ReapOrphaned reclaims rows whose owning order is terminal/gone (owner-liveness backstop).
//
// RELEASE IS A HARD DELETE. Release/ReleaseByOrder/ReleaseByBin/ReleaseByNode and
// ReapOrphaned all DELETE rows — a reservation never transitions to a terminal
// state. So every row on disk is 'pending' or 'confirmed', which is why the partial
// unique indexes' `state IN ('pending','confirmed')` predicate matches every row
// (kept future-proof, and since v44 also pinned by a CHECK).
//
// Phase-0 stub bodies are replaced here. The v43 migration must have run
// before any Acquire call: the partial unique index uq_reservations_bin_active
// is what makes Acquire exactly-one-winner.
//
// REAPING IS OWNER-LIVENESS, NOT AGE. An earlier reaper used expires_at
// (a short ~60s TTL) as a proxy for "orphaned", valid only because Acquire→Confirm was
// milliseconds then. Once reserve-at-plan-time landed, an order in 'sourcing' legitimately
// holds its reservations for minutes-to-hours (or days) while it waits for a source to
// appear — so age is no longer a proxy for orphaned. ReapOrphaned keys on the OWNING
// ORDER's liveness instead: a hold is reclaimed only when its order is terminal or gone,
// never on age (demand is operator-driven and never evaporates). The expires_at column is
// still stamped at Acquire (it is NOT NULL) but is no longer read by any reaper — vestigial
// pending a schema drop. Do NOT re-introduce an age-based reap.
package reservations

import (
	"database/sql"
	"fmt"

	"shingo/protocol"
	"shingo/protocol/clock"
)

// Execer is the minimal interface all functions in this package need.
// Both *sql.DB and *sql.Tx satisfy it, as does any store interface that
// exposes Exec (e.g. BinManifestStore). This avoids forcing callers to
// thread *sql.DB through every layer; they can pass whatever DB handle
// they already hold.
type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// Queryer is the read counterpart of Execer, for the SELECT-returning helpers.
// Both *sql.DB and *sql.Tx satisfy it.
type Queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// RowExecer is what acquire needs: it writes AND reads back, in one statement.
// The dig-exclusion arm has to be evaluated against the same snapshot as the
// insert it guards (a separate pre-read is the race it exists to close), and the
// two outcomes — dug by another order, or lost to a conflict — have to come back
// distinguishable. That is a QueryRow, not an Exec.
//
// *sql.DB, *sql.Tx and service.BinManifestStore all satisfy it.
type RowExecer interface {
	QueryRow(query string, args ...any) *sql.Row
}

// Compile-time proof of what the doc comments promise — a signature drift on the
// concrete DB handles becomes a build error, not a runtime one.
var (
	_ Execer  = (*sql.DB)(nil)
	_ Execer  = (*sql.Tx)(nil)
	_ Queryer = (*sql.DB)(nil)
	_ Queryer = (*sql.Tx)(nil)
)

// State is a reservation's lifecycle state. No longer free-text: v44 adds a CHECK
// pinning the column to these values, so a typo can no longer silently escape the
// partial unique index.
type State string

const (
	StatePending   State = "pending"
	StateConfirmed State = "confirmed"
)

// Kind is the resource a reservation covers — the resource_kind column's domain
// (v44): a bin, a storage slot, or a lane mouth. The mouth kind is the lane-seam
// substrate (v51 mode column + the AcquireLanes family in mouth.go).
type Kind string

const (
	KindBin  Kind = "bin"
	KindSlot Kind = "slot"
	// KindMouth is a lane-mouth hold: one row per (lane, order), node_id = the
	// LANE node, carrying a mode (mouth.go). Unlike bin/slot rows it has NO unique
	// index — same-mode sharing means several active mouth rows on one lane is
	// legal. A mouth hold is per-visit and released by its own owner, so the G3
	// foreign-release class is structurally dead for it.
	//
	// Steal (Rule 2) must NEVER select a mouth row. The helper that comment asked
	// for is BinAndSlotKindsSQL below; compose it rather than typing the pair.
	KindMouth Kind = "mouth"
	// KindOccupancy is the PRESENCE WITNESS: it records a robot physically inside
	// a corridor. Excluded from every strength decision, every wants-per-resource
	// count, and every sweep scope — it reports, it does not promise or protect.
	// It lives in the reservations table as a storage convenience only, which is
	// why naming it here closes the taxonomy rather than forcing it into one of
	// the drawers above.
	//
	// The schema CHECK has carried the value since v76 (store/migrations.go) and
	// the writers spelled it as a raw literal, so the one place a typo could not
	// be caught was the only place it mattered — an occupancy row written under a
	// misspelled kind is a robot in a lane that no admission query can see.
	KindOccupancy Kind = "occupancy"
)

// OccupancyKindSQL renders the presence witness's resource_kind as a quoted SQL
// literal, so a query composes the value from the constant instead of typing it
// out beside it. Splice it into `resource_kind = ` + OccupancyKindSQL().
//
// A function rather than an exported string because it is spliced into SQL: a
// call site reads as a fragment being composed, which is the same shape as
// ActiveStateSQL and OnTheBooksSQL beside it.
func OccupancyKindSQL() string { return "'" + string(KindOccupancy) + "'" }

// BinAndSlotKindsSQL returns 'bin','slot' — the two kinds on which a hold has a
// STRENGTH, for `resource_kind IN (` + BinAndSlotKindsSQL() + `)`.
//
// The other two kinds are not weaker holds, they are different things, and that
// is why a query about strength must not sweep them up. A mouth row is a
// per-visit hold or a cordon and is inserted 'confirmed' with no pending phase —
// there is no such thing as demoting one. An occupancy row is a presence
// witness: it reports where a robot IS, so a query that changed its state would
// be editing a measurement.
//
// KindMouth's doc asked for this helper by name ("when one is written it is
// pinned to resource_kind IN ('bin','slot')") for the steal predicate that does
// not exist yet. The fleet-refusal demote is the first caller.
func BinAndSlotKindsSQL() string { return "'" + string(KindBin) + "','" + string(KindSlot) + "'" }

// Ref is the kind-agnostic identity of a reserved resource: a bin (Kind=bin,
// ID=bins.id) or a slot (Kind=slot, ID=nodes.id). Every primitive keys on a Ref —
// the seed of a future kind-agnostic Claim/Handle aggregate. The exactly-one-of
// CHECK + per-kind partial indexes make (Kind, ID) a row's identity. Callers build
// one with BinRef/SlotRef so a call site reads its kind at a glance.
type Ref struct {
	Kind Kind
	ID   int64
}

// BinRef / SlotRef construct the two Refs in use (bin and slot).
func BinRef(binID int64) Ref   { return Ref{Kind: KindBin, ID: binID} }
func SlotRef(nodeID int64) Ref { return Ref{Kind: KindSlot, ID: nodeID} }

// Reservation is one row of the order's held reservations, as returned by
// ListByOrder. Exactly one of BinID/NodeID is set, per Kind (the other is 0).
type Reservation struct {
	Kind   Kind
	BinID  int64 // bins.id for a bin reservation; 0 for slot/mouth
	NodeID int64 // nodes.id for a slot/mouth reservation; 0 for bin
	State  State
}

// ErrReservationConflict is returned by Acquire/AcquireSlot when another order
// already holds an active (pending or confirmed) reservation on the requested
// resource (bin or slot). Callers should treat this as a transient race: the
// losing order requeues and the scanner retries on the next tick.
var ErrReservationConflict = fmt.Errorf("reservations: resource already reserved (race)")

// ErrLaneDugByAnother is returned when the bin being claimed stands in a lane a
// FOREIGN dig holds. It is a separate sentinel from ErrReservationConflict for
// the reason this package separates every other refusal: THE RELEASER DIFFERS. A
// conflict ends when the winning order lets go of the bin; this ends when the
// dig lets go of the LANE, and a caller told "already reserved" about a bin
// nobody has reserved is sent looking for an owner that does not exist.
//
// Both are transient and both retry on the next tick, so no caller has to branch
// on it today. It exists so the log line and any future caller can be honest.
var ErrLaneDugByAnother = fmt.Errorf("reservations: bin stands in a lane held by another order's dig")

// Acquire inserts a bin reservation row for (orderID, binID) in state "pending".
// reservedBy is the actor tag for forensics. (The former reason + expiresAt params
// are gone in v44 — reason was always "", and expires_at is retired as a reaping
// key: reaping keys on the owner's liveness, not age.)
//
// Returns ErrReservationConflict when an active (pending or confirmed)
// reservation already exists on binID. The unique index uq_reservations_bin_active
// is keyed on bin_id ALONE, so this fires even when THIS SAME order already holds
// the bin — re-Acquiring your own hold conflicts on its own row. Callers that
// retry across ticks (the plan-time reserve/reconcile) MUST therefore
// load-held-first and skip Acquire for bins they already hold, or they will
// report their own held bins as "missing" every tick. A conflict on a bin the
// caller does NOT already hold is a genuine race lost to another order.
// Returns any other non-nil error for transient DB failures.
//
// laneOwner is the order that would hold a lane row on orderID's behalf — its
// compound parent, or orderID itself when it has none, the same pair DigAsker
// carries and laneOwnerFor resolves. It is a REQUIRED parameter rather than a
// derived one because the exemption it buys is what keeps a dig's own child legs
// able to claim inside the lane their parent locked; defaulting it to orderID
// would refuse them and wedge every excavation. Callers with no compound context
// pass orderID twice, which is the correct answer for them.
func Acquire(db RowExecer, orderID, laneOwner, binID int64, reservedBy string) error {
	return acquire(db, orderID, laneOwner, BinRef(binID), reservedBy)
}

// AcquireSlot is Acquire for a destination slot: a pending slot reservation on
// nodeID. Conflict via uq_reservations_slot_active (one active slot row per node).
// Occupancy is NOT consulted here — a slot that physically holds a bin is still
// reservable; the NOT EXISTS(bins) check lives at confirm/claim time only.
//
// ── NOT GUARDED BY THE DIG ARM, AND THAT IS A CONCLUSION, NOT AN OMISSION ──
//
// It was guarded, briefly, on the argument that a slot inside a dug lane is the
// same collision as a bin: ResolveStore filters dug lanes at FIND time
// (resolveStoreLKND / resolveStoreDPTH both walk ListChildNodesUnlocked) and the
// claim did not re-check, so the same TOCTOU exists. The mechanism reasoning was
// right and the CONCLUSION was wrong, because the two resources do not mean the
// same thing:
//
//   - A BIN CLAIM takes ownership of a bin the dig may have to MOVE. Only the
//     claimant can move it, and the dig's own lock is what refuses the claimant
//     entry. That is a cycle with no releaser — the 122-minute wedge.
//   - A SLOT RESERVATION reserves FUTURE SPACE. The holder places when the dig
//     finishes and admits it, so THE DIG FINISHING IS THE RELEASER. There is no
//     cycle, and queueing on a dug lane is what the gate is for.
//
// Three tests said so immediately, and they are the record:
// TestStoreBurst_FiveAtOneDugLane_DivertToDistinctSlots is five stores queueing
// at one dug lane and each taking a distinct slot — by name, the behaviour the
// guard broke. TestSplice_FenceHoldsOnASplicedPlan and
// TestSplice_ComplexStoreDoesItsPreLaneWorkThenDwells are the §R.104 acceptance
// shape: pre-position, dwell while the lane is dug, splice the tail on release.
//
// So the half-guard is CORRECT rather than merely unfinished, and the asymmetry
// is a statement about bins and slots rather than about how far the work got.
func AcquireSlot(db RowExecer, orderID, nodeID int64, reservedBy string) error {
	return acquire(db, orderID, orderID, SlotRef(nodeID), reservedBy)
}

// acquire inserts a pending reservation for ref. Kind-agnostic: the resource_kind
// is a parameter and the target column is routed by it in SQL (no per-kind Go
// branching). ON CONFLICT DO NOTHING catches EITHER per-kind partial unique index,
// so a lost race on a bin or a slot both surface as ErrReservationConflict.
//
// The bare ON CONFLICT DO NOTHING (no conflict target) is LOAD-BEARING: only
// uq_reservations_{bin,slot}_active can fire here today, so a 0-rows result means
// "reserved by someone active". A future author adding any OTHER unique constraint
// to this table would have its violations silently folded into a false
// ErrReservationConflict — handle such a constraint's conflict deliberately.
// ── THE FIFTH READER OF THE DIG-LOCK QUESTION, AND THE ONLY NON-FILTER ────
//
// (dig_exclusion.go carries the accounting; this is entry five.)
//
// Readers 2 and 4 (ListChildNodesUnlocked, NotForeignDugArm) hide dug lanes at
// FIND time. A find-time filter cannot close this, because between the find and
// the claim the lock can land — and did:
//
//	20:00:27.505  order 43 resolves; Lane_03 carries no dig row yet
//	20:00:27.583  order 42 takes MOUTH/dig on Lane_03
//	20:00:27.610  order 43 is granted bin 21, which stands in Lane_03
//
// 43 then held the one bin 42's dig had to relocate, in a lane 43 could never be
// admitted to (lane-dig-active). 42 waited for 43 to move it; 42's own lock was
// what stopped 43 moving. Both robots stood 122 sim-minutes and BRKT — the only
// payload behind order 42 — raised nothing for the rest of the run.
//
// THE ACQUIRE IS THE SERIALIZATION POINT, so the question is asked here where it
// can be answered against the same snapshot as the insert rather than 100ms
// earlier in a different statement.
//
// ── WHAT THIS DOES NOT CLOSE, STATED ──────────────────────────────────────
//
// A mouth row has no unique index (KindMouth's doc says why), so a dig lock and
// a foreign claim committing in genuinely overlapping transactions can still miss
// each other — neither sees the other's uncommitted row. That window is a
// statement wide instead of the 105ms measured above, and when it is lost the
// outcome is exactly today's behaviour: the dig's plan-time pre-check finds the
// blocker claimed and parks under dig-blocker-claimed.
//
// A SYMMETRIC CHECK DOES NOT CLOSE IT, and this comment said it did. Teaching
// AcquireLanesFor to refuse a dig over a lane holding a foreign claim leaves the
// hole exactly where it was, because under READ COMMITTED neither transaction can
// see the other's uncommitted row:
//
//	Tx A (dig lock)   reads bins-in-lane -> nothing visible -> INSERT -> COMMIT
//	Tx B (bin claim)  reads mouth rows   -> nothing visible -> INSERT -> COMMIT
//
// Both check, both see nothing, both succeed. Two parties looking for each other
// is not serialization. What closes it is a SHARED SERIALIZATION POINT — the
// cheapest being pg_advisory_xact_lock keyed on the lane id, taken by this
// function and by AcquireLanesFor, which auto-releases at transaction end and
// lives in its own lock namespace so it cannot tangle with row-lock ordering.
// SELECT ... FOR UPDATE on the lane node and SERIALIZABLE isolation both work too
// and cost more.
//
// IT IS NOT BUILT, ON PURPOSE. The 105ms window above is measured and routine;
// this one is a statement wide and has never been observed. Building a
// serialization primitive for a population no run has produced is the shape this
// house refuses. reconciliation.ListAnomalies counts it instead
// (bin_claimed_in_foreign_dug_lane), and with this arm in place ANY non-zero
// reading is by definition a mutual miss — which is the only way left to reach
// the state. One confirmed occurrence is what earns the advisory lock.
func acquire(db RowExecer, orderID, laneOwner int64, ref Ref, reservedBy string) error {
	var dugByAnother bool
	var inserted int
	err := db.QueryRow(
		// BINS ONLY. `$2 = 'bin'` is the whole scope statement; see AcquireSlot for
		// the round trip that put it back.
		`WITH dug AS (
		   SELECT EXISTS (
		     SELECT 1
		       FROM bins b
		       JOIN nodes n ON n.id = b.node_id
		       JOIN reservations dig_hold
		         ON dig_hold.resource_kind = 'mouth'
		        AND dig_hold.node_id = n.parent_id
		      WHERE $2 = 'bin' AND b.id = $3
		        AND `+ActiveStateSQL("dig_hold.")+`
		        AND dig_hold.mode = $6
		        AND `+DigExclusionSQL("dig_hold.order_id", 1, 7)+`
		   ) AS blocked
		 ),
		 ins AS (
		   INSERT INTO reservations (order_id, resource_kind, bin_id, node_id, state, reserved_by, created_at)
		   SELECT $1, $2,
		     CASE WHEN $2 = 'bin' THEN $3::bigint END,
		     CASE WHEN $2 <> 'bin' THEN $3::bigint END,
		     'pending', $4, $5
		   FROM dug WHERE NOT dug.blocked
		   ON CONFLICT DO NOTHING
		   RETURNING 1
		 )
		 SELECT (SELECT blocked FROM dug), (SELECT count(*) FROM ins)`,
		orderID, string(ref.Kind), ref.ID, reservedBy, clock.Now().UTC(),
		string(ModeDig), laneOwner,
	).Scan(&dugByAnother, &inserted)
	if err != nil {
		return fmt.Errorf("reservations acquire: %w", err)
	}
	if dugByAnother {
		return ErrLaneDugByAnother
	}
	if inserted == 0 {
		// ON CONFLICT DO NOTHING suppressed the insert — another order already
		// holds an active reservation on this resource via its per-kind index.
		// Still the ONLY other reason the insert can write nothing: the dig arm
		// above is reported separately, and no other unique constraint on this
		// table can fire here (see the note on the bare ON CONFLICT below).
		return ErrReservationConflict
	}
	return nil
}

// Confirm transitions the (orderID, binID) bin reservation from "pending" to
// "confirmed", recording that the physical bin claim succeeded. A no-op if the
// row is already confirmed (idempotent for retry safety).
func Confirm(db Execer, orderID, binID int64) error {
	return confirm(db, orderID, BinRef(binID))
}

// ConfirmSlot is Confirm for a slot reservation (pending → confirmed on nodeID).
func ConfirmSlot(db Execer, orderID, nodeID int64) error {
	return confirm(db, orderID, SlotRef(nodeID))
}

// confirm flips ref's pending row to confirmed. resource_kind=$2 scopes the match
// to one kind, so COALESCE(bin_id, node_id)=$3 reads the correct target column
// (the other is NULL for that kind) — kind-agnostic, no branching.
func confirm(db Execer, orderID int64, ref Ref) error {
	_, err := db.Exec(
		`UPDATE reservations SET state='confirmed'
		 WHERE order_id=$1 AND resource_kind=$2 AND state='pending'
		   AND COALESCE(bin_id, node_id)=$3`,
		orderID, string(ref.Kind), ref.ID,
	)
	if err != nil {
		return fmt.Errorf("reservations confirm: %w", err)
	}
	return nil
}

// DemoteConfirmedByOrder flips every one of an order's CONFIRMED bin and slot
// rows back to pending — the paper half of a fleet refusal.
//
// It is the inverse of confirm above, and it lives beside it for the reason the
// blocking drift guard exists: a reservation's state is spelled in this package
// and nowhere else. A caller that wrote `SET state='pending'` into its own query
// would be the second definition of what a hold's state means, on the day the
// tier work changes it.
//
// BIN AND SLOT ONLY. A mouth row is inserted 'confirmed' with no pending phase,
// so demoting one would invent a state it has never had; an occupancy row is a
// presence witness, and rewriting it would be editing a measurement rather than
// a promise. See BinAndSlotKindsSQL.
//
// The row is DEMOTED, NOT DELETED, and that distinction is the whole ruling:
// the bin stays spoken for, the re-dispatch's confirm has the pending row it
// requires, and nothing else can take the resource without outranking the order
// that still holds it. Idempotent — a second call finds nothing confirmed.
func DemoteConfirmedByOrder(db Execer, orderID int64) error {
	_, err := db.Exec(
		`UPDATE reservations SET state=$2
		 WHERE order_id=$1 AND state=$3
		   AND resource_kind IN (`+BinAndSlotKindsSQL()+`)`,
		orderID, string(StatePending), string(StateConfirmed))
	if err != nil {
		return fmt.Errorf("reservations demote-confirmed-by-order: %w", err)
	}
	return nil
}

// Release deletes the (orderID, binID) bin reservation, freeing the bin for future
// reservations. Safe to call even when no row exists (idempotent).
func Release(db Execer, orderID, binID int64) error {
	return release(db, orderID, BinRef(binID))
}

// ReleaseSlot is Release for a slot reservation (deletes the order's row on nodeID).
func ReleaseSlot(db Execer, orderID, nodeID int64) error {
	return release(db, orderID, SlotRef(nodeID))
}

// release deletes ref's row for the order. Same kind-scoped COALESCE match as confirm.
func release(db Execer, orderID int64, ref Ref) error {
	_, err := db.Exec(
		`DELETE FROM reservations
		 WHERE order_id=$1 AND resource_kind=$2 AND COALESCE(bin_id, node_id)=$3`,
		orderID, string(ref.Kind), ref.ID,
	)
	if err != nil {
		return fmt.Errorf("reservations release: %w", err)
	}
	return nil
}

// ReleaseByOrder deletes all reservations for the given order, both pending
// and confirmed. Used by teardown paths (TerminalizeOrder) to ensure no holds
// outlive the order that owns them.
func ReleaseByOrder(db Execer, orderID int64) error {
	_, err := db.Exec(`DELETE FROM reservations WHERE order_id=$1`, orderID)
	if err != nil {
		return fmt.Errorf("reservations release-by-order: %w", err)
	}
	return nil
}

// ReleaseByBin deletes any reservation on binID, in whatever state. Called at
// the delivered moment (ApplyArrival / ApplyMultiBinArrival) in the same tx that
// clears bins.claimed_by, so a bin's reservation lives exactly as long as its
// claim: the bin frees for re-reservation at delivery instead of lingering until
// the owning order's terminal transition. Bin-keyed because the arrival path is
// bin-centric, and the uq_reservations_bin_active index guarantees at most one
// active row per bin (so this deletes exactly the delivering order's hold).
func ReleaseByBin(db Execer, binID int64) error {
	_, err := db.Exec(`DELETE FROM reservations WHERE bin_id=$1`, binID)
	if err != nil {
		return fmt.Errorf("reservations release-by-bin: %w", err)
	}
	return nil
}

// ReleaseByNode deletes any reservation on nodeID — the slot dual of ReleaseByBin.
// Called at the delivered moment in the same tx that clears the slot's
// nodes.claimed_by, so a slot's reservation lives exactly as long as its hard
// claim. Node-keyed because the arrival path is node-centric, and the
// uq_reservations_slot_active index guarantees at most one active slot row per node.
func ReleaseByNode(db Execer, nodeID int64) error {
	_, err := db.Exec(`DELETE FROM reservations WHERE node_id=$1`, nodeID)
	if err != nil {
		return fmt.Errorf("reservations release-by-node: %w", err)
	}
	return nil
}

// ListByOrder returns all reservations held by orderID — both kinds, both states.
// The plan-time reconcile loads its own holds with this BEFORE deciding what to
// keep / release / acquire — the owner-aware step that dodges the per-resource
// unique-index self-conflict documented on Acquire. Kind-threaded: each row carries
// its Kind, and exactly one of BinID/NodeID is set (the other is 0) so the reconcile
// can match a slot row by node and a bin row by bin.
func ListByOrder(db Queryer, orderID int64) ([]Reservation, error) {
	rows, err := db.Query(
		`SELECT resource_kind, bin_id, node_id, state FROM reservations WHERE order_id=$1 ORDER BY id`,
		orderID,
	)
	if err != nil {
		return nil, fmt.Errorf("reservations list-by-order: %w", err)
	}
	defer rows.Close()
	var out []Reservation
	for rows.Next() {
		var r Reservation
		var binID, nodeID sql.NullInt64
		if err := rows.Scan(&r.Kind, &binID, &nodeID, &r.State); err != nil {
			return nil, fmt.Errorf("reservations list-by-order scan: %w", err)
		}
		r.BinID = binID.Int64   // 0 when NULL (slot/mouth rows)
		r.NodeID = nodeID.Int64 // 0 when NULL (bin rows)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReapOrphaned deletes reservation rows — in BOTH states — whose owning order is
// terminal or no longer exists. This is the owner-liveness reaper:
// reclamation keys on the OWNER being dead, NEVER on the hold's age. A hold under a live,
// non-terminal order is sacred no matter how long it has been held — an order in sourcing
// legitimately waits minutes-to-hours (or days) for its source to appear; demand is
// operator-driven and never evaporates.
//
// It is the defense-in-depth backstop BEHIND the terminal chokepoint: TerminalizeOrder
// (store/orders.go) already releases an order's reservations in the same tx that takes it
// terminal, so on the normal path there is nothing here to reap. This catches rows that
// leaked past that path — a crash between the status write and the release, or a raw
// status bypass. Idempotent with the chokepoint: a row already released there simply isn't
// present.
//
// The `order_id NOT IN (orders)` leg is currently unreachable — reservations.order_id is a
// RESTRICT foreign key (migrations.go v42) and orders are never hard-deleted, so a
// reservation can never outlive its order row — but is kept as one-clause insurance against
// a future ON DELETE CASCADE. Returns the number of rows deleted; errors are non-fatal to
// the caller (the reconciliation loop logs and continues).
func ReapOrphaned(db Execer) (int, error) {
	result, err := db.Exec(fmt.Sprintf(
		`DELETE FROM reservations
		 WHERE order_id IN (SELECT id FROM orders WHERE status IN (%s))
		    OR order_id NOT IN (SELECT id FROM orders)`,
		protocol.TerminalStatusSQLList()))
	if err != nil {
		return 0, fmt.Errorf("reservations reap-orphaned: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}
