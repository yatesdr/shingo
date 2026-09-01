//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// dig_steal_contract_docker_test.go — when a dig takes a soft-held blocker, the
// books say so.
//
// A blocker is positional: the dig has no choice about which bins are in its
// way, and the claim CAS admits any bin whose claimed_by is NULL, including one
// another order has promised itself. Positional is an argument about WHICH bin
// though, not about whose turn, so since §7 the take goes by the demand ranking
// and a soft hold CAN stop it — see compoundParent, which states "this dig wins"
// as a priority rather than leaving it to seeding order, and the ranked-take
// suite for the losing path. Everything below is the winning path.
//
// What was missing was the bookkeeping. The holder's reservation survived the
// steal and was deleted much later, at the dig leg's ARRIVAL — so for the whole
// excavation a live row pointed at a bin somebody else was carrying away, and
// nothing anywhere said a dig had taken it. It worked by accident.
//
// Three things become true here: the holder's row is released AT the steal, the
// holder's bin_id goes with it so it recalculates instead of following a plan
// that is no longer true, and the dig's own claim finally has a ledger row.
//
// THE ONE-VICTIM SHAPE OF THESE TESTS IS THE INDEX, NOT THE CONTRACT.
// uq_reservations_bin_active allows a single active row per bin, so every case
// below has exactly one holder to steal from and cannot distinguish "repairs the
// holder" from "repairs A holder". The set-valued contract — every holder, and
// never a hand-placed one this way — is pinned next door in
// dig_steal_setvalued_docker_test.go, which manufactures the multi-holder state
// the narrowing will make ordinary.

// activeBinReservations returns the active reservation rows on a bin, as
// (orderID, state) pairs.
func activeBinReservations(t *testing.T, db *store.DB, binID int64) []struct {
	OrderID int64
	State   string
} {
	t.Helper()
	rows, err := db.DB.Query(`SELECT order_id, state FROM reservations
		WHERE bin_id=$1 AND resource_kind='bin' AND state IN ('pending','confirmed')
		ORDER BY order_id`, binID)
	if err != nil {
		t.Fatalf("read bin reservations: %v", err)
	}
	defer rows.Close()
	var out []struct {
		OrderID int64
		State   string
	}
	for rows.Next() {
		var r struct {
			OrderID int64
			State   string
		}
		if err := rows.Scan(&r.OrderID, &r.State); err != nil {
			t.Fatalf("scan reservation: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// TestDigSteal_ReleasesTheHolderAndClearsItsPointer is the contract.
//
// MUTATION (verified): delete the stealSoftHold call. The holder keeps a live
// reservation on a bin the dig now owns — two books on one bin — and its bin_id
// still points at it, which is the silent steal restored.
func TestDigSteal_ReleasesTheHolderAndClearsItsPointer(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "STEAL-SOFT")

	// A parked order that has SOFT-reserved the bin: a pending reservation plus
	// the bin_id stamp the scanner writes at the same moment (it is stamped at
	// reserve time, not at claim time, which is what makes clearing it necessary).
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "steal-holder" })
	testdb.ReserveBin(t, db, holder.ID, bin.ID)
	if err := db.UpdateOrderBinID(holder.ID, bin.ID); err != nil {
		t.Fatalf("stamp holder bin_id: %v", err)
	}
	before, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("reload bin: %v", err)
	}
	if before.ClaimedBy != nil {
		t.Fatal("fixture: a soft hold must leave claimed_by NULL, or this exercises the hard arm")
	}

	// The dig claims it as a blocker.
	parent := compoundParent(t, db, "steal-parent")
	if _, err := db.CreateCompoundChildren([]store.CompoundChild{childFor(parent, bin.ID, 1)}); err != nil {
		t.Fatalf("the dig must win a soft-held blocker: %v", err)
	}

	// THE HOLDER'S BOOK IS CLOSED, at the steal and not at some later arrival.
	after, err := db.GetOrder(holder.ID)
	if err != nil {
		t.Fatalf("reload holder: %v", err)
	}
	if after.BinID != nil {
		t.Errorf("holder still points at bin %d. bin_id is stamped at soft-reserve time, so the "+
			"holder would re-enter through dispatchHeldBin and confirm by id — and ConfirmClaim "+
			"never re-acquires, so it would wedge on claim_failed instead of recalculating", *after.BinID)
	}
	for _, r := range activeBinReservations(t, db, bin.ID) {
		if r.OrderID == holder.ID {
			t.Errorf("the holder's reservation survived the steal (state %q) — a live book pointing "+
				"at a bin another order is carrying away", r.State)
		}
	}

	// AND THE HOLDER IS STILL DEMAND. The steal takes the plan, never the order.
	if protocol.IsTerminal(after.Status) {
		t.Errorf("holder is %q — losing a reserved bin is a recalculation, not a failure", after.Status)
	}
}

