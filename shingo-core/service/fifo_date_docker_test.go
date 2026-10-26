//go:build docker

package service

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// The confirm reads what the service's own history rows record: a correction
// keeps the carrier's FIFO date, through the same doors a person uses.
func TestConfirm_ACorrectionKeepsTheCarriersFIFODate(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db, EpochAnnounce{})
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-FIFO-DATE", "", 0)
	manifest := `{"items":[{"catid":"PART","qty":80}]}`
	part := sd.Payload.Code
	loaded := time.Date(2026, 10, 19, 19, 1, 1, 0, time.UTC)

	loadedAt := func() time.Time {
		t.Helper()
		b, err := db.GetBin(bin.ID)
		testutil.MustNoErr(t, err, "reload bin")
		if b.LoadedAt == nil {
			t.Fatal("bin has no loaded_at")
		}
		return b.LoadedAt.UTC()
	}
	setAndConfirm := func(uop int, producedAt string) {
		t.Helper()
		_, err := svc.SetForProduction(bin.ID, manifest, part, uop, protocol.DeclaredByPerson)
		testutil.MustNoErr(t, err, "set for production")
		testutil.MustNoErr(t, svc.Confirm(bin.ID, producedAt), "confirm")
	}
	clear := func() {
		t.Helper()
		_, err := svc.ClearForReuse(bin.ID, nil, protocol.DeclaredByPerson)
		testutil.MustNoErr(t, err, "clear")
	}

	// The load, with its production time: the carrier's date.
	setAndConfirm(80, loaded.Format(time.RFC3339))
	if got := loadedAt(); !got.Equal(loaded) {
		t.Fatalf("the load stamped %v, want %v", got, loaded)
	}

	// Case 2: cleared at 80 and the same part set again at 70.
	clear()
	setAndConfirm(70, "")
	if got := loadedAt(); !got.Equal(loaded) {
		t.Errorf("after a clear and the same part set again: loaded_at %v, want the carrier's %v", got, loaded)
	}
	var kept string
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COALESCE(detail->>'kept', '') FROM bin_uop_ledger
		WHERE bin_id=$1 AND op='manifest_confirmed' ORDER BY id DESC LIMIT 1`, bin.ID).Scan(&kept), "read the confirm's detail")
	if kept == "" {
		t.Error("the confirm that kept the date does not say so in its history row")
	}

	// Case 1: the same part set again without a clear (a top-up to 90).
	setAndConfirm(90, "")
	if got := loadedAt(); !got.Equal(loaded) {
		t.Errorf("after a top-up with the same part: loaded_at %v, want the carrier's %v", got, loaded)
	}

	// Past the window: a clear 21 minutes before the re-add is new stock.
	clear()
	_, err := db.DB.Exec(`UPDATE bin_uop_ledger SET applied_at = applied_at - interval '21 minutes'
		WHERE id = (SELECT MAX(id) FROM bin_uop_ledger WHERE bin_id=$1 AND op='clear_for_reuse')`, bin.ID)
	testutil.MustNoErr(t, err, "backdate the clear")
	setAndConfirm(60, "")
	if got := loadedAt(); got.Equal(loaded) || time.Since(got) > time.Hour {
		t.Errorf("a re-add 21 minutes after the clear kept %v, want a new date", got)
	}
}
