package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/uop"
)

// lineside_reporter_test.go — end to end: what actually goes on the wire.
//
// The store query decides what CAN be said; this decides what IS said. The
// entries are a checksum Core compares against its replica, and the payload is
// a primary-key member there — so a wrong name does not degrade a comparison,
// it compares the seat against a part nobody established.

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
	eng.recordLinesideCarrier(nodeID, "ALN_007", domain.KnownCarrier("SYN-PART01E.06"), domain.CarrierFromDelivery)

	eng.reportLinesideLevels()

	entries := reportedEntries(t, db)
	if len(entries) != 1 {
		t.Fatalf("shipped %d entries, want 1: %+v", len(entries), entries)
	}
	if entries[0].PayloadCode != "SYN-PART01E.06" {
		t.Errorf("PayloadCode = %q, want SYN-PART01E.06. SYN-PART09D.06 is the claim's, and it "+
			"is what Springfield shipped once a minute for three days against a carrier holding "+
			"7032 of the other one.", entries[0].PayloadCode)
	}
	if entries[0].BinUOP != 7032 {
		t.Errorf("BinUOP = %d, want 7032", entries[0].BinUOP)
	}
}

// A CARRIER NOBODY IDENTIFIED SHIPS NOTHING. Absence is the designed
// degradation: there is no safe guess at what the carrier is.
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

// A STATION WITH NOTHING AT ANY SEAT SENDS NOTHING. The seat is configured and
// runs, but no carrier is bound and no bucket holds parts, so the level query
// returns no row and the reporter returns before building a message. Core then
// hears nothing from the station at all. PIN (memory close-out, owner ruling
// 2a): the change sends every interval, and names this seat as empty.
func TestPin_2a_NothingAtAnySeatSendsNoReport(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	seedDirectChangeover(t, db)

	eng.reportLinesideLevels()

	msgs, err := db.ListPendingOutbox(50)
	testutil.MustNoErr(t, err, "list outbox")
	for _, m := range msgs {
		if m.MsgType == string(protocol.SubjectLinesideLevelReport) {
			t.Errorf("a report went out for a station with nothing at any seat: %s", m.Payload)
		}
	}
}

// WHAT ONE 60 s REPORT COSTS THE PI. PIN of the statement count, green before
// and after lane A of the memory build: the report is the level SELECT and one
// snapshot enqueue, whatever lane A adds to each row. Lane A states each count
// as of its flushed seq without flushing (the real accumulator, with counts
// pending, is pinned at the same 3 by
// TestReportLinesideLevels_CountIsAsOfFlushedSeqWithoutFlushing). The message
// size is logged, not pinned: it is the number statements.md compares before
// and after.
func TestReportLinesideLevels_StatementsPerReport(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	eng := testEngine(t, db)
	_, nodeID, _, fromClaimID, _ := seedDirectChangeover(t, db)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeForDeliveredBin(nodeID, &fromClaimID, 15, 1, 7032), "bind carrier")
	eng.recordLinesideCarrier(nodeID, "ALN_007", domain.KnownCarrier("PART-A"), domain.CarrierFromDelivery)

	counter.Reset()
	eng.reportLinesideLevels()
	got := counter.Count()

	msgs, err := db.ListPendingOutbox(50)
	testutil.MustNoErr(t, err, "list outbox")
	for _, m := range msgs {
		if m.MsgType == string(protocol.SubjectLinesideLevelReport) {
			t.Logf("report message: %d bytes for 1 row", len(m.Payload))
		}
	}
	if got != 3 {
		t.Errorf("one report issued %d statements, want 3 (the level SELECT, and the snapshot "+
			"enqueue's delete of the unsent predecessor and its insert)", got)
	}
}

