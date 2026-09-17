package domain

import (
	"encoding/json"
	"reflect"
	"testing"

	"shingo/protocol"
)

// flow_advanced_test.go — the columns the flow does not draw.
//
// The Advanced modal (U9b, spec §2 D2) is the desktop's home for the twelve
// claim columns the picture has no line for: the allowed payload list, the
// replenishment policy, the changeover specials the cell does not already
// carry, which robot of an index pair supplies, and auto-confirm.
//
// WHY THE CELL HAS TO CARRY THEM. Expand's carry-through reads the PRIOR
// CLAIM. That is exactly right for a column the composer never shows — the
// value survives an edit it knows nothing about — and exactly wrong for one it
// now does: a modal that changes the reorder point has no way to say so
// through a mechanism whose only input is the row already in the store. So the
// draft has to carry the change, and FlowCell.Advanced is where.
//
// It is a POINTER because "untouched" and "set to what it already was" are
// different sentences: nil means the composer has no opinion and Expand
// behaves exactly as it did before this field existed. That is the first pin
// below, and it is written mechanically — against Expand's own answer for a
// cell that does not carry the field — so it cannot drift as Expand grows.

// advancedPrior is a claim carrying a deliberate value in every column the
// Advanced modal owns, so a save that leaves the modal alone has something to
// lose and a save that uses it has something to overwrite.
func advancedPrior() NodeClaim {
	c := fullClaim(protocol.SwapModeTwoRobotPressIndex, protocol.ClaimRoleProduce)
	c.AllowedPayloadCodes = []string{"SYN-A-P002", "SYN-A-P003"}
	c.ReorderPoint, c.ReorderPointSource, c.AutoReorder = 60, "manual", true
	c.LinesideSoftThreshold = 7
	c.AutoRequestPayload = "SYN-A-P002"
	c.AutoPush = true
	c.EvacuateOnChangeover = true
	c.ChangeoverCarryoverDisposition = CarryoverKeepLineside
	c.KeepStaged = true
	c.IndexRobotSupplies = true
	c.AutoConfirm = true
	return c
}

// TestFlowAdvanced_UntouchedIsExpandUnchanged: a cell whose Advanced is nil
// expands to the same input, field for field, as a cell that does not carry
// the field at all — so every one of the twelve is left exactly as Expand
// carried it.
func TestFlowAdvanced_UntouchedIsExpandUnchanged(t *testing.T) {
	t.Parallel()
	prior := advancedPrior()
	cell := Collapse(prior)
	cell.PayloadCode, cell.InboundSource = "PART-NEW", "ELSEWHERE"
	if cell.Advanced != nil {
		t.Fatalf("Collapse must not fill Advanced — the modal reads the claim through the view, not through the wire: %+v", cell.Advanced)
	}

	got := Expand(cell, &prior, ClaimSourceAdmin, "engineer")

	// The reference: the same cell with the field explicitly absent. Any
	// difference is a regression in the carry-through, not a difference of
	// intent.
	bare := cell
	bare.Advanced = nil
	want := Expand(bare, &prior, ClaimSourceAdmin, "engineer")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("an untouched Advanced changed the write:\n got  %+v\n want %+v", got, want)
	}

	// And the row that lands still holds the engineer's twelve.
	row := MaterializeClaim(got, &prior)
	if !reflect.DeepEqual(row.AllowedPayloadCodes, prior.AllowedPayloadCodes) ||
		row.ReorderPoint != 60 || row.ReorderPointSource != "manual" || !row.AutoReorder ||
		row.LinesideSoftThreshold != 7 || row.AutoRequestPayload != "SYN-A-P002" || !row.AutoPush ||
		!row.EvacuateOnChangeover || row.ChangeoverCarryoverDisposition != CarryoverKeepLineside ||
		!row.KeepStaged || !row.IndexRobotSupplies || !row.AutoConfirm {
		t.Errorf("a column the modal owns was flattened by a save that never opened it: %+v", row)
	}
}

