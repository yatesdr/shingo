package flowspec_test

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

func steadyModes() []protocol.SwapMode { return protocol.AllSwapModes() }

// TestFlowspecTablesAreTotal: every (role, mode) has an answer for every field,
// and no answer is the zero value. A consumer reading Unspecified has asked
// about a field the table does not know — and a map lookup would hand it back
// silently, which is why totality is asserted rather than assumed.
func TestFlowspecTablesAreTotal(t *testing.T) {
	t.Parallel()
	fields := flowspec.Fields()
	known := map[flowspec.Field]bool{}
	for _, f := range fields {
		known[f] = true
	}
	for _, role := range flowspec.Roles() {
		for _, mode := range steadyModes() {
			row := flowspec.Steady(role, mode)
			for _, f := range fields {
				if row[f] == flowspec.Unspecified {
					t.Errorf("Steady(%s, %s)[%s] is Unspecified", role, mode, f)
				}
			}
			for f := range row {
				if !known[f] {
					t.Errorf("Steady(%s, %s) carries %q, which Fields() does not list", role, mode, f)
				}
			}
			if len(row) != len(fields) {
				t.Errorf("Steady(%s, %s) has %d entries, Fields() has %d", role, mode, len(row), len(fields))
			}
		}
	}
	for _, mode := range flowspec.ChangeoverModes() {
		row := flowspec.Changeover(mode)
		if row == nil {
			t.Errorf("Changeover(%s) is nil; an empty table is a non-nil empty map", mode)
		}
		for sf, n := range row {
			if !known[sf.Field] {
				t.Errorf("Changeover(%s) reads %q, which Fields() does not list", mode, sf.Field)
			}
			if sf.Side != flowspec.SideFrom && sf.Side != flowspec.SideTo {
				t.Errorf("Changeover(%s) has side %q", mode, sf.Side)
			}
			if n == flowspec.Unspecified {
				t.Errorf("Changeover(%s)[%v] is Unspecified", mode, sf)
			}
		}
	}
	for _, f := range fields {
		if flowspec.Label(f) == string(f) {
			t.Errorf("field %q has no label", f)
		}
	}
}

// TestFlowspecUnknownModeAnswersAsSimple pins the fallback: a mode with no
// row answers as the retired "simple" does, and Known says which it was.
func TestFlowspecUnknownModeAnswersAsSimple(t *testing.T) {
	t.Parallel()
	want := flowspec.Steady(protocol.ClaimRoleConsume, protocol.SwapModeSimple)
	for _, mode := range []protocol.SwapMode{"", "typo", flowspec.SwapModePressPosition} {
		if flowspec.Known(mode) {
			t.Errorf("Known(%q) = true", mode)
		}
		if got := flowspec.Steady(protocol.ClaimRoleConsume, mode); !reflect.DeepEqual(got, want) {
			t.Errorf("Steady(consume, %q) differs from the simple row", mode)
		}
	}
	for _, mode := range steadyModes() {
		if !flowspec.Known(mode) {
			t.Errorf("Known(%q) = false", mode)
		}
	}
	// The planner's fallback arm: an unknown outgoing mode shares
	// single_robot's changeover fields.
	sr := flowspec.Changeover(protocol.SwapModeSingleRobot)
	for _, mode := range []protocol.SwapMode{"", "typo", protocol.SwapModeSimple} {
		if got := flowspec.Changeover(mode); !reflect.DeepEqual(got, sr) {
			t.Errorf("Changeover(%q) differs from single_robot's table", mode)
		}
	}
}

