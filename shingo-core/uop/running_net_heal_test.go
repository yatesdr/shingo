//go:build docker

package uop_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/audit"
	"shingocore/store/inventory"
	"shingocore/uop"
)

// running_net_heal_test.go — the running net (SYNTH-round2 §3, S3): every
// count message carries its scope's net, and Core applies what it has not
// applied yet. These are the rule's cases one at a time.

func int64p(v int64) *int64 { return &v }

// netDelta is seqDelta carrying the scope's running net, as a new Edge sends it.
func netDelta(binID int64, delta int, net, seq, epoch int64, windowEnd time.Time) *protocol.BinUOPDelta {
	d := seqDelta(binID, delta, seq, epoch, windowEnd)
	d.Net = int64p(net)
	return d
}

type deltaMeta struct {
	Delta       int    `json:"delta"`
	WireDelta   int    `json:"wire_delta"`
	Healed      int    `json:"healed"`
	Net         *int64 `json:"net"`
	SequenceID  int64  `json:"sequence_id"`
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`
}

// lastDeltaMeta reads the metadata of a bin's newest bin_uop_delta row.
func lastDeltaMeta(t *testing.T, db *store.DB, binID int64) deltaMeta {
	t.Helper()
	var raw string
	testutil.MustNoErr(t, db.QueryRow(`SELECT metadata FROM bin_uop_ledger
		WHERE bin_id=$1 AND op='bin_uop_delta' ORDER BY id DESC LIMIT 1`, binID).Scan(&raw), "read ledger metadata")
	var m deltaMeta
	testutil.MustNoErr(t, json.Unmarshal([]byte(raw), &m), "decode ledger metadata")
	return m
}

// A duplicate (same seq, same content) is a no-op: one ledger row, one apply,
// and no rollback (its window is not newer than the one applied).
func TestRunningNet_DuplicateIsANoOp(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-NET-DUP", "PART-A", 100)
	_, err := db.Exec(`UPDATE bins SET delta_epoch=1 WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "set epoch 1")
	d := netDelta(bin.ID, -2, -2, 1, 1, time.Now().UTC())

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, d), "first")
	if err := svc.ApplyBinUOPDelta(testStation, d); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("duplicate: err = %v, want ErrInventoryDeltaSkipped", err)
	}
	if got := binUOP(t, db, bin.ID); got != 98 {
		t.Errorf("uop_remaining = %d, want 98", got)
	}
	if got := deltaLedgerRows(t, db, bin.ID); got != 1 {
		t.Errorf("delta ledger rows = %d, want 1", got)
	}
	if got := exceptionRows(t, db, bin.ID); got != 0 {
		t.Errorf("exception rows = %d, want 0 (a duplicate is not a rollback)", got)
	}
	if got := binEpoch(t, db, bin.ID); got != 1 {
		t.Errorf("delta_epoch = %d, want 1", got)
	}
}

