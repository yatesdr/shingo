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

// TestCATIDChangePrompt_PromptsOnChange pins the B1 prompt-arm half: a confirmed
// CATID CHANGE prompts the operator to start a changeover, pre-filling the target
// when the new part maps to a known style — but never on the baseline read, never
// when the new part already matches the active style, and NEVER auto-starting a
// changeover.
func TestCATIDChangePrompt_PromptsOnChange(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, styleID, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

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
