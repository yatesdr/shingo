package dispatch

import (
	"slices"
	"testing"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/store/payloads"
)

// carrier_keys_blocks_test.go — the carrier keys' pure half: what the block
// builder sends for each step's name (stepsToBlocks).

// blockLines is each block as "<id> <location> <binTask> <action>".
func blockLines(blocks []fleet.OrderBlock) []string {
	out := make([]string, len(blocks))
	for i, b := range blocks {
		out[i] = b.BlockID + " " + b.Location + " " + b.BinTask + " " + b.Action
	}
	return out
}

// TestStepsToBlocks_NamesNilIsToday: a nil names and a names of blanks build
// the same blocks, and those are today's: the default key for each step's
// action, the ids and the actions as before.
func TestStepsToBlocks_NamesNilIsToday(t *testing.T) {
	t.Parallel()
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "SRC"},
		{Action: protocol.ActionDropoff, Node: "STG"},
		{Action: protocol.ActionWait, Node: "STG"},
		{Action: protocol.ActionWait},
		{Action: protocol.ActionPickup, Node: "STG"},
		{Action: protocol.ActionDropoff, Node: "LINE"},
	}
	want := []string{"V-b1 SRC JackLoad pickup", "V-b2 STG JackUnload dropoff", "V-b3 STG Wait wait",
		"V-b5 STG JackLoad pickup", "V-b6 LINE JackUnload dropoff"}
	for _, names := range [][]string{nil, make([]string, len(steps))} {
		if got := blockLines(stepsToBlocks("V", steps, names, 0, nil)); !slices.Equal(got, want) {
			t.Errorf("names %q: blocks %v, want %v", names, got, want)
		}
	}
}

// TestStepsToBlocks_ANameIsSentByItsStep: names[i] is steps[i]'s key; a blank
// one sends the default; the action is the step's whatever the name says.
func TestStepsToBlocks_ANameIsSentByItsStep(t *testing.T) {
	t.Parallel()
	steps := []resolvedStep{
		{Action: protocol.ActionWait, Node: "LINE"},
		{Action: protocol.ActionWait},
		{Action: protocol.ActionPickup, Node: "LINE"},
		{Action: protocol.ActionDropoff, Node: "OUT"},
	}
	names := []string{"wait_4548", "", "jackload_4548", ""}
	want := []string{"V-b3 LINE wait_4548 wait", "V-b5 LINE jackload_4548 pickup", "V-b6 OUT JackUnload dropoff"}
	if got := blockLines(stepsToBlocks("V", steps, names, 2, nil)); !slices.Equal(got, want) {
		t.Errorf("blocks %v, want %v", got, want)
	}
}

// TestStepsToBlocks_TheLoadSequenceKeepsItsNames: the pickup a load sequence
// expands sends the sequence's names and ignores its own; every other step
// sends its name.
func TestStepsToBlocks_TheLoadSequenceKeepsItsNames(t *testing.T) {
	t.Parallel()
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "SRC"},
		{Action: protocol.ActionDropoff, Node: "STG"},
		{Action: protocol.ActionPickup, Node: "STG"},
	}
	seq := &payloads.LoadSequence{TaskNames: []string{"Go_AP1", "load"}, LiftAt: 1}
	names := []string{"jackload_4548", "", "jackload_3230"}
	want := []string{"V-b1 SRC Go_AP1 ", "V-b1-1 SRC load pickup", "V-b2 STG JackUnload dropoff",
		"V-b3 STG jackload_3230 pickup"}
	if got := blockLines(stepsToBlocks("V", steps, names, 0, seq)); !slices.Equal(got, want) {
		t.Errorf("blocks %v, want %v", got, want)
	}
}
