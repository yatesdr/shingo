//go:build docker

package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"

	"shingo/protocol/eventbus"
)

// cancel_return_docker_test.go — the cancel-return policy end to end through the
// watch: a bin left on a robot's deck by a CANCELLED order goes back to where
// the plant sources it from, once per episode, and never on a guess.
//
// Fixtures live in cancel_return_pins_docker_test.go.

// ── fixtures ────────────────────────────────────────────────────────────────

// laneGroup is a node group of nLanes lanes, each depth slots deep (depth 1 is
// the mouth, the slot a robot reaches first).
func laneGroup(t *testing.T, db *store.DB, prefix string, nLanes, depth int) (*nodes.Node, []*nodes.Node, [][]*nodes.Node) {
	t.Helper()
	ngrpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "get NGRP type")
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	testutil.MustNoErr(t, err, "get LANE type")
	grp := &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &ngrpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")
	var lanes []*nodes.Node
	var slots [][]*nodes.Node
	for i := 0; i < nLanes; i++ {
		lane := &nodes.Node{Name: fmt.Sprintf("%s-L%d", prefix, i), NodeTypeID: &laneType.ID,
			ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
		testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
		var row []*nodes.Node
		for d := 1; d <= depth; d++ {
			dd := d
			s := &nodes.Node{Name: fmt.Sprintf("%s-L%d-S%d", prefix, i, d), ParentID: &lane.ID, Enabled: true, Depth: &dd}
			testutil.MustNoErr(t, db.CreateNode(s), "create slot")
			row = append(row, s)
		}
		lanes = append(lanes, lane)
		slots = append(slots, row)
	}
	return grp, lanes, slots
}

