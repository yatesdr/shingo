// catid_set_test.go — the derived part-identity SET (+ shared helpers for the
// CATID test family: guard, auto-arm, post-cutover verify, cleanup).
package engine

import (
	"fmt"
	"reflect"
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
	_, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{
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

// seedCATIDStyles adds styles [from, to) to the process, each with two produce
// claims whose payloads carry catalog CATIDs "40<style><claim>".
func seedCATIDStyles(t *testing.T, db *store.DB, processID int64, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		sid, err := db.CreateStyle(fmt.Sprintf("S-%03d", i), "", processID)
		testutil.MustNoErr(t, err, "create style")
		for j := 0; j < 2; j++ {
			code := fmt.Sprintf("P-%03d-%d", i, j)
			putCatalog(t, db, int64(1000+i*10+j), code, fmt.Sprintf("40%03d%d", i, j))
			seedProduceClaim(t, db, sid, fmt.Sprintf("N-%d", j), code)
		}
	}
}

// stylesForCATID must not scale with the process's style count. It runs on
// every stable CATID change at the press, and the per-style derivation was one
// ListStyleNodeClaims plus one catalog lookup per produce claim for every style
// in the process - ~450 queries per part change at 90 styles x 4 claims, on a
// store pinned to one connection. Counted, not timed: the query count is what
// that connection feels, and it does not vary with the CI runner's load.
func TestStylesForCATID_QueryCountIsConstant(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	eng := testEngine(t, db)
	pid, err := db.CreateProcess("PRESS-4", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")

	seedCATIDStyles(t, db, pid, 0, 2)
	counter.Reset()
	if got := eng.stylesForCATID(pid, "400011"); len(got) != 1 {
		t.Fatalf("matches at 2 styles = %d, want 1", len(got))
	}
	small := counter.Count()

	seedCATIDStyles(t, db, pid, 2, 8)
	counter.Reset()
	if got := eng.stylesForCATID(pid, "400011"); len(got) != 1 {
		t.Fatalf("matches at 8 styles = %d, want 1", len(got))
	}
	large := counter.Count()

	if large != small {
		t.Errorf("stylesForCATID queries: 2 styles = %d, 8 styles = %d - the count must not "+
			"grow with the style count (one per claim on a one-connection store is ~450 per "+
			"PLC part change at 90 styles)", small, large)
	}
}

// TestStylesForCATID_MatchesPerStyleDerivation pins, through the process-wide
// lookup, every rule the per-style derivation had: a multi-part catalog value
// splits into its members, a manual pin overrides derivation verbatim, a
// consume claim contributes nothing, a payload absent from the catalog
// contributes nothing, and a retired style is not a candidate. The multiplicity
// (zero / one / many) is the auto-arm contract, so it is asserted on names, in
// name order, not on counts.
func TestStylesForCATID_MatchesPerStyleDerivation(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, _ := seedProduceNode(t, db, "two_robot") // PROD-STYLE: produce claim WIDGET-A
	eng := testEngine(t, db)

	putCatalog(t, db, 1, "WIDGET-A", "40016911")
	putCatalog(t, db, 2, "PIA15", "40017111")
	putCatalog(t, db, 6, "KIT", "40017111,40017112")

	kit, err := db.CreateStyle("KIT-STYLE", "", processID)
	testutil.MustNoErr(t, err, "create kit")
	seedProduceClaim(t, db, kit, "N-KIT", "KIT")

	pinned, err := db.CreateStyle("PINNED", "", processID)
	testutil.MustNoErr(t, err, "create pinned")
	// Derives 40016911 from WIDGET-A, but the pin wins.
	seedProduceClaim(t, db, pinned, "N-P", "WIDGET-A")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(pinned, "40099999, 40088888"), "pin")

	consumeOnly, err := db.CreateStyle("CONSUME-ONLY", "", processID)
	testutil.MustNoErr(t, err, "create consume-only")
	_, err = upsertClaimRetiredMode(db, processes.NodeClaimInput{
		StyleID: consumeOnly, CoreNodeName: "N-C", Role: "consume", SwapMode: protocol.SwapModeSimple,
		PayloadCode: "PIA15", UOPCapacity: 100,
	})
	testutil.MustNoErr(t, err, "consume claim")

	retired, err := db.CreateStyle("RETIRED", "", processID)
	testutil.MustNoErr(t, err, "create retired")
	seedProduceClaim(t, db, retired, "N-R", "WIDGET-A")
	testutil.MustNoErr(t, db.DeleteStyle(retired), "retire")

	noCat, err := db.CreateStyle("NOCAT", "", processID)
	testutil.MustNoErr(t, err, "create nocat")
	seedProduceClaim(t, db, noCat, "N-NC", "MISSING-PAYLOAD")

	for _, tc := range []struct {
		catid string
		want  []string
	}{
		{"40016911", []string{"PROD-STYLE"}}, // PINNED is overridden, RETIRED is gone, NOCAT has nothing
		{"40017111", []string{"KIT-STYLE"}},  // the kit's first member; CONSUME-ONLY's consume claim does not count
		{"40017112", []string{"KIT-STYLE"}},  // the kit's second member
		{"40099999", []string{"PINNED"}},
		{"40088888", []string{"PINNED"}},
		{"40000000", nil},
		{"", nil},
	} {
		got := matchNames(eng.stylesForCATID(processID, tc.catid))
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("stylesForCATID(%q) = %v, want %v", tc.catid, got, tc.want)
		}
	}
}

// TestDerivedSetSplitsMultiPartCatalog proves the multi-part catalog sync
// end-to-end on the edge half: Core now sends a multi-part payload's FULL
// distinct part list comma-joined (e.g. a kit bin), and the derived set must
// split that value into every member — the same convention a manual
// expected_catid pin uses. Under the old single-value rule a multi-part
// payload synced an empty CATID and contributed nothing, silently leaving
// styles built on it without a part-identity set.
func TestDerivedSetSplitsMultiPartCatalog(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, styleSingle, _ := seedProduceNode(t, db, "two_robot") // produce claim WIDGET-A
	eng := testEngine(t, db)

	// Single-part payload: unchanged behavior (one-member set).
	putCatalog(t, db, 1, "WIDGET-A", "40016911")
	// Multi-part kit: Core sends both distinct parts comma-joined.
	putCatalog(t, db, 6, "KIT", "40017111,40017112")

	styleKit, err := db.CreateStyle("KIT-STYLE", "", processID)
	testutil.MustNoErr(t, err, "create kit style")
	seedProduceClaim(t, db, styleKit, "N-KIT", "KIT")

	sKit, _ := db.GetStyle(styleKit)
	set := eng.styleCATIDSet(sKit)
	if len(set) != 2 || !catidSetHas(set, "40017111") || !catidSetHas(set, "40017112") {
		t.Errorf("multi-part kit set = %v, want both {40017111, 40017112}", set)
	}

	// Whitespace tolerance: Core trims when joining, but be defensive — a
	// value like "40017111, 40017112" must still yield two members.
	putCatalog(t, db, 6, "KIT", "40017111, 40017112")
	sKit, _ = db.GetStyle(styleKit)
	set = eng.styleCATIDSet(sKit)
	if len(set) != 2 || !catidSetHas(set, "40017111") || !catidSetHas(set, "40017112") {
		t.Errorf("spaced multi-part set = %v, want both members", set)
	}

	// A single-part catalog value still yields exactly one member.
	sSingle, _ := db.GetStyle(styleSingle)
	if got := formatCATIDSet(eng.styleCATIDSet(sSingle)); got != "40016911" {
		t.Errorf("single-part set after multi-part sync = %q, want 40016911", got)
	}
}
