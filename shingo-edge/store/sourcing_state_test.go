package store

import (
	"path/filepath"
	"testing"
	"time"

	"shingo/protocol"
)

func srcState(process, style, status string, missing ...string) protocol.SourcingState {
	return protocol.SourcingState{
		ProcessID:  process,
		StyleID:    style,
		Status:     status,
		Missing:    missing,
		ComputedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
}

func findState(states []protocol.SourcingState, process, style string) (protocol.SourcingState, bool) {
	for _, s := range states {
		if s.ProcessID == process && s.StyleID == style {
			return s, true
		}
	}
	return protocol.SourcingState{}, false
}

func TestSourcingState_UpsertRoundTrip(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	in := protocol.SourcingState{
		ProcessID: "SNF2", StyleID: "A", Status: "red",
		Missing: []string{"BIN-B", "BIN-C"},
		AtRisk:  []protocol.SourcingAtRisk{{PayloadCode: "BIN-A", Node: "N1", TimeToEmptySeconds: 42.5}},
		Reason:  "cannot change over — missing BIN-B, BIN-C",
	}
	if err := db.UpsertSourcingState([]protocol.SourcingState{in}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := db.ListSourcingStateForProcess("SNF2")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	s := got[0]
	if s.Status != "red" || len(s.Missing) != 2 || s.Missing[0] != "BIN-B" {
		t.Errorf("missing round-trip = %+v", s)
	}
	if len(s.AtRisk) != 1 || s.AtRisk[0].TimeToEmptySeconds != 42.5 {
		t.Errorf("at_risk round-trip = %+v", s.AtRisk)
	}
	if s.Reason != in.Reason {
		t.Errorf("reason = %q, want %q", s.Reason, in.Reason)
	}
}

func TestSourcingState_SnapshotDropsStaleRows(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	// Seed two styles.
	if err := db.UpsertSourcingState([]protocol.SourcingState{
		srcState("SNF2", "A", "green"),
		srcState("SNF2", "B", "red", "BIN-B"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A full snapshot that no longer includes B must drop it.
	if err := db.ReplaceSourcingState([]protocol.SourcingState{
		srcState("SNF2", "A", "green"),
	}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	all, _ := db.ListSourcingState()
	if _, ok := findState(all, "SNF2", "B"); ok {
		t.Errorf("style B survived a snapshot that dropped it: %+v", all)
	}
	if _, ok := findState(all, "SNF2", "A"); !ok {
		t.Errorf("style A missing after snapshot: %+v", all)
	}
}

func TestSourcingState_DeltaUpdatesInPlace(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	if err := db.ReplaceSourcingState([]protocol.SourcingState{
		srcState("SNF2", "A", "green"),
		srcState("SNF2", "B", "red", "BIN-B"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A change delta for A only must update A and leave B untouched.
	if err := db.UpsertSourcingState([]protocol.SourcingState{
		srcState("SNF2", "A", "red", "BIN-A"),
	}); err != nil {
		t.Fatalf("delta: %v", err)
	}
	all, _ := db.ListSourcingState()
	a, _ := findState(all, "SNF2", "A")
	b, okB := findState(all, "SNF2", "B")
	if a.Status != "red" || len(a.Missing) != 1 || a.Missing[0] != "BIN-A" {
		t.Errorf("A after delta = %+v, want red missing BIN-A", a)
	}
	if !okB || b.Status != "red" {
		t.Errorf("B changed by an A-only delta = %+v", b)
	}
}

// TestSourcingState_SurvivesRestart: written state is on disk, so reopening the
// same DB file (an HMI reload / Edge reboot) still reads it — no Core round-trip.
func TestSourcingState_SurvivesRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "restart.db")

	db1, err := Open(path)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if err := db1.ReplaceSourcingState([]protocol.SourcingState{
		srcState("SNF2", "A", "green"),
		srcState("SNF2", "B", "red", "BIN-B"),
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	db1.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { db2.Close() })
	all, err := db2.ListSourcingState()
	if err != nil {
		t.Fatalf("list after reopen: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("rows after restart = %d, want 2 (persisted)", len(all))
	}
}
