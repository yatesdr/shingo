//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestSwapRemovalLeg_DurableLinkSurvivesFailedIntakeLink pins the Commit-1
// fix: the two-robot evac's link to its supply is persisted ATOMICALLY in the
// CreateOrder INSERT (domain.Order.SiblingOrderUUID), so the pair is still
// recognisable even when the separate post-create link step (the old
// best-effort LinkOrderSiblingsByEdgeUUID, log-and-continue) never recorded it.
//
// This is the ALN_003 fail-open: pre-fix, a failed intake link left
// sibling_order_uuid empty → the leg read as "not a swap leg" → the evac
// PULLED the line bin with no hold at all → line stranded.
//
// The test models the failed-link case by creating the evac via CreateOrder
// ALONE (with the sibling set), skipping the fragile link call entirely.
//
// ── ITS OBSERVABLE MOVED WITH THE MECHANISM, ITS SUBJECT DID NOT ──────────
//
// This used to end by asserting Face 1 held the evac, because Face 1 was what
// read the pointer. Face 1 is deleted — dispatch sends the evac to park, so it
// was guarding a step that moves nothing — and the pointer is read by the PAIR
// RULE instead, which is a stronger dependency than the one it replaced: a lost
// link no longer merely disarms a hold, it makes the pair invisible, and both
// legs would dispatch independently.
//
// So the assertion is now "the durable INSERT is what lets this be recognised as
// half a pair". Same fix, same failure mode, the mechanism that now carries it.
func TestSwapRemovalLeg_DurableLinkSurvivesFailedIntakeLink(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, lineNode, bp := setupTestData(t, db)

	superNode := &nodes.Node{Name: "SWAP-SUPER-DL", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(superNode), "create super node")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Supply leg — exists but holds no replacement bin yet.
	supply := &orders.Order{
		EdgeUUID: "swap-supply-dl", StationID: "ST", OrderType: OrderTypeComplex,
		Status: StatusQueued, Quantity: 1, PayloadCode: bp.Code,
		SourceNode: superNode.Name, DeliveryNode: lineNode.Name, ProcessNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(supply), "create supply leg")

	// The line bin the removal leg would pull — present, so only the hold
	// stops the claim.
	lineBin := &bins.Bin{BinTypeID: 1, Label: "SWAP-LINE-BIN-DL", NodeID: &lineNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(lineBin), "create line bin")

	// Evac leg created via CreateOrder ALONE — the durable INSERT is the ONLY
	// thing that records the sibling here (no LinkOrderSiblingsByEdgeUUID call),
	// simulating a failed intake link.
	evac := &orders.Order{
		EdgeUUID: "swap-removal-dl", StationID: "ST", OrderType: OrderTypeComplex,
		Status: StatusQueued, Quantity: 1, PayloadCode: bp.Code,
		SourceNode: lineNode.Name, DeliveryNode: superNode.Name, ProcessNode: lineNode.Name,
		SiblingOrderUUID: "swap-supply-dl",
		// A two_robot evac shape: it takes the line's bin and has a single pickup,
		// so it cannot fetch its own replacement — the gate's subject.
		StepsJSON: twoRobotEvacSteps(t, lineNode.Name, superNode.Name),
	}
	testutil.MustNoErr(t, db.CreateOrder(evac), "create evac leg")

	// The durable column must round-trip (this is what the fix persists).
	got, err := db.OrderSiblingUUID(evac.ID)
	testutil.MustNoErr(t, err, "read sibling uuid")
	if got != "swap-supply-dl" {
		t.Fatalf("sibling_order_uuid = %q, want %q (durable INSERT did not persist it)", got, "swap-supply-dl")
	}

	// The PAIR must be recognised from the durable column alone — NOT read as a
	// solo order, which is what a lost link produced pre-fix and what would now
	// let both legs dispatch independently of each other.
	evac, _ = db.GetOrderByUUID("swap-removal-dl")
	legs, wait := d.coordinatedPairLegs(evac)
	if wait != nil {
		t.Fatal("evac read as waiting for a partner row that exists — the durable link was not consulted")
	}
	if len(legs) != 2 {
		t.Fatalf("coordinatedPairLegs returned %d leg(s), want 2 — the evac read as a SOLO order even though "+
			"the durable INSERT recorded its sibling, so both legs would dispatch independently", len(legs))
	}
	if legs[0].ID > legs[1].ID {
		t.Fatalf("legs are not in ascending id order (%d, %d) — the leader election depends on it", legs[0].ID, legs[1].ID)
	}
}

// twoRobotEvacSteps is the classic removal shape: hold at the line, lift its bin,
// carry it to the supermarket. One pickup, at the line — so it cannot fetch its
// own replacement and the hold gate applies to it.
func twoRobotEvacSteps(t *testing.T, lineName, destName string) string {
	t.Helper()
	j, err := json.Marshal([]resolvedStep{
		{Action: protocol.ActionWait, Node: lineName},
		{Action: protocol.ActionPickup, Node: lineName},
		{Action: protocol.ActionDropoff, Node: destName},
	})
	testutil.MustNoErr(t, err, "marshal evac steps")
	return string(j)
}

