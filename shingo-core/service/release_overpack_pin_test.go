//go:build docker

package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/audit"
)

// Decision 55, the record half: a cart the line drew below 0 is released at
// the count it reads. Core writes 0 (a count at or below 0 is an empty cart),
// and the release's ledger row says what it was: op released_overpack, before
// the bin's count, after 0, the declared count in its detail. The bins page's
// timeline reads "released at 0 (overpack N)". The fence is the release's as
// for any other count. A cart released empty while Core reads it below 0 is the
// same note: the op and the overpack in the row, nothing else differs. RED
// before: Core refused a negative count ("remainingUOP must be nil, 0, or
// positive"), the error the Edge sees as manifest_sync_failed, and recorded an
// empty release of a bin below 0 as released_empty or released_underpack.

// overpackRow is the release's bin_uop_ledger row: before, after, op and the
// declared count from its detail.
type overpackRow struct {
	before, after      int
	op                 string
	declared, overpack *int
}

func releaseRow(t *testing.T, db *store.DB, binID int64) overpackRow {
	t.Helper()
	var (
		r      overpackRow
		before *int
		detail []byte
	)
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT before_uop, after_uop, op, detail FROM bin_uop_ledger
		WHERE bin_id=$1 AND op LIKE 'released%' ORDER BY id DESC LIMIT 1`, binID).Scan(&before, &r.after, &r.op, &detail),
		"the release's ledger row")
	if before != nil {
		r.before = *before
	}
	if len(detail) > 0 {
		var d struct {
			Declared *int `json:"declared"`
			Overpack *int `json:"overpack"`
		}
		testutil.MustNoErr(t, json.Unmarshal(detail, &d), "the row's detail")
		r.declared, r.overpack = d.Declared, d.Overpack
	}
	return r
}

// timelineLine is the bins page's timeline entry for the release (the legacy
// audit row the page renders), "" when there is none.
func timelineLine(t *testing.T, db *store.DB, binID int64) string {
	t.Helper()
	entries, err := db.ListEntityAudit("bin", binID)
	testutil.MustNoErr(t, err, "the bin's audit")
	for _, e := range entries {
		if strings.HasPrefix(e.Action, "released") {
			return e.Action + ": " + e.NewValue
		}
	}
	return ""
}

func TestPinOverpack_ANegativeReleaseIsWrittenAsZeroAndRecorded(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	manifest := announcingService(db)

	bin := createTestBin(t, db, sd.LineNode.ID, "BIN-OVERPACK", "PART-A", 3)
	order := createTestOrder(t, db, sd.StorageNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)
	e := binEpoch(t, db, bin.ID)

	declared := -2
	if err := manifest.SyncOrClearForReleasedFenced(bin.ID, order.ID, &declared,
		protocol.DispositionReleasePartial, "operator",
		&ReleaseFence{Station: pinStation, ReleaseFence: protocol.ReleaseFence{BinID: bin.ID, Epoch: e, AsOfSeq: 0, AsOfNet: 0}}); err != nil {
		t.Fatalf("a release declaring -2: %v (want it written as 0 and recorded)", err)
	}
	if got := binRemaining(t, db, bin.ID); got != 0 {
		t.Errorf("uop_remaining = %d, want 0", got)
	}
	r := releaseRow(t, db, bin.ID)
	if r.op != audit.OpReleasedOverpack || r.before != 3 || r.after != 0 || r.declared == nil || *r.declared != -2 ||
		r.overpack == nil || *r.overpack != 2 {
		t.Errorf("the release's row = op %q before %d after %d declared %v overpack %v, want %q 3 -> 0 declared -2 overpack 2",
			r.op, r.before, r.after, r.declared, r.overpack, audit.OpReleasedOverpack)
	}
	if got := timelineLine(t, db, bin.ID); !strings.Contains(got, "released at 0 (overpack 2)") {
		t.Errorf("the bins page's timeline reads %q, want \"released at 0 (overpack 2)\"", got)
	}
	if got := binEpoch(t, db, bin.ID); got != e {
		t.Errorf("a fenced release moved delta_epoch %d -> %d: a negative count fences like any other", e, got)
	}

	// The no-owner fallback takes the same count the same way.
	fb := createTestBin(t, db, sd.LineNode.ID, "BIN-OVERPACK-FB", "PART-A", 1)
	fbOrder := createTestOrder(t, db, sd.StorageNode.ID)
	minus := -1
	if err := manifest.SyncOrClearForReleasedNoOwner(fb.ID, fbOrder.ID, &minus, "operator"); err != nil {
		t.Fatalf("the fallback's release declaring -1: %v", err)
	}
	if r := releaseRow(t, db, fb.ID); r.op != audit.OpReleasedOverpack || r.after != 0 || r.declared == nil || *r.declared != -1 {
		t.Errorf("the fallback's row = op %q after %d declared %v, want %q -> 0 declared -1", r.op, r.after, r.declared, audit.OpReleasedOverpack)
	}
}

// TestPinOverpack_AnEmptyReleaseOfABinBelowZeroIsTheSameNote: a cart Core
// reads below 0 released empty (RELEASE EMPTY, or the underpack button) is an
// overpack too, -before: the same op and timeline line, 0 written as before.
// A bin at 0 released empty keeps its op.
func TestPinOverpack_AnEmptyReleaseOfABinBelowZeroIsTheSameNote(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	manifest := announcingService(db)
	for _, tc := range []struct {
		label string
		kind  protocol.UOPDispositionKind
		count int
		want  string
	}{
		{"BIN-OVER-EMPTY", "", -3, audit.OpReleasedOverpack},
		{"BIN-OVER-UNDER", protocol.DispositionReleaseUnderpack, -1, audit.OpReleasedOverpack},
		{"BIN-AT-ZERO", "", 0, audit.OpReleasedEmpty},
	} {
		bin := createTestBin(t, db, sd.LineNode.ID, tc.label, "PART-A", 5)
		_, err := db.DB.Exec(`UPDATE bins SET uop_remaining=$1 WHERE id=$2`, tc.count, bin.ID)
		testutil.MustNoErr(t, err, "the bin's count")
		order := createTestOrder(t, db, sd.StorageNode.ID)
		claimBinForTest(t, db, bin.ID, order.ID)
		zero := 0
		testutil.MustNoErr(t, manifest.SyncOrClearForReleasedFenced(bin.ID, order.ID, &zero, tc.kind, "operator",
			&ReleaseFence{Station: pinStation, ReleaseFence: protocol.ReleaseFence{BinID: bin.ID, Epoch: binEpoch(t, db, bin.ID)}}),
			tc.label+": the empty release")
		if got := binRemaining(t, db, bin.ID); got != 0 {
			t.Errorf("%s: uop_remaining = %d, want 0", tc.label, got)
		}
		r := releaseRow(t, db, bin.ID)
		if r.op != tc.want || r.before != tc.count || r.after != 0 {
			t.Errorf("%s: the row = op %q %d -> %d, want %q %d -> 0", tc.label, r.op, r.before, r.after, tc.want, tc.count)
		}
		if tc.want != audit.OpReleasedOverpack {
			continue
		}
		if r.declared == nil || *r.declared != 0 || r.overpack == nil || *r.overpack != -tc.count {
			t.Errorf("%s: detail declared %v overpack %v, want 0 and %d", tc.label, r.declared, r.overpack, -tc.count)
		}
		want := fmt.Sprintf("released at 0 (overpack %d)", -tc.count)
		if got := timelineLine(t, db, bin.ID); !strings.Contains(got, want) {
			t.Errorf("%s: the timeline reads %q, want %q", tc.label, got, want)
		}
	}
}
