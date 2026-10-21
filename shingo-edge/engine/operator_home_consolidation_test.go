package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// threeSpotLoader is a dedicated home with three buffer spots in the plant's
// order: CLH-BUF-1 open, CLH-BUF-2 and CLH-BUF-3 each a stocked partial of the
// home's part.
func threeSpotLoader() protocol.LoaderInfo {
	return protocol.LoaderInfo{
		Name: "LDR-CLH3", LoaderKey: "loader:clh3", Role: string(protocol.ClaimRoleProduce),
		Layout: "dedicated_positions", Replenishment: "operator",
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "CLH-HOME", PayloadCode: "CLH-PART", HomeKind: "home"},
			{CoreNodeName: "CLH-BUF-1", HomeKind: "buffer"},
			{CoreNodeName: "CLH-BUF-2", HomeKind: "buffer"},
			{CoreNodeName: "CLH-BUF-3", HomeKind: "buffer"},
		},
		Payloads: []protocol.LoaderPayloadInfo{{PayloadCode: "CLH-PART"}},
	}
}

func threeSpotBins() []protocol.NodeBinInfo {
	return []protocol.NodeBinInfo{
		{NodeName: "CLH-HOME", Occupied: true, PayloadCode: "CLH-PART", UOPRemaining: 5, BinID: 11},
		{NodeName: "CLH-BUF-1"},
		{NodeName: "CLH-BUF-2", Occupied: true, PayloadCode: "CLH-PART", UOPRemaining: 3, BinID: 12},
		{NodeName: "CLH-BUF-3", Occupied: true, PayloadCode: "CLH-PART", UOPRemaining: 4, BinID: 13},
	}
}

// clearHomeTo runs ClearLoaderHome naming bufferNode and returns the error,
// the Core clears made and the spots Order A went to.
func clearHomeTo(t *testing.T, bufferNode string) (error, int32, []string) {
	t.Helper()
	srv, clears := intentCoreServer(t, threeSpotBins(), nil)
	db := testEngineDB(t)
	nodeID := seedIntentNode(t, db, "CLH3-PROC", "CLH-HOME")
	eng := newIntentEngine(t, db)
	eng.coreClient = stubCoreClient(srv.URL)
	eng.SetCoreLoaders([]protocol.LoaderInfo{threeSpotLoader()})
	err := eng.ClearLoaderHome(nodeID, bufferNode)
	all, lerr := db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, lerr, "list orders")
	var to []string
	for _, o := range all {
		if o.SourceNode == "CLH-HOME" {
			to = append(to, o.DeliveryNode)
		}
	}
	return err, clears.Load(), to
}

// TestClearLoaderHome_NoSpotNamedTakesTheFirstPartialInThePlantsOrder pins
// what a clear with no buffer_node does: the first buffer spot, in the plant's
// order, holding a stocked bin of the home's part (CLH-BUF-2, not CLH-BUF-3),
// the spot the board's buffer_partial names (service bufferPartialFor).
func TestClearLoaderHome_NoSpotNamedTakesTheFirstPartialInThePlantsOrder(t *testing.T) {
	t.Parallel()
	err, clears, to := clearHomeTo(t, "")
	testutil.MustNoErr(t, err, "ClearLoaderHome")
	if clears != 1 || len(to) != 1 || to[0] != "CLH-BUF-2" {
		t.Fatalf("clear with no spot named: %d clears, Order A to %v; want 1 clear and Order A to CLH-BUF-2", clears, to)
	}
}

// TestClearLoaderHome_TheNamedSpotIsTheOneCleared: given a buffer_node, the
// clear takes that partial and no other, so the board's card and the engine
// cannot pick different partials; a named spot that holds no stocked partial
// of the home's part, or is no buffer spot of the loader, is refused before
// anything is cleared or ordered.
func TestClearLoaderHome_TheNamedSpotIsTheOneCleared(t *testing.T) {
	t.Parallel()
	err, clears, to := clearHomeTo(t, "CLH-BUF-3")
	testutil.MustNoErr(t, err, "ClearLoaderHome naming CLH-BUF-3")
	if clears != 1 || len(to) != 1 || to[0] != "CLH-BUF-3" {
		t.Fatalf("clear naming CLH-BUF-3: %d clears, Order A to %v; want 1 clear and Order A to CLH-BUF-3", clears, to)
	}
	for _, spot := range []string{"CLH-BUF-1", "CLH-HOME", "NOT-A-SPOT"} {
		err, clears, to := clearHomeTo(t, spot)
		if err == nil || clears != 0 || len(to) != 0 {
			t.Errorf("clear naming %s: err=%v, %d clears, Order A to %v; want refused with nothing cleared or ordered",
				spot, err, clears, to)
		}
	}
}
