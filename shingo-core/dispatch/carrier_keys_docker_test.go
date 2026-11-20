//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// keyFamily makes a bin type with a family of its own and returns the family id.
func keyFamily(t *testing.T, db *store.DB, code string) int64 {
	t.Helper()
	testutil.MustNoErr(t, db.CreateBinType(&bins.BinType{Code: code, Description: code}), "type "+code)
	bt, err := db.GetBinTypeByCode(code)
	bt = testutil.Must(t, bt, err, "read type "+code)
	return bt.FamilyID
}

// TestCarrierKeys_TheSetHoldsOnlyKeyedFamiliesAndFollowsTheSave: the set the
// builder reads holds only the families that name a key; the save writes and
// reloads it; a key written around the save is picked up by the floor's reload;
// a family whose keys are both blanked leaves the set.
func TestCarrierKeys_TheSetHoldsOnlyKeyedFamiliesAndFollowsTheSave(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	big := keyFamily(t, db, "CK-4548")
	small := keyFamily(t, db, "CK-3230")
	keyFamily(t, db, "CK-PLAIN")
	d, _ := newTestDispatcher(t, db, simulator.New())

	if changed, err := d.ReloadCarrierKeys(); err != nil || !changed || len(d.carrierKeys()) != 0 {
		t.Fatalf("an unkeyed plant: changed=%v err=%v set=%v, want a first load holding nothing", changed, err, d.carrierKeys())
	}
	testutil.MustNoErr(t, d.SaveCarrierFamilyKeys(big, " jackload_4548 ", "wait_4548"), "save 4548")
	want := map[int64]familyKeys{big: {lift: "jackload_4548", wait: "wait_4548"}}
	if got := d.carrierKeys(); len(got) != 1 || got[big] != want[big] {
		t.Errorf("after the save the set is %v, want %v (trimmed)", got, want)
	}

	testutil.MustNoErr(t, db.SetCarrierFamilyKeys(small, "", "wait_3230"), "a write around the save")
	if got := d.carrierKeys(); len(got) != 1 {
		t.Errorf("a write around the save reached the set before any reload: %v", got)
	}
	d.reloadCarrierKeysOnFloor()
	if got := d.carrierKeys(); got[small] != (familyKeys{wait: "wait_3230"}) || len(got) != 2 {
		t.Errorf("after the floor's reload the set is %v, want 3230's wait key beside 4548", got)
	}
	if changed, err := d.ReloadCarrierKeys(); err != nil || changed {
		t.Errorf("a reload over an unchanged table: changed=%v err=%v, want unchanged", changed, err)
	}

	testutil.MustNoErr(t, d.SaveCarrierFamilyKeys(big, "", ""), "blank 4548")
	if got := d.carrierKeys(); len(got) != 1 || got[small].wait != "wait_3230" {
		t.Errorf("a family with both keys blank stays in the set: %v", got)
	}
}
