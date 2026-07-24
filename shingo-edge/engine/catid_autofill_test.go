// catid_autofill_test.go — auto-fill of style.expected_catid from the catalog CATID.
package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/catalog"
)

func styleExpected(t *testing.T, db *store.DB, id int64) string {
	t.Helper()
	s, err := db.GetStyle(id)
	testutil.MustNoErr(t, err, "get style")
	return s.ExpectedCATID
}

// TestAutoFillExpectedCATID fills a blank style from its produce payload's synced
// CATID, never overwrites an engineer's value, and never guesses when the catalog
// has no CATID for the payload.
func TestAutoFillExpectedCATID(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	// seedProduceNode gives a produce-role claim whose payload code is WIDGET-A.
	_, _, styleA, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	// Catalog (synced from Core) says WIDGET-A is part 40016911.
	testutil.MustNoErr(t, db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 1, Name: "WIDGET-A", Code: "WIDGET-A", CATID: "40016911",
	}), "upsert catalog")

	// Blank style auto-fills from its produce payload's CATID.
	eng.AutoFillExpectedCATIDForStyle(styleA)
	if got := styleExpected(t, db, styleA); got != "40016911" {
		t.Errorf("expected_catid = %q, want 40016911 auto-filled from the catalog", got)
	}

	// Never overwrites an engineer-set value.
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleA, "99999999"), "manual set")
	eng.AutoFillExpectedCATIDForStyle(styleA)
	if got := styleExpected(t, db, styleA); got != "99999999" {
		t.Errorf("auto-fill must not overwrite an engineer value; got %q", got)
	}
}

// TestAutoFillExpectedCATID_NoCatalogCATID_NoFill: when the catalog has no CATID
// for the produce payload (Core reported it ambiguous / none), the style stays
// blank — never guess.
func TestAutoFillExpectedCATID_NoCatalogCATID_NoFill(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, _, styleA, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	testutil.MustNoErr(t, db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 1, Name: "WIDGET-A", Code: "WIDGET-A", CATID: "",
	}), "upsert catalog")

	eng.AutoFillExpectedCATIDForStyle(styleA)
	if got := styleExpected(t, db, styleA); got != "" {
		t.Errorf("no catalog CATID must leave the style blank; got %q", got)
	}
}

// TestHandlePayloadCatalog_StoresCATID confirms the sync carries the CATID onto
// the local catalog row.
func TestHandlePayloadCatalog_StoresCATID(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	testutil.MustNoErr(t, db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 7, Name: "PL-7", Code: "PL-7", CATID: "40012345",
	}), "upsert catalog")

	ce, err := db.GetPayloadCatalogByCode("PL-7")
	testutil.MustNoErr(t, err, "get catalog by code")
	if ce.CATID != "40012345" {
		t.Errorf("catalog CATID = %q, want 40012345 (stored + read back)", ce.CATID)
	}
}
