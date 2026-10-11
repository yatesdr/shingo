//go:build docker

package dispatch

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// TestC2aPin_AFullOnACarrierItsPartMayNotRideIsStoredHeld — GREEN since C2a A.3
// (red at 660fd228; the fixture's bin was made a real full at C2a, see below).
//
// THE PRESS KNOWN ISSUE (keep-staged handoff, owner 2026-10-04). A produce press
// whose carrier changes fills the outgoing part into the incoming carriers left
// on its positions. The move that takes such a full out of the press is
// node-local, and the finder judged the full against its part's carrier rule
// (payload_bin_types, BinTypeRule.Permits) and refused it: the move waited
// "for material" with the full standing on the position it had to leave.
//
// The ruling (the orc's one call, C2a brief A.3): the store door judges a full by
// the thing it is. A full on a carrier its part may not ride is STORED — it has to
// leave the press, and refusing it would leave it blocking a position — with the
// bin's containment marker set (bins.quality_hold), so every finder passes it by;
// the marker names the rule it broke; a person clears it. Role-blind.
//
// The full leaves the press as a MOVE (store is not a wire order type; the Edge
// sends a move). A move of the same kind of full to a node that is not storage is
// judged as before: it waits.
func TestC2aPin_AFullOnACarrierItsPartMayNotRideIsStoredHeld(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP type")

	market := &nodes.Node{Name: "C2AP-MKT", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(market), "create the market")
	for i := 1; i <= 2; i++ {
		s := &nodes.Node{Name: fmt.Sprintf("C2AP-MKT-%d", i), Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(s), "create a market slot")
		testutil.MustNoErr(t, db.SetNodeParent(s.ID, market.ID), "parent the slot")
	}
	press := &nodes.Node{Name: "C2AP-PRESS", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(press), "create the press position")

	// The part rides DEFAULT only; the bin at the press is a carrier it is not listed for.
	listed, err := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, err, "DEFAULT bin type")
	wrong := &bins.BinType{Code: "C2AP-OTHER"}
	testutil.MustNoErr(t, db.CreateBinType(wrong), "create the other carrier")
	part := &payloads.Payload{Code: "C2AP-PART"}
	testutil.MustNoErr(t, db.CreatePayload(part), "create the part")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(part.ID, []int64{listed.ID}), "the part rides DEFAULT only")

	// A FULL: the part is on the bin's manifest, confirmed. CreateBin alone
	// leaves payload_code blank, which is an EMPTY carrier, and a move relocates
	// an empty as it stands, unjudged (finder.go) — so the pin as first written
	// was red on an empty, not on the full it names.
	full := &bins.Bin{BinTypeID: wrong.ID, Label: "C2AP-FULL", NodeID: &press.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(full), "the bin on the wrong carrier")
	testutil.MustNoErr(t, db.SetBinManifest(full.ID, `{"items":[]}`, part.Code, 100), "fill it with the part")
	testutil.MustNoErr(t, db.ConfirmBinManifest(full.ID, ""), "confirm the fill")

	d, emitter := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "c2ap-out", OrderType: OrderTypeMove, PayloadCode: part.Code,
		SourceNode: press.Name, DeliveryNode: market.Name, Quantity: 1,
	})
	if len(emitter.failed) > 0 {
		t.Fatalf("the full's move out of the press was refused (%s): the ruling stores it", emitter.failed[0].errorCode)
	}

	out, err := db.GetOrderByUUID("c2ap-out")
	testutil.MustNoErr(t, err, "read the move back")
	if out.Status == protocol.StatusQueued || protocol.IsTerminal(out.Status) {
		t.Errorf("the move out of the press is %s under %q (%q); the full has to leave the press",
			out.Status, out.QueueCause, out.QueueReason)
	}
	held, by, err := db.BinQualityHold(full.ID)
	testutil.MustNoErr(t, err, "read the bin's hold marker")
	if !held {
		t.Errorf("a %s full on carrier %s (its part rides %s only) went into storage with no containment "+
			"marker: it stands there as ordinary stock", part.Code, wrong.Code, listed.Code)
	} else if !strings.Contains(by, "carrier-rule") || !strings.Contains(by, part.Code) || !strings.Contains(by, wrong.Code) {
		t.Errorf("the bin is held by %q, which does not name the carrier rule it broke", by)
	}

	// The same kind of full moved to a node that is not storage: judged as before,
	// and the wait names the carrier rule. CONVERTED at D1 item 10 (D33): this
	// asserted finder-node-empty, which misnamed a bin-type refusal as an empty
	// node while the full stood on it; the wait is finder-carrier-refused now.
	press2 := &nodes.Node{Name: "C2AP-PRESS2", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(press2), "create a second press position")
	elsewhere := &nodes.Node{Name: "C2AP-ELSEWHERE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(elsewhere), "create a node that is not storage")
	full2 := &bins.Bin{BinTypeID: wrong.ID, Label: "C2AP-FULL2", NodeID: &press2.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(full2), "the second bin on the wrong carrier")
	testutil.MustNoErr(t, db.SetBinManifest(full2.ID, `{"items":[]}`, part.Code, 100), "fill it with the part")
	testutil.MustNoErr(t, db.ConfirmBinManifest(full2.ID, ""), "confirm the fill")
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "c2ap-elsewhere", OrderType: OrderTypeMove, PayloadCode: part.Code,
		SourceNode: press2.Name, DeliveryNode: elsewhere.Name, Quantity: 1,
	})
	other, err := db.GetOrderByUUID("c2ap-elsewhere")
	testutil.MustNoErr(t, err, "read the second move back")
	if other.Status != protocol.StatusQueued || other.QueueCause != "finder-carrier-refused" {
		t.Errorf("a move of the full to a node that is not storage is %s under %q, want queued under %q",
			other.Status, other.QueueCause, "finder-carrier-refused")
	}
	if held2, _, err := db.BinQualityHold(full2.ID); err != nil || held2 {
		t.Errorf("the full moved to a node that is not storage was held (%v, %v): only the store door holds", held2, err)
	}
}