// TestFlowspecRoleMovesExactlyTwoEntries: the role dimension is real but
// narrow. If a third role-dependent entry appears, this names it — a silent
// role difference is how "consume and produce show different fields" becomes
// folklore.
func TestFlowspecRoleMovesExactlyTwoEntries(t *testing.T) {
	t.Parallel()
	for _, mode := range steadyModes() {
		consume := flowspec.Steady(protocol.ClaimRoleConsume, mode)
		produce := flowspec.Steady(protocol.ClaimRoleProduce, mode)
		var differ []flowspec.Field
		for _, f := range flowspec.Fields() {
			if consume[f] != produce[f] {
				differ = append(differ, f)
			}
		}
		// manual_swap hides the lineside threshold for BOTH roles (the loader
		// board owns the numbers), so its one role-dependent entry is auto-push.
		want := []flowspec.Field{flowspec.LinesideSoftThreshold}
		if mode == protocol.SwapModeManualSwap {
			want = []flowspec.Field{flowspec.AutoPush}
		}
		if !reflect.DeepEqual(differ, want) {
			t.Errorf("%s: role changes %v, want %v", mode, differ, want)
		}
		if consume[flowspec.LinesideSoftThreshold] != flowspec.Used || produce[flowspec.LinesideSoftThreshold] != flowspec.Unused {
			if mode != protocol.SwapModeManualSwap {
				t.Errorf("%s: lineside is %v/%v (consume/produce), want Used/Unused", mode,
					consume[flowspec.LinesideSoftThreshold], produce[flowspec.LinesideSoftThreshold])
			}
		}
	}
	// A blank role reads as consume, as UpsertClaim stores it.
	if got, want := flowspec.Steady("", protocol.SwapModeTwoRobot), flowspec.Steady(protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot); !reflect.DeepEqual(got, want) {
		t.Errorf("Steady(\"\", two_robot) differs from consume")
	}
}

// TestFlowspecPinsKnownDisagreements asserts that the readers still disagree
// where they disagreed on 0f1c2c1f. EACH CASE MUST GO RED WHEN ITS
// DISAGREEMENT IS RESOLVED — resolving one is a behaviour change with its own
// commit, and that commit flips the case here. A pin that stayed green through
// the fix would have pinned nothing.
//
// D1, D2: the save path does not require an outbound destination for
// single_robot / two_robot; the planner requires it on the outgoing claim.
// (claim_validation.go validateSwapModeRouting vs changeover_planner.go
// requiredChangeoverFields.)
//
// D3: sequential. Characterised: NO field-set divergence. The save path
// requires paired position, outbound destination and inbound source on every
// claim; the planner requires the first two on the outgoing claim and the third
// on the incoming one. A unary validator cannot say which side, so requiring
// all three on every claim is the union and is correct. Pinned as an absence.
//
// D4 (UpsertClaim has no single_robot / sequential arm) is a store fact and is
// pinned in store/store_test.go as TestStoreStillAcceptsWhatFlowspecRefuses.
//
// D5: press-index staging. The planner reads the incoming claim's inbound
// staging for a staged tooling changeover and the editor SHOWS the fieldset,
// while claimForbiddenFields CLEARS both staging fields at save. Steady says
// Used; the editor's drop is pinned by this name in
// www/static/js/pages/composer-fields.characterization.test.js, which replaced
// processes.js's suite when U9d retired the claim modal.
//
// D6: manual_swap outbound destination. Required by the store and the
// validator, HIDDEN by the editor (the loader board owns it); the stored value
// round-trips through the hidden input.
func TestFlowspecPinsKnownDisagreements(t *testing.T) {
	t.Parallel()
	for _, role := range flowspec.Roles() {
		t.Run("D1_single_robot_outbound_destination_"+string(role), func(t *testing.T) {
			// RESOLVED: save requires it, as the planner always did.
			steady := flowspec.Steady(role, protocol.SwapModeSingleRobot)[flowspec.OutboundDestination]
			plan := flowspec.Changeover(protocol.SwapModeSingleRobot)[flowspec.SideField{Side: flowspec.SideFrom, Field: flowspec.OutboundDestination}]
			if steady != flowspec.Required {
				t.Errorf("D1 reopened: Steady(%s, single_robot)[outbound_destination] = %v, want Required", role, steady)
			}
			if plan != flowspec.Required {
				t.Errorf("D1 shape changed: Changeover(single_robot)[from outbound_destination] = %v, want Required", plan)
			}
		})
		t.Run("D2_two_robot_outbound_destination_"+string(role), func(t *testing.T) {
			// RESOLVED: save requires it, as the planner always did.
			steady := flowspec.Steady(role, protocol.SwapModeTwoRobot)[flowspec.OutboundDestination]
			plan := flowspec.Changeover(protocol.SwapModeTwoRobot)[flowspec.SideField{Side: flowspec.SideFrom, Field: flowspec.OutboundDestination}]
			if steady != flowspec.Required {
				t.Errorf("D2 reopened: Steady(%s, two_robot)[outbound_destination] = %v, want Required", role, steady)
			}
			if plan != flowspec.Required {
				t.Errorf("D2 shape changed: Changeover(two_robot)[from outbound_destination] = %v, want Required", plan)
			}
		})
	}
	t.Run("D3_sequential_has_no_field_set_divergence", func(t *testing.T) {
		save := flowspec.RequiredFields(flowspec.Steady(protocol.ClaimRoleConsume, protocol.SwapModeSequential))
		var saveRouting []flowspec.Field
		for _, f := range save {
			if f != flowspec.PayloadCode {
				saveRouting = append(saveRouting, f)
			}
		}
		planned := map[flowspec.Field]bool{}
		var plan []flowspec.Field
		for _, sf := range flowspec.RequiredSideFields(flowspec.Changeover(protocol.SwapModeSequential)) {
			planned[sf.Field] = true
			plan = append(plan, sf.Field)
		}
		wantRouting := []flowspec.Field{flowspec.PairedCoreNode, flowspec.OutboundDestination, flowspec.InboundSource}
		if !reflect.DeepEqual(saveRouting, wantRouting) {
			t.Errorf("save-time sequential routing requirements = %v, want %v", saveRouting, wantRouting)
		}
		if !reflect.DeepEqual(plan, wantRouting) {
			t.Errorf("plan-time sequential requirements = %v, want %v", plan, wantRouting)
		}
		for _, f := range saveRouting {
			if !planned[f] {
				t.Errorf("sequential: save requires %s, plan does not — D3 has grown a divergence", f)
			}
		}
	})
	t.Run("D5_press_index_staging_used_by_planner_shown_by_editor", func(t *testing.T) {
		row := flowspec.Steady(protocol.ClaimRoleProduce, protocol.SwapModeTwoRobotPressIndex)
		if row[flowspec.InboundStaging] != flowspec.Used || row[flowspec.OutboundStaging] != flowspec.Used {
			t.Errorf("Steady(press_index) staging = %v/%v, want Used/Used (the editor still clears both at save — see composer-fields.characterization.test.js D5)",
				row[flowspec.InboundStaging], row[flowspec.OutboundStaging])
		}
		if got := flowspec.Changeover(protocol.SwapModeTwoRobotPressIndex)[flowspec.SideField{Side: flowspec.SideTo, Field: flowspec.InboundStaging}]; got != flowspec.Used {
			t.Errorf("Changeover(press_index)[to inbound_staging] = %v, want Used", got)
		}
	})
	t.Run("D6_manual_swap_outbound_destination_required_but_hidden", func(t *testing.T) {
		if got := flowspec.Steady(protocol.ClaimRoleConsume, protocol.SwapModeManualSwap)[flowspec.OutboundDestination]; got != flowspec.Required {
			t.Errorf("Steady(manual_swap)[outbound_destination] = %v, want Required (the editor hides the group — see composer-fields.characterization.test.js D6)", got)
		}
	})
}

