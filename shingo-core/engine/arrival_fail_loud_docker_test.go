//go:build docker

package engine

import (
	"shingo/protocol"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestDeliveredWithForeignCargo_FailsLoud is the deferred fail-vs-park
// disposition, landed on the evidence rather than on an argument.
//
// The instrument read 121, then 2, then 1 as three extraction errors were
// corrected, and every surviving specimen was explained benign — the last a
// terminal order whose bin had moved on, closed by the terminal discriminator. A
// refusal that survives all four cuts is a state the claim lifecycle should not
// be able to produce, so the order fails with a named integrity message instead
// of being reported delivered.
//
// Parking lost on a fact found while building: Core does not know what the robot
// is holding, so parking keeps an order alive whose payload is unidentifiable
// while it still holds a runtime slot — the dead-robot wedge.
//
// The order must NOT be announced to Edge as delivered on the way out. Failing an
// order and announcing its delivery in the same breath is the exact lie this
// thread has been unwinding (PLAN §R.5: an order confirmed while its bin sat at
// _TRANSIT).
func TestDeliveredWithForeignCargo_FailsLoud(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewSuccessBackend())

	bt := &bins.BinType{Code: "FL-BT", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	src := &nodes.Node{Name: "FL-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "create source")
	dst := &nodes.Node{Name: "FL-DST", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dst), "create dest")

	// The cargo, claimed by SOMEBODY ELSE and sitting nowhere near the
	// destination — a genuine ownership conflict, not a landed delivery and not a
	// terminal order's leftovers.
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "FL-BIN", NodeID: &src.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")
	// THE RIGHTFUL OWNER MUST BE LIVE *AND* INVISIBLE TO THE SCANNER, and the
	// second half is what this fixture was missing.
	//
	// testdb.CreateOrder defaults to `queued` with no payload code. `queued` is in
	// the acquiring set, so the running engine's fulfillment scanner picks the
	// order up, finds it has no payload to source against, and STRUCTURALLY FAILS
	// it — and failing an order releases its bin claims. The assertion at the end
	// of this test then reads a nil claim and reports that the carrier stole it,
	// which is not what happened at all.
	//
	// It is a race, not a certainty: it only bites when the scanner wins before the
	// assertion. Locally it never did; CI under -race is slower and it did (job
	// 94088537674 at 35445099, "order 1 failed: structural - order has empty
	// payload_code"). Latent since this fixture was written.
	//
	// `in_transit` fixes it by being MORE faithful rather than less: a bin claimed
	// by a live order that is not acquiring is exactly the real situation this test
	// describes — a robot already en route to it — and it is outside
	// IsAcquiring{queued, sourcing}, so nothing sweeps it.
	thief := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "in_transit" })
	if protocol.IsAcquiring(thief.Status) {
		t.Fatalf("fixture: the rightful owner is %q, which is in the acquiring set — the fulfillment "+
			"scanner will structurally fail it and release the very claim this test asserts on. That "+
			"is a RACE, so it will pass locally and flake in CI; keep this order out of {queued, "+
			"sourcing}", thief.Status)
	}
	testdb.ClaimBinForTest(t, db, bin.ID, thief.ID)

	carrier := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = src.Name
		o.DeliveryNode = dst.Name
		o.BinID = &bin.ID
		o.Status = "delivered"
	})
	if _, err := db.Exec(`UPDATE orders SET status='delivered' WHERE id=$1`, carrier.ID); err != nil {
		t.Fatalf("set delivered: %v", err)
	}
	carrier, err := db.GetOrder(carrier.ID)
	testutil.MustNoErr(t, err, "reload carrier")

	eng.handleOrderDelivered(carrier)

	got, err := db.GetOrder(carrier.ID)
	testutil.MustNoErr(t, err, "reload after")
	if got.Status != "failed" {
		t.Fatalf("status = %q, want %q — a robot carrying cargo the ledger assigns to another order "+
			"is an integrity fault, and law 1's carve-out is that genuine faults fail loud", got.Status, "failed")
	}

	// The message must name BOTH orders: the reader has to know who to look at.
	if !strings.Contains(got.ErrorDetail, "cargo does not match the ledger") {
		t.Errorf("failure detail %q does not carry the named integrity message", got.ErrorDetail)
	}
	if !strings.Contains(got.ErrorDetail, "claimed by order") {
		t.Errorf("failure detail does not name the claiming order: %q", got.ErrorDetail)
	}

	// AND THE BIN WAS NOT TOUCHED. The whole point of refusing is that this order
	// does not get to move a bin it does not own.
	gotBin, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "reload bin")
	if gotBin.NodeID == nil || *gotBin.NodeID != src.ID {
		t.Errorf("bin moved to %v — a refused arrival must not place the bin", gotBin.NodeID)
	}
	if gotBin.ClaimedBy == nil || *gotBin.ClaimedBy != thief.ID {
		t.Errorf("bin's claim = %v, want order %d untouched — failing the carrier must not steal the "+
			"rightful owner's claim", gotBin.ClaimedBy, thief.ID)
	}
}