// A dead-lettered middle message heals on the next one, and the ledger row of
// the message that healed it records the heal. The daily roll-up reads the
// APPLIED delta, so the healed parts are in it.
func TestRunningNet_DeadLetteredMiddleHealsOnTheNext(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-NET-HEAL", "PART-A", 100)
	t0 := time.Now().UTC()

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, netDelta(bin.ID, -2, -2, 1, 0, t0)), "seq 1")
	// seq 2 (-4, net -6) never arrives.
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, netDelta(bin.ID, -3, -9, 3, 0, t0.Add(10*time.Second))), "seq 3")
	if got := binUOP(t, db, bin.ID); got != 91 {
		t.Errorf("uop_remaining = %d, want 91", got)
	}
	m := lastDeltaMeta(t, db, bin.ID)
	if m.Delta != -7 || m.WireDelta != -3 || m.Healed != -4 {
		t.Errorf("metadata = %+v, want delta -7, wire_delta -3, healed -4", m)
	}

	day := time.Now().UTC().Truncate(24 * time.Hour)
	_, err := audit.RollupBinUOPDeltaDay(db.DB, day)
	testutil.MustNoErr(t, err, "roll up the day")
	var consumed int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COALESCE(SUM(consumed), 0) FROM bin_uop_delta_daily
		WHERE bin_id=$1 AND day=$2`, bin.ID, day).Scan(&consumed), "read daily roll-up")
	if consumed != 9 {
		t.Errorf("daily consumed = %d, want 9 (2 + the healed 4 + 3)", consumed)
	}
}

// Mixed versions, the NULL anchor. A dedup row an old Edge created (no net) has
// applied_net NULL. The first net-bearing message on it applies its DELTA, not
// its net — the net covers deltas the row already applied — and anchors
// applied_net to the net it carried. After that the net rules.
func TestRunningNet_NullAnchorAppliesDeltaThenNet(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-NET-NULL", "PART-A", 100)
	t0 := time.Now().UTC()

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, seqDelta(bin.ID, -2, 1, 0, t0)), "old-Edge seq 1")
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, netDelta(bin.ID, -3, -40, 2, 0, t0.Add(5*time.Second))), "first net seq 2")
	if got := binUOP(t, db, bin.ID); got != 95 {
		t.Errorf("after the anchor: uop_remaining = %d, want 95 (the delta, not the net)", got)
	}
	cur, found, err := inventory.BinDeltaCursor(db, testStation, bin.ID, 0)
	testutil.MustNoErr(t, err, "read cursor")
	if !found || cur.AppliedNet == nil || *cur.AppliedNet != -40 || cur.LastSeq != 2 {
		t.Errorf("cursor = %+v found=%v, want applied_net -40, last_seq 2", cur, found)
	}
	// seq 3 is lost; seq 4 carries it.
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, netDelta(bin.ID, -1, -45, 4, 0, t0.Add(15*time.Second))), "seq 4")
	if got := binUOP(t, db, bin.ID); got != 90 {
		t.Errorf("after seq 4: uop_remaining = %d, want 90 (net -45 minus the anchor -40)", got)
	}
}

// Mixed versions, the absent row. A scope Core has never applied anything for
// applies the full net: nothing it carries was ever applied.
func TestRunningNet_AbsentRowAppliesTheNet(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-NET-ABSENT", "PART-A", 100)

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, netDelta(bin.ID, -1, -12, 5, 0, time.Now().UTC())), "first seen at seq 5")
	if got := binUOP(t, db, bin.ID); got != 88 {
		t.Errorf("uop_remaining = %d, want 88 (the full net)", got)
	}
}

// Mixed versions, the old Edge. A message with no net applies its delta
// exactly as before this change and leaves applied_net NULL.
func TestRunningNet_NilNetAppliesTheDeltaAndLeavesNoAnchor(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-NET-NIL", "PART-A", 100)
	t0 := time.Now().UTC()

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, seqDelta(bin.ID, -2, 1, 0, t0)), "seq 1")
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, seqDelta(bin.ID, -3, 3, 0, t0.Add(5*time.Second))), "seq 3")
	if got := binUOP(t, db, bin.ID); got != 95 {
		t.Errorf("uop_remaining = %d, want 95", got)
	}
	cur, found, err := inventory.BinDeltaCursor(db, testStation, bin.ID, 0)
	testutil.MustNoErr(t, err, "read cursor")
	if !found || cur.AppliedNet != nil || cur.LastSeq != 3 {
		t.Errorf("cursor = %+v found=%v, want applied_net NULL, last_seq 3", cur, found)
	}
	m := lastDeltaMeta(t, db, bin.ID)
	if m.Delta != -3 || m.WireDelta != -3 || m.Healed != 0 || m.Net != nil {
		t.Errorf("metadata = %+v, want delta -3, wire_delta -3, healed 0, no net", m)
	}
}

// The held mismatch. A consume delta refused for naming the wrong label does
// not consume its seq and does not advance applied_net, so when the next
// message's label agrees the refused parts land with it.
func TestRunningNet_RefusedLabelThenAcceptedLabelLandsBoth(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-NET-HELD", "PART-B", 100)
	t0 := time.Now().UTC()

	refused := netDelta(bin.ID, -5, -5, 1, 0, t0) // labelled PART-A
	if err := svc.ApplyBinUOPDelta(testStation, refused); err == nil || errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("label A: err = %v, want a payload-mismatch refusal", err)
	}
	if _, found, err := inventory.BinDeltaCursor(db, testStation, bin.ID, 0); err != nil || found {
		t.Fatalf("after the refusal: cursor found=%v err=%v, want no dedup row (seq and net unconsumed)", found, err)
	}
	accepted := netDelta(bin.ID, -3, -8, 2, 0, t0.Add(5*time.Second))
	accepted.PayloadCode = "PART-B"
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, accepted), "label B")
	if got := binUOP(t, db, bin.ID); got != 92 {
		t.Errorf("uop_remaining = %d, want 92 (both: the held -5 and the -3)", got)
	}
}

// BinDeltaCursor is the read lane C's record-count fence takes: Core's
// last_seq and applied_net for one (station, bin, epoch).
func TestRunningNet_BinDeltaCursor(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-NET-CUR", "PART-A", 100)
	_, err := db.Exec(`UPDATE bins SET delta_epoch=3 WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "set epoch 3")
	end := time.Now().UTC().Truncate(time.Millisecond)

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, netDelta(bin.ID, -4, -4, 7, 3, end)), "apply")
	cur, found, err := inventory.BinDeltaCursor(db, testStation, bin.ID, 3)
	testutil.MustNoErr(t, err, "read cursor")
	if !found || cur.LastSeq != 7 || cur.AppliedNet == nil || *cur.AppliedNet != -4 ||
		cur.AppliedWindowEnd == nil || !cur.AppliedWindowEnd.Equal(end) {
		t.Errorf("cursor = %+v found=%v, want last_seq 7, applied_net -4, applied_window_end %v", cur, found, end)
	}
	if _, found, err := inventory.BinDeltaCursor(db, testStation, bin.ID, 2); err != nil || found {
		t.Errorf("other epoch: found=%v err=%v, want not found", found, err)
	}
	if _, found, err := inventory.BinDeltaCursor(db, "stn-other", bin.ID, 3); err != nil || found {
		t.Errorf("other station: found=%v err=%v, want not found", found, err)
	}
}

// A pile carries no net: a level replaces the row. A lost middle level costs
// nothing, because the next level is the row as it stands. (This was
// TestRunningNet_BucketLostMiddleHeals, which healed a lost delta through the
// running net; the net is gone from the bucket stream, and the shape it pinned
// holds by construction.)
func TestBucketLevel_LostMiddleLevelCostsNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	node := sd.StorageNode.Name

	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(testStation, makeBucketLevel(node, "PART-N", active, 10, 0, 1)), "seq 1")
	// seq 2 (15) is lost.
	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(testStation, makeBucketLevel(node, "PART-N", active, 13, 2, 3)), "seq 3")
	if qty, _, ok := pileRow(t, db, node, "PART-N", active); !ok || qty != 13 {
		t.Errorf("pile qty = %d (exists %v), want 13", qty, ok)
	}
}
