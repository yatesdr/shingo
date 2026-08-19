package engine

import (
	"errors"
	"testing"

	"shingo/protocol"
)

// apiLoaderFixture installs a shared produce loader over the given windows, the
// way Core syncs one down, so LoaderAt can find it by any of its window names.
func apiLoaderFixture(t *testing.T, eng *Engine, id string, windows []string, payload string) {
	t.Helper()
	positions := make([]protocol.LoaderPosition, len(windows))
	for i, w := range windows {
		positions[i] = protocol.LoaderPosition{CoreNodeName: w, Kind: protocol.LoaderPositionKindWindow}
	}
	eng.SetCoreLoaders([]protocol.LoaderInfo{{
		Name: id, LoaderKey: "loader:" + id, Role: "produce",
		Layout: "shared_window", Replenishment: "operator",
		InboundSource: "EMPTY-SUPER", ConfigGen: 1,
		Positions: positions,
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: payload}},
	}})
}

// TestCreateRetrieveForAPI_BatchIsCappedByTheLoaderBudget is Deploy 1b.
//
// The HTTP order API was the one creation route that never went through the
// reservation seam. Its batch arm looped the order manager directly, so asking
// for five empties at a loader with two windows created five — three of them
// destined for windows that already had one coming. Every other door onto those
// same windows has counted in-flight since the seam was built; this one did not.
//
// It does now, and the answer says what was actually made rather than what was
// asked for.
func TestCreateRetrieveForAPI_BatchIsCappedByTheLoaderBudget(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	mw := true
	eng.cfg.LoadersMultiWindow = &mw

	windows := []string{"API-W1", "API-W2"}
	seedWindowNodes(t, db, "API-PROC", windows)
	apiLoaderFixture(t, eng, "API-LOADER", windows, "PART-A")

	made, err := eng.CreateRetrieveForAPI(APIRetrieveRequest{
		RetrieveEmpty: true,
		Quantity:      1,
		DeliveryNode:  "API-W1",
		PayloadCode:   "PART-A",
		Count:         5,
	})
	if err != nil {
		t.Fatalf("CreateRetrieveForAPI: %v", err)
	}
	if len(made) != 2 {
		t.Errorf("asked for 5 empties at a 2-window loader, created %d — the batch arm is "+
			"not counting what is already inbound, which is the whole job of the seam", len(made))
	}
	if got := inFlightEmpties(t, db, windows); got != 2 {
		t.Errorf("in-flight empties at the loader = %d, want 2 (one per window)", got)
	}

	// Asking again with every window covered is a conflict with the state of the
	// plant, not a bad request — the same answer the operator's own buttons give.
	_, err = eng.CreateRetrieveForAPI(APIRetrieveRequest{
		RetrieveEmpty: true,
		Quantity:      1,
		DeliveryNode:  "API-W1",
		PayloadCode:   "PART-A",
		Count:         1,
	})
	if !errors.Is(err, ErrLoaderBudgetExhausted) {
		t.Errorf("second request err = %v, want ErrLoaderBudgetExhausted — every window already "+
			"has an empty coming, so there is nowhere for another to go", err)
	}
	if got := inFlightEmpties(t, db, windows); got != 2 {
		t.Errorf("in-flight empties after the refused request = %d, want still 2", got)
	}
}

// TestCreateRetrieveForAPI_NonLoaderDestinationIsUnchanged is the other half of
// the rule, and the reason this is not "route everything through the seam".
//
// A press seat, a supermarket slot, a quality-hold spot: no loader owns them, so
// there is no budget for them to belong to. Inventing one would be the mistake
// the RequestEmptyBin simple-mode guard comment already explains. A batch to
// such a destination creates exactly what was asked for, as it always has.
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
