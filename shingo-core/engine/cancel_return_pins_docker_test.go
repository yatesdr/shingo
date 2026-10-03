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
	"shingocore/store/reservations"
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

// holdLaneInDig puts a foreign order in the lane in dig mode, which excludes
// every other hold — the corridor-busy refusal the park exists for.
func holdLaneInDig(t *testing.T, db *store.DB, prefix string, lane *nodes.Node) *orders.Order {
	t.Helper()
	elsewhere := &nodes.Node{Name: prefix + "-ELSEWHERE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(elsewhere), "create the blocker's destination")
	blocker := &orders.Order{
		EdgeUUID: prefix + "-blocker", StationID: "edge.test", OrderType: "move",
		Status: protocol.StatusInTransit, Quantity: 1, DeliveryNode: elsewhere.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(blocker), "create blocking order")
	testutil.MustNoErr(t, reservations.AcquireLanes(db.DB, blocker.ID, reservations.ModeDig,
		"test-blocker", lane.ID), "blocker takes the lane in dig mode")
	return blocker
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

// PIN, CHANGED BY THE DOOR EXTRACTION (SHAPE §3.4). At the base tree a PARKED
// (queued) recovery order refused the next press as "already in flight", exactly
// as a dispatched one did — and a parked order never goes terminal on its own,
// so nothing released it. The button is now that releaser: the press cancels
// the parked order and runs the ladder afresh (here it parks again, behind the
// same held lane). A dispatched one still refuses
// (TestRecoverCarriedBin_SecondCallIsRefused).
func TestPin_RecoverCarriedBin_PressSupersedesAParkedRecovery(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	lane, slot := laneWithSlot(t, db, "PIN-PARK")
	bin := seedCarried(t, db, "AMR-PIN-PARK", slot.Name)
	cacheRobot(eng, dispatchableRobot("AMR-PIN-PARK"))
	holdLaneInDig(t, db, "PIN-PARK", lane)

	parked, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
	testutil.MustNoErr(t, err, "first press parks")
	got, err := db.GetOrder(parked.ID)
	testutil.MustNoErr(t, err, "re-read the parked order")
	if !protocol.IsAcquiring(got.Status) {
		t.Fatalf("setup: the first recovery order is %s, want it parked", got.Status)
	}

	second, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
	testutil.MustNoErr(t, err, "second press on a parked recovery")
	if second.ID == parked.ID {
		t.Fatal("the second press returned the parked order instead of superseding it")
	}
	old, err := db.GetOrder(parked.ID)
	testutil.MustNoErr(t, err, "re-read the superseded order")
	if old.Status != protocol.StatusCancelled {
		t.Errorf("the superseded parked order is %s, want cancelled", old.Status)
	}
	if live := onDeckOrders(t, db, bin.ID); countLive(live) != 1 {
		t.Errorf("%d live on-deck orders after the press, want exactly the new one", countLive(live))
	}
}

func countLive(ords []*orders.Order) int {
	n := 0
	for _, o := range ords {
		if !protocol.IsTerminal(o.Status) {
			n++
		}
	}
	return n
}

// PIN, CHANGED BY THE DOOR EXTRACTION (SHAPE §3.4). At the base tree tier 1
// read the NEWEST order that ever named the bin — a dead recovery order of the
// button's own included — so a recovery that failed at X sent the next press to
// X as "where it was going". It now reads the carrier the door hands it, the
// newest NON-on-deck order.
func TestPin_RecoverCarriedBin_Tier1ReadsTheCarrierNotADeadRecovery(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	orig := &nodes.Node{Name: "PIN-T1-ORIG", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(orig), "create the carrier's destination")
	dead := &nodes.Node{Name: "PIN-T1-DEAD", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dead), "create the dead recovery's destination")
	bin := seedCarried(t, db, "AMR-PIN-T1", orig.Name)

	failed := &orders.Order{
		EdgeUUID: "pin-t1-dead", OrderType: dispatch.OrderTypeMove, Status: protocol.StatusFailed,
		Quantity: 1, BinID: &bin.ID, SourceNode: bins.CarrierNodePrefix + "AMR-PIN-T1",
		DeliveryNode: dead.Name, SourceIntent: dispatch.SourceIntentOnDeck,
		OriginClass: protocol.OriginClassNoDemand,
	}
	testutil.MustNoErr(t, db.CreateOrder(failed), "create a dead recovery order")
	cacheRobot(eng, dispatchableRobot("AMR-PIN-T1"))

	order, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
	testutil.MustNoErr(t, err, "recover")
	if order.DeliveryNode != orig.Name {
		t.Errorf("tier 1 chose %q, want the carrier's destination %s — %s is a dead recovery's",
			order.DeliveryNode, orig.Name, dead.Name)
	}
}

// PIN: the watch, on a loaded deck after a cancel, with a dispatchable robot at
// rest and a claim naming where the payload is sourced from, creates no order —
// it marks the deck and waits.
//
// EXPECTED TO CHANGE (SHAPE §3.1): this is the trigger. After the build the
// same fixture yields one return order into the claim's source group.
func TestPin_Watch_LoadedDeckAfterACancelCreatesNoOrder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)

	grp, _ := storeGroupWithSlot(t, db, "PIN-W")
	seedClaim(t, db, "PROC-PIN-W", "STYLE-A", "LINE-PIN-W", "PART-PIN-W", grp.Name, true)
	bin, _ := seedCancelledCarry(t, db, "AMR-PIN-W", "PART-PIN-W", "LINE-PIN-W")
	cacheRobot(eng, loadedDispatchable("AMR-PIN-W"))

	eng.sweepCarriedBins()

	assertNoRecoveryOrder(t, db, bin.ID)
	if got := binNodeName(t, db, bin.ID); got != bins.CarrierNodePrefix+"AMR-PIN-W" {
		t.Errorf("bin moved to %q on a loaded deck", got)
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
