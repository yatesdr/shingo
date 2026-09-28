//go:build docker

package bins_test

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/internal/helpers"
	"shingocore/store/nodes"
)

// staging_status_docker_test.go — the staging writers re-derive status only
// from {available, staged}.
//
// available and staged are the two statuses the staging state machine owns: a
// bin landing at a lineside slot is staged, a bin landing in storage is
// available, a release turns staged back into available. Every other status —
// flagged, maintenance, retired, or a value outside the enum — was set by a
// person or a door on purpose, and a placement or a release must not flatten
// it. Before this, PlaceBinTx and ReleaseStaged wrote their status
// unconditionally: a flagged bin a robot put down came back available.
//
// Rows cover every status in the enum plus one off-spec value (the column has
// no CHECK constraint, so one is representable).

// stagingStatuses is the prior-status axis: the enum, plus an off-spec value.
func stagingStatuses() []domain.BinStatus {
	return append(domain.AllBinStatuses(), domain.BinStatus("off_spec_status"))
}

// ownsStaging reports whether the staging writers may re-derive this status.
func ownsStaging(s domain.BinStatus) bool {
	return s == domain.BinStatusAvailable || s == domain.BinStatusStaged
}

// binInStatus creates a bin at the storage node and puts it in prior. A staged
// prior goes through bins.Stage so staged_at/staged_expires_at are real.
func binInStatus(t *testing.T, db *store.DB, sd *testdb.StandardData, label string, prior domain.BinStatus) int64 {
	t.Helper()
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, label)
	switch prior {
	case domain.BinStatusAvailable:
	case domain.BinStatusStaged:
		exp := time.Now().Add(time.Hour)
		testutil.MustNoErr(t, bins.Stage(db.DB, bin.ID, &exp), "stage prior")
	default:
		testutil.MustNoErr(t, bins.UpdateStatus(db.DB, bin.ID, prior), "set prior status")
	}
	return bin.ID
}

func freshNode(t *testing.T, db *store.DB, name string) *nodes.Node {
	t.Helper()
	n := &nodes.Node{Name: name, Enabled: true, Zone: "A"}
	testutil.MustNoErr(t, nodes.Create(db.DB, n), "create node "+name)
	return n
}

// TestPlaceBinTx_ArrivalStatusOnlyFromAvailableOrStaged is the prior-status ×
// {staged, unstaged destination} table through the placement primitive.
//
// The placement itself (node_id) always lands; only the status derivation is
// guarded.
func TestPlaceBinTx_ArrivalStatusOnlyFromAvailableOrStaged(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	for _, prior := range stagingStatuses() {
		for _, staged := range []bool{false, true} {
			name := fmt.Sprintf("%s_staged=%v", prior, staged)
			t.Run(name, func(t *testing.T) {
				binID := binInStatus(t, db, sd, "BIN-ARR-"+name, prior)
				dest := freshNode(t, db, "ARR-"+name)
				exp := time.Now().Add(2 * time.Hour)

				tx, err := db.DB.Begin()
				testutil.MustNoErr(t, err, "begin")
				_, err = helpers.PlaceBinTx(tx, helpers.BinPlacement{
					BinID: binID, ToNodeID: dest.ID, Staged: staged, ExpiresAt: &exp,
				})
				if err != nil {
					_ = tx.Rollback()
					t.Fatalf("PlaceBinTx: %v", err)
				}
				testutil.MustNoErr(t, tx.Commit(), "commit")

				got, err := bins.Get(db.DB, binID)
				testutil.MustNoErr(t, err, "get")
				if got.NodeID == nil || *got.NodeID != dest.ID {
					t.Errorf("node_id = %v, want %d: the placement itself must always land", got.NodeID, dest.ID)
				}
				want := prior
				if ownsStaging(prior) {
					want = domain.BinStatusAvailable
					if staged {
						want = domain.BinStatusStaged
					}
				}
				if got.Status != want {
					t.Errorf("status after arrival = %q, want %q (prior %q): arrival re-derives "+
						"status only from available/staged", got.Status, want, prior)
				}
				if want == domain.BinStatusStaged && (got.StagedExpiresAt == nil || got.StagedExpiresAt.Sub(exp).Abs() > time.Millisecond) {
					t.Errorf("staged_expires_at = %v, want %v", got.StagedExpiresAt, exp)
				}
				if want != domain.BinStatusStaged && got.StagedAt != nil {
					t.Errorf("staged_at = %v on a %q bin, want nil", got.StagedAt, got.Status)
				}
			})
		}
	}
}

// TestMoveAndClearStaging_StatusOnlyFromAvailableOrStaged is the by-hand move's
// half of the same fragment: clearStaging drops a staged status and touches
// nothing else.
func TestMoveAndClearStaging_StatusOnlyFromAvailableOrStaged(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	for _, prior := range stagingStatuses() {
		for _, clear := range []bool{false, true} {
			name := fmt.Sprintf("%s_clear=%v", prior, clear)
			t.Run(name, func(t *testing.T) {
				binID := binInStatus(t, db, sd, "BIN-MV-"+name, prior)
				dest := freshNode(t, db, "MV-"+name)
				testutil.MustNoErr(t, bins.MoveAndClearStaging(db.DB, binID, dest.ID, clear), "move")

				got, err := bins.Get(db.DB, binID)
				testutil.MustNoErr(t, err, "get")
				want := prior
				if clear && prior == domain.BinStatusStaged {
					want = domain.BinStatusAvailable
				}
				if got.Status != want {
					t.Errorf("status after move = %q, want %q (prior %q, clearStaging=%v)", got.Status, want, prior, clear)
				}
			})
		}
	}
}

// TestReleaseStaged_OnlyReleasesStaged: a release turns staged into available
// and leaves every other status alone.
func TestReleaseStaged_OnlyReleasesStaged(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	for _, prior := range stagingStatuses() {
		t.Run(string(prior), func(t *testing.T) {
			binID := binInStatus(t, db, sd, "BIN-REL-"+string(prior), prior)
			released, err := bins.ReleaseStaged(db.DB, binID)
			testutil.MustNoErr(t, err, "ReleaseStaged")

			got, gerr := bins.Get(db.DB, binID)
			testutil.MustNoErr(t, gerr, "get")
			wasStaged := prior == domain.BinStatusStaged
			if released != wasStaged {
				t.Errorf("released = %v for prior %q, want %v: the return value is the answer to "+
					"\"was it staged\"", released, prior, wasStaged)
			}
			want := prior
			if wasStaged {
				want = domain.BinStatusAvailable
			}
			if got.Status != want {
				t.Errorf("status after release = %q, want %q (prior %q): only a staged bin is released",
					got.Status, want, prior)
			}
		})
	}
}
