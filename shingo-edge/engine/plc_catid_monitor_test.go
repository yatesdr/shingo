// plc_catid_monitor_test.go — unit tests for the A5 CATID monitor: the tag
// derivation sibling, the value-change debounce state machine, and the
// wrong-part alert.
package engine

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

func TestDeriveIdentityTag(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"MES_P42_Spot_Nut_Farm_2.Prod_Counter_01", "MES_P42_Spot_Nut_Farm_2.CATID_01"},
		{"MES_400Ton.Prod_Counter_01", "MES_400Ton.CATID_01"},
		{"NoParentStruct", ""}, // no dot → no parent struct
		{"", ""},
	}
	for _, c := range cases {
		if got := deriveIdentityTag(c.in); got != c.want {
			t.Errorf("deriveIdentityTag(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestApplyCatidEdge_Debounce scripts the value-change state machine: baseline
// confirm, no-change, a debounced change, flicker that must NOT confirm, and a
// read failure that preserves (does not erase) the confirmed baseline.
func TestApplyCatidEdge_Debounce(t *testing.T) {
	t.Parallel()
	st := &catidState{}
	t0 := time.Unix(0, 0)

	// First successful read → baseline confirm, NOT a change.
	if confirmed, isChange := applyCatidEdge(st, "40016911", true, t0); !confirmed || isChange {
		t.Fatalf("baseline: got (confirmed=%v,isChange=%v), want (true,false)", confirmed, isChange)
	}
	if st.lastConfirmed != "40016911" {
		t.Fatalf("baseline lastConfirmed = %q", st.lastConfirmed)
	}

	// Same value again → no confirmation, no change.
	if confirmed, _ := applyCatidEdge(st, "40016911", true, t0.Add(1*time.Second)); confirmed {
		t.Fatal("unchanged value must not re-confirm")
	}

	// New value appears → starts the debounce window, not yet confirmed.
	tChange := t0.Add(10 * time.Second)
	if confirmed, _ := applyCatidEdge(st, "50029999", true, tChange); confirmed {
		t.Fatal("changed value must not confirm before the debounce elapses")
	}
	// Still within the window → still not confirmed.
	if confirmed, _ := applyCatidEdge(st, "50029999", true, tChange.Add(cutoverDebounce/2)); confirmed {
		t.Fatal("value must not confirm mid-debounce")
	}
	// Debounce elapsed → confirm as a CHANGE.
	if confirmed, isChange := applyCatidEdge(st, "50029999", true, tChange.Add(cutoverDebounce)); !confirmed || !isChange {
		t.Fatalf("post-debounce: got (confirmed=%v,isChange=%v), want (true,true)", confirmed, isChange)
	}
	if st.lastConfirmed != "50029999" {
		t.Fatalf("changed lastConfirmed = %q", st.lastConfirmed)
	}

	// Flicker: a candidate appears then rebounds to the confirmed value before
	// settling → must never confirm the flicker.
	tFlick := tChange.Add(1 * time.Minute)
	applyCatidEdge(st, "77777777", true, tFlick) // start candidate
	if confirmed, _ := applyCatidEdge(st, "50029999", true, tFlick.Add(cutoverDebounce)); confirmed {
		t.Fatal("rebounded flicker must not confirm")
	}
	if st.pending != nil {
		t.Fatal("rebound must clear the pending candidate")
	}

	// Read failure preserves the confirmed baseline (guard keeps last-known).
	applyCatidEdge(st, "88888888", true, tFlick.Add(2*time.Minute)) // start a new candidate
	if confirmed, _ := applyCatidEdge(st, "", false, tFlick.Add(3*time.Minute)); confirmed {
		t.Fatal("read failure must not confirm")
	}
	if !st.seenValue || st.lastConfirmed != "50029999" {
		t.Fatalf("read failure must preserve baseline; seen=%v lastConfirmed=%q", st.seenValue, st.lastConfirmed)
	}
	if st.pending != nil {
		t.Fatal("read failure must cancel the in-flight change window")
	}
}

// TestOnConfirmedCATID_RaisesMismatchAlert pins the A5 alert: when a confirmed
// live CATID diverges from the active style's expected_catid, the monitor emits
// EventCATIDMismatch naming the press + both values. An empty expected_catid
// (unconfigured) stays silent.
func TestOnConfirmedCATID_RaisesMismatchAlert(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, styleID, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	var alarms []CATIDMismatchEvent
	eng.Events.SubscribeTypes(func(evt Event) {
		if a, ok := evt.Payload.(CATIDMismatchEvent); ok {
			alarms = append(alarms, a)
		}
	}, EventCATIDMismatch)

	mon := &catidMonitor{eng: eng, states: map[int64]*catidState{
		processID: {processName: "PRODUCE-PROC", plcName: "test-plc", lastConfirmed: "50029999", seenValue: true},
	}}
	eng.catidMon = mon

	// Unconfigured expected_catid → no alert.
	mon.onConfirmedCATID(processID, mon.states[processID], true)
	if len(alarms) != 0 {
		t.Fatalf("empty expected_catid must not alert, got %d", len(alarms))
	}

	// Configure a divergent expected value → one alert naming both values.
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleID, "40016911"), "set expected_catid")
	mon.onConfirmedCATID(processID, mon.states[processID], true)
	if len(alarms) != 1 {
		t.Fatalf("divergent CATID must raise exactly one alert, got %d", len(alarms))
	}
	a := alarms[0]
	if a.LiveCATID != "50029999" || a.ExpectedCATID != "40016911" || a.ProcessName != "PRODUCE-PROC" || a.StyleName != "PROD-STYLE" {
		t.Errorf("alert payload = %+v, want live=50029999 expected=40016911 press=PRODUCE-PROC style=PROD-STYLE", a)
	}

	// A matching live value → no further alert.
	mon.states[processID].lastConfirmed = "40016911"
	mon.onConfirmedCATID(processID, mon.states[processID], false)
	if len(alarms) != 1 {
		t.Fatalf("matching CATID must not add an alert, got %d", len(alarms))
	}
}

// TestCATIDChangePrompt_PromptsOnChange pins the B1 PROMPT mode: with
// changeover_auto_arm='prompt' a confirmed CATID CHANGE prompts the operator to
// start a changeover, pre-filling the target when the new part maps to a known
// style — but never on the baseline read, never when the new part already matches
// the active style, and NEVER auto-starting a changeover (that is 'auto' mode).
// Since the global default is now 'auto', this test sets the process to 'prompt'
// explicitly to pin the round-2 prompt-only behavior under the off-switch.
func TestCATIDChangePrompt_PromptsOnChange(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, styleID, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	// Off-switch: 'prompt' keeps the round-2 prompt-only behavior (no auto-start).
	testutil.MustNoErr(t, db.SetChangeoverAutoArm(processID, "prompt"), "set prompt mode")

	// Active style runs part 40016911; a second style maps to 50029999.
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleID, "40016911"), "active expected")
	nextStyleID, err := db.CreateStyle("NEXT-STYLE", "next part", processID)
	testutil.MustNoErr(t, err, "create next style")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(nextStyleID, "50029999"), "next expected")

	var prompts []CATIDChangePromptEvent
	eng.Events.SubscribeTypes(func(evt Event) {
		if p, ok := evt.Payload.(CATIDChangePromptEvent); ok {
			prompts = append(prompts, p)
		}
	}, EventCATIDChangePrompt)

	mon := &catidMonitor{eng: eng, states: map[int64]*catidState{
		processID: {processName: "PRODUCE-PROC", plcName: "test-plc", seenValue: true},
	}}
	eng.catidMon = mon
	st := mon.states[processID]

	// Baseline confirmation (isChange=false) → NO prompt.
	st.lastConfirmed = "40016911"
	mon.onConfirmedCATID(processID, st, false)
	if len(prompts) != 0 {
		t.Fatalf("baseline read must not prompt, got %d", len(prompts))
	}

	// Change to a part that maps to a known, non-active style → prompt with a
	// pre-filled target.
	st.lastConfirmed = "50029999"
	mon.onConfirmedCATID(processID, st, true)
	if len(prompts) != 1 {
		t.Fatalf("a change must prompt exactly once, got %d", len(prompts))
	}
	if p := prompts[0]; !p.HasTarget || p.TargetStyleID != nextStyleID || p.TargetStyleName != "NEXT-STYLE" || p.NewCATID != "50029999" {
		t.Errorf("prompt = %+v, want pre-filled NEXT-STYLE target for 50029999", p)
	}

	// Change to a part with no known style → still prompts, but no pre-fill.
	st.lastConfirmed = "99999999"
	mon.onConfirmedCATID(processID, st, true)
	if len(prompts) != 2 {
		t.Fatalf("an unmapped change must still prompt, got %d", len(prompts))
	}
	if p := prompts[1]; p.HasTarget || p.TargetStyleID != 0 {
		t.Errorf("unmapped prompt = %+v, want HasTarget=false", p)
	}

	// Change back to the ACTIVE style's part → suppressed (line is now correct
	// for the running style; nothing to change to).
	st.lastConfirmed = "40016911"
	mon.onConfirmedCATID(processID, st, true)
	if len(prompts) != 2 {
		t.Fatalf("a change matching the active style must not prompt, got %d", len(prompts))
	}
}