// reportedRaw returns each shipped lineside row as its raw JSON keys, so a test
// can see what is on the wire whether or not the Go struct names it.
func reportedRaw(t *testing.T, db *store.DB) []map[string]json.RawMessage {
	t.Helper()
	msgs, err := db.ListPendingOutbox(50)
	testutil.MustNoErr(t, err, "list outbox")
	var out []map[string]json.RawMessage
	for _, m := range msgs {
		if m.MsgType != string(protocol.SubjectLinesideLevelReport) {
			continue
		}
		var env struct {
			P struct {
				Data struct {
					Entries []map[string]json.RawMessage `json:"entries"`
				} `json:"data"`
			} `json:"p"`
		}
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "decode the report envelope")
		out = append(out, env.P.Data.Entries...)
	}
	return out
}

// THE ROW NAMES ITS CARRIER. Each shipped row carries the bound carrier's bin id
// and generation (the runtime row's active_bin_id / active_bin_epoch) and the
// highest delta seq allocated for that (bin, epoch) — the seq Core must have
// applied before a count gap is a divergence rather than a delta in flight
// (seat-count round 1 S1, round 2 S8). Seqs of another generation of the same
// bin, and of another bin, are not this row's. Verify-red at the base: the row
// has none of the three keys.
func TestReportLinesideLevels_RowNamesItsCarrierAndFlushedSeq(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, fromClaimID, _ := seedDirectChangeover(t, db)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeForDeliveredBin(nodeID, &fromClaimID, 15, 2, 7032), "bind carrier")
	eng.recordLinesideCarrier(nodeID, "ALN_007", domain.KnownCarrier("PART-A"), domain.CarrierFromDelivery)
	for _, s := range []struct {
		key   string
		epoch int64
		n     int
	}{{"15", 2, 3}, {"15", 1, 9}, {"16", 2, 5}} {
		for i := 0; i < s.n; i++ {
			_, _, err := db.AllocateInventoryDeltaSeq(protocol.InvDeltaScopeBin, s.key, s.epoch, 0)
			testutil.MustNoErr(t, err, "allocate seq")
		}
	}

	eng.reportLinesideLevels()

	rows := reportedRaw(t, db)
	if len(rows) != 1 {
		t.Fatalf("shipped %d rows, want 1", len(rows))
	}
	for key, want := range map[string]string{"bin_id": "15", "bin_epoch": "2", "flushed_seq": "3"} {
		if got, ok := rows[0][key]; !ok || string(got) != want {
			t.Errorf("%s = %s (present %v), want %s", key, got, ok, want)
		}
	}
}

