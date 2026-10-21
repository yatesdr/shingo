package engine

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/orders"
)

// apiLoaderFixture installs a shared loader over the given windows, the way
// Core syncs one down, so LoaderAt can find it by any of its window names.
func apiLoaderFixture(t *testing.T, eng *Engine, id string, windows []string, payload string) {
	t.Helper()
	apiLoaderFixtureRole(t, eng, id, "produce", windows, payload)
}

func apiLoaderFixtureRole(t *testing.T, eng *Engine, id, role string, windows []string, payload string) {
	t.Helper()
	positions := make([]protocol.LoaderPosition, len(windows))
	for i, w := range windows {
		positions[i] = protocol.LoaderPosition{CoreNodeName: w}
	}
	eng.SetCoreLoaders([]protocol.LoaderInfo{{
		Name: id, LoaderKey: "loader:" + id, Role: role,
		Layout: "shared_window", Replenishment: "operator",
		InboundSource: "EMPTY-SUPER",
		Positions:     positions,
		Payloads:      []protocol.LoaderPayloadInfo{{PayloadCode: payload}},
	}})
}

// TestCreateRetrieveForAPI_BatchIsCappedByCore is Deploy 1b, asked of Core.
//
// The HTTP order API's batch arm once looped the order manager directly, so
// asking for five empties at a loader with two windows created five. At a
// loader's window it now asks Core (the loader retrieve ask, a person's
// origin), which makes one per free window, and the answer says what was
// made rather than what was asked for. Asking again with every window covered
// is a conflict with the state of the plant, answered with Core's reason.
func TestCreateRetrieveForAPI_BatchIsCappedByCore(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	core := answerLoaderAsks(t, eng)

	windows := []string{"API-W1", "API-W2"}
	seedWindowNodes(t, db, "API-PROC", windows)
	apiLoaderFixture(t, eng, "API-LOADER", windows, "PART-A")
	holdSweep(t, eng, nil)

	made, err := eng.CreateRetrieveForAPI(APIRetrieveRequest{
		RetrieveEmpty: true, Quantity: 1, DeliveryNode: "API-W1", PayloadCode: "PART-A", Count: 5,
	})
	testutil.MustNoErr(t, err, "CreateRetrieveForAPI")
	if len(made) != 2 {
		t.Errorf("asked for 5 empties at a 2-window loader, made %d, want 2", len(made))
	}
	if got := inFlightEmpties(t, db, windows); got != 2 {
		t.Errorf("in-flight empties at the loader = %d, want 2 (one per window)", got)
	}
	ask := core.lastAsk()
	if ask.LoaderKey != "loader:API-LOADER" || ask.Direction != protocol.LoaderRetrieveEmpty ||
		ask.Origin != protocol.LoaderAskOperator || ask.Count != 5 || ask.MemberNode != "API-W1" ||
		ask.OriginClass != protocol.OriginClassNoDemand || ask.SourceNode != "" || !ask.AnySource || ask.SkipAutoConfirm {
		t.Errorf("ask = %+v, want five empties at loader:API-LOADER asked by a person at API-W1, no demand, any source", ask)
	}

	_, err = eng.CreateRetrieveForAPI(APIRetrieveRequest{
		RetrieveEmpty: true, Quantity: 1, DeliveryNode: "API-W1", PayloadCode: "PART-A", Count: 1,
	})
	var none *LoaderRetrieveRefused
	if !errors.As(err, &none) || none.Sentence !=
		"node API-W1: no empty ordered — every window is spoken for (API-W1 window-order-open, API-W2 window-order-open)" {
		t.Errorf("second request err = %v, want Core's sentence naming both windows", err)
	}
	if got := inFlightEmpties(t, db, windows); got != 2 {
		t.Errorf("in-flight empties after the refused request = %d, want still 2", got)
	}
}

// TestCreateRetrieveForAPI_AHeldWindowGetsCoresSentence: a manual retrieve to a
// one-window loader whose window has a carrier standing on it, and to one with
// a full already inbound, gets Core's sentence and makes no order (the
// owner's ruling 1: the manual page asks Core, and Core answers). The Edge
// sends no order request of its own.
func TestCreateRetrieveForAPI_AHeldWindowGetsCoresSentence(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, want string
	}{
		{"resident", "node HW-W1: no full ordered — every window is spoken for (HW-W1 occupied)"},
		{"inbound", "node HW-W1: no full ordered — every window is spoken for (HW-W1 window-order-open)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			answerLoaderAsks(t, eng)
			seedWindowNodes(t, db, "HW-PROC", []string{"HW-W1"})
			apiLoaderFixtureRole(t, eng, "HW-ULD", "consume", []string{"HW-W1"}, "PART-H")
			resident := map[string]string{}
			if c.name == "resident" {
				resident["HW-W1"] = "PART-H"
			} else {
				_, err := db.CreateOrder("hw-inbound", "retrieve", nil, false, 1, "HW-W1", "", "EMPTY-SUPER", "", false, "PART-H", "", "")
				testutil.MustNoErr(t, err, "the full inbound")
			}
			holdSweep(t, eng, resident)
			before := liveOrdersTo(t, db, "HW-W1")
			_, err := eng.CreateRetrieveForAPI(APIRetrieveRequest{DeliveryNode: "HW-W1", PayloadCode: "PART-H", Quantity: 1})
			var none *LoaderRetrieveRefused
			if !errors.As(err, &none) || none.Sentence != c.want {
				t.Errorf("err = %v, want Core's sentence %q", err, c.want)
			}
			if after := liveOrdersTo(t, db, "HW-W1"); after != before {
				t.Errorf("live orders %d -> %d, want none made", before, after)
			}
			assertNoOrderRequestSent(t, eng)
		})
	}
}

