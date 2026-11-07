package store

import (
	"testing"
	"time"

	"shingo/protocol"
)

// sourcing_state_feedspin_test.go — what a full sourcing snapshot writes over
// one already held: nothing when no verdict changed (computed_at is ignored by
// the compare and the held one kept), one row per changed verdict, one per row
// that left. Before "perf(edge): sourcing writes only what changed, status
// shows the Core link, one stream per page" every snapshot deleted and
// re-inserted every row; the per-case comments give those counts.
//
// Measured with SQLite's total_changes(): the store holds one connection
// (SetMaxOpenConns(1)), so the difference across a call is every row the call
// inserted, updated or deleted. sourcing_state has no triggers.

func sourcingChanges(t *testing.T, db *DB) int64 {
	t.Helper()
	var n int64
	if err := db.DB.QueryRow(`SELECT total_changes()`).Scan(&n); err != nil {
		t.Fatalf("read total_changes: %v", err)
	}
	return n
}

func TestFeedsPin_SourcingSnapshotRewrites(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	t1 := t0.Add(time.Minute)
	at := func(s protocol.SourcingState, ts time.Time) protocol.SourcingState { s.ComputedAt = ts; return s }
	a := srcState("PROC-1", "A", "green")
	b := srcState("PROC-1", "B", "red", "PART-X")
	bGreen := srcState("PROC-1", "B", "green")

	cases := []struct {
		name   string
		second []protocol.SourcingState
		// rows written by the second snapshot, rows held after it, and the
		// computed_at held for (PROC-1, A)
		wantWrites int64
		wantRows   int
		wantA      time.Time
	}{
		{
			name:       "unchanged snapshot",
			second:     []protocol.SourcingState{at(a, t0), at(b, t0)},
			wantWrites: 0, // was DELETE 2 + INSERT 2
			wantRows:   2,
			wantA:      t0,
		},
		{
			name:       "same verdicts, newer computed_at",
			second:     []protocol.SourcingState{at(a, t1), at(b, t1)},
			wantWrites: 0, // computed_at is ignored by the compare; was 4
			wantRows:   2,
			wantA:      t0, // computed_at is the last verdict change; was t1
		},
		{
			name:       "one verdict changed",
			second:     []protocol.SourcingState{at(a, t0), at(bGreen, t0)},
			wantWrites: 1, // was 4
			wantRows:   2,
			wantA:      t0,
		},
		{
			name:       "one row left",
			second:     []protocol.SourcingState{at(a, t0)},
			wantWrites: 1, // was DELETE 2 + INSERT 1
			wantRows:   1,
			wantA:      t0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := coverageDB(t)
			if err := db.ReplaceSourcingState([]protocol.SourcingState{at(a, t0), at(b, t0)}); err != nil {
				t.Fatalf("first snapshot: %v", err)
			}
			before := sourcingChanges(t, db)
			if err := db.ReplaceSourcingState(tc.second); err != nil {
				t.Fatalf("second snapshot: %v", err)
			}
			if got := sourcingChanges(t, db) - before; got != tc.wantWrites {
				t.Errorf("rows written = %d, want %d", got, tc.wantWrites)
			}
			rows, err := db.ListSourcingState()
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(rows) != tc.wantRows {
				t.Errorf("rows held = %d, want %d", len(rows), tc.wantRows)
			}
			gotA, ok := findState(rows, "PROC-1", "A")
			if !ok {
				t.Fatalf("(PROC-1, A) missing after the second snapshot")
			}
			if !gotA.ComputedAt.Equal(tc.wantA) {
				t.Errorf("(PROC-1, A) computed_at = %v, want %v", gotA.ComputedAt, tc.wantA)
			}
		})
	}
}
