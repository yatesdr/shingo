//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/plantclaims"
)

// cancel_return_pins_docker_test.go — what the carried-bin door and the watch
// do at the base tree, written before the cancel-return build touches either.
//
// Each pin says what it expects to change, if anything, and why. A pin that a
// later commit edits is a pin that commit owes an explanation for.

// ── fixtures, shared with cancel_return_docker_test.go ───────────────────────

// seedCancelledCarry is branch B's leftover: a bin carrying payload on a robot's
// carrier node, and the cancelled order that was carrying it. The order is
// born cancelled, so its birth history row IS its terminal row, written now —
// the fresh, uncoded cancel a person's terminate leaves behind.
func seedCancelledCarry(t *testing.T, db *store.DB, robotID, payload, processNode string) (*bins.Bin, *orders.Order) {
	t.Helper()
	carrier := &nodes.Node{Name: bins.CarrierNodePrefix + robotID, IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(carrier), "create carrier node")
	bt := &bins.BinType{Code: "CRT-" + robotID, Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "carried-" + robotID, NodeID: &carrier.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")
	_, err := db.DB.Exec(`UPDATE bins SET payload_code=$1 WHERE id=$2`, payload, bin.ID)
	testutil.MustNoErr(t, err, "stamp the bin's payload")
	bin.PayloadCode = payload

	ord := &orders.Order{
		EdgeUUID: "carry-" + robotID, StationID: "edge.test", OrderType: "retrieve",
		Status: protocol.StatusCancelled, Quantity: 1, DeliveryNode: processNode,
		ProcessNode: processNode, PayloadCode: payload, RobotID: robotID, BinID: &bin.ID,
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create the cancelled carrier order")
	_, err = db.DB.Exec(`UPDATE orders SET robot_id=$1 WHERE id=$2`, robotID, ord.ID)
	testutil.MustNoErr(t, err, "stamp the carrier's robot")
	return bin, ord
}

// loadedDispatchable is a robot the fleet will take an order for, at rest, with
// a bin on the deck: the state the trigger fires on.
func loadedDispatchable(id string) fleet.RobotStatus {
	r := dispatchableRobot(id)
	r.JackState, r.JackIsFull, r.IsLoaded, r.LiftHeight = 1, true, true, 0.0601
	return r
}

// storeGroupWithSlot is a node group with one free flat storage child.
func storeGroupWithSlot(t *testing.T, db *store.DB, prefix string) (grp, slot *nodes.Node) {
	t.Helper()
	ngrpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "get NGRP type")
	storType := storNodeType(t, db)
	grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &ngrpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")
	slot = &nodes.Node{Name: prefix + "-S1", NodeTypeID: &storType.ID, ParentID: &grp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")
	return grp, slot
}

// storNodeType returns the STOR node type, creating it on a fresh database.
func storNodeType(t *testing.T, db *store.DB) *nodes.NodeType {
	t.Helper()
	if nt, err := db.GetNodeTypeByCode("STOR"); err == nil && nt != nil {
		return nt
	}
	nt := &nodes.NodeType{Code: "STOR", Name: "Storage Slot"}
	if err := db.CreateNodeType(nt); err != nil {
		existing, rerr := db.GetNodeTypeByCode("STOR")
		testutil.MustNoErr(t, rerr, "resolve STOR node type")
		return existing
	}
	return nt
}

// seedClaim mirrors one consume claim: process `proc` running style `style`
// pulls `payload` at `line` from `source`.
func seedClaim(t *testing.T, db *store.DB, proc, style, line, payload, source string, active bool) {
	t.Helper()
	testutil.MustNoErr(t, db.ReplacePlantClaims(proc,
		[]plantclaims.StyleRow{{ProcessID: proc, StyleID: style, ConfigGen: 1, IsActive: active}},
		[]plantclaims.ClaimRow{{ProcessID: proc, StyleID: style, CoreNodeName: line,
			Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSimple,
			PayloadCode: payload, InboundSource: source}}, 0),
		"seed claim "+proc+"/"+style)
}

// seedRoleClaim mirrors one claim with a role and both of its legs: a consume
// claim's inbound is where its fulls come from and its outbound where its spent
// carriers go; a produce claim's inbound is where it draws empties.
func seedRoleClaim(t *testing.T, db *store.DB, proc, node string, role protocol.ClaimRole, payload, inbound, outbound string) {
	t.Helper()
	testutil.MustNoErr(t, db.ReplacePlantClaims(proc,
		[]plantclaims.StyleRow{{ProcessID: proc, StyleID: "STY", ConfigGen: 1, IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: proc, StyleID: "STY", CoreNodeName: node,
			Role: role, SwapMode: protocol.SwapModeSimple, PayloadCode: payload,
			InboundSource: inbound, OutboundDestination: outbound}}, 0),
		"seed claim "+proc)
}

func onDeckOrders(t *testing.T, db *store.DB, binID int64) []*orders.Order {
	t.Helper()
	ords, err := db.ListOrdersByBin(binID, 20)
	testutil.MustNoErr(t, err, "list orders by bin")
	var out []*orders.Order
	for _, o := range ords {
		if o.SourceIntent == dispatch.SourceIntentOnDeck {
			out = append(out, o)
		}
	}
	return out
}

// ── the pins ─────────────────────────────────────────────────────────────────

// PIN, CHANGED BY THE TRIGGER (SHAPE §3.1). At the base tree the watch, on a
// loaded deck after a cancel, with a dispatchable robot at rest and a claim
// naming where the payload is sourced from, created no order — it marked the
// deck and waited for a person. It now orders the bin back into the claim's
// source, and still marks the deck.
func TestPin_Watch_LoadedDeckAfterACancelOrdersTheReturn(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)

	grp, slot := storeGroupWithSlot(t, db, "PIN-W")
	seedClaim(t, db, "PROC-PIN-W", "STYLE-A", "LINE-PIN-W", "PART-PIN-W", grp.Name, true)
	bin, _ := seedCancelledCarry(t, db, "AMR-PIN-W", "PART-PIN-W", "LINE-PIN-W")
	cacheRobot(eng, loadedDispatchable("AMR-PIN-W"))

	eng.sweepCarriedBins()

	ret := onDeckOrders(t, db, bin.ID)
	if len(ret) != 1 || ret[0].DeliveryNode != slot.Name {
		t.Fatalf("on-deck orders = %d, want one return into %s", len(ret), slot.Name)
	}
	if got := binNodeName(t, db, bin.ID); got != bins.CarrierNodePrefix+"AMR-PIN-W" {
		t.Errorf("bin moved to %q on a loaded deck — the return's arrival places it, not the watch", got)
	}
	eng.dropObsMu.Lock()
	_, marked := eng.deckSeenLoaded[bin.ID]
	eng.dropObsMu.Unlock()
	if !marked {
		t.Error("the loaded arm did not record the witness")
	}
}

// PIN: the reconciliation host runs the same watch. sweepStrandedBins places a
// carried bin whose deck it watched go loaded → empty, with no poll involved.
//
// EXPECTED UNCHANGED. The trigger rides both hosts because this is true.
func TestPin_ReconciliationHostRunsTheCarriedWatch(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, testdb.NewTrackingBackend())

	dest := &nodes.Node{Name: "PIN-RECON-DEST", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create destination")
	bin := seedCarried(t, db, "AMR-PIN-R", "")

	cacheRobot(eng, loadedDeck("AMR-PIN-R"))
	eng.sweepStrandedBins()
	cacheRobot(eng, atPoint("AMR-PIN-R", dest.Name, 1, 1))
	eng.sweepStrandedBins()

	if got := binNodeName(t, db, bin.ID); got != dest.Name {
		t.Errorf("bin is at %q, want %s — the reconciliation host's sweep is the watch too", got, dest.Name)
	}
}
