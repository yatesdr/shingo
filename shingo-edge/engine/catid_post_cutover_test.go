// catid_post_cutover_test.go — post-cutover CATID verification.
package engine

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/store"
)

// seedChangeoverTo creates a changeover row from a fresh from-style to toStyleID
// and returns its id, for post-cutover verification tests.
func seedChangeoverTo(t *testing.T, eng *Engine, db *store.DB, processID, toStyleID int64) int64 {
	t.Helper()
	fromID, err := db.CreateStyle("CO-FROM", "from", processID)
	testutil.MustNoErr(t, err, "create from style")
	coID, err := eng.changeoverService.Create(processID, &fromID, toStyleID, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "create changeover")
	return coID
}

// TestPostCutoverVerify_MatchNoFlag: the live part matches the new style within
// the window ⇒ verified, no flag.
func TestPostCutoverVerify_MatchNoFlag(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, styleA, _ := seedProduceNode(t, db, "two_robot")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleA, "70000001"), "set expected")
	eng := testEngine(t, db)
	coID := seedChangeoverTo(t, eng, db, processID, styleA)

	seedCatidMonitor(eng, processID, "PROC", "70000001") // live matches the new style
	cm := eng.catidMon
	now := time.Now()
	cm.openPostCutoverVerify(processID, coID, styleA, verifySet(t, eng, styleA), now.Add(time.Minute))
	cm.checkPostCutoverVerify(processID, cm.states[processID], now)

	co, err := db.LatestFlaggedChangeover(processID)
	testutil.MustNoErr(t, err, "latest flagged")
	if co != nil {
		t.Errorf("a matching part must not flag the changeover, got %+v", co)
	}
	if _, ok := eng.PostCutoverFlag(processID); ok {
		t.Error("PostCutoverFlag must report no flag on a match")
	}
}

// TestPostCutoverVerify_MismatchMapped: the live part disagrees and maps to a
// known style ⇒ flag names both styles, and the one-tap corrective changeover
// clears it.
func TestPostCutoverVerify_MismatchMapped(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, styleA, _ := seedProduceNode(t, db, "two_robot")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleA, "70000001"), "expected A")
	styleB, err := db.CreateStyle("STYLE-B", "b", processID)
	testutil.MustNoErr(t, err, "create style B")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleB, "50029999"), "expected B")
	eng := testEngine(t, db)
	coID := seedChangeoverTo(t, eng, db, processID, styleA)

	seedCatidMonitor(eng, processID, "PROC", "50029999") // wrong part — maps to style B
	cm := eng.catidMon
	now := time.Now()
	cm.openPostCutoverVerify(processID, coID, styleA, verifySet(t, eng, styleA), now) // deadline = now ⇒ window elapsed
	cm.checkPostCutoverVerify(processID, cm.states[processID], now)

	co, err := db.LatestFlaggedChangeover(processID)
	testutil.MustNoErr(t, err, "latest flagged")
	if co == nil || co.VerifyLiveCATID != "50029999" {
		t.Fatalf("expected changeover flagged with live 50029999, got %+v", co)
	}

	flag, ok := eng.PostCutoverFlag(processID)
	if !ok {
		t.Fatal("PostCutoverFlag must report the flag")
	}
	if flag.ExpectedStyleName != "PROD-STYLE" || !flag.HasMapped || flag.MappedStyleName != "STYLE-B" {
		t.Errorf("flag = %+v, want expected PROD-STYLE, mapped STYLE-B", flag)
	}
	if flag.CanUpdateCATID {
		t.Error("a part STYLE-B runs must not offer Update CATID: the pin would be refused")
	}
	for _, want := range []string{"PROD-STYLE", "50029999", "STYLE-B", "please confirm"} {
		if !strings.Contains(flag.Message, want) {
			t.Errorf("message %q missing %q", flag.Message, want)
		}
	}

	// One-tap corrective changeover to the mapped style clears the flag (the flag
	// clear is the first thing StartProcessChangeover does).
	_, _ = eng.StartProcessChangeover(processID, styleB, "corrective", "")
	co, err = db.LatestFlaggedChangeover(processID)
	testutil.MustNoErr(t, err, "latest flagged after corrective")
	if co != nil {
		t.Errorf("corrective changeover must clear the flag, still flagged %+v", co)
	}
}

