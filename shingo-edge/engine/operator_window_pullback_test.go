package engine

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// operator_window_pullback_test.go — the pull-back picker (site 12 of the
// reads) and its source check (site 13) decide from the bins table: the
// market's members are its group row's, each slot's bin is its member row,
// and a table that may not be decided from refuses with its sentence. No HTTP
// read: the Core client points nowhere. The move's naming of the bin and its
// auto-clear are pinned in order_intent_restart_test.go.

// pullbackTable is a produce loader window PB-WIN whose outbound market
// PB-MARKET has two slots, and the engine to pull from it.
func pullbackTable(t *testing.T) (*Engine, int64) {
	t.Helper()
	db := testEngineDB(t)
	nodeID := seedIntentNode(t, db, "PB-PROC", "PB-WIN")
	eng := newIntentEngine(t, db)
	coreNeverAnswers(t, eng)
	eng.SetCoreLoaders([]protocol.LoaderInfo{pullbackLoader("PB-WIN")})
	return eng, nodeID
}

func TestPullFromMarket_DecidesFromTheTable(t *testing.T) {
	t.Parallel()
	eng, nodeID := pullbackTable(t)
	holdActableBins(t, eng,
		protocol.NodeBinsRow{Node: "PB-MARKET", State: protocol.BinsStateGroup, Members: []string{"PB-MKT-1", "PB-MKT-2"}},
		protocol.NodeBinsRow{Node: "PB-MKT-1", BinID: 88, Occupied: true, PayloadCode: "PB-PART", Stocked: true,
			Partial: true, UOPCapacity: 48},
		protocol.NodeBinsRow{Node: "PB-MKT-2"})

	// The picker lists the slot holding a labelled bin, with what its row
	// carries: stocked and partial, no count.
	bins, err := eng.FetchMarketBins(nodeID)
	testutil.MustNoErr(t, err, "FetchMarketBins")
	if len(bins) != 1 || bins[0].NodeName != "PB-MKT-1" || bins[0].PayloadCode != "PB-PART" ||
		!bins[0].Stocked || !bins[0].Partial || bins[0].UOPCapacity != 48 {
		t.Errorf("picker = %+v, want PB-MKT-1 carrying PB-PART, stocked, partial, capacity 48", bins)
	}

	// An empty slot: refused, nothing pulled.
	err = eng.PullFromMarket(nodeID, "PB-MKT-2")
	if err == nil || !strings.Contains(err.Error(), "no bin at market slot PB-MKT-2") {
		t.Errorf("pull from an empty slot: err = %v, want the no-bin refusal", err)
	}
	if moves := emptyOutMoves(t, eng, "PB-MKT-2"); len(moves) != 0 {
		t.Errorf("an empty slot's pull created moves: %+v", moves)
	}

	// A table that may not be decided from: both refuse with the sentence.
	eng.feeds.now = func() time.Time { return time.Now().Add(ActableAckWithin + time.Second) }
	if _, err := eng.FetchMarketBins(nodeID); err == nil ||
		err.Error() != "Core's picture of PB-MKT-1 is not current — try again" {
		t.Errorf("picker, not actable: err = %v, want the not-current sentence", err)
	}
	err = eng.PullFromMarket(nodeID, "PB-MKT-1")
	if err == nil || err.Error() != "Core's picture of PB-MKT-1 is not current — try again" {
		t.Errorf("pull, not actable: err = %v, want the not-current sentence", err)
	}
	if moves := emptyOutMoves(t, eng, "PB-MKT-1"); len(moves) != 0 {
		t.Errorf("a pull from a table not actable created moves: %+v", moves)
	}
}