// ── B1 auto-arm (changeover_auto_arm='auto') ──────────────────────────────────

type autoArmSpyCall struct {
	processID int64
	styleID   int64
	calledBy  string
	notes     string
}

// setupAutoArm wires a process with an active style (expected_catid=activeCATID)
// and a TARGET-STYLE (expected_catid=targetCATID), plus a monitor whose
// StartProcessChangeover is a recording spy. mode is written to the process; ""
// leaves the DDL default ('auto'), which is what the default-on cases exercise.
func setupAutoArm(t *testing.T, mode, activeCATID, targetCATID string) (mon *catidMonitor, st *catidState, processID, targetStyleID int64, calls *[]autoArmSpyCall) {
	t.Helper()
	db := testEngineDB(t)
	processID, _, activeStyleID, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(activeStyleID, activeCATID), "active expected")
	var err error
	targetStyleID, err = db.CreateStyle("TARGET-STYLE", "target", processID)
	testutil.MustNoErr(t, err, "create target style")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(targetStyleID, targetCATID), "target expected")
	if mode != "" {
		testutil.MustNoErr(t, db.SetChangeoverAutoArm(processID, mode), "set auto-arm mode")
	}
	recorded := []autoArmSpyCall{}
	mon = &catidMonitor{
		eng: eng,
		states: map[int64]*catidState{
			processID: {processName: "PRESS-1", plcName: "plc", seenValue: true},
		},
		startChangeover: func(pid, sid int64, cb, notes string) (*processes.Changeover, error) {
			recorded = append(recorded, autoArmSpyCall{pid, sid, cb, notes})
			return &processes.Changeover{ID: 1}, nil
		},
	}
	eng.catidMon = mon
	return mon, mon.states[processID], processID, targetStyleID, &recorded
}

