package sourceability

import (
	"reflect"
	"testing"
	"time"

	"shingocore/store/plantclaims"
)

// pure fixtures — no DB, so this file has no build tag and runs everywhere.

func key(p, s string) plantclaims.ProcessKey { return plantclaims.ProcessKey{ProcessID: p, StyleID: s} }

func claim(node, payload string, seq int, allowed ...string) plantclaims.ClaimRow {
	return plantclaims.ClaimRow{CoreNodeName: node, PayloadCode: payload, AllowedPayloadCodes: allowed, Seq: seq}
}

// byKey indexes the result for order-independent assertions.
func byKey(states []StyleState) map[plantclaims.ProcessKey]StyleState {
	m := make(map[plantclaims.ProcessKey]StyleState, len(states))
	for _, s := range states {
		m[key(s.ProcessID, s.StyleID)] = s
	}
	return m
}

var now = time.Unix(1_700_000_000, 0)

func TestCompute_GreenWhenEverySatisfiable(t *testing.T) {
	k := key("SNF2", "A")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{k},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			k: {claim("N1", "BIN-A", 0), claim("N2", "BIN-B", 1)},
		},
		Pool: map[string]int{"BIN-A": 3, "BIN-B": 1},
	}
	got := byKey(Compute(in, Config{}, now))[k]
	if got.Status != StatusGreen {
		t.Fatalf("status = %q, want green (%+v)", got.Status, got)
	}
	if len(got.Missing) != 0 {
		t.Errorf("missing = %v, want none", got.Missing)
	}
}

func TestCompute_RedListsMissingPayloads(t *testing.T) {
	k := key("SNF2", "A")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{k},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			k: {claim("N1", "BIN-A", 0), claim("N2", "BIN-B", 1), claim("N3", "BIN-C", 2)},
		},
		Pool: map[string]int{"BIN-A": 1}, // B and C absent
	}
	got := byKey(Compute(in, Config{}, now))[k]
	if got.Status != StatusRed {
		t.Fatalf("status = %q, want red", got.Status)
	}
	if !reflect.DeepEqual(got.Missing, []string{"BIN-B", "BIN-C"}) {
		t.Errorf("missing = %v, want [BIN-B BIN-C] (sorted)", got.Missing)
	}
}

func TestCompute_ContentionNetsThePool(t *testing.T) {
	// Two claims need BIN-A but only one is available → the second is unsatisfiable.
	k := key("SNF2", "A")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{k},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			k: {claim("N1", "BIN-A", 0), claim("N2", "BIN-A", 1)},
		},
		Pool: map[string]int{"BIN-A": 1},
	}
	got := byKey(Compute(in, Config{}, now))[k]
	if got.Status != StatusRed {
		t.Fatalf("status = %q, want red (contention)", got.Status)
	}
	if !reflect.DeepEqual(got.Missing, []string{"BIN-A"}) {
		t.Errorf("missing = %v, want [BIN-A]", got.Missing)
	}
}

func TestCompute_AllowedSetFallbackSatisfies(t *testing.T) {
	// Primary BIN-A absent, but the claim allows BIN-A2 which is available.
	k := key("SNF2", "A")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{k},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			k: {claim("N1", "BIN-A", 0, "BIN-A2")},
		},
		Pool: map[string]int{"BIN-A2": 1},
	}
	got := byKey(Compute(in, Config{}, now))[k]
	if got.Status != StatusGreen {
		t.Fatalf("status = %q, want green (allowed fallback)", got.Status)
	}
}

// A style with no claims reports NOT_CONFIGURED, never green. It used to report
// green: with zero claims it satisfied every claim it had, fell through the
// missing check, and emerged as "can change over" — the system's strongest
// claim, derived from the complete absence of configuration.
func TestCompute_StyleWithNoClaimsIsNotConfigured(t *testing.T) {
	k := key("SNF2", "EMPTY")
	got := byKey(Compute(Inputs{Styles: []plantclaims.ProcessKey{k}}, Config{}, now))[k]
	if got.Status != StatusNotConfigured {
		t.Fatalf("status = %q, want not_configured (no claims)", got.Status)
	}
	if got.Status == StatusGreen {
		t.Fatal("an unconfigured style must never report green — it is unknown, not capable")
	}
	if len(got.Missing) != 0 {
		t.Errorf("Missing = %v, want empty — nothing is missing, nothing is configured", got.Missing)
	}
}

// Enabling the at-risk tier must not turn an unconfigured style into a verdict:
// there are no claims to project a time-to-empty for, so the gate is irrelevant
// and the status stays not_configured either way.
func TestCompute_NoClaimsStaysNotConfiguredWithYellowEnabled(t *testing.T) {
	k := key("SNF2", "EMPTY")
	cfg := Config{YellowEnabled: true, Horizon: time.Hour}
	got := byKey(Compute(Inputs{Styles: []plantclaims.ProcessKey{k}}, cfg, now))[k]
	if got.Status != StatusNotConfigured {
		t.Fatalf("status = %q, want not_configured regardless of the at-risk gate", got.Status)
	}
	if len(got.AtRisk) != 0 {
		t.Errorf("AtRisk = %v, want empty — no claims means no lines to project", got.AtRisk)
	}
}

