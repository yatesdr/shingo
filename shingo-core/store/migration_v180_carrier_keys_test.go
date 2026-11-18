//go:build docker

package store_test

import (
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// familyNamed is the family of this code as ListCarrierFamilies reads it.
func familyNamed(t *testing.T, db *store.DB, code string) (bins.CarrierFamily, bool) {
	t.Helper()
	families, err := db.ListCarrierFamilies()
	testutil.MustNoErr(t, err, "list families")
	for _, f := range families {
		if f.Code == code {
			return f, true
		}
	}
	return bins.CarrierFamily{}, false
}

// TestCarrierKeysMigration_V180_EveryFamilyStartsBlank: v180 adds
// carrier_families.lift_bintask and wait_bintask over families that exist, and
// every one reads blank (it sends the default). Re-applied through the migrate
// path; a second open changes nothing.
func TestCarrierKeysMigration_V180_EveryFamilyStartsBlank(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	bt := &bins.BinType{Code: "V180-4548", Description: "v180"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "a type, and its family of one")
	for _, stmt := range []string{
		`ALTER TABLE carrier_families DROP COLUMN lift_bintask`,
		`ALTER TABLE carrier_families DROP COLUMN wait_bintask`,
		`DELETE FROM schema_migrations WHERE version = 180`,
	} {
		_, err := db.Exec(stmt)
		testutil.MustNoErr(t, err, "back to the pre-v180 shape: "+stmt)
	}
	for pass := 1; pass <= 2; pass++ {
		migrated, err := store.Open(cfg)
		testutil.MustNoErr(t, err, "re-open to apply v180")
		f, ok := familyNamed(t, migrated, "V180-4548")
		if !ok || f.LiftBinTask != "" || f.WaitBinTask != "" {
			t.Errorf("pass %d: the family from before v180 reads %+v (found %v), want both keys blank", pass, f, ok)
		}
		if failing := migrated.MigrationsFailingTheirPostCondition(); len(failing) != 0 {
			t.Errorf("pass %d: migrations failing their post-condition: %v", pass, failing)
		}
		testutil.MustNoErr(t, migrated.Close(), "close")
	}
}

// TestCarrierFamilyKeys_SetReadsBackOnEveryTypeOfTheFamily: the one setter
// writes both keys of the family, ListCarrierFamilies reads them back, and a
// family that is gone says so.
func TestCarrierFamilyKeys_SetReadsBackOnEveryTypeOfTheFamily(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testutil.MustNoErr(t, db.CreateBinType(&bins.BinType{Code: "KEYS-A", Description: "a"}), "type A")
	fam, ok := familyNamed(t, db, "KEYS-A")
	if !ok {
		t.Fatal("type A has no family")
	}
	testutil.MustNoErr(t, db.CreateBinType(&bins.BinType{Code: "KEYS-B", Description: "b", FamilyID: fam.ID}), "type B, of A's family")
	testutil.MustNoErr(t, db.SetCarrierFamilyKeys(fam.ID, "jackload_4548", "wait_4548"), "set the keys")
	got, _ := familyNamed(t, db, "KEYS-A")
	if got.LiftBinTask != "jackload_4548" || got.WaitBinTask != "wait_4548" || len(got.Types) != 2 {
		t.Errorf("the family reads %+v, want jackload_4548 / wait_4548 on its two types", got)
	}
	testutil.MustNoErr(t, db.SetCarrierFamilyKeys(fam.ID, "", "wait_4548"), "blank the lift key")
	if got, _ := familyNamed(t, db, "KEYS-A"); got.LiftBinTask != "" || got.WaitBinTask != "wait_4548" {
		t.Errorf("after blanking the lift key the family reads %+v", got)
	}
	if err := db.SetCarrierFamilyKeys(fam.ID+1000, "x", "y"); !errors.Is(err, bins.ErrNoSuchFamily) {
		t.Errorf("setting a family that does not exist: %v, want ErrNoSuchFamily", err)
	}
}

// TestCarrierFamilyKeys_GoWithTheFamily: a family is deleted with its last
// type, by a delete and by a move out (bins.DeleteType, bins.SetTypeFamily), and
// its keys go with it: no keyed family is kept hidden. A type made later under
// the same code has a family of one with blank keys.
func TestCarrierFamilyKeys_GoWithTheFamily(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	keyed := func(code string) bins.CarrierFamily {
		t.Helper()
		bt := &bins.BinType{Code: code, Description: code}
		testutil.MustNoErr(t, db.CreateBinType(bt), "type "+code)
		f, ok := familyNamed(t, db, code)
		if !ok {
			t.Fatalf("type %s has no family", code)
		}
		testutil.MustNoErr(t, db.SetCarrierFamilyKeys(f.ID, "jackload_"+code, "wait_"+code), "key "+code)
		return f
	}

	gone := keyed("GONE")
	bt, err := db.GetBinTypeByCode("GONE")
	bt = testutil.Must(t, bt, err, "read GONE")
	testutil.MustNoErr(t, db.DeleteBinType(bt.ID), "delete the family's last type")
	if _, ok := familyNamed(t, db, "GONE"); ok {
		t.Error("the family outlived its last type's delete")
	}
	remade := &bins.BinType{Code: "GONE", Description: "again"}
	testutil.MustNoErr(t, db.CreateBinType(remade), "a type under the same code")
	if f, ok := familyNamed(t, db, "GONE"); !ok || f.ID == gone.ID || f.LiftBinTask != "" || f.WaitBinTask != "" {
		t.Errorf("the type made again has family %+v (found %v), want a new family with blank keys", f, ok)
	}

	moved := keyed("MOVED")
	other := keyed("OTHER")
	mt, err := db.GetBinTypeByCode("MOVED")
	mt = testutil.Must(t, mt, err, "read MOVED")
	testutil.MustNoErr(t, db.SetBinTypeFamily(mt.ID, other.ID), "move the family's last type out")
	if _, ok := familyNamed(t, db, "MOVED"); ok {
		t.Errorf("family MOVED (%d) outlived its last type's move", moved.ID)
	}
	if f, _ := familyNamed(t, db, "OTHER"); f.LiftBinTask != "jackload_OTHER" || len(f.Types) != 2 {
		t.Errorf("the family moved into reads %+v, want its own keys over both types", f)
	}
}
