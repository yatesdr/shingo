//go:build docker

package service

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// countableBin makes a bin RecordCount will accept: it needs a payload, and that
// payload needs a UOP capacity.
func countableBin(t *testing.T, db *store.DB, sd *testdb.StandardData) *bins.Bin {
	t.Helper()
	b := &bins.Bin{BinTypeID: sd.BinType.ID, Label: "COUNT-" + t.Name(), NodeID: &sd.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(b), "create bin")
	testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, sd.Payload.Code, 10), "set manifest")
	got, err := db.GetBin(b.ID)
	testutil.MustNoErr(t, err, "read bin back")
	return got
}

// TestRecordCount_ClearsTheAnomalyFlag closes a ratchet found in live plant data.
//
// anomaly_at means "this carrier has had counts refused — go cycle count it".
// The count did not clear it, so the flag only ever accumulated. At Hopkinsville
// on 2026-08-02 every one of the ten bins carried it, seven of them counted
// AFTER being flagged and still flagged, the oldest since May. A marker set on
// everything and cleared by nothing tells an operator which carrier to go count
// only in the sense that the answer is "all of them".
func TestRecordCount_ClearsTheAnomalyFlag(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinService(db, NewBinManifestService(db, EpochAnnounce{}))
	bin := countableBin(t, db, sd)
	testutil.MustNoErr(t, svc.MarkAnomaly(bin.ID), "flag the bin")

	flagged, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read back flagged bin")
	if flagged.AnomalyAt == nil {
		t.Fatal("fixture: the bin is not flagged, so the test proves nothing")
	}

	// The operator does exactly what the flag asked for.
	if _, err := svc.RecordCount(flagged, 5, "operator-under-test"); err != nil {
		t.Fatalf("RecordCount: %v", err)
	}

	after, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read back counted bin")
	if after.AnomalyAt != nil {
		t.Error("the bin was counted and is still flagged; the flag asks for a count and then ignores it")
	}
	if after.LastCountedAt == nil {
		t.Error("the count itself did not record")
	}
	if after.UOPRemaining != 5 {
		t.Errorf("uop_remaining = %d, want 5 — clearing the flag must not cost the count", after.UOPRemaining)
	}
}

// TestRecordCount_DoesNotBumpTheEpoch pins the deliberate non-change beside the
// fix above.
//
// A count corrects the number inside a carrier; it does not end that carrier's
// load lifecycle. Bumping delta_epoch here would open a stale-epoch drop window
// against an Edge with no way to learn the new value — the "next bin-state
// refresh" the drop path names does not exist, which is the live gap that has
// Hopkinsville discarding about half its counts.
func TestRecordCount_DoesNotBumpTheEpoch(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinService(db, NewBinManifestService(db, EpochAnnounce{}))
	bin := countableBin(t, db, sd)
	var before int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, bin.ID).Scan(&before),
		"read epoch before")

	fresh, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if _, err := svc.RecordCount(fresh, 3, "operator-under-test"); err != nil {
		t.Fatalf("RecordCount: %v", err)
	}

	var after int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, bin.ID).Scan(&after),
		"read epoch after")
	if after != before {
		t.Errorf("delta_epoch moved %d -> %d; a cycle count is not a new load lifecycle, and bumping it strands the Edge", before, after)
	}
}
