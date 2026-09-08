package service

import (
	"time"

	"testing"

	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/counters"
)

// recordingEmitter captures EmitCounterDelta calls. Satisfies
// CounterDeltaEmitter with the same signature engine's plcEmitter has.
type recordingEmitter struct {
	calls []emittedDelta
}

type emittedDelta struct {
	rpID      int64
	processID int64
	styleID   int64
	delta     int64
	newCount  int64
	anomaly   string
}

func (r *recordingEmitter) EmitCounterDelta(rpID, processID, styleID, delta, newCount int64, anomaly string) {
	r.calls = append(r.calls, emittedDelta{rpID, processID, styleID, delta, newCount, anomaly})
}

func seedCounterFixture(t *testing.T, db *store.DB) (procID, styleID, rpID int64) {
	t.Helper()
	procID, styleID = seedProcessStyle(t, db, "PressLine", "StyleA")
	rpID, err := db.CreateReportingPoint("PLC1", "P42_SNF3", styleID)
	if err != nil {
		t.Fatalf("create reporting point: %v", err)
	}
	return procID, styleID, rpID
}

// TestConfirmAnomaly_ReleasesJumpDeltaDownstream is the regression test for
// the defect this change exists to fix: the poll loop withholds a jump's
// delta "pending operator confirmation", and confirmation emitted nothing,
// so the units never reached hourly_counts or the UOP path in either state.
//
// Verified red: with ConfirmAnomaly's body reverted to the bare
// `UPDATE counter_snapshots SET operator_confirmed = 1 WHERE id = ?` and
// the service method back to `return s.db.ConfirmAnomaly(id)`, this fails
// with "emitted 0 deltas, want 1".
func TestConfirmAnomaly_ReleasesJumpDeltaDownstream(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	procID, styleID, rpID := seedCounterFixture(t, db)

	// A jump as the poll loop records one: confirmed=false, delta withheld.
	snapID, err := db.InsertCounterSnapshot(rpID, 5000, 2432, "jump", false)
	if err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	em := &recordingEmitter{}
	svc := NewCounterService(db, time.UTC)
	svc.SetDeltaEmitter(em)

	if err := svc.ConfirmAnomaly(snapID); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	if len(em.calls) != 1 {
		t.Fatalf("emitted %d deltas, want 1", len(em.calls))
	}
	got := em.calls[0]
	want := emittedDelta{rpID: rpID, processID: procID, styleID: styleID, delta: 2432, newCount: 5000, anomaly: "jump"}
	if got != want {
		t.Errorf("emitted %+v, want %+v", got, want)
	}

	// And the row is gone from the popover.
	open, err := svc.ListUnconfirmedAnomalies()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("unconfirmed after confirm = %d, want 0", len(open))
	}
}

// TestConfirmAnomaly_SecondConfirmEmitsNothing pins the idempotency the
// release makes load-bearing. The popover's Confirm button is an
// undebounced POST, so a double-tap must not book the units twice.
//
// Verified red: with the UPDATE's `AND anomaly = 'jump' AND
// operator_confirmed = 0` guard removed, the second confirm reports one
// row affected, reads the row back and emits again — "emitted 2 deltas
// across two confirms, want 1".
func TestConfirmAnomaly_SecondConfirmEmitsNothing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, _, rpID := seedCounterFixture(t, db)

	snapID, err := db.InsertCounterSnapshot(rpID, 900, 400, "jump", false)
	if err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	em := &recordingEmitter{}
	svc := NewCounterService(db, time.UTC)
	svc.SetDeltaEmitter(em)

	for i := 0; i < 2; i++ {
		if err := svc.ConfirmAnomaly(snapID); err != nil {
			t.Fatalf("confirm %d: %v", i, err)
		}
	}
	if len(em.calls) != 1 {
		t.Fatalf("emitted %d deltas across two confirms, want 1", len(em.calls))
	}
}

// TestConfirmAnomaly_NonJumpSnapshotEmitsNothing pins the other half of the
// guard. Ordinary snapshots already emitted their delta at poll time;
// "confirming" one by id must not book the units a second time.
//
// Verified red: with the guard removed the bare `WHERE id = ?` flips a
// clean snapshot's flag, and the read-back emits — "emitted 1 delta for a
// non-jump snapshot, want 0".
func TestConfirmAnomaly_NonJumpSnapshotEmitsNothing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, _, rpID := seedCounterFixture(t, db)

	// A normal tick: no anomaly, already confirmed by the poll loop.
	snapID, err := db.InsertCounterSnapshot(rpID, 101, 1, "", true)
	if err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	em := &recordingEmitter{}
	svc := NewCounterService(db, time.UTC)
	svc.SetDeltaEmitter(em)

	if err := svc.ConfirmAnomaly(snapID); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(em.calls) != 0 {
		t.Fatalf("emitted %d deltas for a non-jump snapshot, want 0", len(em.calls))
	}
}