// binAt stands a bin of payload on a node.
func binAt(t *testing.T, db *store.DB, at *nodes.Node, payload string) *bins.Bin {
	t.Helper()
	bt := &bins.BinType{Code: "RES-" + at.Name, Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create resident bin type")
	b := &bins.Bin{BinTypeID: bt.ID, Label: "resident-" + at.Name, NodeID: &at.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(b), "create resident bin")
	_, err := db.DB.Exec(`UPDATE bins SET payload_code=$1 WHERE id=$2`, payload, b.ID)
	testutil.MustNoErr(t, err, "stamp resident payload")
	return b
}

// theReturn is the one on-deck order the policy created for the bin.
func theReturn(t *testing.T, db *store.DB, binID int64) *orders.Order {
	t.Helper()
	ords := onDeckOrders(t, db, binID)
	if len(ords) != 1 {
		t.Fatalf("on-deck orders for bin %d = %d, want exactly one return", binID, len(ords))
	}
	return ords[0]
}

func lastBinAction(t *testing.T, db *store.DB, binID int64) (action, actor, detail string) {
	t.Helper()
	err := db.DB.QueryRow(`SELECT action, actor, detail FROM recovery_actions
		WHERE target_type='bin' AND target_id=$1 ORDER BY id DESC LIMIT 1`, binID).Scan(&action, &actor, &detail)
	testutil.MustNoErr(t, err, "read last recovery action")
	return action, actor, detail
}

func countBinActions(t *testing.T, db *store.DB, binID int64, action string) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT count(*) FROM recovery_actions
		WHERE target_type='bin' AND target_id=$1 AND action=$2`, binID, action).Scan(&n), "count actions")
	return n
}

// setCarrierTerminalRow rewrites the carrier's terminal history row: what a
// given producer leaves behind.
func setCarrierTerminalRow(t *testing.T, db *store.DB, ord *orders.Order, status protocol.Status, code, detail string, at time.Time) {
	t.Helper()
	_, err := db.DB.Exec(`UPDATE orders SET status=$1 WHERE id=$2`, string(status), ord.ID)
	testutil.MustNoErr(t, err, "set carrier status")
	_, err = db.DB.Exec(`DELETE FROM order_history WHERE order_id=$1`, ord.ID)
	testutil.MustNoErr(t, err, "clear carrier history")
	_, err = db.DB.Exec(`INSERT INTO order_history (order_id, status, detail, code, created_at)
		VALUES ($1,$2,$3,$4,$5)`, ord.ID, string(status), detail, code, at)
	testutil.MustNoErr(t, err, "write carrier terminal row")
	ord.Status = status
}

func forgetReturnAttempts(e *Engine) {
	e.dropObsMu.Lock()
	e.returnAttempted = nil
	e.dropObsMu.Unlock()
}

// ── 1. the ordinary case ────────────────────────────────────────────────────

// A supply bin cancelled mid-carry goes back into the group its claim sources
// it from — and LKND's consolidation puts it in the lane already holding that
// payload, which is the "slot it left" tie-break written once for every store.
func TestCancelReturn_SupplyBinGoesBackToItsClaimSourceGroup(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)

	grp, _, slots := laneGroup(t, db, "CR1", 2, 2)
	binAt(t, db, slots[0][1], "CR1-P") // lane 0 already holds the payload, at the back
	seedClaim(t, db, "PROC-CR1", "STY", "LINE-CR1", "CR1-P", grp.Name, true)
	bin, carrier := seedCancelledCarry(t, db, "AMR-CR1", "CR1-P", "LINE-CR1")
	cacheRobot(eng, loadedDispatchable("AMR-CR1"))

	eng.sweepCarriedBins()

	ret := theReturn(t, db, bin.ID)
	if ret.DeliveryNode != slots[0][0].Name {
		t.Errorf("return goes to %q, want %s — the free mouth of the lane already holding CR1-P", ret.DeliveryNode, slots[0][0].Name)
	}
	if ret.RecoversOrderID == nil || *ret.RecoversOrderID != carrier.ID {
		t.Errorf("recovers_order_id = %v, want the cancelled order %d", ret.RecoversOrderID, carrier.ID)
	}
	reqs := backend.CreateRequests()
	if len(reqs) != 1 || reqs[0].Vehicle != "AMR-CR1" || len(reqs[0].Blocks) != 1 {
		t.Fatalf("fleet requests = %+v, want one single-block request pinned to AMR-CR1", reqs)
	}
	action, actor, detail := lastBinAction(t, db, bin.ID)
	if action != actionReturnOrdered || actor != cancelReturnActor {
		t.Errorf("audit = %s by %s, want %s by %s", action, actor, actionReturnOrdered, cancelReturnActor)
	}
	if !strings.Contains(detail, grp.Name) {
		t.Errorf("audit detail %q does not name the source it returned to", detail)
	}
	// A DISPATCHED return refuses the button, exactly as a dispatched recovery does.
	if _, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test"); err == nil ||
		!strings.Contains(err.Error(), "already in flight") {
		t.Errorf("Recover on a dispatched return: %v, want 'already in flight'", err)
	}
}

// ── 2. claims, not node_payloads ────────────────────────────────────────────

// node_payloads is a store restriction that defaults to allow; no finder
// sources from it. A claim naming G wins over a declaration naming G2, and a
// payload no claim names holds — even with a declaration.
func TestCancelReturn_TheClaimDecidesNotNodePayloads(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	g, gSlot := storeGroupWithSlot(t, db, "CR2A")
	_, g2Slot := storeGroupWithSlot(t, db, "CR2B")
	pa := seedRecoveryPayload(t, db, "CR2-P")
	testutil.MustNoErr(t, db.AssignPayloadToNode(g2Slot.ID, pa.ID), "declare the payload at G2")
	seedClaim(t, db, "PROC-CR2", "STY", "LINE-CR2", "CR2-P", g.Name, true)

	bin, _ := seedCancelledCarry(t, db, "AMR-CR2A", "CR2-P", "LINE-CR2")
	cacheRobot(eng, loadedDispatchable("AMR-CR2A"))
	eng.sweepCarriedBins()
	if ret := theReturn(t, db, bin.ID); ret.DeliveryNode != gSlot.Name {
		t.Errorf("return goes to %q, want %s — the claim's source, not node_payloads' G2", ret.DeliveryNode, gSlot.Name)
	}

	pq := seedRecoveryPayload(t, db, "CR2-Q")
	testutil.MustNoErr(t, db.AssignPayloadToNode(g2Slot.ID, pq.ID), "declare Q at G2 too")
	lone, _ := seedCancelledCarry(t, db, "AMR-CR2B", "CR2-Q", "LINE-CR2")
	cacheRobot(eng, loadedDispatchable("AMR-CR2B"))
	eng.sweepCarriedBins()
	assertNoRecoveryOrder(t, db, lone.ID)
	action, _, detail := lastBinAction(t, db, lone.ID)
	if action != actionReturnHeld || !strings.Contains(detail, "no claim reports where CR2-Q is sourced from") {
		t.Errorf("audit = %s %q, want a hold saying no claim reports where CR2-Q is sourced from", action, detail)
	}
}

// ── 3. the walk ─────────────────────────────────────────────────────────────

// The cancelled order's own process's source first; full, the next claim's
// source for the same payload. A claim naming a LANE is refused: a lane is not a
// source (the config checks refuse it at save), and a need naming a lane
// searches that lane only, so a bin returned anywhere else in its group is one
// that line never finds.
func TestCancelReturn_WalkFallsToTheNextSourceAndALaneSourceIsRefused(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	own, ownSlot := storeGroupWithSlot(t, db, "CR3-OWN")
	binAt(t, db, ownSlot, "CR3-X") // the own process's source is full
	other, otherSlot := storeGroupWithSlot(t, db, "CR3-OTH")
	seedClaim(t, db, "PROC-CR3A", "STY", "LINE-CR3A", "CR3-P", own.Name, false)
	seedClaim(t, db, "PROC-CR3B", "STY", "LINE-CR3B", "CR3-P", other.Name, false)

	bin, _ := seedCancelledCarry(t, db, "AMR-CR3A", "CR3-P", "LINE-CR3A")
	cacheRobot(eng, loadedDispatchable("AMR-CR3A"))
	eng.sweepCarriedBins()
	if ret := theReturn(t, db, bin.ID); ret.DeliveryNode != otherSlot.Name {
		t.Errorf("return goes to %q, want %s — own source full, the next claim's source", ret.DeliveryNode, otherSlot.Name)
	}

	// The lane-named source: the claim names lane 1 of a two-lane group with
	// room in both lanes. Held, never resolved through the group.
	_, lanes, _ := laneGroup(t, db, "CR3-LN", 2, 2)
	seedClaim(t, db, "PROC-CR3L", "STY", "LINE-CR3L", "CR3-LP", lanes[1].Name, true)
	lb, _ := seedCancelledCarry(t, db, "AMR-CR3L", "CR3-LP", "LINE-CR3L")
	cacheRobot(eng, loadedDispatchable("AMR-CR3L"))
	eng.sweepCarriedBins()
	assertNoRecoveryOrder(t, db, lb.ID)
	if action, _, detail := lastBinAction(t, db, lb.ID); action != actionReturnHeld ||
		!strings.Contains(detail, "a lane is not a source; name its group") {
		t.Errorf("audit = %s %q, want a hold saying a lane is not a source", action, detail)
	}
}

// ── 4. evacuations, and empties by declaration ──────────────────────────────

// Off a press, not off storage: a payload bin goes to its claim's source.
func TestCancelReturn_AnEvacuatedPayloadGoesToItsClaimSource(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	press := &nodes.Node{Name: "CR4-PRESS", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(press), "create press")
	src, srcSlot := storeGroupWithSlot(t, db, "CR4-SRC")
	seedClaim(t, db, "PROC-CR4", "STY", "LINE-CR4", "CR4-P", src.Name, true)
	full, _ := evacOff(t, db, "AMR-CR4F", "CR4-P", press)
	cacheRobot(eng, loadedDispatchable("AMR-CR4F"))
	eng.sweepCarriedBins()
	if ret := theReturn(t, db, full.ID); ret.DeliveryNode != srcSlot.Name {
		t.Errorf("payload evac returns to %q, want its claim source %s", ret.DeliveryNode, srcSlot.Name)
	}
}

// An EMPTY goes where the claims DECLARE empties go, and nowhere else (owner,
// 2026-10-03). Each case on its own database: the walk is plant-wide by design
// ("another area where they are used"), so cases sharing one would find each
// other's declarations.
func TestCancelReturn_AnEmptyGoesWhereClaimsDeclareEmptiesGo(t *testing.T) {
	t.Parallel()

	t.Run("off a consumer line: that line's declared empties-out", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		eng := newTestEngine(t, db, testdb.NewTrackingBackend())
		line := &nodes.Node{Name: "CRE1-LINE", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(line), "create line")
		out, outSlot := storeGroupWithSlot(t, db, "CRE1-OUT")
		seedRoleClaim(t, db, "PROC-CRE1", line.Name, protocol.ClaimRoleConsume, "CRE1-P", "", out.Name)
		empty, _ := evacOff(t, db, "AMR-CRE1", "", line)
		cacheRobot(eng, loadedDispatchable("AMR-CRE1"))
		eng.sweepCarriedBins()
		if ret := theReturn(t, db, empty.ID); ret.DeliveryNode != outSlot.Name {
			t.Errorf("empty off the line returns to %q, want %s, the line's declared empties-out", ret.DeliveryNode, outSlot.Name)
		}
		if _, _, detail := lastBinAction(t, db, empty.ID); !strings.Contains(detail, "a declared place for an empty") {
			t.Errorf("audit detail %q does not say the place was declared", detail)
		}
	})

	t.Run("headed to a press: the press's declared empties-in", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		eng := newTestEngine(t, db, testdb.NewTrackingBackend())
		in, inSlot := storeGroupWithSlot(t, db, "CRE2-IN")
		seedRoleClaim(t, db, "PROC-CRE2", "CRE2-PRESS", protocol.ClaimRoleProduce, "CRE2-P", in.Name, "")
		empty, _ := seedCancelledCarry(t, db, "AMR-CRE2", "", "CRE2-PRESS")
		cacheRobot(eng, loadedDispatchable("AMR-CRE2"))
		eng.sweepCarriedBins()
		if ret := theReturn(t, db, empty.ID); ret.DeliveryNode != inSlot.Name {
			t.Errorf("empty headed to the press returns to %q, want %s, the press's declared empties-in", ret.DeliveryNode, inSlot.Name)
		}
	})

	t.Run("no claim declares an empties place: hold", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		eng := newTestEngine(t, db, testdb.NewTrackingBackend())
		// A maintained group supporting the line is NOT a declaration.
		line := &nodes.Node{Name: "CRE3-LINE", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(line), "create line")
		empty, _ := evacOff(t, db, "AMR-CRE3", "", line)
		bank, _ := storeGroupWithSlot(t, db, "CRE3-BANK")
		testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{GroupNodeID: bank.ID, BinTypeID: empty.BinTypeID, Want: 3}), "level")
		testutil.MustNoErr(t, db.SetMaintainSupports(bank.ID, []int64{line.ID}), "supports the line")
		cacheRobot(eng, loadedDispatchable("AMR-CRE3"))
		eng.sweepCarriedBins()
		assertNoRecoveryOrder(t, db, empty.ID)
		if action, _, detail := lastBinAction(t, db, empty.ID); action != actionReturnHeld ||
			!strings.Contains(detail, "no claim declares a place for an empty") {
			t.Errorf("audit = %s %q, want a hold saying no claim declares a place", action, detail)
		}
	})

	t.Run("declared but fenced: hold naming the fence", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		eng := newTestEngine(t, db, testdb.NewTrackingBackend())
		press := &nodes.Node{Name: "CRE4-PRESS", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(press), "create press")
		other := &nodes.Node{Name: "CRE4-OTHER", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(other), "create the other press")
		strict, _ := storeGroupWithSlot(t, db, "CRE4-STRICT")
		testutil.MustNoErr(t, db.SetMaintainSupports(strict.ID, []int64{other.ID}), "supports another press only")
		testutil.MustNoErr(t, db.SetNodeProperty(strict.ID, nodes.PropStrictSourcing, "on"), "strict")
		seedRoleClaim(t, db, "PROC-CRE4", press.Name, protocol.ClaimRoleProduce, "CRE4-P", strict.Name, "")
		empty, _ := seedCancelledCarry(t, db, "AMR-CRE4", "", press.Name)
		cacheRobot(eng, loadedDispatchable("AMR-CRE4"))
		eng.sweepCarriedBins()
		assertNoRecoveryOrder(t, db, empty.ID)
		if action, _, detail := lastBinAction(t, db, empty.ID); action != actionReturnHeld ||
			!strings.Contains(detail, "a strict group that does not support "+press.Name) {
			t.Errorf("audit = %s %q, want a hold naming the fence", action, detail)
		}
	})
}

// ── 5. E13: the bin on the deck, not the order's part ───────────────────────

// A single-robot swap cancelled after its fourth step has the OLD bin on the
// deck while the order names the NEW part. The return follows the bin.
func TestCancelReturn_SwapCancelledAfterTheLiftReturnsTheOldPayload(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	oldSrc, oldSlot := storeGroupWithSlot(t, db, "CR5-OLD")
	newSrc, _ := storeGroupWithSlot(t, db, "CR5-NEW")
	seedClaim(t, db, "PROC-CR5", "STY-OLD", "LINE-CR5", "CR5-OLDP", oldSrc.Name, false)
	seedClaim(t, db, "PROC-CR5N", "STY-NEW", "LINE-CR5", "CR5-NEWP", newSrc.Name, true)

	bin, carrier := seedCancelledCarry(t, db, "AMR-CR5", "CR5-OLDP", "LINE-CR5")
	_, err := db.DB.Exec(`UPDATE orders SET payload_code='CR5-NEWP', order_type='complex' WHERE id=$1`, carrier.ID)
	testutil.MustNoErr(t, err, "the order names the new part")
	cacheRobot(eng, loadedDispatchable("AMR-CR5"))

	eng.sweepCarriedBins()
	if ret := theReturn(t, db, bin.ID); ret.DeliveryNode != oldSlot.Name {
		t.Errorf("return goes to %q, want %s — the OLD payload's source; the new part's InboundSource is for the bin "+
			"the swap was bringing, not the one on the deck", ret.DeliveryNode, oldSlot.Name)
	}
}

// ── 6. loader positions ─────────────────────────────────────────────────────

// A claim whose source is a loader's position: that loader's home for the
// payload when it is free, else one of that loader's buffers.
func TestCancelReturn_LoaderPositionSourceTakesHomeThenBuffer(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	loader := func(prefix, payload string) (home, buffer *nodes.Node) {
		home = &nodes.Node{Name: prefix + "-HOME", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(home), "create home")
		buffer = &nodes.Node{Name: prefix + "-BUF", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(buffer), "create buffer")
		id, err := db.CreateLoader(loaders.Loader{Name: prefix, Role: "produce",
			Layout: protocol.LoaderLayoutDedicatedPositions, Replenishment: "operator"})
		testutil.MustNoErr(t, err, "create loader")
		testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: home.ID,
			PayloadCode: payload, Kind: loaders.HomeKindHome}), "pin the home")
		testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: buffer.ID,
			Kind: loaders.HomeKindBuffer}), "add a buffer")
		return home, buffer
	}

	freeHome, _ := loader("CR6A", "CR6-P")
	seedClaim(t, db, "PROC-CR6A", "STY", "LINE-CR6A", "CR6-P", freeHome.Name, true)
	a, _ := seedCancelledCarry(t, db, "AMR-CR6A", "CR6-P", "LINE-CR6A")
	cacheRobot(eng, loadedDispatchable("AMR-CR6A"))
	eng.sweepCarriedBins()
	if ret := theReturn(t, db, a.ID); ret.DeliveryNode != freeHome.Name {
		t.Errorf("return goes to %q, want the free home %s", ret.DeliveryNode, freeHome.Name)
	}

	busyHome, buf := loader("CR6B", "CR6-Q")
	binAt(t, db, busyHome, "CR6-Q")
	seedClaim(t, db, "PROC-CR6B", "STY", "LINE-CR6B", "CR6-Q", busyHome.Name, true)
	b, _ := seedCancelledCarry(t, db, "AMR-CR6B", "CR6-Q", "LINE-CR6B")
	cacheRobot(eng, loadedDispatchable("AMR-CR6B"))
	eng.sweepCarriedBins()
	if ret := theReturn(t, db, b.ID); ret.DeliveryNode != buf.Name {
		t.Errorf("return goes to %q, want that loader's buffer %s — its home is occupied", ret.DeliveryNode, buf.Name)
	}
}

// ── 7. the lane-held park and its releasers ─────────────────────────────────

// parkedReturn returns a bin into lane 0 of a two-lane group while lane 0 is
// held OUTBOUND by a foreign order: the resolver offers the lane (only a dig
// hides one), and inbound admission refuses it, so the return parks.
func parkedReturn(t *testing.T, db *store.DB, eng *Engine, prefix string) (bin *bins.Bin, ret, blocker *orders.Order, lanes []*nodes.Node, slots [][]*nodes.Node) {
	t.Helper()
	grp, lanes, slots := laneGroup(t, db, prefix, 2, 2)
	binAt(t, db, slots[0][1], prefix+"-P")
	seedClaim(t, db, "PROC-"+prefix, "STY", "LINE-"+prefix, prefix+"-P", grp.Name, true)
	bin, _ = seedCancelledCarry(t, db, "AMR-"+prefix, prefix+"-P", "LINE-"+prefix)
	cacheRobot(eng, loadedDispatchable("AMR-"+prefix))

	elsewhere := &nodes.Node{Name: prefix + "-ELSEWHERE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(elsewhere), "create blocker destination")
	blocker = &orders.Order{EdgeUUID: prefix + "-blocker", StationID: "edge.test", OrderType: "move",
		Status: protocol.StatusInTransit, Quantity: 1, DeliveryNode: elsewhere.Name}
	testutil.MustNoErr(t, db.CreateOrder(blocker), "create blocker")
	testutil.MustNoErr(t, reservations.AcquireLanes(db.DB, blocker.ID, reservations.ModeOutbound,
		"test-blocker", lanes[0].ID), "blocker holds lane 0 outbound")

	eng.sweepCarriedBins()
	ret = theReturn(t, db, bin.ID)
	if !protocol.IsAcquiring(ret.Status) {
		t.Fatalf("setup: the return is %s aimed at %s, want it parked behind the held lane", ret.Status, ret.DeliveryNode)
	}
	return bin, ret, blocker, lanes, slots
}

func TestCancelReturn_LaneHeldParksAndTheScannerDispatchesIt(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)
	_, ret, blocker, lanes, _ := parkedReturn(t, db, eng, "CR7A")

	testutil.MustNoErr(t, reservations.ReleaseLane(db.DB, blocker.ID, lanes[0].ID), "the lane clears")
	eng.fulfillment.RunOnce()

	done, err := db.GetOrder(ret.ID)
	testutil.MustNoErr(t, err, "re-read the return")
	if protocol.IsAcquiring(done.Status) || done.VendorOrderID == "" {
		t.Fatalf("return is %s (vendor %q) with a free lane — nothing re-drove the park", done.Status, done.VendorOrderID)
	}
	reqs := backend.CreateRequests()
	if last := reqs[len(reqs)-1]; last.Vehicle != "AMR-CR7A" {
		t.Errorf("scanner dispatched the return with Vehicle=%q, want the pin", last.Vehicle)
	}
}

func TestCancelReturn_TheButtonSupersedesAParkedReturn(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	bin, ret, _, _, _ := parkedReturn(t, db, eng, "CR7B")

	// The button's ladder answers at tier 1, the carrier's own destination.
	line := &nodes.Node{Name: "LINE-CR7B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(line), "the carrier's destination exists and is empty")
	order, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
	testutil.MustNoErr(t, err, "the button on a parked return")
	old, err := db.GetOrder(ret.ID)
	testutil.MustNoErr(t, err, "re-read the parked return")
	if old.Status != protocol.StatusCancelled {
		t.Errorf("parked return is %s after the press, want cancelled", old.Status)
	}
	if order.RecoversOrderID != nil {
		t.Errorf("the button's order recovers %v — a press recovers no order in particular", *order.RecoversOrderID)
	}
	// The watch does not try again: the episode had its attempt.
	eng.sweepCarriedBins()
	if n := countLive(onDeckOrders(t, db, bin.ID)); n != 1 {
		t.Errorf("%d live on-deck orders after the press and a sweep, want the button's one", n)
	}
}

func TestCancelReturn_ADigWhileParkedReAimsTheReturn(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	_, ret, blocker, lanes, slots := parkedReturn(t, db, eng, "CR7C")

	// The blocker's hold becomes a dig: the return's lane is being excavated.
	testutil.MustNoErr(t, reservations.ReleaseLane(db.DB, blocker.ID, lanes[0].ID), "drop the outbound hold")
	testutil.MustNoErr(t, reservations.AcquireLanes(db.DB, blocker.ID, reservations.ModeDig,
		"test-dig", lanes[0].ID), "dig lane 0")
	eng.fulfillment.RunOnce()

	done, err := db.GetOrder(ret.ID)
	testutil.MustNoErr(t, err, "re-read the return")
	if done.DeliveryNode != slots[1][1].Name {
		t.Errorf("return aimed at %q after the dig, want %s — Core chose the slot, so a dig re-aims it", done.DeliveryNode, slots[1][1].Name)
	}
}

// ── 8. once per episode ─────────────────────────────────────────────────────

func TestCancelReturn_OneAttemptPerEpisode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	// FAULTED AT THE CANCEL: nothing happens, and nothing is spent.
	g, _ := storeGroupWithSlot(t, db, "CR8")
	seedClaim(t, db, "PROC-CR8", "STY", "LINE-CR8", "CR8-P", g.Name, true)
	bin, carrier := seedCancelledCarry(t, db, "AMR-CR8", "CR8-P", "LINE-CR8")
	faulted := loadedDispatchable("AMR-CR8")
	faulted.IsError = true
	cacheRobot(eng, faulted)
	eng.sweepCarriedBins()
	assertNoRecoveryOrder(t, db, bin.ID)
	if eng.returnTried(bin.ID, carrier.ID) {
		t.Error("a faulted robot spent the episode's attempt — the gate must retry free")
	}
	// The fault clears: one attempt, on that status change, and only one.
	cacheRobot(eng, loadedDispatchable("AMR-CR8"))
	eng.sweepCarriedBins()
	eng.sweepStrandedBins()
	if n := len(onDeckOrders(t, db, bin.ID)); n != 1 {
		t.Errorf("%d on-deck orders, want exactly one return", n)
	}

	// A HOLD IS FINAL for the episode.
	held, _ := seedCancelledCarry(t, db, "AMR-CR8H", "CR8-NOCLAIM", "LINE-CR8")
	cacheRobot(eng, loadedDispatchable("AMR-CR8H"))
	eng.sweepCarriedBins()
	eng.sweepCarriedBins()
	if n := countBinActions(t, db, held.ID, actionReturnHeld); n != 1 {
		t.Errorf("%d hold rows after two sweeps, want one", n)
	}

	// A RESTART after a failed return does not retry: the order table, not the
	// map, is the record.
	failed, fcarrier := seedCancelledCarry(t, db, "AMR-CR8R", "CR8-P", "LINE-CR8")
	rec := fcarrier.ID
	dead := &orders.Order{EdgeUUID: "cr8-dead-return", OrderType: dispatch.OrderTypeMove, Status: protocol.StatusFailed,
		Quantity: 1, BinID: &failed.ID, SourceNode: failed.NodeName, DeliveryNode: "CR8-SOMEWHERE",
		SourceIntent: dispatch.SourceIntentOnDeck, OriginClass: protocol.OriginClassNoDemand, RecoversOrderID: &rec}
	testutil.MustNoErr(t, db.CreateOrder(dead), "a return that failed before the restart")
	forgetReturnAttempts(eng)
	cacheRobot(eng, loadedDispatchable("AMR-CR8R"))
	eng.sweepCarriedBins()
	if n := len(onDeckOrders(t, db, failed.ID)); n != 1 {
		t.Errorf("%d on-deck orders after the restart, want only the failed one", n)
	}
}

// ── 9. E15b: keyed on the episode ───────────────────────────────────────────

// A FAILED carrier is not returned, and that decline is about that carrier. A
// later cancelled order for the same bin is a new episode.
func TestCancelReturn_TheRecordIsKeyedOnTheCarrier(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	g, slot := storeGroupWithSlot(t, db, "CR9")
	seedClaim(t, db, "PROC-CR9", "STY", "LINE-CR9", "CR9-P", g.Name, true)
	bin, first := seedCancelledCarry(t, db, "AMR-CR9", "CR9-P", "LINE-CR9")
	setCarrierTerminalRow(t, db, first, protocol.StatusFailed, string(protocol.TermGraceTimeout), "grace timeout", clock.Now().UTC())
	cacheRobot(eng, loadedDispatchable("AMR-CR9"))
	eng.sweepCarriedBins()
	assertNoRecoveryOrder(t, db, bin.ID)

	second := &orders.Order{EdgeUUID: "cr9-second", StationID: "edge.test", OrderType: "retrieve",
		Status: protocol.StatusCancelled, Quantity: 1, DeliveryNode: "LINE-CR9", ProcessNode: "LINE-CR9",
		PayloadCode: "CR9-P", BinID: &bin.ID}
	testutil.MustNoErr(t, db.CreateOrder(second), "a later cancelled order for the same bin")
	eng.sweepCarriedBins()
	ret := theReturn(t, db, bin.ID)
	if ret.RecoversOrderID == nil || *ret.RecoversOrderID != second.ID || ret.DeliveryNode != slot.Name {
		t.Errorf("return %+v, want one recovering order %d into %s", ret, second.ID, slot.Name)
	}
}

// ── 10. E15a: two bins, one deck ────────────────────────────────────────────

func TestCancelReturn_TwoBinsOnOneCarrierNodeHold(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	g, _ := storeGroupWithSlot(t, db, "CR10")
	seedClaim(t, db, "PROC-CR10", "STY", "LINE-CR10", "CR10-P", g.Name, true)
	lifted, _ := seedCancelledCarry(t, db, "AMR-CR10", "CR10-P", "LINE-CR10")
	stale := &bins.Bin{BinTypeID: lifted.BinTypeID, Label: "stale-cr10", NodeID: lifted.NodeID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(stale), "a stale bin left on the same carrier node")
	cacheRobot(eng, loadedDispatchable("AMR-CR10"))

	eng.sweepCarriedBins()

	assertNoRecoveryOrder(t, db, lifted.ID)
	assertNoRecoveryOrder(t, db, stale.ID)
	if action, _, detail := lastBinAction(t, db, lifted.ID); action != actionReturnHeld || !strings.Contains(detail, "another bin") {
		t.Errorf("audit = %s %q, want a hold naming the other bin on the node", action, detail)
	}
}

// ── 11. E14: only a deck that is certainly loaded, on a robot that let go ───

func TestCancelReturn_UncertainJackOrBusyRobotOrdersNothing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	g, _ := storeGroupWithSlot(t, db, "CR11")
	seedClaim(t, db, "PROC-CR11", "STY", "LINE-CR11", "CR11-P", g.Name, true)
	bin, _ := seedCancelledCarry(t, db, "AMR-CR11", "CR11-P", "LINE-CR11")

	moving := loadedDispatchable("AMR-CR11")
	moving.JackState = 2 // mid-travel: no certain reading
	cacheRobot(eng, moving)
	eng.sweepCarriedBins()
	assertNoRecoveryOrder(t, db, bin.ID)

	busy := loadedDispatchable("AMR-CR11")
	busy.Busy = true // the terminate did not take
	cacheRobot(eng, busy)
	eng.sweepCarriedBins()
	assertNoRecoveryOrder(t, db, bin.ID)

	// And it was a stand-down, not a decline: the robot lets go, the return goes.
	cacheRobot(eng, loadedDispatchable("AMR-CR11"))
	eng.sweepCarriedBins()
	theReturn(t, db, bin.ID)
}

// ── 12. E12 and E15c: no deadlock, no double ────────────────────────────────

// A cancel emitted from INSIDE a fulfillment scan — under scanMu, the way a
// reshuffle dissolve cancels — runs the cancel handler synchronously under that
// lock. Nothing cancel-return does may run there: the bin is parked on its
// carrier by branch B, and the return comes from the watch afterwards.
func TestCancelReturn_ACancelUnderTheScannerLockDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	g, _ := storeGroupWithSlot(t, db, "CR12")
	seedClaim(t, db, "PROC-CR12", "STY", "LINE-CR12", "CR12-P", g.Name, true)
	// A live order carrying a lifted bin at _TRANSIT.
	bin, ord := seedStranded(t, db, "AMR-CR12")
	_, err := db.DB.Exec(`UPDATE bins SET payload_code='CR12-P', claimed_by=$1 WHERE id=$2`, ord.ID, bin.ID)
	testutil.MustNoErr(t, err, "the order holds the bin")
	_, err = db.DB.Exec(`UPDATE orders SET status='in_transit', delivery_node='LINE-CR12', process_node='LINE-CR12',
		payload_code='CR12-P' WHERE id=$1`, ord.ID)
	testutil.MustNoErr(t, err, "the order is live")
	cacheRobot(eng, loadedDispatchable("AMR-CR12"))

	// The scan fails a malformed queued order — under scanMu — and this
	// subscriber cancels the carrying order right there.
	var once sync.Once
	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[EventType, OrderFailedEvent]) {
		once.Do(func() {
			live, gerr := db.GetOrder(ord.ID)
			if gerr == nil {
				eng.dispatcher.Lifecycle().CancelOrder(live, "edge.test", "cancelled by test",
					dispatch.CancelCause{Code: protocol.TermOperatorCancelled, Actor: "operator:test"})
			}
		})
	}, EventOrderFailed)
	bad := &orders.Order{EdgeUUID: "cr12-bad", StationID: "edge.test", OrderType: "retrieve",
		Status: protocol.StatusQueued, Quantity: 1, DeliveryNode: "LINE-CR12"}
	testutil.MustNoErr(t, db.CreateOrder(bad), "a queued order the scan will fail")

	done := make(chan struct{})
	go func() { eng.fulfillment.RunOnce(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the scan did not return: a cancel under scanMu deadlocked")
	}
	c, err := db.GetOrder(ord.ID)
	testutil.MustNoErr(t, err, "re-read the carrying order")
	if c.Status != protocol.StatusCancelled {
		t.Fatalf("setup: the carrying order is %s — the in-scan cancel did not happen", c.Status)
	}
	if got := binNodeName(t, db, bin.ID); got != bins.CarrierNodePrefix+"AMR-CR12" {
		t.Fatalf("bin is at %q, want parked on the carrier by branch B", got)
	}
	assertNoRecoveryOrder(t, db, bin.ID)

	eng.sweepCarriedBins()
	theReturn(t, db, bin.ID)
}

// Both hosts at once: the poll's sweep and the reconciliation sweep race on
// the same bin. One return, and no concurrent map write (run under -race).
func TestCancelReturn_TwoHostsMakeOneReturn(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	g, _ := storeGroupWithSlot(t, db, "CR12B")
	seedClaim(t, db, "PROC-CR12B", "STY", "LINE-CR12B", "CR12B-P", g.Name, true)
	bin, _ := seedCancelledCarry(t, db, "AMR-CR12B", "CR12B-P", "LINE-CR12B")
	cacheRobot(eng, loadedDispatchable("AMR-CR12B"))

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); eng.sweepCarriedBins() }()
		go func() { defer wg.Done(); eng.sweepStrandedBins() }()
	}
	wg.Wait()
	if n := len(onDeckOrders(t, db, bin.ID)); n != 1 {
		t.Errorf("%d on-deck orders from two racing hosts, want one", n)
	}
}

// ── 13 and 14. who is eligible ──────────────────────────────────────────────

// Through the watch: every cancelled producer returns — an abandon and a fleet
// stop alike — while a failed or skipped order keeps its bin on the deck, and a
// cancel older than the window is not this deck's episode.
func TestCancelReturn_EligibilityThroughTheWatch(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	g, _ := storeGroupWithSlot(t, db, "CR13")
	// A slot for each case that returns, so one return never leaves the next
	// with nowhere to go.
	st := storNodeType(t, db)
	testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: "CR13-S2", NodeTypeID: &st.ID, ParentID: &g.ID, Enabled: true}),
		"a second slot")
	seedClaim(t, db, "PROC-CR13", "STY", "LINE-CR13", "CR13-P", g.Name, true)
	now := clock.Now().UTC()

	cases := []struct {
		robot  string
		status protocol.Status
		code   string
		detail string
		at     time.Time
		want   bool
	}{
		{"AMR-CR13-FAIL", protocol.StatusFailed, string(protocol.TermGraceTimeout), "grace timeout", now, false},
		{"AMR-CR13-SKIP", protocol.StatusSkipped, string(protocol.TermNotNeeded), "not needed", now, false},
		{"AMR-CR13-ABAN", protocol.StatusCancelled, "", "abandoned: stuck in in_transit past 30m0s", now, true},
		{"AMR-CR13-FLEET", protocol.StatusCancelled, "", "fleet order stopped", now, true},
		{"AMR-CR13-OLD", protocol.StatusCancelled, string(protocol.TermOperatorCancelled), "cancelled by operator", now.Add(-3 * time.Hour), false},
	}
	for _, c := range cases {
		bin, carrier := seedCancelledCarry(t, db, c.robot, "CR13-P", "LINE-CR13")
		setCarrierTerminalRow(t, db, carrier, c.status, c.code, c.detail, c.at)
		cacheRobot(eng, loadedDispatchable(c.robot))
		eng.sweepCarriedBins()
		if got := len(onDeckOrders(t, db, bin.ID)) == 1; got != c.want {
			t.Errorf("%s (%s, %q, %q): returned=%v, want %v", c.robot, c.status, c.code, c.detail, got, c.want)
		}
	}
}

// Two hosts, two bins, one payload, a source with exactly two free slots: the
// sim-rig shape of evidence S2 (2026-10-02), where the poll host and the
// reconciliation host each took one bin of a cancelled swap in the same instant,
// both chose SMN_001, and the loser's one attempt was spent on a lost
// reservation. Every round must end with two live returns to two different
// slots. Several rounds because the interleaving is the scheduler's to pick.
func TestCancelReturn_TwoHostsOnTwoBinsNeverChooseOneSlot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	for round := 0; round < 6; round++ {
		prefix := fmt.Sprintf("CR15-%d", round)
		grp, _ := storeGroupWithSlot(t, db, prefix)
		st := storNodeType(t, db)
		second := &nodes.Node{Name: prefix + "-S2", NodeTypeID: &st.ID, ParentID: &grp.ID, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(second), "a second free slot")
		payload := prefix + "-P"
		seedClaim(t, db, "PROC-"+prefix, "STY", "LINE-"+prefix, payload, grp.Name, true)
		a, _ := seedCancelledCarry(t, db, "AMR-"+prefix+"A", payload, "LINE-"+prefix)
		b, _ := seedCancelledCarry(t, db, "AMR-"+prefix+"B", payload, "LINE-"+prefix)
		cacheRobot(eng, loadedDispatchable("AMR-"+prefix+"A"))
		cacheRobot(eng, loadedDispatchable("AMR-"+prefix+"B"))

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); eng.sweepCarriedBins() }()
		go func() { defer wg.Done(); eng.sweepStrandedBins() }()
		wg.Wait()

		ra, rb := theReturn(t, db, a.ID), theReturn(t, db, b.ID)
		if protocol.IsTerminal(ra.Status) || protocol.IsTerminal(rb.Status) || ra.DeliveryNode == rb.DeliveryNode {
			t.Fatalf("round %d: returns %d→%s (%s) and %d→%s (%s) — two hosts must not choose one slot",
				round, ra.ID, ra.DeliveryNode, ra.Status, rb.ID, rb.DeliveryNode, rb.Status)
		}
	}
}

// ── the overflow hop (owner's rule, SHAPE §3.3) ──────────────────────────────

// emptiesBank is a maintained group of one carrier type at its declared level
// (one slot holds the level's one empty, one slot stands free), DECLARED by the
// press's produce claim as where it draws empties, with `overflow` as the bank's
// configured overflow destination ("" for none).
func emptiesBank(t *testing.T, db *store.DB, prefix string, binTypeID int64, press *nodes.Node, overflow string) *nodes.Node {
	t.Helper()
	bank, full := storeGroupWithSlot(t, db, prefix)
	st := storNodeType(t, db)
	testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: prefix + "-S2", NodeTypeID: &st.ID, ParentID: &bank.ID, Enabled: true}),
		"a free slot: the bank refuses for its LEVEL, not for room")
	testutil.MustNoErr(t, db.CreateBin(&bins.Bin{BinTypeID: binTypeID, Label: prefix + "-resident",
		NodeID: &full.ID, Status: "available"}), "the empty that makes the level")
	testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{GroupNodeID: bank.ID, BinTypeID: binTypeID, Want: 1}),
		"a level of one")
	seedRoleClaim(t, db, "PROC-"+prefix, press.Name, protocol.ClaimRoleProduce, prefix+"-PART", bank.Name, "")
	if overflow != "" {
		testutil.MustNoErr(t, db.SetNodeProperty(bank.ID, nodes.PropOverflowDestination, overflow), "name the overflow")
	}
	return bank
}

// evacOff makes the carried bin an evacuation off `node` (a press or a line):
// its process is that node.
func evacOff(t *testing.T, db *store.DB, robot, payload string, node *nodes.Node) (*bins.Bin, *orders.Order) {
	t.Helper()
	b, o := seedCancelledCarry(t, db, robot, payload, "")
	_, err := db.DB.Exec(`UPDATE orders SET source_node=$1, delivery_node='OUTBOUND', process_node=$1,
		order_type='move' WHERE id=$2`, node.Name, o.ID)
	testutil.MustNoErr(t, err, "make it an evacuation off "+node.Name)
	return b, o
}

// Each case on its own database, because the empties walk is plant-wide.
func TestCancelReturn_AMaintainedGroupAtLevelOverflowsOneHop(t *testing.T) {
	t.Parallel()

	t.Run("at level with an overflow: lands in the overflow, and the audit names the hop", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		eng := newTestEngine(t, db, testdb.NewTrackingBackend())
		press := &nodes.Node{Name: "CR16A-PRESS", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(press), "create press")
		market, marketSlot := storeGroupWithSlot(t, db, "CR16A-MKT")
		a, _ := evacOff(t, db, "AMR-CR16A", "", press)
		bank := emptiesBank(t, db, "CR16A-BANK", a.BinTypeID, press, market.Name)
		cacheRobot(eng, loadedDispatchable("AMR-CR16A"))
		eng.sweepCarriedBins()
		if ret := theReturn(t, db, a.ID); ret.DeliveryNode != marketSlot.Name {
			t.Errorf("return goes to %q, want %s in the overflow group — %s is at its level", ret.DeliveryNode, marketSlot.Name, bank.Name)
		}
		if _, _, detail := lastBinAction(t, db, a.ID); !strings.Contains(detail, "at its declared level, overflowed to "+market.Name) {
			t.Errorf("audit detail %q does not name the hop", detail)
		}
	})

	t.Run("at level with no overflow: hold, saying why", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		eng := newTestEngine(t, db, testdb.NewTrackingBackend())
		press := &nodes.Node{Name: "CR16B-PRESS", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(press), "create press")
		b, _ := evacOff(t, db, "AMR-CR16B", "", press)
		emptiesBank(t, db, "CR16B-BANK", b.BinTypeID, press, "")
		cacheRobot(eng, loadedDispatchable("AMR-CR16B"))
		eng.sweepCarriedBins()
		assertNoRecoveryOrder(t, db, b.ID)
		if action, _, detail := lastBinAction(t, db, b.ID); action != actionReturnHeld ||
			!strings.Contains(detail, "at its declared level and it names no overflow") {
			t.Errorf("audit = %s %q, want a hold naming the level and the missing overflow", action, detail)
		}
	})

	t.Run("any other refusal: no hop", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		eng := newTestEngine(t, db, testdb.NewTrackingBackend())
		// A payload bin offered to an empties-only bank is refused for what it
		// carries, not for the level — overflow or not.
		press := &nodes.Node{Name: "CR16C-PRESS", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(press), "create press")
		market, _ := storeGroupWithSlot(t, db, "CR16C-MKT")
		c, _ := evacOff(t, db, "AMR-CR16C", "CR16C-P", press)
		bank := emptiesBank(t, db, "CR16C-BANK", c.BinTypeID, press, market.Name)
		seedClaim(t, db, "PROC-CR16C", "STY", "LINE-CR16C", "CR16C-P", bank.Name, true)
		cacheRobot(eng, loadedDispatchable("AMR-CR16C"))
		eng.sweepCarriedBins()
		assertNoRecoveryOrder(t, db, c.ID)
		if action, _, detail := lastBinAction(t, db, c.ID); action != actionReturnHeld ||
			strings.Contains(detail, "overflow") || !strings.Contains(detail, "empties-only") {
			t.Errorf("audit = %s %q, want a hold for the empties-only fence and no hop", action, detail)
		}
	})
}

// ── the drift guard: what this policy names is what the finder serves ───────

// Every place the declarations name, resolved to the concrete node the policy
// would set a bin down on, is a node at which the finder's own predicate finds
// that bin: BinSourceableSQL (via FindSourceBinFIFO) for a full, the empty
// carrier predicate (FindEmptyCompatibleBin) for an empty. One fixture; the two
// halves — store/return_sources.go and the find SQL — pinned against each other.
func TestReturnSources_LandWhereTheFinderLooks(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	grp, _ := storeGroupWithSlot(t, db, "DG-SRC")
	seedRoleClaim(t, db, "PROC-DG1", "DG-LINE", protocol.ClaimRoleConsume, "DG-P", grp.Name, "")
	outGrp, _ := storeGroupWithSlot(t, db, "DG-OUT")
	seedRoleClaim(t, db, "PROC-DG2", "DG-LINE2", protocol.ClaimRoleConsume, "DG-Q", "", outGrp.Name)

	check := func(payload string, carrierNode string) {
		t.Helper()
		b, carrier := seedCancelledCarry(t, db, "AMR-DG-"+payload, payload, carrierNode)
		srcs, err := eng.returnSources(b, carrier)
		testutil.MustNoErr(t, err, "returnSources")
		if len(srcs) == 0 {
			t.Fatalf("no declared place for %q — the fixture declares one", payload)
		}
		for _, s := range srcs {
			dest, why := eng.storeInto(s.node, b)
			if dest == nil {
				t.Fatalf("%s refused (%s) in a fixture with room", s.name, why)
			}
			_, err := db.DB.Exec(`UPDATE bins SET node_id=$1, status='available', manifest_confirmed=true WHERE id=$2`, dest.ID, b.ID)
			testutil.MustNoErr(t, err, "stand the bin at "+dest.Name)
			var found *bins.Bin
			if payload != "" {
				found, err = db.FindSourceBinFIFO(payload, 0)
			} else {
				found, err = db.FindEmptyCompatibleBin("DG-Q", "", 0, bins.EmptyFence{}, reservations.Anyone)
			}
			if err != nil || found == nil || found.ID != b.ID {
				t.Errorf("%s: the bin set down at %s (declared via %s) is not what the finder finds (got %v, %v) — "+
					"return_sources.go names a place the find predicate excludes", payload, dest.Name, s.name, found, err)
			}
		}
	}
	check("DG-P", "DG-LINE")
	check("", "DG-LINE2")
}
