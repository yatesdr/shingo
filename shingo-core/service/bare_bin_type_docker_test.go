//go:build docker

package service

import (
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/payloads"
)

// bare_bin_type_docker_test.go — the save-time refusals that keep a bare bin
// type a label and nothing else: an unloader's bare type is flagged bare, only
// an unloader has one, and no payload rule ever names a bare type — checked
// from both sides, since either edit can create the illegal state.

type bareFixture struct {
	bin     *BinService
	payload *PayloadService
	loader  *LoaderService
	bare    *bins.BinType
	plain   *bins.BinType
	pl      *payloads.Payload
}

func newBareFixture(t *testing.T) bareFixture {
	t.Helper()
	db := testDB(t)
	f := bareFixture{
		bin:     NewBinService(db, nil),
		payload: NewPayloadService(db),
		loader:  NewLoaderService(db, nil),
		bare:    &bins.BinType{Code: "BARE-HALF", Bare: true},
		plain:   &bins.BinType{Code: "BARE-STD"},
		pl:      &payloads.Payload{Code: "BARE-PART", UOPCapacity: 10},
	}
	testutil.MustNoErr(t, db.CreateBinType(f.bare), "create bare type")
	testutil.MustNoErr(t, db.CreateBinType(f.plain), "create plain type")
	testutil.MustNoErr(t, db.CreatePayload(f.pl), "create payload")
	return f
}

func TestBareBinType_CreatedAndReadBack(t *testing.T) {
	t.Parallel()
	f := newBareFixture(t)
	got, err := f.bin.GetBinType(f.bare.ID)
	testutil.MustNoErr(t, err, "read bare")
	if !got.Bare {
		t.Error("a type created bare reads back not bare")
	}
	got, err = f.bin.GetBinType(f.plain.ID)
	testutil.MustNoErr(t, err, "read plain")
	if got.Bare {
		t.Error("a type created without the flag reads back bare; the default must be false")
	}
}

func TestLoaderService_BareType(t *testing.T) {
	t.Parallel()
	f := newBareFixture(t)
	consumeID, err := f.loader.Create("BARE-UNLOADER", loaders.RoleConsume, loaders.LayoutSharedWindow, "", "", "", false)
	testutil.MustNoErr(t, err, "create unloader")
	produceID, err := f.loader.Create("BARE-LOADER", loaders.RoleProduce, loaders.LayoutSharedWindow, "", "", "", false)
	testutil.MustNoErr(t, err, "create loader")
	update := func(id int64, name string, bare int64) error {
		return f.loader.Update(LoaderUpdate{ID: id, Name: name, Layout: loaders.LayoutSharedWindow, BareBinTypeID: bare})
	}

	// Stored, and kept across re-saves (the page re-sends it on every edit).
	for i := 1; i <= 2; i++ {
		testutil.MustNoErr(t, update(consumeID, "BARE-UNLOADER", f.bare.ID), "save unloader bare type")
		got, err := f.loader.Get(consumeID)
		testutil.MustNoErr(t, err, "re-read")
		if got.BareBinTypeID == nil || *got.BareBinTypeID != f.bare.ID {
			t.Fatalf("save %d: bare_bin_type_id = %v, want %d", i, got.BareBinTypeID, f.bare.ID)
		}
	}

	if err := update(consumeID, "BARE-UNLOADER", f.plain.ID); !errors.Is(err, ErrBareTypeNotBare) {
		t.Errorf("unloader naming a type that is not bare: err = %v, want ErrBareTypeNotBare", err)
	}
	if err := update(produceID, "BARE-LOADER", f.bare.ID); !errors.Is(err, ErrBareTypeProduce) {
		t.Errorf("produce loader with a bare type: err = %v, want ErrBareTypeProduce", err)
	}
	got, err := f.loader.Get(produceID)
	testutil.MustNoErr(t, err, "re-read loader")
	if got.BareBinTypeID != nil {
		t.Errorf("refused update landed: %+v", got)
	}

	// Zero clears it.
	testutil.MustNoErr(t, update(consumeID, "BARE-UNLOADER", 0), "clear")
	got, err = f.loader.Get(consumeID)
	testutil.MustNoErr(t, err, "re-read")
	if got.BareBinTypeID != nil {
		t.Errorf("bare_bin_type_id = %v after a save with none, want NULL", *got.BareBinTypeID)
	}
}

// TestBareBinType_NeverInAPayloadRule checks both directions.
func TestBareBinType_NeverInAPayloadRule(t *testing.T) {
	t.Parallel()
	f := newBareFixture(t)

	// Listing a bare type in a rule is refused, and nothing is written.
	err := f.payload.SetBinTypes(f.pl.ID, []int64{f.plain.ID, f.bare.ID})
	if !errors.Is(err, ErrBareTypeInPayloadRule) {
		t.Fatalf("rule naming a bare type: err = %v, want ErrBareTypeInPayloadRule", err)
	}
	rules, err := f.payload.ListBinTypes(f.pl.ID)
	testutil.MustNoErr(t, err, "list rules")
	if len(rules) != 0 {
		t.Errorf("refused rule save wrote %d rows", len(rules))
	}

	// The legal rule lands; then flagging its type bare is refused.
	testutil.MustNoErr(t, f.payload.SetBinTypes(f.pl.ID, []int64{f.plain.ID}), "legal rule")
	flag := *f.plain
	flag.Bare = true
	if err := f.bin.UpdateBinType(&flag); !errors.Is(err, ErrBareTypeInPayloadRule) {
		t.Errorf("flagging bare a type a rule lists: err = %v, want ErrBareTypeInPayloadRule", err)
	}
	got, err := f.bin.GetBinType(f.plain.ID)
	testutil.MustNoErr(t, err, "re-read")
	if got.Bare {
		t.Error("the refused flag landed")
	}

	// Once no rule lists it, it may be flagged.
	testutil.MustNoErr(t, f.payload.SetBinTypes(f.pl.ID, nil), "drop rule")
	testutil.MustNoErr(t, f.bin.UpdateBinType(&flag), "flag an unlisted type")
}

// TestBareBinType_UnflagRefusedWhileAnUnloaderNamesIt: un-flagging would leave
// a loader whose bare type is not bare.
func TestBareBinType_UnflagRefusedWhileAnUnloaderNamesIt(t *testing.T) {
	t.Parallel()
	f := newBareFixture(t)
	id, err := f.loader.Create("BARE-UNF", loaders.RoleConsume, loaders.LayoutSharedWindow, "", "", "", false)
	testutil.MustNoErr(t, err, "create unloader")
	testutil.MustNoErr(t, f.loader.Update(LoaderUpdate{ID: id, Name: "BARE-UNF",
		Layout: loaders.LayoutSharedWindow, BareBinTypeID: f.bare.ID}), "set bare type")

	unflag := *f.bare
	unflag.Bare = false
	if err := f.bin.UpdateBinType(&unflag); !errors.Is(err, ErrBareTypeInUse) {
		t.Fatalf("un-flagging a live unloader's bare type: err = %v, want ErrBareTypeInUse", err)
	}
	// Other edits to the still-bare type go through.
	rename := *f.bare
	rename.Description = "renamed"
	testutil.MustNoErr(t, f.bin.UpdateBinType(&rename), "edit a bare type in use")

	// An archived loader owns nothing, so it does not hold the flag.
	testutil.MustNoErr(t, f.loader.Delete(id), "archive")
	testutil.MustNoErr(t, f.bin.UpdateBinType(&unflag), "un-flag once only an archived loader names it")
}