// TestPostCutoverVerify_MismatchUnmapped: the live part disagrees and maps to NO
// style ⇒ flag says so (no mapped style), so the station points at config.
func TestPostCutoverVerify_MismatchUnmapped(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, styleA, _ := seedProduceNode(t, db, "two_robot")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleA, "70000001"), "expected A")
	eng := testEngine(t, db)
	coID := seedChangeoverTo(t, eng, db, processID, styleA)

	seedCatidMonitor(eng, processID, "PROC", "99999999") // wrong part, maps to nothing
	cm := eng.catidMon
	now := time.Now()
	cm.openPostCutoverVerify(processID, coID, styleA, verifySet(t, eng, styleA), now)
	cm.checkPostCutoverVerify(processID, cm.states[processID], now)

	flag, ok := eng.PostCutoverFlag(processID)
	if !ok {
		t.Fatal("PostCutoverFlag must report the flag")
	}
	if flag.HasMapped || flag.MappedStyleName != "" {
		t.Errorf("unmapped CATID must have no mapped style, got %+v", flag)
	}
	if !strings.Contains(flag.Message, "99999999") || !strings.Contains(flag.Message, "PROD-STYLE") {
		t.Errorf("message %q must name the live part and the set style", flag.Message)
	}
	if !flag.CanUpdateCATID {
		t.Error("a part no style runs must offer Update CATID")
	}
}

// flagWithLive flags a changeover to the process's active style (styleA, set
// {70000001}) with live part live, as the watch does when its window elapses,
// and returns the changeover id and the monitor.
func flagWithLive(t *testing.T, eng *Engine, db *store.DB, processID, styleA int64, live string) (int64, *catidMonitor) {
	t.Helper()
	coID := seedChangeoverTo(t, eng, db, processID, styleA)
	seedCatidMonitor(eng, processID, "PROC", live)
	cm := eng.catidMon
	now := time.Now()
	cm.openPostCutoverVerify(processID, coID, styleA, verifySet(t, eng, styleA), now)
	cm.checkPostCutoverVerify(processID, cm.states[processID], now)
	if co, err := db.LatestFlaggedChangeover(processID); err != nil || co == nil || co.ID != coID {
		t.Fatalf("setup: changeover %d not flagged: %+v, %v", coID, co, err)
	}
	return coID, cm
}

// clearedEvents records every EventChangeoverVerifyCleared.
func clearedEvents(eng *Engine) *[]CATIDVerifyClearedEvent {
	var got []CATIDVerifyClearedEvent
	eng.Events.SubscribeTypes(func(evt Event) {
		if p, ok := evt.Payload.(CATIDVerifyClearedEvent); ok {
			got = append(got, p)
		}
	}, EventChangeoverVerifyCleared)
	return &got
}

// TestPostCutoverFlag_ClearsWhenThePressCatchesUp: shingo's cutover finished
// before the die change, so the watch flagged the old part (HK P400
// 2026-11-11). When the PLC then reports a part of the style the changeover
// set, the flag clears and every station is told; a part of another style
// leaves it standing.
func TestPostCutoverFlag_ClearsWhenThePressCatchesUp(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, styleA, _ := seedProduceNode(t, db, "two_robot")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleA, "70000001"), "expected A")
	styleB, err := db.CreateStyle("STYLE-B", "b", processID)
	testutil.MustNoErr(t, err, "create style B")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleB, "50029999"), "expected B")
	eng := testEngine(t, db)
	cleared := clearedEvents(eng)
	coID, cm := flagWithLive(t, eng, db, processID, styleA, "50029999") // still the old part
	st := cm.states[processID]

	// A part no style of the changeover runs: the flag stands.
	st.lastConfirmed = "99999999"
	cm.onConfirmedCATID(processID, st, true)
	if co, err := db.LatestFlaggedChangeover(processID); err != nil || co == nil {
		t.Fatalf("a part that is not the set style's must leave the flag standing, got %+v (%v)", co, err)
	}

	// The die is changed: the PLC reports the set style's part.
	st.lastConfirmed = "70000001"
	cm.onConfirmedCATID(processID, st, true)
	if co, err := db.LatestFlaggedChangeover(processID); err != nil || co != nil {
		t.Fatalf("the press reporting the set style's part must clear the flag, still %+v (%v)", co, err)
	}
	if _, ok := eng.PostCutoverFlag(processID); ok {
		t.Error("PostCutoverFlag must report no flag once the press caught up")
	}
	if len(*cleared) != 1 || (*cleared)[0].ProcessID != processID || (*cleared)[0].ChangeoverID != coID {
		t.Errorf("cleared events = %+v, want one naming process %d changeover %d", *cleared, processID, coID)
	}
	if s, err := db.GetStyle(styleA); err != nil || s.ExpectedCATID != "70000001" {
		t.Errorf("catching up must write no pin: style A pin = %q (%v)", s.ExpectedCATID, err)
	}
}

