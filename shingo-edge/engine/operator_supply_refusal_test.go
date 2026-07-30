package engine

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// operator_supply_refusal_test.go — the supplier's write path.
//
// EVERY CASE RUNS TWICE, ONCE PER BOARD LAYOUT, because the owner's rule is "all
// loaders" and the two shapes reach the card differently: a shared_window
// renders one card per payload on one node, a dedicated_positions board renders
// one card per home across several nodes. They reach the SAME key, and proving
// that is the point of running both — a design that needed to know the layout to
// identify the card would be the wrong design.

// loaderFixture is one loader window set up as a card the operator can act on.
type loaderFixture struct {
	db     *store.DB
	eng    *Engine
	nodeID int64
	core   string
}

// seedLoaderCard builds a manual_swap loader window on its own station, in one
// of the two layouts. dedicated marks it home-location; everything else is
// identical, which is the property under test.
func seedLoaderCard(t *testing.T, dedicated bool) *loaderFixture {
	t.Helper()
	db := testEngineDB(t)

	processID, err := db.CreateProcess("LOAD-PROC", "loader test", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	stationID, err := db.CreateOperatorStation(stations.Input{
		ProcessID: processID, Code: "BL", Name: "Bin Loader", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create station")

	core := "SMN_014"
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: core, Code: "W1",
		Name: "Window 1", Sequence: 1, Enabled: true, OperatorStationID: &stationID,
	})
	testutil.MustNoErr(t, err, "create loader node")

	styleID, err := db.CreateStyle("Style-A", "s", processID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &styleID), "set active style")
	_, err = upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: core, Role: "produce",
		SwapMode: protocol.SwapModeManualSwap, PayloadCode: "PART-A", UOPCapacity: 100,
		InboundSource: "MARKET", OutboundDestination: "OUT-MARKET",
	})
	testutil.MustNoErr(t, err, "upsert loader claim")
	db.EnsureProcessNodeRuntime(nodeID)

	if dedicated {
		testutil.MustNoErr(t, db.SetHomeLocationLoader(core, true, "test"), "mark dedicated layout")
	}
	return &loaderFixture{db: db, eng: testEngine(t, db), nodeID: nodeID, core: core}
}

// call puts a live order for the payload at this window — the "somebody asked"
// term, without which no card can be red and no refusal is accepted.
func (f *loaderFixture) call(t *testing.T, payload string) int64 {
	t.Helper()
	id, err := f.db.CreateOrder("uuid-"+payload+"-"+f.core, orders.TypeRetrieve,
		&f.nodeID, true, 1, f.core, "", "MARKET", "", false, payload)
	testutil.MustNoErr(t, err, "create call")
	testutil.MustNoErr(t, f.db.UpdateOrderStatus(id, string(orders.StatusQueued)), "queue call")
	return id
}

func eachLayout(t *testing.T, run func(t *testing.T, f *loaderFixture)) {
	t.Helper()
	for _, tc := range []struct {
		name      string
		dedicated bool
	}{
		{"shared_window", false},
		{"dedicated_positions", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run(t, seedLoaderCard(t, tc.dedicated))
		})
	}
}

func TestRefuseSupply_WritesOneRowKeyedOnTheCard(t *testing.T) {
	t.Parallel()
	eachLayout(t, func(t *testing.T, f *loaderFixture) {
		f.call(t, "PART-A")

		testutil.MustNoErr(t, f.eng.RefuseSupply(f.nodeID, "PART-A", "Bin Loader"), "refuse")
		// A second press is the same statement said twice.
		testutil.MustNoErr(t, f.eng.RefuseSupply(f.nodeID, "PART-A", "Bin Loader"), "refuse again")

		open, err := f.db.ListOpenSupplyRefusals()
		testutil.MustNoErr(t, err, "list")
		if len(open) != 1 {
			t.Fatalf("got %d rows, want exactly 1 — the card is the key on both layouts", len(open))
		}
		if open[0].LoaderNode != f.core || open[0].PayloadCode != "PART-A" {
			t.Errorf("keyed %s/%s, want %s/PART-A", open[0].LoaderNode, open[0].PayloadCode, f.core)
		}
	})
}