// TestFlowAdvanced_SetWritesExactlyThoseColumns: Apply lands every field the
// modal draws, and nothing else about the row moves.
func TestFlowAdvanced_SetWritesExactlyThoseColumns(t *testing.T) {
	t.Parallel()
	prior := advancedPrior()
	cell := Collapse(prior)
	cell.Advanced = &FlowAdvanced{
		AllowedPayloadCodes:   []string{"SYN-A-P011"},
		ReorderPoint:          12,
		ReorderPointSource:    "calculated",
		AutoReorder:           false,
		LinesideSoftThreshold: 0,
		AutoRequestPayload:    "",
		AutoPush:              false,
		EvacuateOnChangeover:  false,
		CarryoverDisposition:  CarryoverOutboundStaging,
		IndexRobotSupplies:    false,
		AutoConfirm:           false,
	}
	in := Expand(cell, &prior, ClaimSourceAdmin, "engineer")
	row := MaterializeClaim(in, &prior)

	if !reflect.DeepEqual(row.AllowedPayloadCodes, []string{"SYN-A-P011"}) {
		t.Errorf("allowed list = %v, want the modal's", row.AllowedPayloadCodes)
	}
	if row.ReorderPoint != 12 || row.ReorderPointSource != "calculated" || row.AutoReorder {
		t.Errorf("replenishment did not land: %d / %q / %v", row.ReorderPoint, row.ReorderPointSource, row.AutoReorder)
	}
	if row.LinesideSoftThreshold != 0 || row.AutoRequestPayload != "" || row.AutoPush {
		t.Errorf("a cleared field did not clear: %d / %q / %v", row.LinesideSoftThreshold, row.AutoRequestPayload, row.AutoPush)
	}
	if row.EvacuateOnChangeover || row.ChangeoverCarryoverDisposition != CarryoverOutboundStaging {
		t.Errorf("changeover specials did not land: %v / %q", row.EvacuateOnChangeover, row.ChangeoverCarryoverDisposition)
	}
	// AND THE WITHHELD COLUMN SURVIVED AN APPLY. keep_staged is not on
	// FlowAdvanced any more, so the modal cannot write it in either direction:
	// the prior's true is still true after a save that set every field the
	// block does carry. That is what "the stored column is untouched" means.
	if !row.KeepStaged {
		t.Error("an Apply cleared keep_staged — the block does not carry that column and must not be able to move it")
	}
	if row.IndexRobotSupplies || row.AutoConfirm {
		t.Errorf("hardware/policy did not land: %v / %v", row.IndexRobotSupplies, row.AutoConfirm)
	}

	// AND NOTHING ELSE MOVED. The flow's own columns are the cell's, the
	// engineer's untouched ones are the prior's, and the modal reaches
	// neither. uop_capacity is the one to watch: it is not modelled (the
	// catalog resolves it on read) and the modal must not invent a number.
	if row.CoreNodeName != prior.CoreNodeName || row.SwapMode != prior.SwapMode || row.Role != prior.Role ||
		row.PayloadCode != prior.PayloadCode || row.PairedCoreNode != prior.PairedCoreNode ||
		row.InboundSource != prior.InboundSource || row.OutboundDestination != prior.OutboundDestination ||
		row.InboundStaging != prior.InboundStaging || row.OutboundStaging != prior.OutboundStaging {
		t.Errorf("the modal moved a column the picture owns:\n got  %+v\n want %+v", row, prior)
	}
	// sequence LEFT this list when the modal grew a Board section for it
	// (owner ruling, Q1). uop_capacity is the one to watch: it is not modelled
	// (the catalog resolves it on read) and the modal must not invent a number.
	if row.UOPCapacity != prior.UOPCapacity || row.KeyTask != prior.KeyTask ||
		row.ReuseCompatibleBins != prior.ReuseCompatibleBins {
		t.Errorf("the modal moved a column it does not draw: uop %d, key_task %q, reuse %v",
			row.UOPCapacity, row.KeyTask, row.ReuseCompatibleBins)
	}
}

// TestFlowAdvanced_BoardOrderIsTheModals: `sequence` is the order the loader
// board draws a position in. It is a real column an engineer sets, it had no
// editor on either surface, and R2's list did not name it — so it went where
// every other column the picture does not draw goes, in a Board section of its
// own (owner ruling 2026-09-10, Q1).
//
// What this pins is the pointer semantics, which is the whole reason it can
// live there safely: Expand speaks sequence only when the modal has an opinion,
// so a save from the station HMI — or a desktop save nobody opened the sheet on
// — leaves the store to assign and keep its own order.
func TestFlowAdvanced_BoardOrderIsTheModals(t *testing.T) {
	t.Parallel()
	prior := NodeClaim{
		StyleID: 4, CoreNodeName: "PLN_01", Role: "consume", SwapMode: "two_robot_press_index",
		PayloadCode: "PIA27", Sequence: 7,
	}
	cell := Collapse(prior)

	// Nil Advanced: the store keeps its order.
	if in := Expand(cell, &prior, "test", "eng"); in.Sequence != nil {
		t.Errorf("Expand spoke sequence with no opinion to give (%d); an untouched modal must leave the store's order alone", *in.Sequence)
	}

	// An opinion: it lands, and nothing else does.
	adv := AdvancedOf(&prior)
	if adv.Sequence != 7 {
		t.Fatalf("AdvancedOf read sequence as %d, want the claim's 7 — the sheet would open on the wrong number", adv.Sequence)
	}
	adv.Sequence = 2
	cell.Advanced = adv
	in := Expand(cell, &prior, "test", "eng")
	if in.Sequence == nil || *in.Sequence != 2 {
		t.Fatalf("Expand did not carry the modal's board order: %v", in.Sequence)
	}
	row := MaterializeClaim(in, &prior)
	if row.Sequence != 2 {
		t.Errorf("sequence = %d, want 2", row.Sequence)
	}
	if row.UOPCapacity != prior.UOPCapacity || row.KeyTask != prior.KeyTask || row.ReuseCompatibleBins != prior.ReuseCompatibleBins {
		t.Errorf("the Board section moved a column it does not draw: uop %d, key_task %q, reuse %v",
			row.UOPCapacity, row.KeyTask, row.ReuseCompatibleBins)
	}
}

