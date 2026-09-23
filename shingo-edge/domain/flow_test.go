package domain

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"shingo/protocol"
)

// flow_test.go — the composer's cell shape and the round-trip law.
//
// A FlowCell is what the HMI composer authors; a NodeClaim is what the store
// holds. Collapse and Expand translate between them, and the law that makes
// the composer safe to point at a real plant row is that the translation
// loses nothing: Expand(Collapse(c), &c) writes c back, and Collapse reads an
// unlocked cell out of what Expand wrote. A cell the composer cannot express
// in full is LOCKED, and a locked cell expands to its prior claim untouched.

// fullClaim is a claim carrying a value in every column the composer authors
// and nothing in any column it does not — the shape an unlocked cell
// round-trips through.
func fullClaim(mode protocol.SwapMode, role protocol.ClaimRole) NodeClaim {
	return NodeClaim{
		ID: 41, StyleID: 7, CoreNodeName: "PLN_01", Role: role, SwapMode: mode,
		PayloadCode: "SYN-A-P002", PairedCoreNode: "PLN_02", SecondPairedCoreNode: "PLN_03",
		InboundSource: "Supermarket Empty Totes", InboundStaging: "PLN_02", OutboundStaging: "PLN_05",
		OutboundDestination: "Supermarket Area", ChangeoverEvacDestination: "Supermarket Area",
		ChangeoverEvacNodes: []string{"PLN_01", "PLN_02"}, KeyRoute: []string{"LM314", "LM315"},
		ReorderPointSource: "legacy", ChangeoverCarryoverDisposition: CarryoverReplace,
		Sequence: 3, Source: ClaimSourceAdmin, CalledBy: "engineer",
	}
}

// claimColumnsEqual compares two claims on every column the INSERT writes —
// which is every field but the timestamps and the runtime falling edge.
func claimColumnsEqual(t *testing.T, label string, got, want NodeClaim) {
	t.Helper()
	got.CreatedAt = want.CreatedAt
	got.UpdatedAt, want.UpdatedAt = nil, nil
	got.RetiredAt, want.RetiredAt = nil, nil
	got.BelowReorderSince, want.BelowReorderSince = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got  %+v\n want %+v", label, got, want)
	}
}

func TestFlowCollapse_CellKeysAreThePresetKeys(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(Collapse(fullClaim(protocol.SwapModeTwoRobotPressIndex, protocol.ClaimRoleProduce)))
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(keys))
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	want := append([]string{}, flowPresetNodeKeys...)
	want = append(want, flowPresetNodeListKeys...)
	want = append(want, "role", "swap_mode", "payload_code", "key_route")
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cell JSON keys = %v, want exactly the preset keys plus role/swap_mode/payload_code/key_route = %v", got, want)
	}
	// A preset is a flow with the parts blank: the payload key the preset
	// validator refuses must be the one the cell writes.
	if _, ok := keys["payload_code"]; !ok {
		t.Error("the cell has no payload_code key; ValidateFlowPreset refuses payload under that name")
	}
}

// TestFlowRoundTrip_ExpandCollapseWritesTheClaimBack: Expand(Collapse(c), &c)
// == c on every column the INSERT writes, for every configurable mode and
// both roles, on a claim the composer can express in full.
func TestFlowRoundTrip_ExpandCollapseWritesTheClaimBack(t *testing.T) {
	t.Parallel()
	for _, mode := range append(protocol.ConfigurableSwapModes(), protocol.SwapModeSimple) {
		for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
			c := fullClaim(mode, role)
			if mode == protocol.SwapModeManualSwap {
				c.AutoConfirm = true // the store forces it; it is the mode's default
			}
			cell := Collapse(c)
			in := Expand(cell, &c, ClaimSourceHMI, "Press 400")
			want := c
			want.Source, want.CalledBy = ClaimSourceHMI, "Press 400"
			claimColumnsEqual(t, string(mode)+"/"+string(role), MaterializeClaim(in, &c), want)
			if in.Source != ClaimSourceHMI || in.CalledBy != "Press 400" {
				t.Errorf("%s/%s: attribution not stamped: %q/%q", mode, role, in.Source, in.CalledBy)
			}
		}
	}
}

// TestFlowRoundTrip_CollapseReadsTheCellOutOfAnInsert:
// Collapse(MaterializeClaim(Expand(cell, nil, ...), nil)) == cell for an unlocked cell — a
// brand-new claim written from a cell reads back as that cell.
func TestFlowRoundTrip_CollapseReadsTheCellOutOfAnInsert(t *testing.T) {
	t.Parallel()
	for _, mode := range protocol.ConfigurableSwapModes() {
		for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
			cell := Collapse(fullClaim(mode, role))
			in := Expand(cell, nil, ClaimSourceHMI, "Press 400")
			in.StyleID = 9
			back := Collapse(MaterializeClaim(in, nil))
			if !reflect.DeepEqual(back, cell) {
				t.Errorf("%s/%s: cell did not survive an insert:\n got  %+v\n want %+v", mode, role, back, cell)
			}
		}
	}
	// And a cell with nothing but a position, a mode and a part.
	bare := FlowCell{CoreNodeName: "PLN_03", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "P"}
	back := Collapse(MaterializeClaim(Expand(bare, nil, ClaimSourceHMI, "s"), nil))
	if !reflect.DeepEqual(back, bare) {
		t.Errorf("bare cell did not survive an insert:\n got  %+v\n want %+v", back, bare)
	}
}