// TestCreateRetrieveForAPI_TheOverridesRideAsCoreReadsThem: a named source
// rides the ask (Core pulls from it); the staging node and auto-confirm, which
// Core reads on no order, are recorded on this Edge's row for the order Core
// made, where the orders page and the delivery handler read them.
func TestCreateRetrieveForAPI_TheOverridesRideAsCoreReadsThem(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	core := answerLoaderAsks(t, eng)
	seedWindowNodes(t, db, "OV-PROC", []string{"OV-W1"})
	apiLoaderFixture(t, eng, "OV-LDR", []string{"OV-W1"}, "PART-O")
	holdSweep(t, eng, nil)

	made, err := eng.CreateRetrieveForAPI(APIRetrieveRequest{
		RetrieveEmpty: true, Quantity: 1, DeliveryNode: "OV-W1", PayloadCode: "PART-O",
		SourceNode: "OTHER-MARKET", StagingNode: "OV-STAGE", LoadType: "standard", AutoConfirm: true,
	})
	testutil.MustNoErr(t, err, "CreateRetrieveForAPI")
	if ask := core.lastAsk(); ask.SourceNode != "OTHER-MARKET" || ask.AnySource {
		t.Errorf("ask source = %q any=%v, want the named source", ask.SourceNode, ask.AnySource)
	}
	if len(made) != 1 {
		t.Fatalf("made %d, want 1", len(made))
	}
	o, err := db.GetOrderByUUID(made[0].UUID)
	testutil.MustNoErr(t, err, "read the order Core made")
	if o.SourceNode != "OTHER-MARKET" || o.StagingNode != "OV-STAGE" || !o.AutoConfirm {
		t.Errorf("row: source %q staging %q auto_confirm %v, want OTHER-MARKET, OV-STAGE, true", o.SourceNode, o.StagingNode, o.AutoConfirm)
	}
}

// TestCreateRetrieveForAPI_ACoreWithoutTheKindIsRefused: a Core that does not
// answer loader.retrieve gets no ask, and the Edge makes nothing of its own.
func TestCreateRetrieveForAPI_ACoreWithoutTheKindIsRefused(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	seedWindowNodes(t, db, "NK-PROC", []string{"NK-W1"})
	apiLoaderFixture(t, eng, "NK-LDR", []string{"NK-W1"}, "PART-N")
	_, err := eng.CreateRetrieveForAPI(APIRetrieveRequest{RetrieveEmpty: true, DeliveryNode: "NK-W1", PayloadCode: "PART-N"})
	if err == nil || !strings.Contains(err.Error(), "does not answer the loader.retrieve ask") {
		t.Errorf("err = %v, want the kind refused", err)
	}
	if got := inFlightEmpties(t, db, []string{"NK-W1"}); got != 0 {
		t.Errorf("empties = %d, want none", got)
	}
}

// TestCreateRetrieveForAPI_NonLoaderDestinationIsUnchanged is the other half of
// the rule, and the reason this is not "ask Core for everything".
//
// A press position, a supermarket slot, a quality-hold spot: no loader owns them, so
// there is no window budget for them to belong to. A batch to such a
// destination creates exactly what was asked for, as it always has.
func TestCreateRetrieveForAPI_NonLoaderDestinationIsUnchanged(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	nodes := []string{"PLAIN-NODE"}
	seedWindowNodes(t, db, "PLAIN-PROC", nodes)
	// Deliberately NO loader installed for this node.

	made, err := eng.CreateRetrieveForAPI(APIRetrieveRequest{
		RetrieveEmpty: true,
		Quantity:      1,
		DeliveryNode:  "PLAIN-NODE",
		PayloadCode:   "PART-A",
		Count:         3,
	})
	if err != nil {
		t.Fatalf("CreateRetrieveForAPI: %v", err)
	}
	if len(made) != 3 {
		t.Errorf("a destination no loader owns created %d of 3 — nothing should have capped it", len(made))
	}
}

// liveOrdersTo counts the non-terminal orders delivering to node.
func liveOrdersTo(t *testing.T, db interface {
	ListActiveOrdersByDeliveryNodeSet([]string) ([]orders.Order, error)
}, node string) int {
	t.Helper()
	list, err := db.ListActiveOrdersByDeliveryNodeSet([]string{node})
	testutil.MustNoErr(t, err, "list live orders")
	return len(list)
}

// assertNoOrderRequestSent fails when eng's outbox holds an order request: the
// Edge made an order of its own.
func assertNoOrderRequestSent(t *testing.T, eng *Engine) {
	t.Helper()
	msgs, err := eng.db.ListPendingOutbox(100)
	testutil.MustNoErr(t, err, "list outbox")
	for _, m := range msgs {
		if m.MsgType == protocol.TypeOrderRequest {
			t.Errorf("outbox holds an order request; want none (Core makes the order)")
		}
	}
}