// TestConfirmAnomaly_NoEmitterStillConfirms keeps the nil-sink path honest:
// fixtures that build a bare CounterService must still be able to clear the
// popover.
func TestConfirmAnomaly_NoEmitterStillConfirms(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, _, rpID := seedCounterFixture(t, db)

	snapID, err := db.InsertCounterSnapshot(rpID, 900, 400, "jump", false)
	if err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
	svc := NewCounterService(db, time.UTC)
	if err := svc.ConfirmAnomaly(snapID); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	open, _ := svc.ListUnconfirmedAnomalies()
	if len(open) != 0 {
		t.Errorf("unconfirmed after confirm = %d, want 0", len(open))
	}
}

// bucketAt turns an RFC3339 UTC instant into the hour bucket it belongs to.
func bucketAt(t *testing.T, rfc3339 string) int64 {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		t.Fatalf("parse %q: %v", rfc3339, err)
	}
	return counters.HourBucket(ts)
}

// TestHourlyTotals_ReportsPlantLocalHours pins the read half of the UTC move:
// counts are STORED in UTC buckets and the plant zone is applied on the way
// out, so the operator's chart is keyed by the hour on the plant's wall clock.
//
// 12:00Z is 07:00 in Chicago, and an hour that lands just before local midnight
// belongs to the previous local day — the boundary Hopkinsville was getting
// wrong for three months, in the opposite direction.
func TestHourlyTotals_ReportsPlantLocalHours(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("no tzdata for America/Chicago: %v", err)
	}
	db := testdb.Open(t)
	procID, styleID, _ := seedCounterFixture(t, db)
	svc := NewCounterService(db, loc)

	// 07:00 and 08:00 plant-local on 2026-05-04.
	mustUpsert(t, db, procID, styleID, bucketAt(t, "2026-05-04T12:00:00Z"), 10)
	mustUpsert(t, db, procID, styleID, bucketAt(t, "2026-05-04T13:00:00Z"), 4)
	// 23:00 plant-local on 2026-05-03 — the previous day, and must not appear.
	mustUpsert(t, db, procID, styleID, bucketAt(t, "2026-05-04T04:00:00Z"), 99)

	totals, err := svc.HourlyTotals(procID, "2026-05-04")
	if err != nil {
		t.Fatalf("hourly totals: %v", err)
	}
	if totals[7] != 10 {
		t.Errorf("local hour 7 = %d, want 10 — buckets are not being mapped to plant hours", totals[7])
	}
	if totals[8] != 4 {
		t.Errorf("local hour 8 = %d, want 4", totals[8])
	}
	if totals[23] != 0 {
		t.Errorf("local hour 23 = %d, want 0 — the previous plant day leaked into this one", totals[23])
	}
}

// TestHourlyTotals_DSTFallBackSumsTheRepeatedHour pins the deliberate choice at
// the one moment a local hour is ambiguous.
//
// On 2026-11-01 Chicago passes through 01:00 twice — 06:00Z as CDT and 07:00Z
// as CST. They are two distinct hours of real production that share one label
// on the operator's clock, so the chart adds them. The old plant-local schema
// reached the same number by accident, through a UNIQUE collision that merged
// the two rows in storage and left nothing able to tell them apart again.
func TestHourlyTotals_DSTFallBackSumsTheRepeatedHour(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("no tzdata for America/Chicago: %v", err)
	}
	db := testdb.Open(t)
	procID, styleID, _ := seedCounterFixture(t, db)
	svc := NewCounterService(db, loc)

	mustUpsert(t, db, procID, styleID, bucketAt(t, "2026-11-01T06:00:00Z"), 40) // 01:00 CDT
	mustUpsert(t, db, procID, styleID, bucketAt(t, "2026-11-01T07:00:00Z"), 2)  // 01:00 CST

	totals, err := svc.HourlyTotals(procID, "2026-11-01")
	if err != nil {
		t.Fatalf("hourly totals: %v", err)
	}
	if totals[1] != 42 {
		t.Errorf("local hour 1 = %d, want 42 — both passes through the repeated hour "+
			"must be reported", totals[1])
	}
}

func mustUpsert(t *testing.T, db *store.DB, procID, styleID, bucket, delta int64) {
	t.Helper()
	if err := db.UpsertHourlyCount(procID, styleID, bucket, delta); err != nil {
		t.Fatalf("upsert bucket %d: %v", bucket, err)
	}
}