// TestSwapSibling_ReverseBacklinkRepairedOnRead pins the on-read repair: when
// the supply's back-link (supply→evac) is missing — e.g. the intake back-link
// write failed, or the supply row arrived after the evac — processing the evac
// heals it, so the peer-death handler (Commit 3) can find the evac from the
// supply side.
func TestSwapSibling_ReverseBacklinkRepairedOnRead(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, lineNode, bp := setupTestData(t, db)

	superNode := &nodes.Node{Name: "SWAP-SUPER-RB", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(superNode), "create super node")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Supply with NO back-link recorded (sibling_order_uuid = "").
	supply := &orders.Order{
		EdgeUUID: "swap-supply-rb", StationID: "ST", OrderType: OrderTypeComplex,
		Status: StatusQueued, Quantity: 1, PayloadCode: bp.Code,
		SourceNode: superNode.Name, DeliveryNode: lineNode.Name, ProcessNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(supply), "create supply leg")

	// Evac with the forward link present.
	evac := &orders.Order{
		EdgeUUID: "swap-removal-rb", StationID: "ST", OrderType: OrderTypeComplex,
		Status: StatusQueued, Quantity: 1, PayloadCode: bp.Code,
		SourceNode: lineNode.Name, DeliveryNode: superNode.Name, ProcessNode: lineNode.Name,
		SiblingOrderUUID: "swap-supply-rb",
		StepsJSON:        twoRobotEvacSteps(t, lineNode.Name, superNode.Name),
	}
	testutil.MustNoErr(t, db.CreateOrder(evac), "create evac leg")

	// Precondition: the reverse link is genuinely missing.
	if s, _ := db.OrderSiblingUUID(supply.ID); s != "" {
		t.Fatalf("precondition: supply back-link = %q, want empty", s)
	}

	// Resolving the evac's pair triggers the on-read repair. It used to be
	// triggered by the swap-hold gate, which is deleted; the repair moved to
	// coordinatedPairLegs, which runs for every coordinated leg on every pass
	// instead of only for legs that reached a gate.
	evac, _ = db.GetOrderByUUID("swap-removal-rb")
	_, _ = d.coordinatedPairLegs(evac)

	// The supply's back-link is now healed.
	healed, err := db.OrderSiblingUUID(supply.ID)
	testutil.MustNoErr(t, err, "read supply back-link")
	if healed != "swap-removal-rb" {
		t.Fatalf("supply back-link = %q, want %q (on-read repair did not heal it)", healed, "swap-removal-rb")
	}
}

// TestSwapPeerTerminalRace_LiveLegResolvesDeadSibling pins the SPR 2424/2425 fix.
// A swap's supply leg can be created AND skip (moot: supermarket empty) in the same
// tick, BEFORE its evac leg exists. HandleSwapPeerTerminal fires from the supply's
// side, finds no peer, and no-ops. The evac is then created linked to the already-
// dead supply. It must NOT hold forever: DispatchPreparedComplex re-runs the unwind
// from the surviving side (healing the back-link first), and the supply-skip
// cancels the evac — a moot swap, nothing to replace.
func TestSwapPeerTerminalRace_LiveLegResolvesDeadSibling(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, lineNode, bp := setupTestData(t, db)

	superNode := &nodes.Node{Name: "SWAP-SUPER-RACE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(superNode), "create super node")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Supply leg — already SKIPPED (moot) before the evac exists, and with NO
	// back-link (the unwind that fired here found nothing to cancel).
	supplySteps, err := json.Marshal([]resolvedStep{
		{Action: protocol.ActionPickup, Node: superNode.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	})
	testutil.MustNoErr(t, err, "marshal supply steps")
	supply := &orders.Order{
		EdgeUUID: "race-supply", StationID: "ST", OrderType: OrderTypeComplex,
		Status: StatusSkipped, Quantity: 1, PayloadCode: bp.Code,
		SourceNode: superNode.Name, DeliveryNode: lineNode.Name, ProcessNode: lineNode.Name,
		StepsJSON: string(supplySteps),
	}
	testutil.MustNoErr(t, db.CreateOrder(supply), "create skipped supply")

	// A line bin is present, so ONLY a sibling gate could hold the evac.
	lineBin := &bins.Bin{BinTypeID: 1, Label: "RACE-LINE-BIN", NodeID: &lineNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(lineBin), "create line bin")

	// Evac leg — created AFTER the supply already skipped, carrying only the forward
	// sibling pointer (the supply's back-link is still missing, modelling the race).
	evac := &orders.Order{
		EdgeUUID: "race-evac", StationID: "ST", OrderType: OrderTypeComplex,
		Status: StatusQueued, Quantity: 1, PayloadCode: bp.Code,
		SourceNode: lineNode.Name, DeliveryNode: superNode.Name, ProcessNode: lineNode.Name,
		SiblingOrderUUID: "race-supply",
		StepsJSON:        twoRobotEvacSteps(t, lineNode.Name, superNode.Name),
	}
	testutil.MustNoErr(t, db.CreateOrder(evac), "create evac")

	// Dispatch the evac: instead of holding forever on the dead supply, the
	// surviving-side unwind must resolve (cancel) it.
	if derr := d.DispatchPreparedComplex(evac); derr == nil {
		t.Fatal("evac dispatched with no error — a leg whose supply sibling already skipped must be resolved, not left to hold")
	}
	got, gerr := db.GetOrderByUUID("race-evac")
	testutil.MustNoErr(t, gerr, "reload evac")
	if !protocol.IsTerminal(got.Status) {
		t.Fatalf("evac status = %q, want terminal (cancelled) — it must not wedge holding for a dead supply sibling (SPR 2424/2425)", got.Status)
	}
}
