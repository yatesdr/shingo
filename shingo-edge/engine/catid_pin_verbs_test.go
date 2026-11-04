// catid_pin_verbs_test.go — the CATID map: the evidence a changeover leaves
// (start_catid, end_catid), the suggestion read from it, and the human verbs
// that write a pin.
package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// TestVerifyWatch_EmptySetRecordsEndCATID: a changeover to a style with no
// CATIDs still gets its watch; at its close the PLC's part is recorded on the
// row, and nothing is flagged (there is nothing to check against).
func TestVerifyWatch_EmptySetRecordsEndCATID(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, _, styleA, _ := seedProduceNode(t, db, "two_robot") // WIDGET-A has no catalog CATID: the set is empty
	eng := testEngine(t, db)
	co := seedChangeoverTo(t, eng, db, pid, styleA)
	seedCatidMonitor(eng, pid, "PROC", "1003")
	eng.openPostCutoverVerify(pid, co)
	cm := eng.catidMon
	if _, open := cm.verify[pid]; !open {
		t.Fatal("no watch on a changeover to a style with no CATIDs")
	}
	cm.checkPostCutoverVerify(pid, cm.states[pid], time.Now().Add(postCutoverVerifyWindow+time.Second))
	row := changeoverRow(t, db, co)
	if row.EndCATID != "1003" || row.VerifyLiveCATID != "" {
		t.Errorf("row after the watch = end %q flag %q, want end 1003 and no flag", row.EndCATID, row.VerifyLiveCATID)
	}
}

// TestVerifyWatch_NonIdentityIsNeverFlagged: an unused slot's -1 after a
// cutover is not a wrong part.
func TestVerifyWatch_NonIdentityIsNeverFlagged(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, _, styleA, _ := seedProduceNode(t, db, "two_robot")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleA, "1001"), "pin")
	eng := testEngine(t, db)
	co := seedChangeoverTo(t, eng, db, pid, styleA)
	seedCatidMonitor(eng, pid, "PROC", "-1")
	eng.openPostCutoverVerify(pid, co)
	eng.catidMon.checkPostCutoverVerify(pid, eng.catidMon.states[pid], time.Now().Add(postCutoverVerifyWindow+time.Second))
	if row := changeoverRow(t, db, co); row.VerifyLiveCATID != "" || row.EndCATID != "" {
		t.Errorf("row = flag %q end %q, want neither for -1", row.VerifyLiveCATID, row.EndCATID)
	}
}

func set(vs ...string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, v := range vs {
		m[v] = struct{}{}
	}
	return m
}

// TestSuggest_NeverFromTheStartValue: a value the PLC was already on when the
// changeover started is the outgoing part, whatever the watch saw.
func TestSuggest_NeverFromTheStartValue(t *testing.T) {
	t.Parallel()
	co := processes.Changeover{StartCATID: "1003", EndCATID: "1003"}
	if v, ok := suggestCATIDPin(co, set(), set()); ok {
		t.Errorf("suggested %s, the start value", v)
	}
	co.StartCATID = "1001"
	if v, ok := suggestCATIDPin(co, set(), set()); !ok || v != "1003" {
		t.Errorf("a value that changed during the changeover = %q %v, want 1003 suggested", v, ok)
	}
}

// TestSuggest_UnchangedValueSuggestsNothing: a value already in the to-style's
// set, or still the from-style's, is no news.
func TestSuggest_UnchangedValueSuggestsNothing(t *testing.T) {
	t.Parallel()
	co := processes.Changeover{StartCATID: "", EndCATID: "1001"}
	if _, ok := suggestCATIDPin(co, set(), set("1001")); ok {
		t.Error("suggested the from-style's own value")
	}
	if _, ok := suggestCATIDPin(co, set("1001"), set()); ok {
		t.Error("suggested a value already in the to-style's set")
	}
	if _, ok := suggestCATIDPin(processes.Changeover{EndCATID: "-1"}, set(), set()); ok {
		t.Error("suggested a value that names no part")
	}
}

