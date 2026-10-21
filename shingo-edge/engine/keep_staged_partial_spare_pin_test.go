package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// A PARTIAL ON A PRODUCE SPOT: is it the spare the keeper keeps there?
//
// A produce cell's spare is a carrier to fill. A partial of the cell's own part
// is one (finishing it is the point of a partial home); a partial of another
// part is not. Each row reads the spot from a node-bins row (the part's
// capacity on it, as Core sends it: partial is below the PART'S capacity, the
// crossing the bins table's Partial carries) and asks spareIsRight. Rows carry today (the tree before produce
// partials) and after (under it); the test asserts after, and a row whose
// after differs names its ruling.
func TestProducePartialsPin_KeeperReadsAPartialOnTheSpot(t *testing.T) {
	claim := &processes.NodeClaim{Role: protocol.ClaimRoleProduce, PayloadCode: "PP-X", UOPCapacity: 40}
	catalog := []protocol.PayloadBinTypeInfo{{PayloadCode: "PP-X", BinTypeCode: "PP-T"}, {PayloadCode: "PP-Y", BinTypeCode: "PP-T"}}
	rows := []struct {
		name         string
		row          protocol.NodeBinInfo
		today, after bool
	}{
		{"an unstamped empty of a permitted carrier",
			protocol.NodeBinInfo{Occupied: true, BinTypeCode: "PP-T", BinID: 1}, true, true},
		// Today the produce arm wanted payload '' and nothing else: a partial of
		// the claim's own part read "not a spare", and the keeper asked for a
		// hand. Ruling: a partial of the claim's own part is a right spare.
		{"a partial of the claim's part (17 of 40)",
			protocol.NodeBinInfo{Occupied: true, BinTypeCode: "PP-T", BinID: 2, PayloadCode: "PP-X", UOPRemaining: 17, UOPCapacity: 40}, false, true},
		{"a partial of another part",
			protocol.NodeBinInfo{Occupied: true, BinTypeCode: "PP-T", BinID: 3, PayloadCode: "PP-Y", UOPRemaining: 17, UOPCapacity: 40}, false, false},
		{"a full of the claim's part",
			protocol.NodeBinInfo{Occupied: true, BinTypeCode: "PP-T", BinID: 4, PayloadCode: "PP-X", UOPRemaining: 40, UOPCapacity: 40}, false, false},
	}
	for _, r := range rows {
		got := spareIsRight(claim, readOfBinsRow(binsRowsOf(map[string]protocol.NodeBinInfo{"SPOT": r.row})[0], catalog))
		t.Logf("PIN %-45s %v", r.name, got)
		if got != r.after {
			t.Errorf("%s: spareIsRight = %v, want %v (today %v)", r.name, got, r.after, fmt.Sprint(r.today))
		}
	}
}