// TestFlowspecChangeoverOrderReproducesPlannerLists pins the diagnostic order
// requiredChangeoverFields produced before it consulted the table. The
// planner's error text is built from this list in this order; a reordering of
// Fields() would change what the operator reads.
func TestFlowspecChangeoverOrderReproducesPlannerLists(t *testing.T) {
	t.Parallel()
	from, to := flowspec.SideFrom, flowspec.SideTo
	sf := func(s flowspec.Side, f flowspec.Field) flowspec.SideField {
		return flowspec.SideField{Side: s, Field: f}
	}
	cases := map[protocol.SwapMode][]flowspec.SideField{
		protocol.SwapModeSingleRobot:        {sf(to, flowspec.InboundStaging), sf(from, flowspec.OutboundStaging), sf(from, flowspec.OutboundDestination)},
		protocol.SwapModeTwoRobot:           {sf(to, flowspec.InboundStaging), sf(from, flowspec.OutboundDestination)},
		protocol.SwapModeTwoRobotPressIndex: {sf(from, flowspec.PairedCoreNode), sf(from, flowspec.OutboundDestination)},
		flowspec.SwapModePressPosition:      {sf(from, flowspec.OutboundDestination), sf(to, flowspec.InboundSource)},
		protocol.SwapModeSequential:         {sf(from, flowspec.PairedCoreNode), sf(from, flowspec.OutboundDestination), sf(to, flowspec.InboundSource)},
		protocol.SwapModeManualSwap:         nil,
		protocol.SwapModeSimple:             {sf(to, flowspec.InboundStaging), sf(from, flowspec.OutboundStaging), sf(from, flowspec.OutboundDestination)},
	}
	for mode, want := range cases {
		got := flowspec.RequiredSideFields(flowspec.Changeover(mode))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("RequiredSideFields(Changeover(%s)) = %v, want %v", mode, got, want)
		}
	}
	// Every mode Changeover knows is covered above; a new arm needs a row here.
	for _, mode := range flowspec.ChangeoverModes() {
		if _, ok := cases[mode]; !ok {
			t.Errorf("ChangeoverModes() has %s and this test does not", mode)
		}
	}
}

