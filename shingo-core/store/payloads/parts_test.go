//go:build docker

package payloads_test

import (
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/payloads"
)

// A part carries two names for one physical thing, and the only interesting
// question the write path has is what to do when a second answer arrives for
// the controls one.

// TestUpsertPart_FillsAnUnknownCATID: a part recorded without a cat id gets one
// the first time somebody knows it. Nobody is contradicted, so nothing is asked.
func TestUpsertPart_FillsAnUnknownCATID(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t).DB

	first, err := payloads.UpsertPart(db, "51015-LH", "", "left bracket")
	testutil.MustNoErr(t, err, "create the part with no cat id")
	if first.CATID != "" {
		t.Fatalf("a part created without a cat id came back with %q", first.CATID)
	}

	second, err := payloads.UpsertPart(db, "51015-LH", "40016911", "")
	testutil.MustNoErr(t, err, "supply the cat id")
	if second.CATID != "40016911" {
		t.Errorf("cat id = %q, want 40016911 — an empty one is an absent answer, not a "+
			"statement that the part has none", second.CATID)
	}
	if second.Description != "left bracket" {
		t.Errorf("description = %q, want the one already recorded — a blank field in a "+
			"later call is a box nobody touched", second.Description)
	}
}

// TestUpsertPart_RefusesADifferentCATID is the standing drift-catcher.
//
// Overwriting silently gives one part two controls identities over time, and a
// style's derived set then accepts a cat id that belongs to something else.
// Keeping silently swallows a real correction. Neither is answerable here, so
// the write refuses and the caller asks a person.
func TestUpsertPart_RefusesADifferentCATID(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t).DB

	_, err := payloads.UpsertPart(db, "51015-RH", "40016911", "")
	testutil.MustNoErr(t, err, "create the part")

	_, err = payloads.UpsertPart(db, "51015-RH", "99999999", "")
	var conflict *payloads.ErrCATIDConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("a second, different cat id was accepted (err = %v). A part with two "+
			"controls identities is how a cell's wrong-part guard starts accepting the "+
			"wrong one, and neither value can be assumed right.", err)
	}
	if conflict.Existing != "40016911" || conflict.Incoming != "99999999" {
		t.Errorf("the conflict does not carry both values: %+v", conflict)
	}

	// And nothing changed.
	got, err := payloads.GetPartByNumber(db, "51015-RH")
	testutil.MustNoErr(t, err, "re-read the part")
	if got.CATID != "40016911" {
		t.Errorf("cat id = %q after a refused write, want the original 40016911", got.CATID)
	}
}

// TestSetPartCATID_IsTheAnswerToTheConflict: the override exists, deliberately,
// as its own call. An ordinary manifest save cannot reach it, so a part's
// controls identity never changes as a side effect of editing a payload.
func TestSetPartCATID_IsTheAnswerToTheConflict(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t).DB

	_, err := payloads.UpsertPart(db, "51015-CTR", "40016911", "")
	testutil.MustNoErr(t, err, "create the part")
	testutil.MustNoErr(t, payloads.SetPartCATID(db, "51015-CTR", "40017111"), "answer the conflict")

	got, err := payloads.GetPartByNumber(db, "51015-CTR")
	testutil.MustNoErr(t, err, "re-read the part")
	if got.CATID != "40017111" {
		t.Errorf("cat id = %q, want the value the operator confirmed", got.CATID)
	}
	if err := payloads.SetPartCATID(db, "NO-SUCH-PART", "1"); err == nil {
		t.Error("setting a cat id on a part that does not exist reported success")
	}
}
