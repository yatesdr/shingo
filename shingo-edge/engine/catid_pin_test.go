// catid_pin_test.go — the manual expected_catid pin, and the machinery that
// used to delete it.
package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// TestExpectedCATIDPin_SurvivesACatalogSync is the replacement for a test that
// asserted the opposite.
//
// ClearRedundantExpectedCATIDs ran after every payload-catalog sync and deleted
// any pin that agreed with the value derived from the style's produce payloads.
// Its argument was that the pin was redundant — and it was, right up until the
// derived value changed or emptied, at which point the style had no pin either,
// because a scheduler had removed it. Drift with a scheduler: the honest home
// for a person's answer emptied itself on a timer, which is a large part of why
// the answers ended up in the BOM instead.
//
// A pin is permanent human intent now. When a style carries one it IS the set,
// it always wins over the derived value, and only a person removes it.
func TestExpectedCATIDPin_SurvivesACatalogSync(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, styleA, _ := seedProduceNode(t, db, "two_robot") // produce claim WIDGET-A
	eng := testEngine(t, db)

	putCatalog(t, db, 1, "WIDGET-A", "40016911")
	putCatalog(t, db, 2, "PIA15", "40017111")

	// The shape that used to be deleted: a pin identical to the derived value.
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleA, "40016911"), "pin, matching the derived value")

	// And one that never was: a pin that disagrees.
	styleDiff, err := db.CreateStyle("DIFF", "", processID)
	testutil.MustNoErr(t, err, "create diff style")
	seedProduceClaim(t, db, styleDiff, "N-DIFF", "PIA15") // derives 40017111
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleDiff, "99999999"), "pin, disagreeing")

	eng.HandlePayloadCatalog([]protocol.CatalogPayloadInfo{
		{ID: 1, Name: "WIDGET-A", Code: "WIDGET-A", CATID: "40016911"},
		{ID: 2, Name: "PIA15", Code: "PIA15", CATID: "40017111"},
	})

	if got := styleExpected(t, db, styleA); got != "40016911" {
		t.Errorf("a pin equal to the derived value was cleared by a catalog sync (got %q). "+
			"Redundant today is not redundant after the payload's manifest changes, and "+
			"the style would then have neither a pin nor a derivable value", got)
	}
	if got := styleExpected(t, db, styleDiff); got != "99999999" {
		t.Errorf("a disagreeing pin = %q, want 99999999 — it is the override, and the "+
			"override is the whole reason the column exists", got)
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
