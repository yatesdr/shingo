//go:build docker

package bins_test

import (
	"testing"

	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
)

// sourceable_status_docker_test.go — the guard that keeps one rule from becoming
// two.
//
// domain.BinStatus.Sourceable (Go) and bins.SourceableStatusSQL (SQL) are the
// same rule written twice because sourcing decides membership in both languages:
// the pure predicates rank candidates in Go, the scoped readers filter in SQL.
// Two spellings of one rule is exactly how the eligibility drift this guards
// against started, so the pair is tested exhaustively rather than by example.
//
// EXHAUSTIVE ON PURPOSE. Every constant in the enum is exercised, plus a value
// from outside it. The status column carries no CHECK constraint and write-time
// validation is deferred, so an off-spec value is representable — and the two
// implementations must agree about it, not merely about the happy set.
//
// Adding a seventh status fails this test until it is classified on BOTH sides.
// That failure is the point: it is cheaper than discovering months later that one
// reader admits a status another refuses.
//
// External test package (bins_test) because internal/testdb imports store/bins;
// an in-package test would be an import cycle.

// offSpecStatus is deliberately not a domain constant. It stands for the
// incident-recovery values the schema permits, and pins the fail-closed
// behaviour: an unrecognised status must be sourceable in NEITHER language.
const offSpecStatus = domain.BinStatus("quarantine")

func TestSourceableStatus_GoSQLAgree(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	cases := []domain.BinStatus{
		domain.BinStatusAvailable,
		domain.BinStatusStaged,
		domain.BinStatusFlagged,
		domain.BinStatusMaintenance,
		domain.BinStatusQualityHold,
		domain.BinStatusRetired,
		offSpecStatus,
	}

	for _, status := range cases {
		status := status
		t.Run(string(status), func(t *testing.T) {
			b := &bins.Bin{
				BinTypeID: sd.BinType.ID,
				Label:     "SOURCEABLE-" + string(status),
				Status:    status,
				NodeID:    &sd.StorageNode.ID,
			}
			if err := db.CreateBin(b); err != nil {
				t.Fatalf("create bin with status %q: %v", status, err)
			}

			// The SQL side, evaluated exactly as a scoped reader composes it:
			// the fragment appended to a WHERE over the bins table aliased `b`.
			var sqlSourceable bool
			q := `SELECT EXISTS (SELECT 1 FROM bins b WHERE b.id = $1 AND ` +
				bins.SourceableStatusSQL + `)`
			if err := db.DB.QueryRow(q, b.ID).Scan(&sqlSourceable); err != nil {
				t.Fatalf("evaluate SourceableStatusSQL for %q: %v", status, err)
			}

			goSourceable := status.Sourceable()

			if goSourceable != sqlSourceable {
				t.Errorf("status %q: Go Sourceable()=%v but SourceableStatusSQL=%v — "+
					"the two spellings of one rule have drifted; update both",
					status, goSourceable, sqlSourceable)
			}

			// BlocksPickup is defined as the inverse and is what the pure
			// predicates actually call, so pin the relationship too.
			if status.BlocksPickup() == goSourceable {
				t.Errorf("status %q: BlocksPickup()=%v is not the inverse of Sourceable()=%v",
					status, status.BlocksPickup(), goSourceable)
			}
		})
	}
}

// TestSourceableStatus_OffSpecFailsClosed states the fail-closed default on its
// own, so the reason survives even if the table above is edited: a status nobody
// has classified must not become sourceable by accident.
func TestSourceableStatus_OffSpecFailsClosed(t *testing.T) {
	t.Parallel()
	if offSpecStatus.Sourceable() {
		t.Errorf("off-spec status %q is sourceable — the allow-list has become a "+
			"reject-list and an unrecognised status now admits by default", offSpecStatus)
	}
}
