package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
)

// lineside_reporter_test.go — end to end: what actually goes on the wire.
//
// The store query decides what CAN be said; this decides what IS said. Under
// lineside_decision_mode=edge_reports, which is the default and what both
// plants resolve to, these entries DECIDE replenishment on Core, and the
// payload is a primary-key member there — so a wrong name does not degrade an
// adjustment, it mints an authoritative row under a part nobody established.

func reportedEntries(t *testing.T, db *store.DB) []protocol.LinesideLevelEntry {
	t.Helper()
	msgs, err := db.ListPendingOutbox(50)
	testutil.MustNoErr(t, err, "list outbox")
	var out []protocol.LinesideLevelEntry
	for _, m := range msgs {
		if m.MsgType != string(protocol.SubjectLinesideLevelReport) {
			continue
		}
		var env struct {
			P struct {
				Data protocol.LinesideLevelReport `json:"data"`
			} `json:"p"`
		}
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "decode the report envelope")
		out = append(out, env.P.Data.Entries...)
	}
	return out
}

// THE INCIDENT, ON THE WIRE. The claim names one part, the carrier is another,
// and only one of them may leave the building.
func TestReportLinesideLevels_ShipsTheCarriersPart(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, fromClaimID, _ := seedDirectChangeover(t, db)

	testutil.MustNoErr(t, db.SetProcessNodeRuntimeForDeliveredBin(nodeID, &fromClaimID, 15, 1, 7032), "bind carrier")
	eng.recordLinesideCarrier(nodeID, "ALN_007", domain.KnownCarrier("63125-6TA0A.06"), domain.CarrierFromDelivery)

	eng.reportLinesideLevels()

	entries := reportedEntries(t, db)
	if len(entries) != 1 {
		t.Fatalf("shipped %d entries, want 1: %+v", len(entries), entries)
	}
	if entries[0].PayloadCode != "63125-6TA0A.06" {
		t.Errorf("PayloadCode = %q, want 63125-6TA0A.06. 74871-6SA1A.06 is the claim's, and it "+
			"is what Springfield shipped once a minute for three days against a carrier holding "+
			"7032 of the other one.", entries[0].PayloadCode)
	}
	if entries[0].BinUOP != 7032 {
		t.Errorf("BinUOP = %d, want 7032", entries[0].BinUOP)
	}
}

// A CARRIER NOBODY IDENTIFIED SHIPS NOTHING. Absence is the designed
// degradation: past Core's 3-minute staleness window the node falls back to its
// ledger term and makes no adjustment, which is a less-corrected number rather
// than a wrong one.
func TestReportLinesideLevels_WithholdsAnUnidentifiedCarrier(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, fromClaimID, _ := seedDirectChangeover(t, db)

	// A bound carrier with real parts and no identity — a bind by a path that
	// carries no envelope, or an older Core.
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeForDeliveredBin(nodeID, &fromClaimID, 16, 1, 4200), "bind carrier")

	eng.reportLinesideLevels()

	for _, e := range reportedEntries(t, db) {
		t.Errorf("shipped %+v for a carrier nobody identified. There is no safe guess here: the "+
			"only one available is the claim, which is the requested identity.", e)
	}
}

// A cleared carrier is KNOWN and has no part number to report under, so it
// ships nothing rather than reporting zero on-hand of the requested part.
func TestReportLinesideLevels_WithholdsAKnownEmptyCarrier(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, fromClaimID, _ := seedDirectChangeover(t, db)

	testutil.MustNoErr(t, db.SetProcessNodeRuntimeForDeliveredBin(nodeID, &fromClaimID, 17, 1, 0), "bind carrier")
	eng.recordLinesideCarrier(nodeID, "ALN_007", domain.KnownCarrier(""), domain.CarrierFromOperator)

	eng.reportLinesideLevels()

	for _, e := range reportedEntries(t, db) {
		t.Errorf("shipped %+v for an empty carrier", e)
	}
}