// TestFlowspecRoutingFieldsOrder pins the save-time finding order the same
// way: single_robot reports inbound before outbound staging, sequential
// reports paired, destination, source.
func TestFlowspecRoutingFieldsOrder(t *testing.T) {
	t.Parallel()
	want := []flowspec.Field{flowspec.InboundStaging, flowspec.OutboundStaging, flowspec.PairedCoreNode, flowspec.OutboundDestination, flowspec.InboundSource}
	if got := flowspec.RoutingFields(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RoutingFields() = %v, want %v", got, want)
	}
	if got := flowspec.Fields()[:len(want)]; !reflect.DeepEqual(got, want) {
		t.Fatalf("Fields() does not open with the routing fields: %v", got)
	}
	seq := flowspec.RequiredFields(flowspec.Steady(protocol.ClaimRoleConsume, protocol.SwapModeSequential))
	if want := []flowspec.Field{flowspec.PairedCoreNode, flowspec.OutboundDestination, flowspec.InboundSource, flowspec.PayloadCode}; !reflect.DeepEqual(seq, want) {
		t.Errorf("RequiredFields(sequential) = %v, want %v", seq, want)
	}
}

// TestFlowspecExportIsDeterministic: render twice, assert equal, and assert
// the rendering never carries an Unspecified need. Random values, time and
// map ordering all fail the first assertion; a hole in the table fails the
// second.
func TestFlowspecExportIsDeterministic(t *testing.T) {
	t.Parallel()
	a, err := flowspec.ExportJSON()
	if err != nil {
		t.Fatal(err)
	}
	b, err := flowspec.ExportJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("two renderings differ")
	}
	if strings.Contains(string(a), `: ""`) {
		t.Error("export carries an Unspecified need (rendered as an empty string)")
	}
	if !bytes.HasSuffix(a, []byte("\n")) || bytes.Contains(a, []byte("\r")) {
		t.Error("export must end with one LF and carry no CR")
	}
}

// THERE IS NO flowspec.json ANY MORE. It was a committed rendering of these
// tables that nothing ran — the review-diff artefact the schema snapshot and
// the defaults snapshot are, and a good pattern — except that
// static/operator-station/flowspec-data.js is the SAME BYTES with a
// `window.FLOWSPEC =` in front, is committed for the same reason, and is also
// what both surfaces actually load. Two renderings of one table is the shape
// this whole round is about, and the one to keep is the one with a reader.
//
//	go test ./www -run TestStationFlowspecFileMatchesGo -update
//
// regenerates it, and TestStationFlowspecFileMatchesGo holds it to
// flowspec.ExportJSON byte for byte.

// TestFlowspecSaveCoversPlan is the property the whole exercise exists for:
// for every configurable mode, every field the changeover planner will
// require of a claim in that mode — on either side of the pair — is a field
// the save path requires of every claim in that mode. A claim that saves
// clean is a claim the planner will not refuse for a missing field after the
// operator presses START (the 2026-06-23 ALN_001 class).
//
// Held true by D1 and D2 for single_robot and two_robot, by construction for
// press-index and sequential, and vacuously for manual_swap, which has no
// changeover table. Going RED here means a mode's plan-time needs grew past
// its save-time rules — which is exactly the moment to add the rule, not the
// exception.
func TestFlowspecSaveCoversPlan(t *testing.T) {
	t.Parallel()
	for _, mode := range protocol.ConfigurableSwapModes() {
		for _, role := range flowspec.Roles() {
			save := flowspec.Steady(role, mode)
			for _, sf := range flowspec.RequiredSideFields(flowspec.Changeover(mode)) {
				if save[sf.Field] != flowspec.Required {
					t.Errorf("%s: the planner requires %s on the %s-claim; Steady(%s, %s)[%s] = %v, want Required",
						mode, sf.Field, sf.Side, role, mode, sf.Field, save[sf.Field])
				}
			}
		}
	}
}