func TestCompute_YellowGatedAtOutput(t *testing.T) {
	// A line projects empty within the horizon. The gate is at the OUTPUT: when
	// yellow is disabled the style reports GREEN with at_risk OMITTED (not merely
	// downgraded-but-populated), so every reader sees the same gated result.
	k := key("SNF2", "A")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{k},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			k: {claim("N1", "BIN-A", 0)},
		},
		Pool:       map[string]int{"BIN-A": 1},
		LineUOP:    map[string]int{"N1": 10},         // 10 UOP at the line
		RatePerSec: map[string]float64{"BIN-A": 1.0}, // 1 UOP/sec → 10s to empty
	}
	cfg := Config{YellowEnabled: false, Horizon: time.Minute}

	got := byKey(Compute(in, cfg, now))[k]
	if got.Status != StatusGreen {
		t.Fatalf("status = %q, want green (yellow dark)", got.Status)
	}
	if len(got.AtRisk) != 0 {
		t.Fatalf("at-risk = %+v, want omitted when the tier is dark", got.AtRisk)
	}

	// Same inputs, yellow enabled → the style now reports YELLOW with the line.
	cfg.YellowEnabled = true
	got = byKey(Compute(in, cfg, now))[k]
	if got.Status != StatusYellow {
		t.Fatalf("status = %q, want yellow (enabled)", got.Status)
	}
	if len(got.AtRisk) != 1 || got.AtRisk[0].NodeName != "N1" {
		t.Fatalf("at-risk = %+v, want the line surfaced when enabled", got.AtRisk)
	}
	if !got.AtRisk[0].Known || got.AtRisk[0].TimeToEmpty != 10*time.Second {
		t.Errorf("TTE = %v, want 10s", got.AtRisk[0].TimeToEmpty)
	}
}

func TestCompute_HealthyLineNotAtRisk(t *testing.T) {
	// Plenty of UOP relative to the rate → TTE beyond the horizon → not at risk.
	k := key("SNF2", "A")
	in := Inputs{
		Styles:     []plantclaims.ProcessKey{k},
		Claims:     map[plantclaims.ProcessKey][]plantclaims.ClaimRow{k: {claim("N1", "BIN-A", 0)}},
		Pool:       map[string]int{"BIN-A": 1},
		LineUOP:    map[string]int{"N1": 10_000},
		RatePerSec: map[string]float64{"BIN-A": 1.0},
	}
	cfg := Config{YellowEnabled: true, Horizon: time.Minute}
	got := byKey(Compute(in, cfg, now))[k]
	if got.Status != StatusGreen {
		t.Fatalf("status = %q, want green (healthy line)", got.Status)
	}
	if len(got.AtRisk) != 0 {
		t.Errorf("at-risk = %+v, want none", got.AtRisk)
	}
}

// ── in-process staged stock (SPR 2026-07-29) ─────────────────────────────────
//
// A bin standing at the consuming node satisfies the claim even though dispatch
// cannot fetch it. Before this, "the parts are at the line" and "the parts do not
// exist" both rendered as RED / "no available bin in Shingo" — said about
// CARRIER-0010 sitting at ALN_007 holding the payload the claim named.

func TestCompute_StagedStockInSameProcessSatisfies(t *testing.T) {
	k := key("SNF4", "SYN-PART04B.95")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{k},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			k: {claim("ALN_007", "SYN-PART01E.06", 0)},
		},
		// Nothing fetchable anywhere — the only bin is staged at the claim's node.
		Pool:   map[string]int{},
		OnLine: map[string]map[string]int{"SNF4": {"SYN-PART01E.06": 1}},
	}
	got := byKey(Compute(in, Config{}, now))[k]
	if got.Status != StatusGreen {
		t.Fatalf("status = %q, want green — the bin is already at the line (%+v)", got.Status, got)
	}
}

// THE SCOPING RULE, and the one that fails dangerously: leak it and a staged bin
// anywhere in the plant turns every process's claim for that payload green.
func TestCompute_StagedStockInAnotherProcessDoesNotSatisfy(t *testing.T) {
	k := key("SNF4", "SYN-PART04B.95")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{k},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			k: {claim("ALN_007", "SYN-PART01E.06", 0)},
		},
		Pool:   map[string]int{},
		OnLine: map[string]map[string]int{"SNF2": {"SYN-PART01E.06": 1}},
	}
	got := byKey(Compute(in, Config{}, now))[k]
	if got.Status != StatusRed {
		t.Fatalf("status = %q, want red — the staged bin belongs to another process", got.Status)
	}
	if !reflect.DeepEqual(got.Missing, []string{"SYN-PART01E.06"}) {
		t.Errorf("missing = %v, want the unsatisfied payload", got.Missing)
	}
}