// TestFlowExpand_HonoursTheUpdateBoundary: the eighteen unconditional columns
// come from the cell where it has a field and from the prior where it does
// not (INSERT defaults with no prior); the pointer-gated columns the composer
// does not author stay nil, so an update leaves them alone. The three it does
// author (evac nodes, evac destination, key route) are spoken.
func TestFlowExpand_HonoursTheUpdateBoundary(t *testing.T) {
	t.Parallel()
	prior := fullClaim(protocol.SwapModeTwoRobot, protocol.ClaimRoleConsume)
	prior.UOPCapacity, prior.ReorderPoint, prior.LinesideSoftThreshold = 1420, 100, 7
	prior.AllowedPayloadCodes = []string{"P1", "P2"}
	prior.AutoRequestPayload, prior.EvacuateOnChangeover, prior.AutoConfirm = "P1", true, true
	prior.ReuseCompatibleBins, prior.AutoPush = true, true
	cell := FlowCell{CoreNodeName: "PLN_01", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "NEW",
		InboundStaging: "S", InboundSource: "SRC", OutboundDestination: "D", KeyRoute: []string{"LM1"}}
	in := Expand(cell, &prior, ClaimSourceHMI, "Press 400")

	// From the cell.
	if in.PayloadCode != "NEW" || in.InboundStaging != "S" || in.PairedCoreNode != "" || in.OutboundStaging != "" {
		t.Errorf("cell fields not taken from the cell: %+v", in)
	}
	// From the prior: the unconditional columns the cell has no field for.
	// UOPCapacity is NOT among them and must not be: the store does not write
	// the column, so carrying it forward was echoing a number nothing reads.
	if in.UOPCapacity != 0 {
		t.Errorf("Expand carried uop_capacity (%d) from the prior; the store does not write it", in.UOPCapacity)
	}
	if in.ReorderPoint != 100 || in.LinesideSoftThreshold != 7 ||
		!reflect.DeepEqual(in.AllowedPayloadCodes, []string{"P1", "P2"}) || in.AutoRequestPayload != "P1" ||
		!in.EvacuateOnChangeover || !in.AutoConfirm || !in.ReuseCompatibleBins || !in.AutoPush {
		t.Errorf("unconditional columns not carried from the prior: %+v", in)
	}
	// Spoken: the three pointer-gated fields the cell carries.
	if in.KeyRoute == nil || !reflect.DeepEqual(*in.KeyRoute, []string{"LM1"}) {
		t.Errorf("key_route not spoken: %v", in.KeyRoute)
	}
	if in.ChangeoverEvacNodes == nil || len(*in.ChangeoverEvacNodes) != 0 {
		t.Errorf("changeover_evac_nodes must be spoken as empty for a cell that marks nothing: %v", in.ChangeoverEvacNodes)
	}
	if in.ChangeoverEvacDestination == nil || *in.ChangeoverEvacDestination != "" {
		t.Errorf("changeover_evac_destination must be spoken as blank: %v", in.ChangeoverEvacDestination)
	}
	// Silent: the nine the composer never speaks about.
	if in.ReorderPointSource != nil || in.AutoReorder != nil || in.KeepStaged != nil || in.Sequence != nil ||
		in.IndexRobotSupplies != nil || in.ChangeoverCarryoverDisposition != nil || in.KeyTask != nil ||
		in.SourcePresetID != nil || in.SourcePresetVersion != nil {
		t.Errorf("a pointer-gated column the composer does not author was spoken: %+v", in)
	}

	// No prior: the INSERT defaults.
	fresh := Expand(cell, nil, ClaimSourceHMI, "Press 400")
	if fresh.UOPCapacity != 0 || fresh.ReorderPoint != 0 || fresh.AllowedPayloadCodes != nil || fresh.AutoRequestPayload != "" ||
		fresh.EvacuateOnChangeover || fresh.AutoConfirm || fresh.LinesideSoftThreshold != 0 || fresh.ReuseCompatibleBins || fresh.AutoPush {
		t.Errorf("with no prior the unconditional columns must be the INSERT defaults: %+v", fresh)
	}
}

