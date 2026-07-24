// guard_catid_mismatch_test.go — regression tests for the hop A5 guard that
// refuses outgoing-style relief when the press's live CATID_01 diverges from the
// active style's expected_catid (2026-07-23).
package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// seedCatidMonitor attaches a catidMonitor to eng with a single process's live
// CATID pre-confirmed to live. Mirrors what the poll loop would leave behind.
func seedCatidMonitor(eng *Engine, processID int64, procName, live string) {
	eng.catidMon = &catidMonitor{
		eng: eng,
		states: map[int64]*catidState{
			processID: {
				plcName:       "test-plc",
				tagName:       "MES.CATID_01",
				processName:   procName,
				lastConfirmed: live,
				seenValue:     true,
			},
		},
	}
}

// TestGuardCatidMismatch_InertAndBlocking exercises every branch of the guard:
// inert on empty expected_catid, inert without a monitor / observation, exempt
// for loaders, blocks on divergence, passes on a match.
func TestGuardCatidMismatch_InertAndBlocking(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedProduceNode(t, db, "two_robot")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")
	eng := testEngine(t, db)

	lineClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeTwoRobot, StyleID: styleID}

	// 1. expected_catid unset → INERT even with a live value present.
	seedCatidMonitor(eng, processID, "PRODUCE-PROC", "99999999")
	if err := eng.guardCatidMismatch(node, lineClaim); err != nil {
		t.Errorf("unconfigured expected_catid must be inert, got: %v", err)
	}

	// Configure the active style's expected_catid.
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleID, "40016911"), "set expected_catid")

	// 2. No monitor at all → fail-open (inert).
	eng.catidMon = nil
	if err := eng.guardCatidMismatch(node, lineClaim); err != nil {
		t.Errorf("no monitor must be fail-open, got: %v", err)
	}

	// 3. Monitor present but no observation yet (seenValue=false) → fail-open.
	eng.catidMon = &catidMonitor{eng: eng, states: map[int64]*catidState{
		processID: {seenValue: false},
	}}
	if err := eng.guardCatidMismatch(node, lineClaim); err != nil {
		t.Errorf("unobserved CATID must be fail-open, got: %v", err)
	}

	// 4. Live matches expected → passes.
	seedCatidMonitor(eng, processID, "PRODUCE-PROC", "40016911")
	if err := eng.guardCatidMismatch(node, lineClaim); err != nil {
		t.Errorf("matching CATID must not block, got: %v", err)
	}

	// 5. Live diverges from expected → BLOCKS, stating BOTH sides (live part +
	//    active style's expected) and pointing at the pending resolution.
	seedCatidMonitor(eng, processID, "PRODUCE-PROC", "40016911")
	eng.catidMon.states[processID].lastConfirmed = "50029999"
	err = eng.guardCatidMismatch(node, lineClaim)
	if err == nil {
		t.Fatal("divergent CATID must block outgoing-style relief")
	}
	// "Press reports CATID 50029999; active style is PROD-STYLE (runs CATID 40016911) — ..."
	for _, want := range []string{"Press reports", "50029999", "40016911", "PROD-STYLE", "changeover"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("block message %q missing %q", err.Error(), want)
		}
	}

	// 6. Loaders (manual_swap) are exempt even when the part diverges.
	loaderClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeManualSwap, StyleID: styleID}
	if err := eng.guardCatidMismatch(node, loaderClaim); err != nil {
		t.Errorf("manual_swap loader must stay available despite CATID divergence, got: %v", err)
	}
}

// TestRequestProduceSwap_BlockedOnCatidMismatch pins the guard on the actual
// relief path: a produce swap is refused when the press's live CATID diverges
// from the active style's expected value, with a clear message.
func TestRequestProduceSwap_BlockedOnCatidMismatch(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleID, "40016911"), "set expected_catid")
	seedCatidMonitor(eng, processID, "PRODUCE-PROC", "50029999") // wrong part on press

	if _, err := eng.RequestProduceSwap(nodeID); err == nil {
		t.Fatal("expected RequestProduceSwap to be refused when live CATID mismatches the active style")
	} else if !strings.Contains(err.Error(), "CATID") {
		t.Errorf("error = %q, want the CATID mismatch message", err.Error())
	}
}

