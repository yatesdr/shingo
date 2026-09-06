//go:build docker

package payloads_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/payloads"
)

// TestPayloadCATIDs pins the catalog's part-identity feed: per payload, the
// DISTINCT cat ids its manifest lines resolve to, comma-joined in order. A
// single-part payload yields that one value; a kit yields its full list, which
// the edge splits into the style's part-identity SET (membership semantics, so
// a kit contributes every part instead of nothing). Payloads whose lines
// resolve to no cat id are omitted, so the edge derives nothing from them.
//
// IT READS THE PART'S CAT ID, not the manifest line's own value. The DISTINCT
// still does work after v109's unique index made a payload's lines unique by
// part: two different PARTS can carry the same controls identity, which is a
// data error worth seeing rather than a schema violation, and the guard's set
// must not double-count it.
func TestPayloadCATIDs(t *testing.T) {
	t.Parallel()
	sdb := testdb.Open(t)
	db := sdb.DB

	single := &payloads.Payload{Code: "CID-SINGLE", UOPCapacity: 1}
	testutil.MustNoErr(t, payloads.Create(db, single), "create single")
	multi := &payloads.Payload{Code: "CID-MULTI", UOPCapacity: 1}
	testutil.MustNoErr(t, payloads.Create(db, multi), "create multi")
	shared := &payloads.Payload{Code: "CID-SHARED", UOPCapacity: 1}
	testutil.MustNoErr(t, payloads.Create(db, shared), "create shared")
	none := &payloads.Payload{Code: "CID-NONE", UOPCapacity: 1}
	testutil.MustNoErr(t, payloads.Create(db, none), "create none")

	// A line names a part; the part carries the cat id.
	line := func(pid int64, partNumber, catid string) {
		t.Helper()
		testutil.MustNoErr(t, payloads.CreateItem(db, &payloads.ManifestItem{
			PayloadID: pid, PartNumber: partNumber, PartsPerCycle: 1,
		}, catid), "create manifest item "+partNumber)
	}
	line(single.ID, "CID-P-A", "40016911")
	line(multi.ID, "CID-P-B", "50029999")
	line(multi.ID, "CID-P-C", "40016911")
	// Two DIFFERENT parts carrying one cat id — allowed by the schema (a
	// unique index there would turn a data error into a migration that will
	// not run) and collapsed by the DISTINCT.
	line(shared.ID, "CID-P-D", "40017111")
	line(shared.ID, "CID-P-E", "40017111")
	// none: no manifest rows at all

	catids, err := payloads.PayloadCATIDs(db)
	testutil.MustNoErr(t, err, "PayloadCATIDs")

	if catids[single.ID] != "40016911" {
		t.Errorf("single-part payload CATID = %q, want 40016911", catids[single.ID])
	}
	if got, want := catids[multi.ID], "40016911,50029999"; got != want {
		t.Errorf("kit CATIDs = %q, want %q — every part it holds, so the guard accepts any "+
			"of them", got, want)
	}
	if got, want := catids[shared.ID], "40017111"; got != want {
		t.Errorf("two parts sharing a cat id = %q, want %q once", got, want)
	}
	if v, ok := catids[none.ID]; ok {
		t.Errorf("no-manifest payload must be omitted, got %q", v)
	}
}

// TestPayloadCATIDs_ReadsAnUnrepointedLine is the correction window, and it is
// the property v108's own predicate refuses to ship without.
//
// A kit's lines are not re-pointed by the migration — a kit's payload code is
// nobody's component part number — so until a person types those components the
// line still carries the cat id it always did. If this read saw only
// parts.catid, every such cell's derived set would empty on the deploy, and an
// empty set is INERT rather than loud: the wrong-part guard would stop guarding
// with nothing in any log.
func TestPayloadCATIDs_ReadsAnUnrepointedLine(t *testing.T) {
	t.Parallel()
	sdb := testdb.Open(t)
	db := sdb.DB

	p := &payloads.Payload{Code: "CID-WINDOW", UOPCapacity: 1}
	testutil.MustNoErr(t, payloads.Create(db, p), "create payload")
	// The pre-v108 shape: a line holding a cat id, pointing at no part.
	if _, err := db.Exec(
		`INSERT INTO payload_manifest (payload_id, part_number, parts_per_cycle) VALUES ($1, '33142', 1)`,
		p.ID); err != nil {
		t.Fatalf("seed an un-re-pointed line: %v", err)
	}

	catids, err := payloads.PayloadCATIDs(db)
	testutil.MustNoErr(t, err, "PayloadCATIDs")
	if catids[p.ID] != "33142" {
		t.Errorf("derived cat id = %q, want 33142. A line with no part behind it still "+
			"carries the value the guard has always derived from; losing it disarms the "+
			"cell that runs this payload, silently.", catids[p.ID])
	}
}
