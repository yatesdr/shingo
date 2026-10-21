//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// pair_trade_pins_docker_test.go — what the Edge's coordinated pairs and the
// compound advance do today, pinned before a home clear's two-robot trade is
// built beside them. The trade's legs are compound children: the scanner skips
// a child before its coordinated branch, so none of them reaches pair
// admission (complex_pair.go) or the vacated-slot rule (vacated_slot.go).
// These pins say those stay as they are.

// TestPairPin_MutualTradeAtALoaderRePlacesTheBufferLeg: two Edge complex legs
// of one dedicated loader, each lifting before its first wait and each
// dropping where the other lifts, with every buffer full. Pair admission never
// hands one leg the other's lift (a mutual grant is impossible,
// plan/vacated.go): the loader park finds the home occupied and re-points the
// leg lifting the buffer back onto the buffer its own pickup vacates (an
// own-lift stamp, Partner 0), and both legs go in the one pass. The other leg
// still drops on that buffer, unstamped.
//
// RECORDED, NOT ENDORSED: both legs now name the one buffer as their drop, and
// nothing but the station's two releases orders them. No Edge door authors
// this shape today. The pin exists so that any change to pair admission or the
// loader park shows up here; the home clear's trade does not come this way.
func TestPairPin_MutualTradeAtALoaderRePlacesTheBufferLeg(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	stageA, stageB := prNode(t, db, "MT-STAGE-A"), prNode(t, db, "MT-STAGE-B")
	now := time.Now().UTC()
	makeLoaderBin(t, db, "PART-X", home.ID, "mt-home-full", 10, now)
	bufferBin := makeLoaderBin(t, db, "PART-X", buffer.ID, "mt-buffer-full", 10, now.Add(-time.Hour))

	prSubmitLeg(d, "mt-a", "mt-b", "PART-X", home.Name,
		prPick(buffer.Name), prWait(stageA.Name), prDrop(home.Name))
	prSubmitLeg(d, "mt-b", "mt-a", "PART-X", home.Name,
		prPick(home.Name), prWait(stageB.Name), prDrop(buffer.Name))
	prScanPass(t, d, db, "mt-b", "mt-a")

	a, b := prReloadUUID(t, db, "mt-a"), prReloadUUID(t, db, "mt-b")
	if a.VendorOrderID == "" || b.VendorOrderID == "" {
		t.Fatalf("the pair did not go in one pass: a %s vendor=%q cause=%q, b %s vendor=%q cause=%q",
			a.Status, a.VendorOrderID, a.QueueCause, b.Status, b.VendorOrderID, b.QueueCause)
	}
	if a.DeliveryNode != buffer.Name {
		t.Errorf("the leg lifting the buffer drops on %s, want the buffer %s its own pickup vacates", a.DeliveryNode, buffer.Name)
	}
	if fd := vsFinalDrop(t, a); fd.Node != buffer.Name || fd.Vacate == nil || fd.Vacate.Bin != bufferBin.ID ||
		fd.Vacate.Partner != 0 || fd.Anchor != home.Name {
		t.Errorf("its drop = %+v (stamp %+v), want %s with an own-lift stamp on bin %d, anchored at %s",
			fd, fd.Vacate, buffer.Name, bufferBin.ID, home.Name)
	}
	if fd := vsFinalDrop(t, b); b.DeliveryNode != buffer.Name || fd.Node != buffer.Name || fd.Vacate != nil {
		t.Errorf("the leg lifting the home drops on %s (stamp %+v), want %s unstamped", fd.Node, fd.Vacate, buffer.Name)
	}
}

// TestCompoundPin_AdvanceSendsOneLegPerCall: two plain-move children, both
// admissible, none carrying a stored vacate wait. One advance sends exactly
// the first by sequence and returns; the next advance sends the second. The
// trade's continuation must not change this for any other child.
func TestCompoundPin_AdvanceSendsOneLegPerCall(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	src2 := prNode(t, db, "ONE-SRC-2")
	parent := &orders.Order{EdgeUUID: "one-per-call-parent", StationID: "line-1",
		OrderType: OrderTypeRetrieve, Status: protocol.StatusReshuffling, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")
	var kids []*orders.Order
	for i, src := range []string{sd.StorageNode.Name, src2.Name} {
		c := &orders.Order{EdgeUUID: "one-per-call-child" + string(rune('1'+i)), StationID: "line-1",
			OrderType: OrderTypeMove, Status: StatusPending, Quantity: 1,
			ParentOrderID: &parent.ID, Sequence: i + 1, SourceNode: src, DeliveryNode: sd.LineNode.Name}
		testutil.MustNoErr(t, db.CreateOrder(c), "create child")
		kids = append(kids, c)
	}
	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	sent := func() (n int, first bool) {
		for i, k := range kids {
			if prReload(t, db, k.ID).VendorOrderID != "" {
				n++
				first = first || i == 0
			}
		}
		return n, first
	}
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "first advance")
	if n, first := sent(); n != 1 || !first {
		t.Fatalf("after one advance %d legs are with the fleet (first by sequence: %v), want exactly the first", n, first)
	}
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "second advance")
	if n, _ := sent(); n != 2 {
		t.Fatalf("after two advances %d legs are with the fleet, want 2", n)
	}
}
