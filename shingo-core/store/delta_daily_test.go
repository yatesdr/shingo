//go:build docker

package store_test

// The time axis for the drop panel. Same population as delta_integrity_test.go
// and the same seed helpers; what is asserted here is the bucketing and the
// SIGN, because those are the two things a window total cannot carry and the
// two the old copy got wrong.

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
)

func TestDeltaIntegrityDaily_BucketsByDayAndKeepsTheSign(t *testing.T) {
	db := testdb.Open(t)

	// Noon five days back, so nothing lands near a day boundary and the test
	// does not depend on when it runs.
	base := time.Now().UTC().Truncate(24 * time.Hour).Add(-5*24*time.Hour + 12*time.Hour)
	const day = 24 * 60 // seedDrop's offset is in minutes

	binID := int(seedBinWithUOP(t, db, "DD-BIN", "PART-DD", 500))

	// Day 0: consumption that never landed. Negative delta — the count is left
	// reading HIGH. This is Springfield's actual shape (12,514 of these, and
	// not one dropped credit on record).
	seedDrop(t, db, binID, 0, "stale_epoch_dropped", "PART-DD", -100, 500, base)
	seedDrop(t, db, binID, 5, "payload_mismatch_dropped", "PART-DD", -100, 500, base)

	// Day 2: a dropped CREDIT. Positive delta — the count reads LOW, and this
	// is the only direction that can explain a negative ledger.
	seedDrop(t, db, binID, 2*day, "stale_epoch_dropped", "PART-DD", 50, 500, base)

	// Day 0 also gets a rebind, which is NOT a drop and must not appear.
	seedDrop(t, db, binID, 10, "payload_rebound_with_inventory", "PART-DD", 300, 500, base)

	days, err := db.DeltaIntegrityDaily(base.Add(-time.Hour), "UTC")
	if err != nil {
		t.Fatalf("delta integrity daily: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("got %d day buckets, want 2 (the rebind must not create a third)", len(days))
	}

	// Day 0: the two consume drops, summed with their sign.
	if days[0].NetDelta != -200 {
		t.Errorf("day 0 net = %d, want -200 (dropped consumes leave the count reading high)", days[0].NetDelta)
	}
	if days[0].DropRows != 2 {
		t.Errorf("day 0 drop rows = %d, want 2 — the rebind is not a drop", days[0].DropRows)
	}
	if days[0].StaleEpochRows != 1 || days[0].PayloadMismatchRows != 1 {
		t.Errorf("day 0 cause split = %d stale / %d mismatch, want 1 / 1",
			days[0].StaleEpochRows, days[0].PayloadMismatchRows)
	}

	// Day 2 carries the opposite sign, and it must survive as a separate
	// bucket rather than netting against day 0. A window total would report
	// -150 here and hide that the two directions happened on different days
	// for different reasons.
	if days[1].NetDelta != 50 {
		t.Errorf("day 2 net = %d, want +50 (a dropped credit leaves the count reading low)", days[1].NetDelta)
	}

	if !days[1].Day.After(days[0].Day) {
		t.Errorf("buckets out of order: %v then %v", days[0].Day, days[1].Day)
	}
}

func TestDeltaIntegrityDaily_OmitsDaysWithNoDrops(t *testing.T) {
	db := testdb.Open(t)

	base := time.Now().UTC().Truncate(24 * time.Hour).Add(-5*24*time.Hour + 12*time.Hour)
	const day = 24 * 60

	binID := int(seedBinWithUOP(t, db, "DD-GAP", "PART-GAP", 300))
	// Two drops four days apart. The three days between them have none.
	seedDrop(t, db, binID, 0, "stale_epoch_dropped", "PART-GAP", -10, 300, base)
	seedDrop(t, db, binID, 4*day, "stale_epoch_dropped", "PART-GAP", -10, 300, base)

	days, err := db.DeltaIntegrityDaily(base.Add(-time.Hour), "UTC")
	if err != nil {
		t.Fatalf("delta integrity daily: %v", err)
	}

	// Two rows, not five. The gap days are absent rather than zero-filled: at
	// a plant they are weekends and shutdowns, and a zero-height bar would
	// assert a measured quiet day where there was no production to measure.
	if len(days) != 2 {
		t.Fatalf("got %d buckets, want 2 — empty days must be omitted, not zero-filled", len(days))
	}
}