// TestFlowExpand_EditsLandAndTheEngineersColumnsSurvive replaces
// TestFlowExpand_LockedCellIsThePriorRestamped.
//
// That test pinned the old refusal: a claim carrying keep_staged /
// index_robot_supplies / key_task / a reorder point collapsed LOCKED, and
// Expand threw the cell away and re-stamped the prior — so an operator editing
// such a cell changed nothing at all. Both halves of that are now wrong, and
// what replaces them is the guarantee the refusal was standing in front of: the
// edit lands, and every column the cell does not carry comes back untouched.
func TestFlowExpand_EditsLandAndTheEngineersColumnsSurvive(t *testing.T) {
	t.Parallel()
	prior := fullClaim(protocol.SwapModeTwoRobotPressIndex, protocol.ClaimRoleProduce)
	prior.KeepStaged, prior.IndexRobotSupplies, prior.KeyTask = true, true, "load"
	prior.ReorderPoint, prior.ReorderPointSource, prior.AutoReorder = 60, "manual", true

	cell := Collapse(prior)
	cell.PayloadCode, cell.InboundSource = "PART-NEW", "ELSEWHERE"
	in := Expand(cell, &prior, ClaimSourceHMI, "Press 400")

	if in.PayloadCode != "PART-NEW" || in.InboundSource != "ELSEWHERE" {
		t.Errorf("the operator's edit did not land: payload %q, inbound source %q", in.PayloadCode, in.InboundSource)
	}
	// Everything the cell does not carry, exactly as the engineer left it.
	//
	// Read off the ROW, not the input: the two mechanisms are different and
	// only one of them puts a value on the input. Expand copies the
	// unconditional columns from the prior (reorder_point below), and leaves
	// the pointer-gated ones nil so updateClaim does not name them at all
	// (keep_staged, index_robot_supplies, key_task, reorder_point_source,
	// auto_reorder). MaterializeClaim models both.
	got := MaterializeClaim(in, &prior)
	if !got.KeepStaged || !got.IndexRobotSupplies || got.KeyTask != "load" {
		t.Errorf("a changeover special did not survive the edit: keep_staged %v, index_robot_supplies %v, key_task %q",
			got.KeepStaged, got.IndexRobotSupplies, got.KeyTask)
	}
	if got.ReorderPoint != 60 || got.ReorderPointSource != "manual" || !got.AutoReorder {
		t.Errorf("the replenishment policy did not survive the edit: %d / %q / %v",
			got.ReorderPoint, got.ReorderPointSource, got.AutoReorder)
	}

	// A cell with no prior has nothing to carry and expands as the cell says.
	fresh := Expand(FlowCell{CoreNodeName: "X", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "P"}, nil, ClaimSourceHMI, "s")
	if fresh.PayloadCode != "P" || fresh.CoreNodeName != "X" {
		t.Errorf("a cell with no prior did not expand as the cell: %+v", fresh)
	}
}

// TestFlowExpand_DegenerateAllowedListFollowsThePayload: a prior whose
// allowed list is exactly its one payload is not locked, and when the cell
// changes the part the list follows it rather than pinning the old one.
func TestFlowExpand_DegenerateAllowedListFollowsThePayload(t *testing.T) {
	t.Parallel()
	prior := fullClaim(protocol.SwapModeTwoRobot, protocol.ClaimRoleConsume)
	prior.AllowedPayloadCodes = []string{prior.PayloadCode}
	cell := Collapse(prior)
	cell.PayloadCode = "NEW-PART"
	in := Expand(cell, &prior, ClaimSourceHMI, "s")
	if !reflect.DeepEqual(in.AllowedPayloadCodes, []string{"NEW-PART"}) {
		t.Errorf("allowed list = %v, want [NEW-PART] — the degenerate list follows the part", in.AllowedPayloadCodes)
	}
	same := Expand(Collapse(prior), &prior, ClaimSourceHMI, "s")
	if !reflect.DeepEqual(same.AllowedPayloadCodes, []string{prior.PayloadCode}) {
		t.Errorf("unchanged part: allowed list = %v, want the prior's", same.AllowedPayloadCodes)
	}
}

// TestFlowInputFromClaim_IsTotal: InputFromClaim speaks every column,
// including the pointer-gated ones, so a stored claim can be validated or
// rewritten in full.
func TestFlowInputFromClaim_IsTotal(t *testing.T) {
	t.Parallel()
	c := fullClaim(protocol.SwapModeTwoRobotPressIndex, protocol.ClaimRoleProduce)
	c.KeepStaged, c.AutoReorder, c.IndexRobotSupplies, c.KeyTask = true, true, true, "load"
	c.ReorderPointSource, c.ChangeoverCarryoverDisposition = "manual", CarryoverKeepLineside
	pid, pv := int64(3), 2
	c.SourcePresetID, c.SourcePresetVersion = &pid, &pv
	in := InputFromClaim(c)
	claimColumnsEqual(t, "InputFromClaim", MaterializeClaim(in, &c), c)
	if in.KeepStaged == nil || in.AutoReorder == nil || in.IndexRobotSupplies == nil || in.KeyTask == nil ||
		in.ReorderPointSource == nil || in.ChangeoverCarryoverDisposition == nil || in.Sequence == nil ||
		in.SourcePresetID == nil || in.SourcePresetVersion == nil {
		t.Errorf("InputFromClaim left a pointer-gated column silent: %+v", in)
	}
}
