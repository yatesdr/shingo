package engine

import (
	"testing"

	"shingoedge/config"
	"shingoedge/plc"
	"shingoedge/store"
	"shingoedge/store/counters"
)

// SyncProcessCounter and the counter seed. A reporting point re-pointed to
// another style on the SAME tag keeps counting: the tag's running total is the
// one it has been reading. A reporting point re-pointed to a DIFFERENT tag is
// unread again, so its first read of the new tag seeds rather than charging the
// new tag's whole total (plc TestCounterSeed_*).

const seedPLC, seedTagA, seedTagB = "press", "Count_A", "Count_B"

// seedRig is a process counting on seedTagA under style s1, read once at 100.
func seedRig(t *testing.T) (eng *Engine, db *store.DB, proc, s1, s2, rpID int64) {
	t.Helper()
	db = testEngineDB(t)
	eng = testEngine(t, db)
	eng.plcMgr = plc.NewManager(nil, config.WarLinkConfig{}, 0, 0, nil, nil)
	var err error
	if proc, err = db.CreateProcess("PRESS", "", seedPLC, seedTagA, true); err != nil {
		t.Fatalf("create process: %v", err)
	}
	if s1, err = db.CreateStyle("S1", "", proc); err != nil {
		t.Fatalf("create style: %v", err)
	}
	if s2, err = db.CreateStyle("S2", "", proc); err != nil {
		t.Fatalf("create style: %v", err)
	}
	if err := db.SetActiveStyle(proc, &s1); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	if err := eng.SyncProcessCounter(proc); err != nil {
		t.Fatalf("sync: %v", err)
	}
	rp, err := db.GetReportingPointByStyleID(s1)
	if err != nil {
		t.Fatalf("reporting point for s1: %v", err)
	}
	if err := db.UpdateReportingPointCounter(rp.ID, 100); err != nil {
		t.Fatalf("read the counter: %v", err)
	}
	return eng, db, proc, s1, s2, rp.ID
}

func reportingPoints(t *testing.T, db *store.DB) []counters.ReportingPoint {
	t.Helper()
	rps, err := db.ListReportingPoints()
	if err != nil {
		t.Fatalf("list reporting points: %v", err)
	}
	return rps
}

// TestCounterSeed_StyleRepointOnTheSameTagKeepsCounting: a style change moves
// the reporting point to the new style and keeps its count and its read.
func TestCounterSeed_StyleRepointOnTheSameTagKeepsCounting(t *testing.T) {
	t.Parallel()
	eng, db, proc, _, s2, rpID := seedRig(t)

	if err := db.SetActiveStyle(proc, &s2); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	if err := eng.SyncProcessCounter(proc); err != nil {
		t.Fatalf("sync: %v", err)
	}

	rps := reportingPoints(t, db)
	if len(rps) != 1 {
		t.Fatalf("reporting points = %+v, want the one, re-pointed", rps)
	}
	rp := rps[0]
	if rp.ID != rpID || rp.StyleID != s2 || rp.TagName != seedTagA {
		t.Errorf("reporting point = id %d style %d tag %q, want id %d style %d tag %q", rp.ID, rp.StyleID, rp.TagName, rpID, s2, seedTagA)
	}
	if rp.LastCount != 100 || rp.LastPollAt == nil {
		t.Errorf("last_count/last_poll_at = %d/%v, want 100 and the read kept", rp.LastCount, rp.LastPollAt)
	}
}

// TestCounterSeed_TagChangeSeeds: the process's counter moves to another tag.
// The style's reporting point is re-pointed to it and is unread again, and no
// second reporting point is left counting the old tag onto the same style.
func TestCounterSeed_TagChangeSeeds(t *testing.T) {
	t.Parallel()
	eng, db, proc, s1, _, rpID := seedRig(t)

	if err := db.UpdateProcess(proc, "PRESS", "", seedPLC, seedTagB, true); err != nil {
		t.Fatalf("update process: %v", err)
	}
	if err := eng.SyncProcessCounter(proc); err != nil {
		t.Fatalf("sync: %v", err)
	}

	rps := reportingPoints(t, db)
	if len(rps) != 1 {
		t.Fatalf("reporting points = %+v, want one: the old tag must not keep counting onto the style", rps)
	}
	rp := rps[0]
	if rp.ID != rpID || rp.StyleID != s1 || rp.TagName != seedTagB {
		t.Errorf("reporting point = id %d style %d tag %q, want id %d style %d tag %q", rp.ID, rp.StyleID, rp.TagName, rpID, s1, seedTagB)
	}
	if rp.LastPollAt != nil {
		t.Errorf("last_poll_at = %v after a tag change, want NULL: the new tag's first read seeds", rp.LastPollAt)
	}
}
