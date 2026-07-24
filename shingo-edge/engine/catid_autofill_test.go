// catid_autofill_test.go — retiring redundant expected_catid stamps once the
// guard derives the part-identity set live from the claims' payloads.
package engine

import (
	"testing"

	"shingo/protocol/testutil"
)

// TestClearRedundantExpectedCATIDs: a pin equal to the derived single value is
// cleared (it was a backfill stamp); a differing pin, a multi-value pin, and a
// pin whose payload has no derivable CATID are all left untouched.
func TestClearRedundantExpectedCATIDs(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, styleA, _ := seedProduceNode(t, db, "two_robot") // produce claim WIDGET-A
	eng := testEngine(t, db)

	putCatalog(t, db, 1, "WIDGET-A", "40016911")
	putCatalog(t, db, 2, "PIA15", "40017111")
	putCatalog(t, db, 3, "PIA16", "40017112")

	// (a) Redundant: pin == derived single value → cleared.
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleA, "40016911"), "pin redundant")

	// (b) Differing pin (human intent) → left.
	styleDiff, err := db.CreateStyle("DIFF", "", processID)
	testutil.MustNoErr(t, err, "create diff")
	seedProduceClaim(t, db, styleDiff, "N-DIFF", "WIDGET-A") // derives 40016911
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleDiff, "99999999"), "pin differing")

	// (c) Multi-value pin (a two-part style a human pinned) → left, never
	//     collapsed against a single derived value.
	styleMulti, err := db.CreateStyle("MULTI", "", processID)
	testutil.MustNoErr(t, err, "create multi")
	seedProduceClaim(t, db, styleMulti, "N-ML", "PIA15")
	seedProduceClaim(t, db, styleMulti, "N-MR", "PIA16")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleMulti, "40017111,40017112"), "pin multi")

	// (d) Pin whose payload has no derivable CATID (payload absent from catalog)
	//     → derived empty, nothing to compare → left.
	styleNoCat, err := db.CreateStyle("NOCAT", "", processID)
	testutil.MustNoErr(t, err, "create nocat")
	seedProduceClaim(t, db, styleNoCat, "N-NC", "MISSING-PAYLOAD")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleNoCat, "40016911"), "pin nocat")

	eng.ClearRedundantExpectedCATIDs()

	if got := styleExpected(t, db, styleA); got != "" {
		t.Errorf("redundant single-value pin should be cleared, got %q", got)
	}
	if got := styleExpected(t, db, styleDiff); got != "99999999" {
		t.Errorf("differing pin must be left (human intent), got %q", got)
	}
	if got := styleExpected(t, db, styleMulti); got != "40017111,40017112" {
		t.Errorf("multi-value pin must be left, got %q", got)
	}
	if got := styleExpected(t, db, styleNoCat); got != "40016911" {
		t.Errorf("pin with no derivable CATID must be left, got %q", got)
	}
}

// TestHandlePayloadCatalog_StoresCATID confirms the sync carries the CATID onto
// the local catalog row.
func TestHandlePayloadCatalog_StoresCATID(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	putCatalog(t, db, 7, "PL-7", "40012345")

	ce, err := db.GetPayloadCatalogByCode("PL-7")
	testutil.MustNoErr(t, err, "get catalog by code")
	if ce.CATID != "40012345" {
		t.Errorf("catalog CATID = %q, want 40012345 (stored + read back)", ce.CATID)
	}
}