// TestDigSteal_LedgerRowFollowsTheClaim closes hold class 3: a dig's claim used
// to be stamped with no reservation behind it — a claimed_by pointing at nothing
// in the books.
//
// It stayed open because an honest row needs an answer to "whose entry wins when
// a dig and a holder both have books on one bin". The steal above IS that answer,
// which is why this lands with it and could not land before it.
func TestDigSteal_LedgerRowFollowsTheClaim(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "STEAL-LEDGER")

	parent := compoundParent(t, db, "ledger-parent")
	child := childFor(parent, bin.ID, 1)
	if _, err := db.CreateCompoundChildren([]store.CompoundChild{child}); err != nil {
		t.Fatalf("create compound: %v", err)
	}

	rows := activeBinReservations(t, db, bin.ID)
	if len(rows) != 1 {
		t.Fatalf("bin has %d active reservation(s), want exactly 1 — uq_reservations_bin_active "+
			"allows one, and a dig with none is the hold-class-3 gap", len(rows))
	}
	if rows[0].OrderID != child.Order.ID {
		t.Errorf("ledger row belongs to order %d, want the claiming child %d — the books must say "+
			"what the claim says", rows[0].OrderID, child.Order.ID)
	}
	if rows[0].State != string(reservations.StateConfirmed) {
		t.Errorf("ledger row is %q, want confirmed — the claim it records is a hard claim already, "+
			"and a pending row would say the dig is still deciding", rows[0].State)
	}

	// The claim and the row agree.
	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("reload bin: %v", err)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != child.Order.ID {
		t.Errorf("claimed_by = %v, want %d", got.ClaimedBy, child.Order.ID)
	}
}

// TestDigSteal_OneBinTwiceKeepsOneRow is the dedupe half, and it is the reason
// the ledger row is a delete-then-insert rather than an insert.
//
// A plan legitimately touches one bin twice — an unbury followed by a retrieve of
// the same bin — and the index allows one active row per bin. The row follows the
// claim: last write wins for both, together, which is the property
// wiring_completion.go's teleport-guard skip already relies on for claimed_by.
func TestDigSteal_OneBinTwiceKeepsOneRow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "STEAL-TWICE")

	parent := compoundParent(t, db, "twice-parent")
	kids := []store.CompoundChild{childFor(parent, bin.ID, 1), childFor(parent, bin.ID, 2)}
	if _, err := db.CreateCompoundChildren(kids); err != nil {
		t.Fatalf("a plan that touches one bin twice must be creatable: %v", err)
	}

	rows := activeBinReservations(t, db, bin.ID)
	if len(rows) != 1 {
		t.Fatalf("bin has %d active reservation(s) after two steps, want 1 — a second insert would "+
			"violate uq_reservations_bin_active and fail the whole compound", len(rows))
	}
	if rows[0].OrderID != kids[1].Order.ID {
		t.Errorf("ledger row belongs to %d, want the LAST step's child %d — the row follows the "+
			"claim, and claimed_by is last-write-wins", rows[0].OrderID, kids[1].Order.ID)
	}
}

// TestDigSteal_TheCompoundsOwnRowIsSupersededNotStolen is the narrowness
// assertion. A row belonging to the parent is the SAME demand holding its own bin
// across steps — the multi-burial loop re-plans after the parent has resumed and
// been dispatched, by which point the parent can hold the claim. Reporting that
// as a theft would name the order as its own victim and, worse, clear the
// parent's bin_id out from under a live plan.
func TestDigSteal_TheCompoundsOwnRowIsSupersededNotStolen(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "STEAL-OWN")

	parent := compoundParent(t, db, "own-parent")
	testdb.ReserveBin(t, db, parent.ID, bin.ID)
	if err := db.UpdateOrderBinID(parent.ID, bin.ID); err != nil {
		t.Fatalf("stamp parent bin_id: %v", err)
	}

	child := childFor(parent, bin.ID, 1)
	if _, err := db.CreateCompoundChildren([]store.CompoundChild{child}); err != nil {
		t.Fatalf("a child must be able to take a bin its own parent holds: %v", err)
	}

	after, err := db.GetOrder(parent.ID)
	if err != nil {
		t.Fatalf("reload parent: %v", err)
	}
	if after.BinID == nil || *after.BinID != bin.ID {
		t.Errorf("the parent's bin_id was cleared — its own compound is not a thief, and the parent " +
			"still owes a retrieve of that very bin")
	}
	rows := activeBinReservations(t, db, bin.ID)
	if len(rows) != 1 || rows[0].OrderID != child.Order.ID {
		t.Errorf("reservations = %+v, want exactly the claiming child %d — superseded, not stolen",
			rows, child.Order.ID)
	}
}
