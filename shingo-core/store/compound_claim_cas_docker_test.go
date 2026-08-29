//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// compound_claim_cas_docker_test.go — the sibling-scoped compare-and-set on a
// compound's bin claims.
//
// CreateCompoundChildren used to overwrite bins.claimed_by unconditionally, so a
// reshuffle could take a bin out from under an unrelated order that was already
// carrying it, and the UPDATE reported success either way.
//
// The predicate is deliberately not bins.Claim's. A multi-step plan INTENTIONALLY
// overlaps claims — one bin can appear in several steps and the last write is the
// one that stands — so refusing a repeat claim would be a behaviour change rather
// than a fence. What it refuses is a claim held from OUTSIDE the compound.

// claimFor hard-claims a bin for an order through the production reserve →
// claim → confirm sequence, so the row under test is a real claim rather than a
// value written straight into the column.
func claimFor(t *testing.T, db *store.DB, binID, orderID int64) {
	t.Helper()
	testdb.ClaimBinForTest(t, db, binID, orderID)
	got, err := db.GetBin(binID)
	if err != nil {
		t.Fatalf("reload bin %d after claim: %v", binID, err)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != orderID {
		t.Fatalf("fixture: bin %d is not claimed by order %d (claimed_by=%v) — the case under test "+
			"needs the claim to actually exist when the child claim runs", binID, orderID, got.ClaimedBy)
	}
}

func compoundParent(t *testing.T, db *store.DB, uuid string) *orders.Order {
	t.Helper()
	return testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.StationID = "line-1"
		o.Status = protocol.StatusReshuffling
	})
}

func childFor(parent *orders.Order, binID int64, seq int) store.CompoundChild {
	pid := parent.ID
	return store.CompoundChild{
		Order: &orders.Order{
			EdgeUUID: parent.EdgeUUID + "-child" + string(rune('0'+seq)),
			// The station a leg inherits from its parent (compound.go).
			StationID:     parent.StationID,
			OrderType:     protocol.OrderTypeMove,
			Status:        protocol.StatusPending,
			Quantity:      1,
			ParentOrderID: &pid,
			Sequence:      seq,
			BinID:         &binID,
		},
		BinID: binID,
	}
}

// TestCompoundClaim_RefusesABinHeldOutsideTheCompound is the case the CAS exists
// for, and it is the one DESIGN §16 rule 7 is about: the refusal is the FIRST
// thing that can reject this write, so the unrelated claim has to genuinely be on
// the row when it runs. Writing claimed_by directly, or claiming for an order that
// does not exist, would let an upstream precondition (the FK, the demoted-CAS
// seatbelt in bins.Claim) do the rejecting instead and the test would pass
// without the predicate.
//
// MUTATION (verified): restore the unconditional
// `UPDATE bins SET claimed_by=$1 WHERE id=$2`. The compound is created, the
// stranger's bin is taken, and this test's own "still claimed by the stranger"
// assertion fires.
func TestCompoundClaim_RefusesABinHeldOutsideTheCompound(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "CAS-STRANGER")

	stranger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "cas-stranger" })
	claimFor(t, db, bin.ID, stranger.ID)

	parent := compoundParent(t, db, "cas-parent")
	_, err := db.CreateCompoundChildren([]store.CompoundChild{childFor(parent, bin.ID, 1)})
	if err == nil {
		t.Fatal("CreateCompoundChildren succeeded against a bin held by an unrelated order — the " +
			"reshuffle would drive to a bin another order is already carrying")
	}

	after, gErr := db.GetBin(bin.ID)
	if gErr != nil {
		t.Fatalf("reload bin: %v", gErr)
	}
	if after.ClaimedBy == nil || *after.ClaimedBy != stranger.ID {
		t.Fatalf("bin %d is now claimed by %v, want the unrelated order %d — the claim was taken",
			bin.ID, after.ClaimedBy, stranger.ID)
	}

	// The whole transaction rolls back, so no half-built compound is left behind.
	kids, lErr := db.ListChildOrders(parent.ID)
	if lErr != nil {
		t.Fatalf("list children: %v", lErr)
	}
	if len(kids) != 0 {
		t.Errorf("compound left %d child order(s) behind after a refused claim; a compound missing "+
			"one leg's bin strands mid-dig", len(kids))
	}
}