// TestSuggest_NeverFromTheActiveStyle: a value the PLC sent during a run with
// no changeover gets no suggestion — "the style that was active" is the trap.
// A changeover that ended on it does.
func TestSuggest_NeverFromTheActiveStyle(t *testing.T) {
	t.Parallel()
	eng, db, pid, a, b, _ := healthRig(t, "1003")
	testutil.MustNoErr(t, db.RecordPLCCATIDObservation(pid, "plc", "1003"), "observe during the run")
	it := healthKinds(t, eng, pid)[catidKindUnmapped]
	if _, has := it.Data["suggestion"]; has {
		t.Fatalf("a value seen with no changeover was suggested: %+v", it)
	}
	co, err := eng.changeoverService.Create(pid, &a, b, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "changeover")
	testutil.MustNoErr(t, db.SetChangeoverStartCATID(co, "1001"), "start")
	testutil.MustNoErr(t, eng.CompleteProcessProductionCutover(pid), "cut over")
	testutil.MustNoErr(t, db.SetChangeoverEndCATID(co, "1003"), "end")
	it = healthKinds(t, eng, pid)[catidKindUnmapped]
	sug, _ := it.Data["suggestion"].(map[string]any)
	if sug == nil || sug["style_id"] != b || !strings.Contains(it.Detail, "ended on 1003") {
		t.Errorf("after a changeover to B that ended on 1003: %+v, want B suggested", it)
	}
}

// TestCatidPin_OneClickKeepsTheCatalogSet: a pin IS the set, so a one-click
// writes the style's catalog set plus the value, not the value alone.
func TestCatidPin_OneClickKeepsTheCatalogSet(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, _, styleA, _ := seedProduceNode(t, db, "two_robot")
	putCatalog(t, db, 1, "WIDGET-A", "70000011")
	eng := testEngine(t, db)
	testutil.MustNoErr(t, eng.AddStyleCATID(pid, styleA, "1003", "engineer", "the strip"), "one-click")
	style, err := db.GetStyle(styleA)
	testutil.MustNoErr(t, err, "style")
	got := eng.styleCATIDSet(style)
	if !catidSetHas(got, "1003") || !catidSetHas(got, "70000011") {
		t.Errorf("set after the one-click = %v (pin %q), want the catalog's 70000011 and 1003", got, style.ExpectedCATID)
	}
}

// TestCatidPin_RefusesACATIDAnotherStyleHolds: one CATID names one style, by
// every verb — the typed field, the one-click.
func TestCatidPin_RefusesACATIDAnotherStyleHolds(t *testing.T) {
	t.Parallel()
	eng, _, pid, a, b, _ := healthRig(t, "1001")
	if err := eng.CheckStyleCATIDPin(pid, b, "1002,1001"); !errors.Is(err, ErrCATIDTaken) {
		t.Errorf("typed pin holding A's 1001 = %v, want ErrCATIDTaken", err)
	}
	if err := eng.AddStyleCATID(pid, b, "1001", "engineer", "test"); !errors.Is(err, ErrCATIDTaken) {
		t.Errorf("one-click of A's 1001 onto B = %v, want ErrCATIDTaken", err)
	}
	if err := eng.CheckStyleCATIDPin(pid, a, "1001,1004"); err != nil {
		t.Errorf("a style's own value refused: %v", err)
	}
	if err := eng.CheckStyleCATIDPin(pid, b, "-1"); err == nil {
		t.Error("a typed -1 accepted")
	}
}

// TestCatidPin_ConfirmAppendsTheFlaggedValue: the post-cutover flag's Confirm
// says the reported part IS the to-style's; the value joins its set.
func TestCatidPin_ConfirmAppendsTheFlaggedValue(t *testing.T) {
	t.Parallel()
	eng, db, pid, a, _, _ := healthRig(t, "1001")
	co := seedChangeoverTo(t, eng, db, pid, a)
	testutil.MustNoErr(t, db.SetChangeoverVerifyMismatch(co, "1005"), "flag")
	testutil.MustNoErr(t, eng.ConfirmPostCutoverFlag(pid), "confirm")
	style, err := db.GetStyle(a)
	testutil.MustNoErr(t, err, "style")
	if got := eng.styleCATIDSet(style); !catidSetHas(got, "1005") || !catidSetHas(got, "1001") {
		t.Errorf("A's set after the Confirm = %v, want 1001 and the flagged 1005", got)
	}
	if row := changeoverRow(t, db, co); row.VerifyLiveCATID != "" {
		t.Errorf("flag still set: %q", row.VerifyLiveCATID)
	}
}

// TestChangeover_RecordsStartCATID: the start writes the PLC's confirmed part
// on the row.
func TestChangeover_RecordsStartCATID(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, _, _, to := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	seedCatidMonitor(eng, pid, "P3-PROC", "1001")
	co, _ := startChangeover(t, eng, db, pid, to)
	if row := changeoverRow(t, db, co.ID); row.StartCATID != "1001" {
		t.Errorf("start_catid = %q, want 1001", row.StartCATID)
	}
}
