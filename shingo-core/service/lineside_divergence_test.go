package service

import (
	"testing"

	"shingo/protocol"
	"shingocore/store"
)

// A count gap with a delta still in flight (or Core ahead of the report) is neither a divergence nor an
// agreement: its key comes back undecided, so an episode already open for it is
// neither closed nor re-opened. Without this, a running seat whose report
// happens to catch a delta in flight would close its open episode and re-open
// it a minute later.
func TestClassifyLinesideReport_InFlightGapIsUndecided(t *testing.T) {
	t.Parallel()
	bin := int64(7)
	e := protocol.LinesideLevelEntry{CoreNodeName: "ALN_1", PayloadCode: "PART-A", BinCount: 1, BinUOP: 90,
		BinID: &bin, BinEpoch: 2, FlushedSeq: 11}
	core := store.LinesideCoreSide{
		Carriers: []store.LinesideCoreCarrier{{BinID: 7, NodeName: "ALN_1", Payload: "PART-A", Epoch: 2, UOP: 100, AtSeat: true, LastSeq: 10}},
		Buckets:  map[store.LinesideReportSeat]int{},
	}
	divs, undecided := ClassifyLinesideReport([]protocol.LinesideLevelEntry{e}, core)
	if len(divs) != 0 {
		t.Errorf("divergences = %+v, want none while seq 11 has not reached Core (last_seq 10)", divs)
	}
	if !undecided["count|ALN_1|PART-A|7"] {
		t.Errorf("undecided = %v, want the carrier's count key", undecided)
	}

	core.Carriers[0].LastSeq = 11
	divs, undecided = ClassifyLinesideReport([]protocol.LinesideLevelEntry{e}, core)
	if len(divs) != 1 || divs[0].Class != ReportDivergenceCount || len(undecided) != 0 {
		t.Errorf("settled: divs %+v undecided %v, want one count divergence", divs, undecided)
	}

	// Core AHEAD of the report (a later window arrived first): Core's count
	// includes a delta the report's count does not, so it is undecided too.
	core.Carriers[0].LastSeq = 12
	divs, undecided = ClassifyLinesideReport([]protocol.LinesideLevelEntry{e}, core)
	if len(divs) != 0 || !undecided["count|ALN_1|PART-A|7"] {
		t.Errorf("Core ahead: divs %+v undecided %v, want undecided", divs, undecided)
	}
}

// An Edge carrier at epoch 0 is the unknown-generation sentinel Core always
// applies; it is not an epoch divergence.
func TestClassifyLinesideReport_EpochZeroIsNotADivergence(t *testing.T) {
	t.Parallel()
	bin := int64(8)
	e := protocol.LinesideLevelEntry{CoreNodeName: "ALN_1", PayloadCode: "PART-A", BinCount: 1, BinUOP: 100, BinID: &bin}
	core := store.LinesideCoreSide{
		Carriers: []store.LinesideCoreCarrier{{BinID: 8, NodeName: "ALN_1", Payload: "PART-A", Epoch: 5, UOP: 100, AtSeat: true}},
		Buckets:  map[store.LinesideReportSeat]int{},
	}
	if divs, _ := ClassifyLinesideReport([]protocol.LinesideLevelEntry{e}, core); len(divs) != 0 {
		t.Errorf("divergences = %+v, want none for an epoch-0 row that agrees on the count", divs)
	}
}