// THE COUNT IS STATED AS OF FLUSHEDSEQ, AND NOTHING IS FLUSHED TO GET IT. A
// carrier bound at 7032 takes a 5-part tick: the runtime row reads 7027 at
// once, the accumulator holds -5 for the next flush, and nothing has been
// allocated. The report must say 7032 at flushed seq 0 — what Core holds once
// it has applied everything up to that seq — and must issue no statement and
// no message beyond its own. A bucket drained by 3 on the same tick reports its
// pre-drain qty the same way. Verify-red against the reporter that flushed
// first (lane A's first cut): its flush allocated and enqueued the bin and the
// bucket windows, so the report cost 7 statements and two delta messages, and
// said 7027 / 17 at seq 1.
func TestReportLinesideLevels_CountIsAsOfFlushedSeqWithoutFlushing(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	eng := testEngine(t, db)
	mut := uop.New(db, "stn-test", db, db, db)
	eng.SetInventoryDeltaSink(mut)
	_, nodeID, _, fromClaimID, _ := seedDirectChangeover(t, db)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeForDeliveredBin(nodeID, &fromClaimID, 15, 1, 7032), "bind carrier")
	eng.recordLinesideCarrier(nodeID, "ALN_007", domain.KnownCarrier("PART-A"), domain.CarrierFromDelivery)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read node")
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	styleID := int64(0)
	if rt.ActiveClaimID != nil {
		c, cerr := db.GetStyleNodeClaim(*rt.ActiveClaimID)
		testutil.MustNoErr(t, cerr, "read claim")
		styleID = c.StyleID
	}
	_, err = db.CaptureLinesideBucket(nodeID, "", styleID, "PART-A", 20)
	testutil.MustNoErr(t, err, "seed bucket")

	// One tick, as the tick path does it: the database first, then the record.
	testutil.MustNoErr(t, db.UpdateProcessNodeUOP(nodeID, 7027), "tick: runtime count")
	_, _, err = db.DrainLinesideBucket(nodeID, "PART-A", 3)
	testutil.MustNoErr(t, err, "tick: bucket drain")
	testutil.MustNoErr(t, mut.Consumed(uop.TickEvent{
		NodeID: nodeID, StyleID: styleID, CoreNodeName: node.CoreNodeName,
		BinID: 15, PayloadCode: "PART-A", BinEpoch: 1, BinRemainder: 5,
		Drains: map[string]uop.LinesideDrain{"PART-A": {Qty: 3, StyleID: styleID}},
	}), "tick: record")

	counter.Reset()
	eng.reportLinesideLevels()
	if got := counter.Count(); got != 3 {
		t.Errorf("the report issued %d statements, want 3 — it must not flush to state its count", got)
	}

	rows := reportedRaw(t, db)
	if len(rows) != 1 {
		t.Fatalf("shipped %d rows, want 1", len(rows))
	}
	for key, want := range map[string]string{"bin_uop": "7032", "bucket_qty": "20", "bin_id": "15"} {
		if got := string(rows[0][key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	if fs, ok := rows[0]["flushed_seq"]; ok {
		t.Errorf("flushed_seq = %s, want absent (0): nothing was flushed", fs)
	}
	msgs, err := db.ListPendingOutbox(50)
	testutil.MustNoErr(t, err, "list outbox")
	for _, m := range msgs {
		if m.MsgType != string(protocol.SubjectLinesideLevelReport) {
			t.Errorf("the report put a %s message in the outbox; it must add none", m.MsgType)
		}
	}

	// And once the accumulator flushes, the same seat reports the runtime's
	// count at the new seq: the pair moves together.
	mut.Flush()
	_, err = db.Exec(`DELETE FROM outbox WHERE msg_type = ?`, string(protocol.SubjectLinesideLevelReport))
	testutil.MustNoErr(t, err, "clear the first report")
	eng.reportLinesideLevels()
	rows = reportedRaw(t, db)
	if len(rows) != 1 || string(rows[0]["bin_uop"]) != "7027" || string(rows[0]["flushed_seq"]) != "1" || string(rows[0]["bucket_qty"]) != "17" {
		t.Errorf("after the flush: %v, want bin_uop 7027, flushed_seq 1, bucket_qty 17", rows)
	}
}

// TICKS AND REPORTS RACE, AND EVERY REPORT STILL SAYS THE SAME NUMBER. With the
// accumulator never flushed, Core's count as of FlushedSeq 0 is the bind count
// however many ticks have landed, so every report taken while ticks run through
// the production door must say 7032. A report that read the runtime row after a
// tick's write and the accumulator before its record would say 7031 or less:
// countMu is what closes that window.
func TestReportLinesideLevels_TicksRacingReportsNeverMoveTheStatedCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	mut := uop.New(db, "stn-test", db, db, db)
	eng.SetInventoryDeltaSink(mut)
	processID, nodeID, _, fromClaimID, _ := seedDirectChangeover(t, db)
	claim, err := db.GetStyleNodeClaim(fromClaimID)
	testutil.MustNoErr(t, err, "read claim")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeForDeliveredBin(nodeID, &fromClaimID, 15, 1, 7032), "bind carrier")
	eng.recordLinesideCarrier(nodeID, "ALN_007", domain.KnownCarrier("PART-A"), domain.CarrierFromDelivery)

	const ticks = 200
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < ticks; i++ {
			eng.handleCounterDelta(CounterDeltaEvent{ProcessID: processID, StyleID: claim.StyleID, Delta: 1})
		}
	}()
	reports := 0
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		eng.reportLinesideLevels()
		reports++
		for _, r := range reportedRaw(t, db) {
			if got := string(r["bin_uop"]); got != "7032" {
				t.Fatalf("report %d said bin_uop %s while ticks ran, want 7032 (nothing flushed)", reports, got)
			}
		}
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.RemainingUOPCached != 7032-ticks {
		t.Fatalf("runtime count = %d after %d ticks, want %d — the ticks did not land, so the race was not exercised",
			rt.RemainingUOPCached, ticks, 7032-ticks)
	}
	t.Logf("%d reports raced %d ticks", reports, ticks)
}

