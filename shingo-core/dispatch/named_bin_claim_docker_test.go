//go:build docker

package dispatch

import (
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// A MOVE THAT NAMES ITS BIN HOLDS AND CLAIMS THAT BIN, ONLY ON ITS SOURCE.
//
// The test is inside each statement that holds or claims a bin
// (reservations.NamedBinSQL), not a read before it: the bin hold, and the three
// bin claim statements behind ConfirmClaim. Another bin on the source is
// refused, and so is the named bin once it has left the source.

func namedMove(t *testing.T, f *endedOwnerFixture, source string, bin int64) *orders.Order {
	t.Helper()
	return testdb.CreateOrder(t, f.db, func(o *orders.Order) {
		o.OrderType, o.Status, o.SourceNode, o.DeliveryNode = OrderTypeMove, StatusSourcing, source, f.slot.Name
		o.NamedBinID = &bin
	})
}

func TestNamedBin_TheHoldTakesOnlyTheNamedBinOnItsSource(t *testing.T) {
	t.Parallel()
	f := seedEndedOwner(t, "NBH")
	other := testdb.CreateBinAtNode(t, f.db, f.payload, f.src.ID, "NBH-OTHER")

	o := namedMove(t, f, f.src.Name, f.bin)
	if err := reservations.Acquire(f.db.DB, o.ID, o.ID, other.ID, "test"); !errors.Is(err, reservations.ErrNotTheNamedBin) {
		t.Fatalf("hold another bin on the source: %v, want ErrNotTheNamedBin", err)
	}
	testutil.MustNoErr(t, reservations.Acquire(f.db.DB, o.ID, o.ID, f.bin, "test"), "hold the named bin")

	// Gone from the source, the named bin is refused too.
	gone := namedMove(t, f, f.src.Name, other.ID)
	_, err := f.db.DB.Exec(`UPDATE bins SET node_id=$1 WHERE id=$2`, f.slot.ID, other.ID)
	testutil.MustNoErr(t, err, "the named bin leaves the source")
	if err := reservations.Acquire(f.db.DB, gone.ID, gone.ID, other.ID, "test"); !errors.Is(err, reservations.ErrNotTheNamedBin) {
		t.Fatalf("hold the named bin off its source: %v, want ErrNotTheNamedBin", err)
	}

	// A source named PARENT.CHILD matches the bin on the child.
	dotted := namedMove(t, f, "NBH-NGRP."+f.slot.Name, other.ID)
	testutil.MustNoErr(t, reservations.Acquire(f.db.DB, dotted.ID, dotted.ID, other.ID, "test"),
		"hold the named bin on a source named PARENT.CHILD")
}

func TestNamedBin_EveryClaimRefusesTheBinOnceItHasLeft(t *testing.T) {
	t.Parallel()
	uop := func(n int) *int { return &n }
	for _, c := range []struct {
		name string
		uop  *int
	}{{"claim", nil}, {"clear-and-claim", uop(0)}, {"sync-and-claim", uop(5)}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := seedEndedOwner(t, "NBC-"+c.name[:4])
			o := namedMove(t, f, f.src.Name, f.bin)
			testutil.MustNoErr(t, reservations.Acquire(f.db.DB, o.ID, o.ID, f.bin, "test"), "hold the named bin")
			_, err := f.db.DB.Exec(`UPDATE bins SET node_id=$1 WHERE id=$2`, f.slot.ID, f.bin)
			testutil.MustNoErr(t, err, "the bin leaves the source while held")

			if err := f.d.binManifest.ConfirmClaim(f.bin, o.ID, c.uop); !errors.Is(err, reservations.ErrNotTheNamedBin) {
				t.Fatalf("claim the named bin off its source: %v, want ErrNotTheNamedBin", err)
			}
			var claimed int
			testutil.MustNoErr(t, f.db.DB.QueryRow(`SELECT count(*) FROM bins WHERE claimed_by=$1`, o.ID).Scan(&claimed), "claims")
			if claimed != 0 {
				t.Fatalf("the move claimed %d bin(s), want none", claimed)
			}

			// Back on the source, the same claim lands.
			_, err = f.db.DB.Exec(`UPDATE bins SET node_id=$1 WHERE id=$2`, f.src.ID, f.bin)
			testutil.MustNoErr(t, err, "the bin is back on the source")
			testutil.MustNoErr(t, f.d.binManifest.ConfirmClaim(f.bin, o.ID, c.uop), "claim the named bin on its source")
		})
	}
}