// TestGuardCatidMismatch_NeverFiresWhenExpectedCATIDBlank pins the inert-on-blank
// rule directly: a style with no expected_catid never blocks, even when a live
// value is observed and diverges from anything — an unconfigured guard is a
// no-op, so default-safe on plants that have not filled the field in.
func TestGuardCatidMismatch_NeverFiresWhenExpectedCATIDBlank(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedProduceNode(t, db, "two_robot")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")
	eng := testEngine(t, db)

	// expected_catid is left unset on the seeded style. A live value is present
	// and would diverge from any configured value — but blank ⇒ never fires.
	seedCatidMonitor(eng, processID, "PRODUCE-PROC", "50029999")
	lineClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeTwoRobot, StyleID: styleID}
	if err := eng.guardCatidMismatch(node, lineClaim); err != nil {
		t.Errorf("blank expected_catid must never block, got: %v", err)
	}
	// And through the real relief path, the request is not refused for CATID.
	if _, err := eng.RequestProduceSwap(nodeID); err != nil && strings.Contains(err.Error(), "CATID") {
		t.Errorf("blank expected_catid must not refuse the request for a CATID mismatch, got: %v", err)
	}
}

// TestGuardCatidMismatch_TwoPartMembership pins the left/right two-position case
// on the DERIVED set: a style whose two produce claims run two different parts
// accepts EITHER part on the press (whichever side reports), and blocks only a
// part that is neither. This is the false-block the derived-set model prevents —
// a single expected value would have refused relief whenever the press reported
// the "other" side.
func TestGuardCatidMismatch_TwoPartMembership(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedProduceNode(t, db, "two_robot")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")
	eng := testEngine(t, db)

	// Make the seeded style two-position: add a second produce claim with a second
	// payload, and give both payloads catalog CATIDs (left 40017111 / right 40017112).
	putCatalog(t, db, 1, "WIDGET-A", "40017111") // seedProduceNode's payload → left
	putCatalog(t, db, 2, "PIA16", "40017112")    // right
	seedProduceClaim(t, db, styleID, "N-RIGHT", "PIA16")

	lineClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeTwoRobot, StyleID: styleID}

	// Press reports the LEFT part → member → passes.
	seedCatidMonitor(eng, processID, "PRODUCE-PROC", "40017111")
	if err := eng.guardCatidMismatch(node, lineClaim); err != nil {
		t.Errorf("left part is a member of the style's set, must not block, got: %v", err)
	}
	// Press reports the RIGHT part → member → passes (the old single-value guard
	// would have blocked here).
	eng.catidMon.states[processID].lastConfirmed = "40017112"
	if err := eng.guardCatidMismatch(node, lineClaim); err != nil {
		t.Errorf("right part is a member of the style's set, must not block, got: %v", err)
	}
	// Press reports a part in NEITHER side → blocks, naming both valid parts.
	eng.catidMon.states[processID].lastConfirmed = "40099999"
	err = eng.guardCatidMismatch(node, lineClaim)
	if err == nil {
		t.Fatal("a part in neither side of the set must block")
	}
	for _, want := range []string{"40099999", "40017111", "40017112"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("block message %q missing %q (both valid parts must be named)", err.Error(), want)
		}
	}
}

// TestGuardCatidMismatch_PinnedTwoPart proves the manual override accepts a
// comma-separated list: a human pinning a two-part style names both parts and the
// guard treats either as valid — pinning does not recreate the single-value
// false-block on the other side.
func TestGuardCatidMismatch_PinnedTwoPart(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedProduceNode(t, db, "two_robot")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")
	eng := testEngine(t, db)

	// Non-empty pin = use exactly that set (derivation ignored).
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleID, "40017111, 40017112"), "pin two-part")
	lineClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeTwoRobot, StyleID: styleID}

	seedCatidMonitor(eng, processID, "PRODUCE-PROC", "40017111")
	if err := eng.guardCatidMismatch(node, lineClaim); err != nil {
		t.Errorf("first pinned part must be accepted, got: %v", err)
	}
	eng.catidMon.states[processID].lastConfirmed = "40017112"
	if err := eng.guardCatidMismatch(node, lineClaim); err != nil {
		t.Errorf("second pinned part must be accepted (no false-block), got: %v", err)
	}
	eng.catidMon.states[processID].lastConfirmed = "40099999"
	if err := eng.guardCatidMismatch(node, lineClaim); err == nil {
		t.Fatal("a part outside the pinned list must block")
	}
}

// TestGuardCatidMismatch_EmptyDerivedInert pins addition 4: a style whose produce
// payload has NO catalog CATID (derived set empty) and no pin is inert exactly
// like a blank pin today — it never blocks, even on a divergent live value.
func TestGuardCatidMismatch_EmptyDerivedInert(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedProduceNode(t, db, "two_robot")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")
	eng := testEngine(t, db)

	// Payload present in the catalog but with NO CATID → derived set empty, no pin.
	putCatalog(t, db, 1, "WIDGET-A", "")
	seedCatidMonitor(eng, processID, "PRODUCE-PROC", "50029999")
	lineClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeTwoRobot, StyleID: styleID}
	if err := eng.guardCatidMismatch(node, lineClaim); err != nil {
		t.Errorf("empty derived set (no catalog CATID) must be inert, got: %v", err)
	}
}