// TestCompoundClaim_AllowsTheOverlapsItsOwnPlanCreates is the other half, and
// without it the CAS could tighten to bins.Claim's predicate and look correct.
//
// Three arms, each a claim state the predicate must ADMIT:
//   - unclaimed — the ordinary case;
//   - held by a SIBLING — one bin in several steps, last write wins, which
//     engine/wiring_completion.go's teleport-guard skip depends on;
//   - held by the PARENT — both planners emit a `retrieve` step carrying the
//     buried target bin, and the multi-burial loop re-plans after the parent has
//     resumed and been dispatched.
//
// MUTATION (verified): narrow the predicate to `claimed_by IS NULL OR claimed_by
// = $1` (bins.Claim's). The parent and sibling arms fire; `unclaimed` still
// passes, which is the split that makes the mutation informative — it is exactly
// the rebuild the brief ruled out, and the two arms that fail are the two
// behaviours the ruling preserves.
//
// The first attempt at this mutation left a stray `$3 = $3` in the WHERE clause
// and all three arms failed on argument encoding instead. Recorded per DESIGN
// §16 rule 2: something firing before your assertion means the mutation was
// wrong, and a malformed mutation is indistinguishable from a working one if you
// only read the pass/fail count.
func TestCompoundClaim_AllowsTheOverlapsItsOwnPlanCreates(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	t.Run("unclaimed", func(t *testing.T) {
		bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "CAS-FREE")
		parent := compoundParent(t, db, "cas-free-parent")
		if _, err := db.CreateCompoundChildren([]store.CompoundChild{childFor(parent, bin.ID, 1)}); err != nil {
			t.Fatalf("claiming an unclaimed bin must succeed: %v", err)
		}
		assertClaimedByAChildOf(t, db, bin.ID, parent.ID)
	})

	t.Run("held by the parent", func(t *testing.T) {
		bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "CAS-PARENT")
		parent := compoundParent(t, db, "cas-parent-holds")
		claimFor(t, db, bin.ID, parent.ID)

		if _, err := db.CreateCompoundChildren([]store.CompoundChild{childFor(parent, bin.ID, 1)}); err != nil {
			t.Fatalf("a child must be able to take a bin its own PARENT holds: %v.\nThe planners emit "+
				"a retrieve step carrying the buried target — the very bin the parent retrieve exists "+
				"to fetch — so excluding the parent fails a claim that works today", err)
		}
		assertClaimedByAChildOf(t, db, bin.ID, parent.ID)
	})

	t.Run("held by a sibling, last write wins", func(t *testing.T) {
		bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "CAS-SIBLING")
		parent := compoundParent(t, db, "cas-sibling")

		// Two steps over ONE bin — the unbury-then-retrieve shape.
		kids := []store.CompoundChild{childFor(parent, bin.ID, 1), childFor(parent, bin.ID, 2)}
		if _, err := db.CreateCompoundChildren(kids); err != nil {
			t.Fatalf("a plan that touches one bin twice must be creatable: %v", err)
		}
		after, err := db.GetBin(bin.ID)
		if err != nil {
			t.Fatalf("reload bin: %v", err)
		}
		if after.ClaimedBy == nil || *after.ClaimedBy != kids[1].Order.ID {
			t.Errorf("claimed_by = %v, want the LAST step's child %d — last-write-wins is what "+
				"wiring_completion.go's teleport-guard skip is written against",
				after.ClaimedBy, kids[1].Order.ID)
		}
	})
}

func assertClaimedByAChildOf(t *testing.T, db *store.DB, binID, parentID int64) {
	t.Helper()
	after, err := db.GetBin(binID)
	if err != nil {
		t.Fatalf("reload bin %d: %v", binID, err)
	}
	if after.ClaimedBy == nil {
		t.Fatalf("bin %d ended unclaimed", binID)
	}
	owner, err := db.GetOrder(*after.ClaimedBy)
	if err != nil {
		t.Fatalf("get claiming order %d: %v", *after.ClaimedBy, err)
	}
	if owner.ParentOrderID == nil || *owner.ParentOrderID != parentID {
		t.Errorf("bin %d is claimed by order %d, which is not a child of compound %d",
			binID, owner.ID, parentID)
	}
}