// tickAutoArm simulates one monitor tick for the auto-arm tracker: it sets the
// debounced value (when the read is ok) and runs evaluateAutoArm, firing any
// resulting intent exactly as the real tick does after unlocking cm.mu.
func tickAutoArm(mon *catidMonitor, processID int64, st *catidState, value string, ok bool, now time.Time) {
	if ok {
		st.lastConfirmed = value
		st.seenValue = true
	}
	if intent := mon.evaluateAutoArm(processID, st, ok, now); intent != nil {
		mon.fireAutoArm(*intent)
	}
}

// TestAutoArm_CleanFlip_ArmsOnceWithTarget pins the happy path AND default-on: a
// process with NO explicit mode (DDL default 'auto') whose live CATID holds a new,
// mapped value across >=3 reads spanning >=60s auto-starts the changeover to that
// target — exactly once — and raises the station notification.
func TestAutoArm_CleanFlip_ArmsOnceWithTarget(t *testing.T) {
	t.Parallel()
	mon, st, processID, targetStyleID, calls := setupAutoArm(t, "", "40016911", "50029999")

	// Default-on: an unconfigured process reads as 'auto'.
	if got := mon.eng.processChangeoverAutoArm(processID); got != "auto" {
		t.Fatalf("default changeover_auto_arm = %q, want auto (default-on everywhere)", got)
	}

	var notes []CATIDAutoArmedEvent
	mon.eng.Events.SubscribeTypes(func(evt Event) {
		if n, ok := evt.Payload.(CATIDAutoArmedEvent); ok {
			notes = append(notes, n)
		}
	}, EventCATIDAutoArmed)

	t0 := time.Unix(0, 0)
	tickAutoArm(mon, processID, st, "50029999", true, t0) // read 1
	if len(*calls) != 0 {
		t.Fatalf("armed after 1 read, want 0")
	}
	tickAutoArm(mon, processID, st, "50029999", true, t0.Add(30*time.Second)) // read 2, 30s
	if len(*calls) != 0 {
		t.Fatalf("armed after 2 reads/30s (< window), want 0")
	}
	tickAutoArm(mon, processID, st, "50029999", true, t0.Add(60*time.Second)) // read 3, 60s → arm
	if len(*calls) != 1 {
		t.Fatalf("want exactly 1 arm at 3 reads/60s, got %d", len(*calls))
	}
	c := (*calls)[0]
	if c.processID != processID || c.styleID != targetStyleID || c.calledBy != "auto-arm" {
		t.Errorf("arm = %+v, want process %d style %d calledBy=auto-arm", c, processID, targetStyleID)
	}
	if len(notes) != 1 || notes[0].TargetStyleID != targetStyleID || notes[0].NewCATID != "50029999" {
		t.Errorf("station notification = %+v, want one for target %d / CATID 50029999", notes, targetStyleID)
	}

	// Continued stability must NOT re-arm.
	tickAutoArm(mon, processID, st, "50029999", true, t0.Add(90*time.Second))
	tickAutoArm(mon, processID, st, "50029999", true, t0.Add(120*time.Second))
	if len(*calls) != 1 {
		t.Fatalf("re-armed a still-stable value: got %d, want 1", len(*calls))
	}
}

