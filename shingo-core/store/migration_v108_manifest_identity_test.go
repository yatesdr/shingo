//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/payloads"
)

// v107/v108 move a manifest line's identity onto a part: the line names the
// PART NUMBER CMS books against, and the cat id a PLC declares moves to the
// part's own row. Nothing is invented — every value the plants hold today is a
// cat id, so the correction MOVES them.
//
// The tests below pin the two properties the correction is worthless without:
// no cell's wrong-part guard changes what it accepts, and a line that could not
// be corrected without inventing something is left visibly uncorrected.

// seedUncorrectedPayload puts a payload into the pre-v108 shape: a manifest
// line holding a cat id in the part-number column, with no part behind it.
func seedUncorrectedPayload(t *testing.T, db *store.DB, code string, values ...string) int64 {
	t.Helper()
	p := &payloads.Payload{Code: code, UOPCapacity: 10}
	testutil.MustNoErr(t, db.CreatePayload(p), "create payload "+code)
	for _, v := range values {
		if _, err := db.Exec(
			`INSERT INTO payload_manifest (payload_id, part_number, parts_per_cycle) VALUES ($1, $2, 1)`,
			p.ID, v); err != nil {
			t.Fatalf("seed manifest line %s/%s: %v", code, v, err)
		}
	}
	return p.ID
}

// derivedFor reads the value the Edge's catalog sync would carry for a payload
// — the thing the wrong-part guard turns into a style's expected set.
func derivedFor(t *testing.T, db *store.DB, payloadID int64) string {
	t.Helper()
	catids, err := db.PayloadCATIDs()
	testutil.MustNoErr(t, err, "PayloadCATIDs")
	return catids[payloadID]
}

// TestV110_MovesTheCATIDToThePartAndLeavesTheGuardAlone is the correction's
// whole contract on a single-line payload: the line comes out naming the
// payload code (which for a bin of one part IS that part's number), the old
// value comes out on the part as its cat id, and the value the guard derives
// is byte-identical across the move.
func TestV110_MovesTheCATIDToThePartAndLeavesTheGuardAlone(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// The template database has already migrated, so put a payload back into the
	// pre-correction shape and re-run the correction over it.
	payloadID := seedUncorrectedPayload(t, db, "V110-SINGLE", "10276")
	beforeDerived := derivedFor(t, db, payloadID)
	if beforeDerived != "10276" {
		t.Fatalf("pre-correction derived value = %q, want the raw manifest value", beforeDerived)
	}

	testutil.MustNoErr(t, store.RunManifestIdentityCorrection(db), "re-run v108")

	items, err := db.ListPayloadManifest(payloadID)
	testutil.MustNoErr(t, err, "ListPayloadManifest")
	if len(items) != 1 {
		t.Fatalf("manifest has %d lines, want 1", len(items))
	}
	if items[0].PartNumber != "V110-SINGLE" {
		t.Errorf("line names %q, want the payload code — for a bin of one part the "+
			"code IS the part number CMS books against", items[0].PartNumber)
	}
	if items[0].PartID == 0 {
		t.Error("line has no part_id: the FK is the half of this that cannot be forgotten")
	}
	if items[0].CATID != "10276" {
		t.Errorf("part carries cat id %q, want 10276 — the old manifest value was never "+
			"wrong, only mislabeled, so the correction MOVES it", items[0].CATID)
	}

	if after := derivedFor(t, db, payloadID); after != beforeDerived {
		t.Errorf("the guard's derived value changed across the correction: %q -> %q. "+
			"An empty or altered set silently disarms or re-aims the wrong-part guard at "+
			"whatever cell runs this payload, and nothing logs it",
			beforeDerived, after)
	}
}

// TestV110_LeavesAKitAloneAndStillDerivesItsCATIDs: a kit's payload code names
// the KIT, so it is nobody's component part number and there is nothing in the
// database that is. Those lines are left for a person — and the guard must not
// notice, which is what the COALESCE in PayloadCATIDs is for.
func TestV110_LeavesAKitAloneAndStillDerivesItsCATIDs(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	payloadID := seedUncorrectedPayload(t, db, "V110-KIT", "33142", "33143")
	before := derivedFor(t, db, payloadID)
	if before != "33142,33143" {
		t.Fatalf("pre-correction derived = %q, want both values", before)
	}

	testutil.MustNoErr(t, store.RunManifestIdentityCorrection(db), "re-run v108")

	items, err := db.ListPayloadManifest(payloadID)
	testutil.MustNoErr(t, err, "ListPayloadManifest")
	for _, it := range items {
		if it.PartID != 0 {
			t.Errorf("kit line %s was re-pointed at part %d — a kit component's part number "+
				"is not derivable from the kit's code, so seeding one would be inventing it",
				it.PartNumber, it.PartID)
		}
	}
	if after := derivedFor(t, db, payloadID); after != before {
		t.Errorf("a kit's derived cat ids changed across the correction: %q -> %q. "+
			"The un-re-pointed line still carries the value it always did and the read has "+
			"to see it, or the guard at this cell goes inert with nothing in any log",
			before, after)
	}
}

// TestV110_RefusesWhenTheGuardWouldChange is the predicate itself. Nothing in
// the shipped correction can produce this state; the test manufactures one (a
// part minted by hand with a DIFFERENT cat id than the line holds) to prove the
// refusal is real rather than a comment about one.
func TestV110_RefusesWhenTheGuardWouldChange(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	payloadID := seedUncorrectedPayload(t, db, "V110-REFUSE", "40016911")
	// Somebody entered this part already, with a different controls identity.
	if _, err := db.Exec(`INSERT INTO parts (part_number, catid) VALUES ('V110-REFUSE', '99999999')`); err != nil {
		t.Fatalf("seed conflicting part: %v", err)
	}

	err := store.RunManifestIdentityCorrection(db)
	if err == nil {
		t.Fatal("the correction accepted a change to a payload's derived cat-id set. " +
			"That set is what the wrong-part guard compares the live PLC reading against, " +
			"an empty or altered one is INERT rather than loud, and six live monitors " +
			"across the two plants carry no manual pin to fall back on")
	}
	if items, lerr := db.ListPayloadManifest(payloadID); lerr == nil {
		if len(items) != 1 || items[0].PartNumber != "40016911" {
			t.Errorf("the refused transaction left changes behind: %+v", items)
		}
	}
}
