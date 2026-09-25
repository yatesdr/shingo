//go:build docker

package uop_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"

	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/uop"
)

const (
	active   = protocol.LinesideBucketActive
	stranded = protocol.LinesideBucketStranded
)

func levelService(db *store.DB) *uop.InventoryDeltaService {
	return uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})
}

// pileRow reads Core's mirror row for one (node, payload, state): its qty and
// the station that last sent its level, and whether it exists.
func pileRow(t *testing.T, db *store.DB, node, payload string, state protocol.LinesideBucketState) (qty int, station string, ok bool) {
	t.Helper()
	err := db.QueryRow(`SELECT qty, station FROM lineside_buckets
		WHERE core_node_name=$1 AND payload_code=$2 AND state=$3`, node, payload, string(state)).Scan(&qty, &station)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false
	}
	testutil.MustNoErr(t, err, "read pile row")
	return qty, station, true
}

// drainRows returns the drain ledger's (before, after) pairs for one payload,
// oldest first.
func drainRows(t *testing.T, db *store.DB, payload string) [][2]int {
	t.Helper()
	rows, err := db.Query(`SELECT before_qty, after_qty FROM lineside_drain_ledger
		WHERE payload_code=$1 ORDER BY id`, payload)
	testutil.MustNoErr(t, err, "read drain ledger")
	defer rows.Close()
	var out [][2]int
	for rows.Next() {
		var b, a int
		testutil.MustNoErr(t, rows.Scan(&b, &a), "scan drain row")
		out = append(out, [2]int{b, a})
	}
	testutil.MustNoErr(t, rows.Err(), "drain rows")
	return out
}

// A level SETS the row: up, down and up again, one row throughout, each level
// replacing the last rather than adding to it.
func TestBucketLevel_SetsTheRowUpAndDown(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := levelService(db)
	node := sd.LineNode.Name

	for i, want := range []int{40, 25, 60} {
		testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(testStation,
			makeBucketLevel(node, "PART-LV", active, want, 0, int64(i+1))), "level")
		if qty, _, ok := pileRow(t, db, node, "PART-LV", active); !ok || qty != want {
			t.Fatalf("after level %d: qty = %d (exists %v), want %d", i+1, qty, ok, want)
		}
	}
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM lineside_buckets WHERE core_node_name=$1`, node).Scan(&n), "count")
	if n != 1 {
		t.Errorf("rows at the node = %d, want 1", n)
	}
}

// Qty 0 means the row is gone: the level deletes it, and the dedup row stays,
// so a late level of the deleted row (a lower seq) cannot bring it back.
func TestBucketLevel_ZeroDeletesTheRowAndALateLevelCannotReviveIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := levelService(db)
	node := sd.LineNode.Name

	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(testStation, makeBucketLevel(node, "PART-Z", active, 10, 0, 1)), "seq 1")
	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(testStation, makeBucketLevel(node, "PART-Z", active, 0, 10, 2)), "seq 2 to 0")
	if _, _, ok := pileRow(t, db, node, "PART-Z", active); ok {
		t.Fatal("a level of 0 left the row in place")
	}
	late := makeBucketLevel(node, "PART-Z", active, 10, 0, 1)
	late.WindowEnd = late.WindowEnd.Add(-time.Hour)
	if err := svc.ApplyLinesideBucketLevel(testStation, late); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("late seq 1: err = %v, want ErrInventoryDeltaSkipped", err)
	}
	if _, _, ok := pileRow(t, db, node, "PART-Z", active); ok {
		t.Error("a late level revived a deleted row")
	}
}

// A level at or below the row's high-water seq, with no later window, is
// skipped: a duplicate, or a late level the one that passed it replaced.
func TestBucketLevel_StaleSeqIsSkipped(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := levelService(db)
	node := sd.LineNode.Name

	first := makeBucketLevel(node, "PART-S", active, 30, 0, 5)
	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(testStation, first), "seq 5")
	if err := svc.ApplyLinesideBucketLevel(testStation, first); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("duplicate: err = %v, want ErrInventoryDeltaSkipped", err)
	}
	late := makeBucketLevel(node, "PART-S", active, 99, 0, 4)
	late.WindowEnd = first.WindowEnd.Add(-time.Hour)
	if err := svc.ApplyLinesideBucketLevel(testStation, late); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("late seq 4: err = %v, want ErrInventoryDeltaSkipped", err)
	}
	if qty, _, _ := pileRow(t, db, node, "PART-S", active); qty != 30 {
		t.Errorf("qty = %d, want 30 (the skipped levels are not applied)", qty)
	}
}

// The active and the stranded row of one (node, payload) are two rows under
// two seq scopes: a level of one never touches the other, and their seqs are
// numbered independently.
func TestBucketLevel_ActiveAndStrandedRowsAreIndependent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := levelService(db)
	node := sd.LineNode.Name

	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(testStation, makeBucketLevel(node, "PART-AS", active, 12, 0, 1)), "active seq 1")
	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(testStation, makeBucketLevel(node, "PART-AS", stranded, 7, 0, 1)), "stranded seq 1")
	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(testStation, makeBucketLevel(node, "PART-AS", active, 0, 0, 2)), "active seq 2 to 0")

	if _, _, ok := pileRow(t, db, node, "PART-AS", active); ok {
		t.Error("active row survived its level of 0")
	}
	if qty, _, ok := pileRow(t, db, node, "PART-AS", stranded); !ok || qty != 7 {
		t.Errorf("stranded row = %d (exists %v), want 7: the active row's level must not touch it", qty, ok)
	}
}

// One physical pile reported by two stations is one row: the conflict target
// is the pile, never the station, which rides along as the last reporter. (v65
// kept station out of the key; the level keeps it out.)
func TestBucketLevel_OneNodeOneRowAcrossStations(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := levelService(db)
	node := sd.LineNode.Name

	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel("PLANT.EDGE-1", makeBucketLevel(node, "PART-2E", active, 30, 0, 1)), "edge-1")
	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel("PLANT.EDGE-2", makeBucketLevel(node, "PART-2E", active, 42, 0, 1)), "edge-2")
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM lineside_buckets WHERE core_node_name=$1`, node).Scan(&n), "count")
	qty, station, _ := pileRow(t, db, node, "PART-2E", active)
	if n != 1 || qty != 42 || station != "PLANT.EDGE-2" {
		t.Errorf("rows=%d qty=%d station=%q, want one row at 42 last sent by PLANT.EDGE-2", n, qty, station)
	}
}