// FLUSHES AND TICKS RACE REPORTS, AND EVERY REPORT STATES THE NET AT ITS OWN
// FLUSHEDSEQ. Each flushed bin_uop_delta carries its seq and the scope's
// running net; a report at flushed seq s must state the bind count plus the net
// that seq s carried (the bind count itself at s = 0). A report whose SELECT
// and pending snapshot straddled a flush would count that flush's window twice
// or not at all; the flush lock is what keeps the pair together.
func TestReportLinesideLevels_FlushesRacingReportsStateTheNetAtFlushedSeq(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	mut := uop.New(db, "stn-test", db, db, db)
	eng.SetInventoryDeltaSink(mut)
	processID, nodeID, _, fromClaimID, _ := seedDirectChangeover(t, db)
	claim, err := db.GetStyleNodeClaim(fromClaimID)
	testutil.MustNoErr(t, err, "read claim")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeForDeliveredBin(nodeID, &fromClaimID, 15, 1, 7032), "bind carrier")
	eng.recordLinesideCarrier(nodeID, "ALN_007", domain.KnownCarrier("PART-A"), domain.CarrierFromDelivery)

	const ticks = 200
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < ticks; i++ {
			eng.handleCounterDelta(CounterDeltaEvent{ProcessID: processID, StyleID: claim.StyleID, Delta: 1})
			if i%7 == 0 {
				mut.Flush()
			}
		}
	}()
	type stated struct{ seq, uop int64 }
	var seen []stated
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		eng.reportLinesideLevels()
		for _, r := range reportedRaw(t, db) {
			var s stated
			testutil.MustNoErr(t, json.Unmarshal(r["bin_uop"], &s.uop), "bin_uop")
			if raw, ok := r["flushed_seq"]; ok {
				testutil.MustNoErr(t, json.Unmarshal(raw, &s.seq), "flushed_seq")
			}
			seen = append(seen, s)
		}
	}
	mut.Flush()

	// The net each seq carried, from the delta messages themselves.
	rows, err := db.Query(`SELECT payload FROM outbox WHERE msg_type = ?`, string(protocol.SubjectBinUOPDelta))
	testutil.MustNoErr(t, err, "read deltas")
	netAt := map[int64]int64{0: 0}
	for rows.Next() {
		var payload []byte
		testutil.MustNoErr(t, rows.Scan(&payload), "scan delta")
		var env struct {
			P struct {
				Data protocol.BinUOPDelta `json:"data"`
			} `json:"p"`
		}
		testutil.MustNoErr(t, json.Unmarshal(payload, &env), "decode delta")
		if env.P.Data.Net != nil {
			netAt[env.P.Data.SequenceID] = *env.P.Data.Net
		}
	}
	testutil.MustNoErr(t, rows.Err(), "delta rows")
	rows.Close()

	moved := 0
	for i, s := range seen {
		net, ok := netAt[s.seq]
		if !ok {
			t.Fatalf("report %d names flushed seq %d, which no delta message carries", i, s.seq)
		}
		if s.uop != 7032+net {
			t.Errorf("report %d at flushed seq %d stated %d, want %d (bind 7032 + net %d)", i, s.seq, s.uop, 7032+net, net)
		}
		if s.seq > 0 {
			moved++
		}
	}
	if moved == 0 || len(netAt) < 3 {
		t.Fatalf("%d reports after a flush, %d seqs flushed — the race was not exercised", moved, len(netAt)-1)
	}
	t.Logf("%d reports, %d at a flushed seq, %d seqs", len(seen), moved, len(netAt)-1)
}
