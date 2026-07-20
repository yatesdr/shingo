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
// CreateOrder INSERT (domain.Order.SiblingOrderUUID), so the starvation hold
// still fires even when the separate post-create link step (the old
// best-effort LinkOrderSiblingsByEdgeUUID, log-and-continue) never recorded it.
//
// This is the ALN_003 fail-open: pre-fix, a failed intake link left
// sibling_order_uuid empty → swapLegHeld read it as "not a swap leg" →
// the evac PULLED the line bin with no supply hold → line stranded.
//
// The test models the failed-link case by creating the evac via CreateOrder
// ALONE (with the sibling set), skipping the fragile link call entirely.
func TestSwapRemovalLeg_DurableLinkSurvivesFailedIntakeLink(t *testing.T) {
	t.Parallel()
	db := testDB(t)
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

	// The gate must HOLD (fail closed) because the supply has no claimed bin —
	// NOT fail open as it did pre-fix when the link was lost.
	evac, _ = db.GetOrderByUUID("swap-removal-dl")
	evacSteps, ok := decodeSteps(evac.StepsJSON)
	if !ok {
		t.Fatal("evac has no readable steps")
	}
	if held, _ := d.swapLegHeld(evac, evacSteps); !held {
		t.Fatal("evac must be held while supply has no claimed bin, even though the intake link step never ran")
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
	db := testDB(t)
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

	// Processing the evac triggers the on-read repair.
	evac, _ = db.GetOrderByUUID("swap-removal-rb")
	evacSteps, _ := decodeSteps(evac.StepsJSON)
	_, _ = d.swapLegHeld(evac, evacSteps)

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
	db := testDB(t)
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