// Drained on an active level writes exactly one drain-ledger row, before =
// Qty + Drained and after = Qty: the consumption rate's drain arm.
func TestBucketLevel_DrainedWritesOneLedgerRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := levelService(db)
	node := sd.LineNode.Name

	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(testStation, makeBucketLevel(node, "PART-DR", active, 50, 0, 1)), "capture")
	if got := drainRows(t, db, "PART-DR"); len(got) != 0 {
		t.Fatalf("a capture wrote drain rows %v, want none (a pull is not consumption)", got)
	}
	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(testStation, makeBucketLevel(node, "PART-DR", active, 38, 12, 2)), "drain 12")
	got := drainRows(t, db, "PART-DR")
	if len(got) != 1 || got[0] != [2]int{50, 38} {
		t.Fatalf("drain rows = %v, want exactly [[50 38]]", got)
	}
	var nodeID int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT node_id FROM lineside_drain_ledger WHERE payload_code='PART-DR'`).Scan(&nodeID), "node id")
	if nodeID != sd.LineNode.ID {
		t.Errorf("drain row node_id = %d, want %d", nodeID, sd.LineNode.ID)
	}
}

// A stranded level writes no drain row, whatever it carries: a strand is a
// count correction, not consumption.
func TestBucketLevel_StrandedLevelWritesNoLedgerRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := levelService(db)
	node := sd.LineNode.Name

	testutil.MustNoErr(t, svc.ApplyLinesideBucketLevel(testStation, makeBucketLevel(node, "PART-SN", stranded, 9, 4, 1)), "stranded")
	if got := drainRows(t, db, "PART-SN"); len(got) != 0 {
		t.Errorf("drain rows = %v, want none for a stranded level", got)
	}
	if qty, _, ok := pileRow(t, db, node, "PART-SN", stranded); !ok || qty != 9 {
		t.Errorf("stranded row = %d (exists %v), want 9", qty, ok)
	}
}

// A level Core cannot place is refused before anything is written: a node name
// Core does not know, a state that is neither of the two, a negative qty.
func TestBucketLevel_RefusesWhatItCannotPlace(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := levelService(db)
	node := sd.LineNode.Name

	for name, l := range map[string]*protocol.LinesideBucketLevel{
		"unknown node":    makeBucketLevel("NO-SUCH-NODE", "PART-X", active, 5, 0, 1),
		"unknown state":   makeBucketLevel(node, "PART-X", "inactive", 5, 0, 1),
		"negative qty":    makeBucketLevel(node, "PART-X", active, -1, 0, 1),
		"missing payload": makeBucketLevel(node, "", active, 5, 0, 1),
	} {
		err := svc.ApplyLinesideBucketLevel(testStation, l)
		if err == nil || errors.Is(err, uop.ErrInventoryDeltaSkipped) {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM inventory_delta_dedup WHERE scope_kind=$1`,
		protocol.InvDeltaScopeBucketLevel).Scan(&n), "count dedup")
	if n != 0 {
		t.Errorf("refused levels left %d dedup row(s), want 0", n)
	}
}