// TestFlowAdvanced_ExplicitListBeatsTheDegenerateRule: Expand's rule that a
// prior's one-payload allowed list follows the cell's part is a guess made in
// the absence of an opinion. The modal is an opinion, so it wins.
func TestFlowAdvanced_ExplicitListBeatsTheDegenerateRule(t *testing.T) {
	t.Parallel()
	prior := advancedPrior()
	prior.AllowedPayloadCodes = []string{prior.PayloadCode}
	cell := Collapse(prior)
	cell.PayloadCode = "NEW-PART"

	// No opinion: the list follows the part, as before.
	if in := Expand(cell, &prior, ClaimSourceAdmin, "e"); !reflect.DeepEqual(in.AllowedPayloadCodes, []string{"NEW-PART"}) {
		t.Errorf("without the modal the degenerate list must still follow the part: %v", in.AllowedPayloadCodes)
	}
	// An opinion: the engineer's two codes stand, part change or not.
	cell.Advanced = AdvancedOf(&prior)
	cell.Advanced.AllowedPayloadCodes = []string{"A", "B"}
	if in := Expand(cell, &prior, ClaimSourceAdmin, "e"); !reflect.DeepEqual(in.AllowedPayloadCodes, []string{"A", "B"}) {
		t.Errorf("the modal's allowed list = %v, want [A B]", in.AllowedPayloadCodes)
	}
}

// TestFlowAdvanced_OfClaimIsWhatTheModalShows: AdvancedOf reads the row the
// desktop is about to edit, and expanding it straight back writes the row
// unchanged — the modal's own round-trip law, so opening it and pressing
// Apply without touching anything is a no-op.
func TestFlowAdvanced_OfClaimIsWhatTheModalShows(t *testing.T) {
	t.Parallel()
	prior := advancedPrior()
	cell := Collapse(prior)
	cell.Advanced = AdvancedOf(&prior)

	row := MaterializeClaim(Expand(cell, &prior, ClaimSourceAdmin, "engineer"), &prior)
	claimColumnsEqual(t, "Apply with nothing changed", row, prior)
}

// TestFlowAdvanced_NoPriorTakesTheModalsValues: "+ Add a position" has no row
// to carry from, so what the modal shows is what the INSERT gets.
func TestFlowAdvanced_NoPriorTakesTheModalsValues(t *testing.T) {
	t.Parallel()
	cell := FlowCell{
		CoreNodeName: "PLN_06", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot,
		PayloadCode: "P", InboundSource: "SRC", InboundStaging: "PLN_05",
		Advanced: &FlowAdvanced{ReorderPoint: 40, ReorderPointSource: "manual", AutoReorder: true, AutoConfirm: true},
	}
	in := Expand(cell, nil, ClaimSourceAdmin, "engineer")
	row := MaterializeClaim(in, nil)
	if row.ReorderPoint != 40 || row.ReorderPointSource != "manual" || !row.AutoReorder || !row.AutoConfirm {
		t.Errorf("a fresh position did not take the modal's values: %+v", row)
	}
	if row.ChangeoverCarryoverDisposition != CarryoverReplace {
		t.Errorf("a fresh position's blank disposition must read as replace, got %q", row.ChangeoverCarryoverDisposition)
	}
}

// TestFlowAdvanced_IsNotAPresetKey: a preset is a flow with the parts blank,
// and Advanced is not part of that shape — flow_presets is U10's and carries
// no policy. Collapse leaving it nil is what keeps the two shapes identical
// on the wire, and TestFlowCollapse_CellKeysAreThePresetKeys next door is the
// pin that would break first if that changed.
func TestFlowAdvanced_IsNotAPresetKey(t *testing.T) {
	t.Parallel()
	for _, f := range append(append([]string{}, flowPresetNodeKeys...), flowPresetNodeListKeys...) {
		if f == "advanced" {
			t.Fatalf("advanced reached the preset key set")
		}
	}
	raw, err := json.Marshal(Collapse(advancedPrior()))
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys["advanced"]; ok {
		t.Errorf("a collapsed cell marshalled an advanced key: %s", raw)
	}
}