// TestRefuseSupply_RejectsAPayloadNobodyAskedFor is owner decision 2, enforced
// rather than documented. The control must not fire for anything nobody asked
// about — and on a dedicated board this is the ONLY term carrying that rule,
// because LoadablePayloadCodesAt makes the active-style term always true there.
func TestRefuseSupply_RejectsAPayloadNobodyAskedFor(t *testing.T) {
	t.Parallel()
	eachLayout(t, func(t *testing.T, f *loaderFixture) {
		// A live call exists — but for a different payload.
		f.call(t, "PART-A")

		if err := f.eng.RefuseSupply(f.nodeID, "PART-B", "Bin Loader"); err == nil {
			t.Fatal("refused a payload with no outstanding call — a refusal answers a request, " +
				"and this is the term owner decision 2 rests on")
		}
		open, _ := f.db.ListOpenSupplyRefusals()
		if len(open) != 0 {
			t.Errorf("wrote %d rows for a payload nobody asked about", len(open))
		}
	})
}

func TestRefuseSupply_RejectsANodeThatIsNotALoaderWindow(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	// A cell node: real, claimed, on a station — but not a loader.
	processID, nodeID, _, _, _, _ := seedChangeoverScenario(t, db)
	_ = processID
	eng := testEngine(t, db)

	if err := eng.RefuseSupply(nodeID, "PART-OLD", "someone"); err == nil {
		t.Fatal("accepted a refusal against a cell node — the control belongs to the loader card")
	}
}

func TestUndoSupplyRefusal_ClearsTheRowAndTheAck(t *testing.T) {
	t.Parallel()
	eachLayout(t, func(t *testing.T, f *loaderFixture) {
		f.call(t, "PART-A")
		testutil.MustNoErr(t, f.eng.RefuseSupply(f.nodeID, "PART-A", "Bin Loader"), "refuse")
		_, err := f.db.AckSupplyRefusal(f.core, "PART-A", "wait", "SNF2")
		testutil.MustNoErr(t, err, "ack")

		testutil.MustNoErr(t, f.eng.UndoSupplyRefusal(f.nodeID, "PART-A"), "undo")
		if _, err := f.db.GetSupplyRefusal(f.core, "PART-A"); !errors.Is(err, store.ErrNoOpenRefusal) {
			t.Fatalf("row survived undo: %v", err)
		}

		// And the next refusal must ask the cell again — a surviving ack would
		// silence the modal for a statement the cell has never seen.
		testutil.MustNoErr(t, f.eng.RefuseSupply(f.nodeID, "PART-A", "Bin Loader"), "re-refuse")
		again, gerr := f.db.GetSupplyRefusal(f.core, "PART-A")
		testutil.MustNoErr(t, gerr, "get after re-refuse")
		if again.Answered() {
			t.Error("a new refusal came back already-answered")
		}
	})
}

// TestUndoSupplyRefusal_WorksAfterTheCallIsGone. A refusal can outlive the order
// that justified it — the cell changed over, or the order was cancelled. The
// operator must still be able to take back something they said, so undo
// deliberately does not require a live call the way refusing does.
func TestUndoSupplyRefusal_WorksAfterTheCallIsGone(t *testing.T) {
	t.Parallel()
	eachLayout(t, func(t *testing.T, f *loaderFixture) {
		orderID := f.call(t, "PART-A")
		testutil.MustNoErr(t, f.eng.RefuseSupply(f.nodeID, "PART-A", "Bin Loader"), "refuse")
		testutil.MustNoErr(t, f.db.UpdateOrderStatus(orderID, string(orders.StatusCancelled)), "cancel the call")

		if err := f.eng.UndoSupplyRefusal(f.nodeID, "PART-A"); err != nil {
			t.Fatalf("undo refused once the call was gone: %v — a person must always be able "+
				"to withdraw what they said", err)
		}
	})
}