// TestDismissPostCutoverFlag: Dismiss clears the flag and tells every station,
// writing no pin; with nothing flagged it is a no-op.
func TestDismissPostCutoverFlag(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, styleA, _ := seedProduceNode(t, db, "two_robot")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleA, "70000001"), "expected A")
	eng := testEngine(t, db)
	cleared := clearedEvents(eng)
	coID, _ := flagWithLive(t, eng, db, processID, styleA, "99999999")

	testutil.MustNoErr(t, eng.DismissPostCutoverFlag(processID), "dismiss")
	if co, err := db.LatestFlaggedChangeover(processID); err != nil || co != nil {
		t.Fatalf("Dismiss must clear the flag, still %+v (%v)", co, err)
	}
	if len(*cleared) != 1 || (*cleared)[0].ChangeoverID != coID {
		t.Errorf("cleared events = %+v, want one naming changeover %d", *cleared, coID)
	}
	if s, err := db.GetStyle(styleA); err != nil || s.ExpectedCATID != "70000001" {
		t.Errorf("Dismiss must write no pin: style A pin = %q (%v)", s.ExpectedCATID, err)
	}
	testutil.MustNoErr(t, eng.DismissPostCutoverFlag(processID), "dismiss with nothing flagged")
	if len(*cleared) != 1 {
		t.Errorf("a Dismiss with nothing flagged must emit nothing, got %d events", len(*cleared))
	}
}

// TestPostCutoverVerify_TwoPartMemberNoFlag: the new style is two-position and the
// live part is EITHER side of its derived set ⇒ verified, no flag. Mirrors the
// guard's membership rule at the verify step — a two-part cutover must not raise a
// false post-cutover flag just because the press settled on the "other" side.
func TestPostCutoverVerify_TwoPartMemberNoFlag(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, styleA, _ := seedProduceNode(t, db, "two_robot")
	// Two-position new style: left WIDGET-A / right PIA16, both with catalog CATIDs.
	putCatalog(t, db, 1, "WIDGET-A", "70000011")
	putCatalog(t, db, 2, "PIA16", "70000012")
	seedProduceClaim(t, db, styleA, "N-RIGHT", "PIA16")
	eng := testEngine(t, db)
	coID := seedChangeoverTo(t, eng, db, processID, styleA)

	seedCatidMonitor(eng, processID, "PROC", "70000012") // press settled on the RIGHT side
	cm := eng.catidMon
	now := time.Now()
	cm.openPostCutoverVerify(processID, coID, styleA, verifySet(t, eng, styleA), now.Add(time.Minute))
	cm.checkPostCutoverVerify(processID, cm.states[processID], now)

	if co, err := db.LatestFlaggedChangeover(processID); err != nil {
		t.Fatalf("latest flagged: %v", err)
	} else if co != nil {
		t.Errorf("a member of the two-part set must not flag the changeover, got %+v", co)
	}
	if _, ok := eng.PostCutoverFlag(processID); ok {
		t.Error("PostCutoverFlag must report no flag when the live part is in the new style's set")
	}
}

// verifySet reads a style's CATID set as the completion path does when it opens
// the watch.
func verifySet(t *testing.T, eng *Engine, styleID int64) map[string]struct{} {
	t.Helper()
	style, err := eng.db.GetStyle(styleID)
	testutil.MustNoErr(t, err, "get style")
	return eng.styleCATIDSet(style)
}
