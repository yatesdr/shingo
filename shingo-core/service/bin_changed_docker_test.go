//go:build docker

package service

import (
	"reflect"
	"testing"

	"shingocore/internal/testdb"
)

// The station LOAD and count call the bin hook once each, with the bin, after
// the write commits; a refused LOAD calls nothing. The CLEAR's hook is the
// engine's (TestClearForReuseStampAndBookDeparture_CallsTheBinHook).
func TestStationDoors_CallTheBinHook(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	manifest := NewBinManifestService(db, EpochAnnounce{})
	var got []int64
	manifest.SetBinChangedFunc(func(binID int64) { got = append(got, binID) })
	svc := NewBinService(db, manifest)
	bin := countableBin(t, db, sd)

	if _, _, err := manifest.LoadAtLine(bin.ID, `{"items":[]}`, "NO-SUCH-PART", nil); err == nil {
		t.Fatal("fixture: a LOAD of an unknown payload was not refused")
	}
	if len(got) != 0 {
		t.Errorf("a refused LOAD called the hook with %v", got)
	}
	if _, _, err := manifest.LoadAtLine(bin.ID, `{"items":[]}`, sd.Payload.Code, nil); err != nil {
		t.Fatalf("LoadAtLine: %v", err)
	}
	loaded, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("read bin: %v", err)
	}
	if _, err := svc.RecordCount(loaded, 4, "operator-under-test"); err != nil {
		t.Fatalf("RecordCount: %v", err)
	}
	if want := []int64{bin.ID, bin.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("hook calls = %v, want %v (the LOAD, then the count)", got, want)
	}
}
