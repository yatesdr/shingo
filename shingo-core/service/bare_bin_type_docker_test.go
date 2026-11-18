//go:build docker

package service

import (
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/payloads"
)

// bare_bin_type_docker_test.go — a bare marker is derived from its carrier and
// is a label and nothing else: bare cannot be set by hand, and no payload rule
// ever names a marker.

type bareFixture struct {
	db      *store.DB
	bin     *BinService
	payload *PayloadService
	bare    *bins.BinType
	plain   *bins.BinType
	pl      *payloads.Payload
}

func newBareFixture(t *testing.T) bareFixture {
	t.Helper()
	db := testDB(t)
	f := bareFixture{
		db:      db,
		bin:     NewBinService(db, nil),
		payload: NewPayloadService(db),
		plain:   &bins.BinType{Code: "BARE-STD"},
		pl:      &payloads.Payload{Code: "BARE-PART", UOPCapacity: 10},
	}
	testutil.MustNoErr(t, db.CreateBinType(f.plain), "create plain type")
	markerID, err := db.EnsureBareMarker(f.plain.ID)
	testutil.MustNoErr(t, err, "derive marker")
	f.bare, err = db.GetBinType(markerID)
	testutil.MustNoErr(t, err, "read marker")
	testutil.MustNoErr(t, db.CreatePayload(f.pl), "create payload")
	return f
}

// TestBareBinType_DerivedFromItsCarrier: the marker is the carrier's code with
// the suffix, bare, and names the carrier; asking again reuses it, and asking
// for a marker's marker is the marker itself (a stage-1 CLEAR tapped twice).
func TestBareBinType_DerivedFromItsCarrier(t *testing.T) {
	t.Parallel()
	f := newBareFixture(t)
	if !f.bare.Bare || f.bare.BareOf == nil || *f.bare.BareOf != f.plain.ID || f.bare.Code != "BARE-STD"+BareMarkerSuffix {
		t.Fatalf("marker = %s bare %v of %v, want BARE-STD%s, bare, naming carrier %d",
			f.bare.Code, f.bare.Bare, f.bare.BareOf, BareMarkerSuffix, f.plain.ID)
	}
	got, err := f.bin.GetBinType(f.plain.ID)
	testutil.MustNoErr(t, err, "read plain")
	if got.Bare || got.BareOf != nil {
		t.Error("the carrier reads back bare")
	}
	db := f.bin.db
	for _, from := range []int64{f.plain.ID, f.bare.ID} {
		id, err := db.EnsureBareMarker(from)
		testutil.MustNoErr(t, err, "re-derive")
		if id != f.bare.ID {
			t.Errorf("marker of type %d = %d, want the one marker %d", from, id, f.bare.ID)
		}
	}
}

// TestBareBinType_CannotBeSetByHand: bare is generated from bare_of, so the
// admin doors write nothing for it — a type saved with the flag reads back
// plain, and a marker re-saved without it stays bare.
func TestBareBinType_CannotBeSetByHand(t *testing.T) {
	t.Parallel()
	f := newBareFixture(t)
	flag := *f.plain
	flag.Bare = true
	testutil.MustNoErr(t, f.bin.UpdateBinType(&flag), "save with the flag")
	if got, err := f.bin.GetBinType(f.plain.ID); err != nil || got.Bare {
		t.Errorf("a hand-set bare flag landed (err %v)", err)
	}
	unflag := *f.bare
	unflag.Bare = false
	unflag.Description = "renamed"
	testutil.MustNoErr(t, f.bin.UpdateBinType(&unflag), "re-save the marker")
	if got, err := f.bin.GetBinType(f.bare.ID); err != nil || !got.Bare || got.Description != "renamed" {
		t.Errorf("marker after a re-save = %+v (err %v), want it still bare with the edit", got, err)
	}
}

// TestBareBinType_NeverInAPayloadRule: listing a marker in a rule is refused,
// and nothing is written.
func TestBareBinType_NeverInAPayloadRule(t *testing.T) {
	t.Parallel()
	f := newBareFixture(t)
	err := f.payload.SetBinTypes(f.pl.ID, []int64{f.plain.ID, f.bare.ID})
	if !errors.Is(err, ErrBareTypeInPayloadRule) {
		t.Fatalf("rule naming a bare type: err = %v, want ErrBareTypeInPayloadRule", err)
	}
	rules, err := f.payload.ListBinTypes(f.pl.ID)
	testutil.MustNoErr(t, err, "list rules")
	if len(rules) != 0 {
		t.Errorf("refused rule save wrote %d rows", len(rules))
	}
	testutil.MustNoErr(t, f.payload.SetBinTypes(f.pl.ID, []int64{f.plain.ID}), "legal rule")
}

// TestBareBinType_TakesItsCarriersRobotGroup: the marker has no edit of its own
// on the Bins page, so the carrier's save carries its robot group to it; the
// two are one cart, with a bin on it or without.
func TestBareBinType_TakesItsCarriersRobotGroup(t *testing.T) {
	t.Parallel()
	f := newBareFixture(t)
	for _, group := range []string{"JACK-2", ""} {
		carrier, err := f.bin.GetBinType(f.plain.ID)
		carrier = testutil.Must(t, carrier, err, "read the carrier")
		carrier.RequiredRobotGroup = group
		testutil.MustNoErr(t, f.bin.UpdateBinType(carrier), "save the carrier's robot group")
		marker, err := f.bin.GetBinType(f.bare.ID)
		marker = testutil.Must(t, marker, err, "read the marker")
		if marker.RequiredRobotGroup != group {
			t.Errorf("after the carrier's save with group %q the marker's group is %q", group, marker.RequiredRobotGroup)
		}
	}
}

// TestBareBinType_GoesWithItsCarrier: the carrier's delete takes its marker
// along, and a bin on the marker holds the whole delete back.
func TestBareBinType_GoesWithItsCarrier(t *testing.T) {
	t.Parallel()
	t.Run("a bin on the marker holds it back", func(t *testing.T) {
		t.Parallel()
		f := newBareFixture(t)
		testutil.MustNoErr(t, f.db.CreateBin(&bins.Bin{BinTypeID: f.bare.ID, Label: "BARE-CART-1", Status: "available"}), "a bare cart")
		if err := f.bin.DeleteBinType(f.plain.ID); err == nil {
			t.Fatal("the carrier was deleted with a bare cart of its marker in the plant")
		}
		for _, id := range []int64{f.plain.ID, f.bare.ID} {
			if _, err := f.bin.GetBinType(id); err != nil {
				t.Errorf("type %d is gone after a refused delete: %v", id, err)
			}
		}
	})
	t.Run("no bins: both go", func(t *testing.T) {
		t.Parallel()
		f := newBareFixture(t)
		testutil.MustNoErr(t, f.bin.DeleteBinType(f.plain.ID), "delete the carrier")
		for _, id := range []int64{f.plain.ID, f.bare.ID} {
			if _, err := f.bin.GetBinType(id); err == nil {
				t.Errorf("type %d outlived its carrier's delete", id)
			}
		}
	})
}
