package dispatch

import "testing"

// TestStepsToBlocks_UnconfiguredByteIdentical pins the default path: a nil load
// sequence yields exactly today's blocks — same ids, locations, and binTasks —
// so an unconfigured payload emits a byte-identical wire order (no behavior
// change). This is the snapshot the "empty = today's single load block" contract
// rests on.
func TestStepsToBlocks_UnconfiguredByteIdentical(t *testing.T) {
	plan := buildTransportPlan("SRC", "DST", false)
	blocks := stepsToBlocks("v-1", plan, 0, nil)
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(blocks))
	}
	want := []struct{ id, loc, task string }{
		{"v-1-b1", "SRC", "JackLoad"},
		{"v-1-b2", "DST", "JackUnload"},
	}
	for i, w := range want {
		if blocks[i].BlockID != w.id || blocks[i].Location != w.loc || blocks[i].BinTask != w.task {
			t.Errorf("block[%d] = {%s %s %s}, want {%s %s %s}", i,
				blocks[i].BlockID, blocks[i].Location, blocks[i].BinTask, w.id, w.loc, w.task)
		}
	}
}

// TestStepsToBlocks_ConfiguredExpandsLoadLeg pins the golden wire shape from the
// evidence doc: a configured payload's LOAD leg becomes N same-location blocks
// named by the sequence, in registry order, with unique ids — while the delivery
// leg is untouched. Matches the RDS team's working Postman order (four
// same-location named blocks) embedded in a normal transport.
func TestStepsToBlocks_ConfiguredExpandsLoadLeg(t *testing.T) {
	plan := buildTransportPlan("A", "DST", false)
	seq := []string{"Go_AP1", "Spin_90", "load", "Spin_inverse_90"}
	blocks := stepsToBlocks("v-2", plan, 0, seq)
	if len(blocks) != 5 {
		t.Fatalf("want 5 blocks (4 load + 1 deliver), got %d: %+v", len(blocks), blocks)
	}
	// Four same-location named load blocks, in registry order.
	for i, name := range seq {
		if blocks[i].BinTask != name {
			t.Errorf("load block[%d] binTask = %q, want %q", i, blocks[i].BinTask, name)
		}
		if blocks[i].Location != "A" {
			t.Errorf("load block[%d] location = %q, want A", i, blocks[i].Location)
		}
	}
	// Delivery leg unchanged (JackUnload at the destination).
	if blocks[4].BinTask != "JackUnload" || blocks[4].Location != "DST" {
		t.Errorf("deliver block = {%s %s}, want {JackUnload DST}", blocks[4].BinTask, blocks[4].Location)
	}
	// Every block id is unique within the vendor order (SEER's only contract).
	seen := map[string]bool{}
	for _, b := range blocks {
		if seen[b.BlockID] {
			t.Errorf("duplicate blockId %q", b.BlockID)
		}
		seen[b.BlockID] = true
	}
}

// TestStepsToBlocks_ConfiguredSingleName covers a one-name sequence: still an
// expansion (one named block), delivery intact — the degenerate case that must
// not collapse back to JackLoad.
func TestStepsToBlocks_ConfiguredSingleName(t *testing.T) {
	plan := buildTransportPlan("A", "DST", false)
	blocks := stepsToBlocks("v-3", plan, 0, []string{"load"})
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(blocks))
	}
	if blocks[0].BinTask != "load" || blocks[0].Location != "A" {
		t.Errorf("load block = {%s %s}, want {load A}", blocks[0].BinTask, blocks[0].Location)
	}
	if blocks[1].BinTask != "JackUnload" {
		t.Errorf("deliver binTask = %q, want JackUnload", blocks[1].BinTask)
	}
}
