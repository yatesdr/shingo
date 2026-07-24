// catid_set_test.go — the derived part-identity SET (+ shared helpers for the
// CATID test family: guard, auto-arm, post-cutover verify, cleanup).
package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/catalog"
	"shingoedge/store/processes"
)

func styleExpected(t *testing.T, db *store.DB, id int64) string {
	t.Helper()
	s, err := db.GetStyle(id)
	testutil.MustNoErr(t, err, "get style")
	return s.ExpectedCATID
}

// seedProduceClaim adds one PRODUCE claim (node + payload) to an existing style —
// call twice with different nodes/payloads to build a two-position left/right style.
func seedProduceClaim(t *testing.T, db *store.DB, styleID int64, coreNode, payloadCode string) {
	t.Helper()
	_, err := upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        coreNode,
		Role:                "produce",
		SwapMode:            protocol.SwapModeSimple,
		PayloadCode:         payloadCode,
		UOPCapacity:         100,
		InboundSource:       "EMPTY-STORAGE",
		InboundStaging:      "IN",
		OutboundStaging:     "OUT",
		OutboundDestination: "FILLED",
		AutoRequestPayload:  payloadCode,
	})
	testutil.MustNoErr(t, err, "produce claim "+payloadCode)
}

// putCatalog upserts a synced catalog entry giving payload `code` the part id `catid`.
func putCatalog(t *testing.T, db *store.DB, id int64, code, catid string) {
	t.Helper()
	testutil.MustNoErr(t, db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: id, Name: code, Code: code, CATID: catid,
	}), "catalog "+code)
}

// TestStyleCATIDSet covers the derived single-part set, the two-part left/right
// set, and the manual comma-list pin overriding derivation.
func TestStyleCATIDSet(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, styleA, _ := seedProduceNode(t, db, "two_robot") // produce claim WIDGET-A
	eng := testEngine(t, db)

	putCatalog(t, db, 1, "WIDGET-A", "40016911")
	putCatalog(t, db, 2, "PIA15", "40017111")
	putCatalog(t, db, 3, "PIA16", "40017112")

	// Single-part style: one produce payload → one CATID.
	sA, _ := db.GetStyle(styleA)
	if got := formatCATIDSet(eng.styleCATIDSet(sA)); got != "40016911" {
		t.Errorf("single-part set = %q, want 40016911", got)
	}

	// Two-position style: two produce claims, two payloads → two CATIDs.
	styleTwo, err := db.CreateStyle("TWO-PART", "", processID)
	testutil.MustNoErr(t, err, "create two-part")
	seedProduceClaim(t, db, styleTwo, "N-LEFT", "PIA15")
	seedProduceClaim(t, db, styleTwo, "N-RIGHT", "PIA16")
	sTwo, _ := db.GetStyle(styleTwo)
	set := eng.styleCATIDSet(sTwo)
	if len(set) != 2 || !catidSetHas(set, "40017111") || !catidSetHas(set, "40017112") {
		t.Errorf("two-part set = %v, want {40017111, 40017112}", set)
	}

	// Manual comma-list pin overrides derivation verbatim.
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleTwo, "40099999, 40088888"), "pin")
	sTwo, _ = db.GetStyle(styleTwo)
	set = eng.styleCATIDSet(sTwo)
	if len(set) != 2 || !catidSetHas(set, "40099999") || !catidSetHas(set, "40088888") {
		t.Errorf("pinned set = %v, want the pin verbatim {40099999, 40088888}", set)
	}
}

// TestStylesForCATID_DetectsAmbiguity proves the uniqueness assumption is checked
// in code: a part id claimed by two styles returns both (auto-arm treats that as
// ambiguous).
func TestStylesForCATID_DetectsAmbiguity(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)
	putCatalog(t, db, 5, "SHARED", "40050000")

	s1, err := db.CreateStyle("AMB-1", "", processID)
	testutil.MustNoErr(t, err, "create amb-1")
	seedProduceClaim(t, db, s1, "N1", "SHARED")
	s2, err := db.CreateStyle("AMB-2", "", processID)
	testutil.MustNoErr(t, err, "create amb-2")
	seedProduceClaim(t, db, s2, "N2", "SHARED")

	if got := len(eng.stylesForCATID(processID, "40050000")); got != 2 {
		t.Fatalf("stylesForCATID matches = %d, want 2 (ambiguous)", got)
	}
	if got := len(eng.stylesForCATID(processID, "40099999")); got != 0 {
		t.Errorf("unknown CATID matches = %d, want 0", got)
	}
}
