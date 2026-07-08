//go:build docker

package dispatch

import (
	"testing"

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
// sibling_order_uuid = '' → swapRemovalLegHeld read it as "not a swap leg" →
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
	if held, _ := d.swapRemovalLegHeld(evac); !held {
		t.Fatal("evac must be held while supply has no claimed bin, even though the intake link step never ran")
	}
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
	}
	testutil.MustNoErr(t, db.CreateOrder(evac), "create evac leg")

	// Precondition: the reverse link is genuinely missing.
	if s, _ := db.OrderSiblingUUID(supply.ID); s != "" {
		t.Fatalf("precondition: supply back-link = %q, want empty", s)
	}

	// Processing the evac triggers the on-read repair.
	evac, _ = db.GetOrderByUUID("swap-removal-rb")
	_, _ = d.swapRemovalLegHeld(evac)

	// The supply's back-link is now healed.
	healed, err := db.OrderSiblingUUID(supply.ID)
	testutil.MustNoErr(t, err, "read supply back-link")
	if healed != "swap-removal-rb" {
		t.Fatalf("supply back-link = %q, want %q (on-read repair did not heal it)", healed, "swap-removal-rb")
	}
}
