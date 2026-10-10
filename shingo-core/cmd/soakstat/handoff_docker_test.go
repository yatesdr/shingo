//go:build docker

package main

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/audit"
	"shingocore/store/messaging"
)

// TestHandoff_ReadsEachNumberOffTheRealSchema seeds each of the section's
// sources the way Core writes them (the outbox through BuildDataEnvelope, so
// the JSON path is the real envelope's) and reads every number back.
func TestHandoff_ReadsEachNumberOffTheRealSchema(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	line := sd.LineNode.Name

	enqueue := func(subject string, body any) {
		t.Helper()
		data, msgType, err := messaging.BuildDataEnvelope(subject, "core", protocol.StationBroadcast, body)
		testutil.MustNoErr(t, err, "build envelope")
		_, err = db.Exec(`INSERT INTO outbox (topic, payload, msg_type, station_id) VALUES ('d', $1, $2, '*')`, data, msgType)
		testutil.MustNoErr(t, err, "seed outbox")
	}
	enqueue(protocol.SubjectUOPAdjustment, &protocol.UOPAdjustment{BinID: 1, CoreNodeName: line, Bound: true})
	enqueue(protocol.SubjectUOPAdjustment, &protocol.UOPAdjustment{BinID: 2, CoreNodeName: "SLN_009", Bound: true})
	enqueue(protocol.SubjectUOPAdjustment, &protocol.UOPAdjustment{BinID: 2, CoreNodeName: "SLN_009", Bound: true})
	enqueue(protocol.SubjectUOPAdjustment, &protocol.UOPAdjustment{BinID: 3, CoreNodeName: line, NewRemaining: 5}) // a count, not a placement
	enqueue(protocol.SubjectBinEpochRefresh, &protocol.BinEpochRefresh{BinID: 3, CoreNodeName: "_TRANSIT", Epoch: 4})
	enqueue(protocol.SubjectBinEpochRefresh, &protocol.BinEpochRefresh{BinID: 3, CoreNodeName: line, Epoch: 5})

	_, err := db.Exec(`INSERT INTO style_claims (process_id, style_id, core_node_name, role, swap_mode) VALUES ('p1', 's1', $1, 'consume', 'single_robot')`, line)
	testutil.MustNoErr(t, err, "seed a claim")
	_, err = db.Exec(`INSERT INTO process_styles (process_id, style_id, station_id) VALUES ('p1', 's1', 'line-1')`)
	testutil.MustNoErr(t, err, "seed the claim's station")

	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "HO-1")
	_, err = db.Exec(`UPDATE bins SET uop_remaining = 0 WHERE id = $1`, bin.ID)
	testutil.MustNoErr(t, err, "the carrier is labelled and empty")
	_, err = db.Exec(`INSERT INTO bin_uop_exception (kind, bin_id, actor, occurred_at, op, detail)
		VALUES ($1, $2, 'line-1', NOW(), $3, '{"delta": -3}')`, audit.ExcStaleEpoch, bin.ID, audit.OpStaleEpochDropped)
	testutil.MustNoErr(t, err, "seed a stale drop")
	ledger := func(op, reason, station string, before, after int) {
		t.Helper()
		_, err := db.Exec(`INSERT INTO bin_uop_ledger (bin_id, before_uop, after_uop, op, reason, station)
			VALUES ($1, $2, $3, $4, $5, $6)`, bin.ID, before, after, op, reason, station)
		testutil.MustNoErr(t, err, "seed a ledger row")
	}
	ledger(audit.OpReleasedPartial, "", "", 40, 40)
	ledger("bin_uop_delta", "consume_tick", "line-1", 40, 39)
	ledger("bin_uop_delta", "consume_tick", "line-1", 39, 37)
	ledger(audit.OpSetForProduction, "", "", 37, 50) // the next generation: ticks after it are not counted
	ledger("bin_uop_delta", "consume_tick", "line-1", 50, 49)
	ledger(audit.OpReleasedEmpty, "", "", 49, 0)

	h, err := readHandoff(db.DB)
	testutil.MustNoErr(t, err, "read the handoff")
	if sumCounts(h.placements) != 3 {
		t.Errorf("placements = %+v, want the three Bound adjustments only", h.placements)
	}
	if !h.claims["line-1"][line] {
		t.Errorf("claims = %v, want line-1 modelling %s", h.claims, line)
	}
	if len(h.stale) != 1 || h.stale[0].station != "line-1" || h.stale[0].n != 1 || h.stale[0].units != 3 {
		t.Errorf("stale = %+v, want line-1 one drop of 3", h.stale)
	}
	changed := 0
	for _, r := range h.releases {
		changed += r.changed
	}
	if len(h.releases) != 2 || changed != 1 {
		t.Errorf("releases = %+v, want two ops and one that moved the count", h.releases)
	}
	if sumCounts(h.refreshes) != 2 || h.refreshes[0].n != 1 {
		t.Errorf("refreshes = %+v, want two, one per node", h.refreshes)
	}
	if len(h.zeroAtRest) != 1 || h.zeroAtRest[0].node != line {
		t.Errorf("labelled at 0 = %+v, want the one carrier at %s", h.zeroAtRest, line)
	}
	a := h.after
	if a.releases != 2 || a.followed != 1 || a.ticks != 2 || a.units != 3 || a.maxPer != 2 || a.byStation["line-1"] != 2 {
		t.Errorf("ticks after release = %+v, want 1 of 2 releases followed by 2 ticks (3 units) from line-1", a)
	}
}