// TestAutoArm_BouncingTag_NeverArms: a value that changes on every read never
// accumulates the required stability, even over minutes.
func TestAutoArm_BouncingTag_NeverArms(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoArm(t, "auto", "40016911", "50029999")
	t0 := time.Unix(0, 0)
	values := []string{"50029999", "77777777", "50029999", "88888888", "50029999", "99999999"}
	for i, v := range values {
		tickAutoArm(mon, processID, st, v, true, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("a bouncing tag armed %d time(s), want 0", len(*calls))
	}
}

// TestAutoArm_ZeroAndUnreadable_NeverArm: a stable zero value never arms, and
// unreadable reads reset the tracker (the last-good value survives for the guard,
// but the auto-arm window restarts).
func TestAutoArm_ZeroAndUnreadable_NeverArm(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoArm(t, "auto", "40016911", "50029999")
	t0 := time.Unix(0, 0)
	for i := 0; i < 5; i++ { // stable "0" well past the window
		tickAutoArm(mon, processID, st, "0", true, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("a zero value armed %d time(s), want 0", len(*calls))
	}
	st.lastConfirmed = "50029999" // last good value survives an unreadable read
	for i := 5; i < 10; i++ {
		tickAutoArm(mon, processID, st, "", false, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("unreadable reads armed %d time(s), want 0", len(*calls))
	}
}

// TestAutoArm_UnmappedValue_NeverArms: a stable value that maps to no configured
// style never arms (matches nothing → silent, no auto-start).
func TestAutoArm_UnmappedValue_NeverArms(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoArm(t, "auto", "40016911", "50029999")
	t0 := time.Unix(0, 0)
	for i := 0; i < 4; i++ {
		tickAutoArm(mon, processID, st, "12345678", true, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("an unmapped value armed %d time(s), want 0", len(*calls))
	}
}

// TestAutoArm_ChangeoverInProgress_NeverArms: a stable mapped value never arms
// while a non-terminal changeover already exists for the process.
func TestAutoArm_ChangeoverInProgress_NeverArms(t *testing.T) {
	t.Parallel()
	mon, st, processID, targetStyleID, calls := setupAutoArm(t, "auto", "40016911", "50029999")
	_, err := mon.eng.db.DB.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state) VALUES (?, ?, 'in_progress')`, processID, targetStyleID)
	testutil.MustNoErr(t, err, "seed in-progress changeover")
	if !mon.eng.changeoverInProgress(processID) {
		t.Fatal("test setup: expected changeoverInProgress=true")
	}
	t0 := time.Unix(0, 0)
	for i := 0; i < 4; i++ {
		tickAutoArm(mon, processID, st, "50029999", true, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("armed while a changeover was in progress: got %d, want 0", len(*calls))
	}
}

// TestAutoArm_DoubleFlipWithinWindow_NeverArms: A→B→A→B within the settle window
// never arms; each flip restarts the window. It only arms once B truly holds for
// the full window after the LAST flip.
func TestAutoArm_DoubleFlipWithinWindow_NeverArms(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoArm(t, "auto", "40016911", "50029999")
	t0 := time.Unix(0, 0)
	tickAutoArm(mon, processID, st, "50029999", true, t0)                     // B, count 1
	tickAutoArm(mon, processID, st, "50029999", true, t0.Add(20*time.Second)) // B, count 2 (20s)
	tickAutoArm(mon, processID, st, "40016911", true, t0.Add(30*time.Second)) // flip to active A → reset
	tickAutoArm(mon, processID, st, "50029999", true, t0.Add(45*time.Second)) // flip back to B → reset (firstSeen=45s)
	tickAutoArm(mon, processID, st, "50029999", true, t0.Add(75*time.Second)) // B, count 2 (30s since last flip)
	if len(*calls) != 0 {
		t.Fatalf("a double-flip armed within the window: got %d, want 0", len(*calls))
	}
	// B has now held a full 60s since the last flip (t=45s) → arms once.
	tickAutoArm(mon, processID, st, "50029999", true, t0.Add(105*time.Second))
	if len(*calls) != 1 {
		t.Fatalf("B stable 60s after the last flip must arm once, got %d", len(*calls))
	}
}

// TestAutoArm_AmbiguousMapping_PromptsNoArm pins addition 1: when a stable new
// part maps to MORE than one style (uniqueness is checked, not assumed) the
// monitor NEVER auto-arms a guess — it falls back to the operator prompt naming
// every candidate style, with no pre-filled target.
func TestAutoArm_AmbiguousMapping_PromptsNoArm(t *testing.T) {
	t.Parallel()
	mon, st, processID, targetStyleID, calls := setupAutoArm(t, "auto", "40016911", "50029999")

	// A SECOND style also runs part 50029999 → the part is now ambiguous.
	otherID, err := mon.eng.db.CreateStyle("TARGET-STYLE-2", "target2", processID)
	testutil.MustNoErr(t, err, "create second target style")
	testutil.MustNoErr(t, mon.eng.db.SetStyleExpectedCATID(otherID, "50029999"), "second target expected")

	var prompts []CATIDChangePromptEvent
	mon.eng.Events.SubscribeTypes(func(evt Event) {
		if p, ok := evt.Payload.(CATIDChangePromptEvent); ok {
			prompts = append(prompts, p)
		}
	}, EventCATIDChangePrompt)

	t0 := time.Unix(0, 0)
	tickAutoArm(mon, processID, st, "50029999", true, t0)
	tickAutoArm(mon, processID, st, "50029999", true, t0.Add(30*time.Second))
	tickAutoArm(mon, processID, st, "50029999", true, t0.Add(60*time.Second)) // stable → decision

	if len(*calls) != 0 {
		t.Fatalf("an ambiguous part must never auto-arm, got %d arm(s)", len(*calls))
	}
	if len(prompts) != 1 {
		t.Fatalf("ambiguity must emit exactly one operator prompt, got %d", len(prompts))
	}
	p := prompts[0]
	if p.HasTarget {
		t.Errorf("ambiguous prompt must not pre-fill a target, got %+v", p)
	}
	if len(p.Candidates) != 2 {
		t.Fatalf("ambiguous prompt must name both candidate styles, got %d", len(p.Candidates))
	}
	gotIDs := map[int64]bool{p.Candidates[0].StyleID: true, p.Candidates[1].StyleID: true}
	if !gotIDs[targetStyleID] || !gotIDs[otherID] {
		t.Errorf("candidates = %+v, want both %d and %d", p.Candidates, targetStyleID, otherID)
	}
	// Staying stable must not re-decide (armHandled) — still exactly one prompt.
	tickAutoArm(mon, processID, st, "50029999", true, t0.Add(90*time.Second))
	if len(prompts) != 1 || len(*calls) != 0 {
		t.Fatalf("a settled ambiguous value must not re-prompt/arm: prompts=%d arms=%d", len(prompts), len(*calls))
	}
}

// TestAutoArm_PromptMode_NeverAutoArms: in 'prompt' mode a stable mapped value
// never auto-starts (the operator is prompted instead — covered by
// TestCATIDChangePrompt_PromptsOnChange).
func TestAutoArm_PromptMode_NeverAutoArms(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoArm(t, "prompt", "40016911", "50029999")
	t0 := time.Unix(0, 0)
	for i := 0; i < 4; i++ {
		tickAutoArm(mon, processID, st, "50029999", true, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("prompt mode auto-armed %d time(s), want 0", len(*calls))
	}
}

// TestAutoArm_OffMode_Silent: in 'off' mode a stable mapped value neither arms nor
// prompts.
func TestAutoArm_OffMode_Silent(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoArm(t, "off", "40016911", "50029999")
	t0 := time.Unix(0, 0)
	for i := 0; i < 4; i++ {
		tickAutoArm(mon, processID, st, "50029999", true, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("off mode armed %d time(s), want 0", len(*calls))
	}
	var prompts int
	mon.eng.Events.SubscribeTypes(func(evt Event) {
		if _, ok := evt.Payload.(CATIDChangePromptEvent); ok {
			prompts++
		}
	}, EventCATIDChangePrompt)
	st.lastConfirmed = "50029999"
	mon.onConfirmedCATID(processID, st, true)
	if prompts != 0 {
		t.Fatalf("off mode prompted %d time(s), want 0", prompts)
	}
}
