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
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleA, "40016911"), "set expected")
	eng := testEngine(t, db)
	coID := seedChangeoverTo(t, eng, db, processID, styleA)

	seedCatidMonitor(eng, processID, "PROC", "40016911") // live matches the new style
	cm := eng.catidMon
	now := time.Now()
	cm.openPostCutoverVerify(processID, coID, styleA, "40016911", now.Add(time.Minute))
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
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleA, "40016911"), "expected A")
	styleB, err := db.CreateStyle("STYLE-B", "b", processID)
	testutil.MustNoErr(t, err, "create style B")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleB, "50029999"), "expected B")
	eng := testEngine(t, db)
	coID := seedChangeoverTo(t, eng, db, processID, styleA)

	seedCatidMonitor(eng, processID, "PROC", "50029999") // wrong part — maps to style B
	cm := eng.catidMon
	now := time.Now()
	cm.openPostCutoverVerify(processID, coID, styleA, "40016911", now) // deadline = now ⇒ window elapsed
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
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleA, "40016911"), "expected A")
	eng := testEngine(t, db)
	coID := seedChangeoverTo(t, eng, db, processID, styleA)

	seedCatidMonitor(eng, processID, "PROC", "99999999") // wrong part, maps to nothing
	cm := eng.catidMon
	now := time.Now()
	cm.openPostCutoverVerify(processID, coID, styleA, "40016911", now)
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
}
