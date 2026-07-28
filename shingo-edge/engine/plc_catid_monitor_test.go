// plc_catid_monitor_test.go — unit tests for the A5 CATID monitor: the tag
// derivation sibling, the value-change debounce state machine, and the
// wrong-part alert.
package engine

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
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
	if confirmed, _ := applyCatidEdge(st, "50029999", true, tChange.Add(plcEdgeDebounce/2)); confirmed {
		t.Fatal("value must not confirm mid-debounce")
	}
	// Debounce elapsed → confirm as a CHANGE.
	if confirmed, isChange := applyCatidEdge(st, "50029999", true, tChange.Add(plcEdgeDebounce)); !confirmed || !isChange {
		t.Fatalf("post-debounce: got (confirmed=%v,isChange=%v), want (true,true)", confirmed, isChange)
	}
	if st.lastConfirmed != "50029999" {
		t.Fatalf("changed lastConfirmed = %q", st.lastConfirmed)
	}

	// Flicker: a candidate appears then rebounds to the confirmed value before
	// settling → must never confirm the flicker.
	tFlick := tChange.Add(1 * time.Minute)
	applyCatidEdge(st, "77777777", true, tFlick) // start candidate
	if confirmed, _ := applyCatidEdge(st, "50029999", true, tFlick.Add(plcEdgeDebounce)); confirmed {
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

// ── B1 auto-CUTOVER (changeover_auto_arm='auto') ─────────────────────────────
//
// The monitor used to auto-START changeovers here. It now auto-COMPLETES one
// already in progress, and never starts anything. See decideAutoArm for why the
// condition inverted; the short version is that the problem this feature exists
// for is the operator who runs a changeover and then forgets to press CUTOVER,
// leaving shingo attributing new parts to the old style. Starting changeovers
// put four robots on the Hopkinsville floor chasing a style with zero stock
// (2026-07-28).

type autoCutoverSpyCall struct {
	processID   int64
	triggeredBy string
}

// setupAutoCutover wires a process with an active style (expected_catid =
// activeCATID) plus a second style, and a monitor whose completeCutover is a
// recording spy. mode is written to the process; "" leaves the DDL default.
// withChangeover seeds the in-progress changeover row the cutover path requires.
func setupAutoCutover(t *testing.T, mode, activeCATID, targetCATID string, withChangeover bool) (mon *catidMonitor, st *catidState, processID, targetStyleID int64, calls *[]autoCutoverSpyCall) {
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
	if withChangeover {
		_, err := db.DB.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state) VALUES (?, ?, 'in_progress')`, processID, targetStyleID)
		testutil.MustNoErr(t, err, "seed in-progress changeover")
	}
	recorded := []autoCutoverSpyCall{}
	mon = &catidMonitor{
		eng: eng,
		states: map[int64]*catidState{
			processID: {processName: "PRESS-1", plcName: "plc", seenValue: true},
		},
		completeCutover: func(pid int64, triggeredBy string) error {
			recorded = append(recorded, autoCutoverSpyCall{pid, triggeredBy})
			return nil
		},
	}
	eng.catidMon = mon
	return mon, mon.states[processID], processID, targetStyleID, &recorded
}

// tickAutoCutover simulates one monitor tick: set the debounced value (when the
// read is ok) and run evaluateAutoArm, firing any resulting intent exactly as the
// real tick does after unlocking cm.mu.
func tickAutoCutover(mon *catidMonitor, processID int64, st *catidState, value string, ok bool, now time.Time) {
	if ok {
		st.lastConfirmed = value
		st.seenValue = true
	}
	if intent := mon.evaluateAutoArm(processID, st, ok, now); intent != nil {
		mon.fireAutoCutover(*intent)
	}
}

// TestAutoCutover_CleanFlip_CutsOverOnce is the happy path AND default-on: with a
// changeover in progress, a live CATID that leaves the active style and holds
// across >=3 reads spanning >=60s presses CUTOVER exactly once.
func TestAutoCutover_CleanFlip_CutsOverOnce(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoCutover(t, "", "40016911", "50029999", true)

	if got := mon.eng.processChangeoverAutoArm(processID); got != "auto" {
		t.Fatalf("default changeover_auto_arm = %q, want auto (default-on everywhere)", got)
	}

	var notes []CATIDAutoCutoverEvent
	mon.eng.Events.SubscribeTypes(func(evt Event) {
		if n, ok := evt.Payload.(CATIDAutoCutoverEvent); ok {
			notes = append(notes, n)
		}
	}, EventCATIDAutoCutover)

	t0 := time.Unix(0, 0)
	tickAutoCutover(mon, processID, st, "50029999", true, t0)
	if len(*calls) != 0 {
		t.Fatalf("cut over after 1 read, want 0")
	}
	tickAutoCutover(mon, processID, st, "50029999", true, t0.Add(30*time.Second))
	if len(*calls) != 0 {
		t.Fatalf("cut over after 2 reads/30s (< window), want 0")
	}
	tickAutoCutover(mon, processID, st, "50029999", true, t0.Add(60*time.Second))
	if len(*calls) != 1 {
		t.Fatalf("want exactly 1 cutover at 3 reads/60s, got %d", len(*calls))
	}
	if c := (*calls)[0]; c.processID != processID || c.triggeredBy != "auto-catid" {
		t.Errorf("cutover = %+v, want process %d triggeredBy=auto-catid", c, processID)
	}
	if len(notes) != 1 || notes[0].NewCATID != "50029999" {
		t.Errorf("station notification = %+v, want one for CATID 50029999", notes)
	}

	tickAutoCutover(mon, processID, st, "50029999", true, t0.Add(90*time.Second))
	tickAutoCutover(mon, processID, st, "50029999", true, t0.Add(120*time.Second))
	if len(*calls) != 1 {
		t.Fatalf("re-fired on a still-stable value: got %d, want 1", len(*calls))
	}
}

// TestAutoCutover_NoChangeoverInProgress_NeverFires is the regression pin for
// Hopkinsville 2026-07-28. The monitor must do NOTHING when no changeover is
// running — it previously STARTED one here, which dispatched four robots toward
// a style with zero available stock.
func TestAutoCutover_NoChangeoverInProgress_NeverFires(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoCutover(t, "auto", "40016911", "50029999", false)
	t0 := time.Unix(0, 0)
	for i := 0; i < 6; i++ {
		tickAutoCutover(mon, processID, st, "50029999", true, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("fired with no changeover in progress: got %d, want 0 (never start one)", len(*calls))
	}
}

// TestAutoCutover_StillOnActiveStylePart_NeverFires: while the press still reports
// a part belonging to the ACTIVE style it has not cut over, so neither do we.
func TestAutoCutover_StillOnActiveStylePart_NeverFires(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoCutover(t, "auto", "40016911", "50029999", true)
	t0 := time.Unix(0, 0)
	for i := 0; i < 6; i++ {
		tickAutoCutover(mon, processID, st, "40016911", true, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("cut over while the press was still on the active style's part: got %d, want 0", len(*calls))
	}
}

// TestAutoCutover_UnmappedValue_StillCutsOver pins a DELIBERATE behaviour change.
// The old auto-start path refused a part that mapped to no configured style,
// because it had to pick a target. Cutover has no target to pick: the changeover
// already knows where it is going, and the press has demonstrably left the old
// part. Completing is correct even for an unrecognised part — the post-cutover
// verification watch compares the live part against the new active style and
// raises the operator flag when they disagree.
func TestAutoCutover_UnmappedValue_StillCutsOver(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoCutover(t, "auto", "40016911", "50029999", true)
	t0 := time.Unix(0, 0)
	for i := 0; i < 3; i++ {
		tickAutoCutover(mon, processID, st, "12345678", true, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 1 {
		t.Fatalf("an unmapped part that left the active style must still cut over: got %d, want 1", len(*calls))
	}
}

// TestAutoCutover_BouncingTag_NeverFires: a value that changes on every read never
// accumulates the required stability, even over minutes.
func TestAutoCutover_BouncingTag_NeverFires(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoCutover(t, "auto", "40016911", "50029999", true)
	t0 := time.Unix(0, 0)
	values := []string{"50029999", "77777777", "50029999", "88888888", "50029999", "99999999"}
	for i, v := range values {
		tickAutoCutover(mon, processID, st, v, true, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("a bouncing tag fired %d time(s), want 0", len(*calls))
	}
}

// TestAutoCutover_ZeroAndUnreadable_NeverFire: a stable zero never fires, and
// unreadable reads reset the tracker (the last-good value survives for the guard,
// but the window restarts).
func TestAutoCutover_ZeroAndUnreadable_NeverFire(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoCutover(t, "auto", "40016911", "50029999", true)
	t0 := time.Unix(0, 0)
	for i := 0; i < 5; i++ {
		tickAutoCutover(mon, processID, st, "0", true, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("a zero value fired %d time(s), want 0", len(*calls))
	}
	st.lastConfirmed = "50029999"
	for i := 5; i < 10; i++ {
		tickAutoCutover(mon, processID, st, "", false, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("unreadable reads fired %d time(s), want 0", len(*calls))
	}
}

// TestAutoCutover_DoubleFlipWithinWindow_NeverFires: A→B→A→B within the settle
// window never fires; each flip restarts the window. It fires only once B truly
// holds for the full window after the LAST flip.
func TestAutoCutover_DoubleFlipWithinWindow_NeverFires(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoCutover(t, "auto", "40016911", "50029999", true)
	t0 := time.Unix(0, 0)
	tickAutoCutover(mon, processID, st, "50029999", true, t0)
	tickAutoCutover(mon, processID, st, "50029999", true, t0.Add(20*time.Second))
	tickAutoCutover(mon, processID, st, "40016911", true, t0.Add(30*time.Second))
	tickAutoCutover(mon, processID, st, "50029999", true, t0.Add(45*time.Second))
	tickAutoCutover(mon, processID, st, "50029999", true, t0.Add(75*time.Second))
	if len(*calls) != 0 {
		t.Fatalf("a double-flip fired within the window: got %d, want 0", len(*calls))
	}
	tickAutoCutover(mon, processID, st, "50029999", true, t0.Add(105*time.Second))
	if len(*calls) != 1 {
		t.Fatalf("B stable 60s after the last flip must fire once, got %d", len(*calls))
	}
}

// TestAutoCutover_PromptMode_NeverFires: in 'prompt' mode the monitor never acts
// on its own — the operator is prompted instead.
func TestAutoCutover_PromptMode_NeverFires(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoCutover(t, "prompt", "40016911", "50029999", true)
	t0 := time.Unix(0, 0)
	for i := 0; i < 4; i++ {
		tickAutoCutover(mon, processID, st, "50029999", true, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("prompt mode cut over %d time(s), want 0", len(*calls))
	}
}

// TestAutoCutover_OffMode_Silent: in 'off' mode a stable new part neither cuts
// over nor prompts.
func TestAutoCutover_OffMode_Silent(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoCutover(t, "off", "40016911", "50029999", true)
	t0 := time.Unix(0, 0)
	for i := 0; i < 4; i++ {
		tickAutoCutover(mon, processID, st, "50029999", true, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("off mode cut over %d time(s), want 0", len(*calls))
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