// Both pools count, and neither is double-spent: two claims for one payload with
// one staged bin and one fetchable bin are both satisfiable — but a third would
// not be.
func TestCompute_StagedAndFetchableBothCountOnce(t *testing.T) {
	k2, k3 := key("SNF4", "two"), key("SNF4", "three")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{k2, k3},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			k2: {claim("ALN_006", "P", 0), claim("ALN_007", "P", 1)},
			k3: {claim("ALN_006", "P", 0), claim("ALN_007", "P", 1), claim("ALN_008", "P", 2)},
		},
		Pool:   map[string]int{"P": 1},
		OnLine: map[string]map[string]int{"SNF4": {"P": 1}},
	}
	got := byKey(Compute(in, Config{}, now))
	if got[k2].Status != StatusGreen {
		t.Errorf("two claims / two bins: status = %q, want green", got[k2].Status)
	}
	if got[k3].Status != StatusRed {
		t.Errorf("three claims / two bins: status = %q, want red", got[k3].Status)
	}
}

// ── The carrier-rule finding ────────────────────────────────────────────────
//
// Bins standing in a carrier their payload is not declared to travel in COUNT
// as stock and are refused by every sourcing reader, so they contribute nothing
// to Pool and the style goes RED on a payload the plant is holding. Compute
// carries the finding alongside Missing so the operator sentence can say which
// of the two problems this is.

// TestCompute_FindingIsScopedToTheMissingSet. A payload with flagged stock that
// is nonetheless satisfiable has a shortage nobody is waiting on. Naming it in
// a changeover verdict would put a maintenance job in front of an operator
// trying to change over; /material-flags and /inventory own that reading.
func TestCompute_FindingIsScopedToTheMissingSet(t *testing.T) {
	k := key("SNF2", "A")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{k},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			k: {claim("N1", "BIN-A", 0), claim("N2", "BIN-B", 1)},
		},
		// BIN-B is satisfiable and ALSO has a flagged carrier somewhere.
		Pool:              map[string]int{"BIN-A": 0, "BIN-B": 2},
		UndeclaredCarrier: map[string]int{"BIN-A": 3, "BIN-B": 1},
	}
	got := byKey(Compute(in, Config{}, now))[k]
	if got.Status != StatusRed {
		t.Fatalf("status = %q, want red", got.Status)
	}
	want := []PayloadCount{{PayloadCode: "BIN-A", Bins: 3}}
	if !reflect.DeepEqual(got.UndeclaredCarriers, want) {
		t.Errorf("UndeclaredCarriers = %+v, want %+v.\nBIN-B is satisfiable — its "+
			"flagged carrier is a maintenance job, not a changeover blocker.",
			got.UndeclaredCarriers, want)
	}
	// THE PAYLOAD STAYS MISSING. It is unsourceable; the finding says why, it
	// does not excuse it, and Missing is what every reader of the wire indexes
	// blocked changeovers by.
	if len(got.Missing) != 1 || got.Missing[0] != "BIN-A" {
		t.Errorf("missing = %v, want [BIN-A]", got.Missing)
	}
}

// TestCompute_NoFindingsLeavesTheSliceNil is the quiet arm. payload_bin_types
// is sparsely populated and the ordinary plant flags nothing; a slice that came
// back empty-but-non-nil would still make wireChanged and the sentence treat
// "nothing flagged" as a state worth reporting.
func TestCompute_NoFindingsLeavesTheSliceNil(t *testing.T) {
	k := key("SNF2", "A")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{k},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{k: {claim("N1", "BIN-A", 0)}},
		Pool:   map[string]int{"BIN-A": 0},
	}
	got := byKey(Compute(in, Config{}, now))[k]
	if got.Status != StatusRed {
		t.Fatalf("status = %q, want red", got.Status)
	}
	if got.UndeclaredCarriers != nil {
		t.Errorf("UndeclaredCarriers = %+v on a plant with no findings, want nil",
			got.UndeclaredCarriers)
	}
}

// TestCompute_AZeroCountIsNotAFinding. availablePoolByPayload leaves a payload
// OUT of the map rather than storing a zero, and this is the assertion that a
// zero arriving by any other route still says nothing: "0 bins of BIN-A in
// undeclared carriers" is a sentence that would be printed to an operator.
func TestCompute_AZeroCountIsNotAFinding(t *testing.T) {
	k := key("SNF2", "A")
	in := Inputs{
		Styles:            []plantclaims.ProcessKey{k},
		Claims:            map[plantclaims.ProcessKey][]plantclaims.ClaimRow{k: {claim("N1", "BIN-A", 0)}},
		Pool:              map[string]int{"BIN-A": 0},
		UndeclaredCarrier: map[string]int{"BIN-A": 0},
	}
	got := byKey(Compute(in, Config{}, now))[k]
	if len(got.UndeclaredCarriers) != 0 {
		t.Errorf("UndeclaredCarriers = %+v for a zero count, want none", got.UndeclaredCarriers)
	}
}
