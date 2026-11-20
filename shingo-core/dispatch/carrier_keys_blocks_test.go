package dispatch

import (
	"slices"
	"testing"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/fleet"
	"shingocore/store/payloads"
)

// carrier_keys_blocks_test.go — the carrier keys' pure half: what the block
// builder sends for each step's name (stepsToBlocks), and the rule that names
// a step's key (carrierKeyNames).

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

// ckRow is an order_bins row: bin lifted at plan step pick, set down at drop.
func ckRow(bin int64, pick, drop int) domain.Cart {
	return domain.Cart{BinID: bin, PickStep: &pick, DropStep: &drop}
}

// ckOpen is an open lift: bin lifted at plan step pick, its set-down not
// planned yet (a dwelling dig leg).
func ckOpen(bin int64, pick int) domain.Cart {
	return domain.Cart{BinID: bin, PickStep: &pick}
}

func ckPick(n string) resolvedStep { return resolvedStep{Action: protocol.ActionPickup, Node: n} }
func ckDrop(n string) resolvedStep { return resolvedStep{Action: protocol.ActionDropoff, Node: n} }
func ckWait(n, kind string) resolvedStep {
	return resolvedStep{Action: protocol.ActionWait, Node: n, WaitKind: kind}
}

// The carts of the rule's cases, and their families: a 45x48 and a 32x30,
// both keyed; a third family that names only a wait key; a cart whose family
// names none; and a cart with no family on record.
const (
	ckBig, ckSmall, ckWaitOnly, ckPlain, ckLost     int64 = 1, 2, 3, 4, 5
	ckFamBig, ckFamSmall, ckFamWaitOnly, ckFamPlain int64 = 45, 32, 7, 9
)

var ckKeys = map[int64]familyKeys{
	ckFamBig:      {lift: "jackload_4548", wait: "wait_4548"},
	ckFamSmall:    {lift: "jackload_3230", wait: "wait_3230"},
	ckFamWaitOnly: {wait: "wait_only"},
}

func ckFamilyOf(bin int64) (int64, bool) {
	fam, ok := map[int64]int64{ckBig: ckFamBig, ckSmall: ckFamSmall, ckWaitOnly: ckFamWaitOnly, ckPlain: ckFamPlain}[bin]
	return fam, ok
}

// TestCarrierKeyNames_TheRule: a lift sends its cart's family's lift key; a
// wait sends the wait key of the cart lifted next at its own node; everything
// else, and every doubt, sends the default. Each case is one run of a plan, as
// a create or an append sends it.
func TestCarrierKeyNames_TheRule(t *testing.T) {
	t.Parallel()
	supply := []resolvedStep{ckPick("SRC"), ckDrop("STG"), ckWait("STG", WaitKindStation), ckPick("STG"), ckDrop("LINE")}
	evac := []resolvedStep{ckWait("LINE", WaitKindStation), ckPick("LINE"), ckDrop("OUT")}
	single := []resolvedStep{ckPick("SRC"), ckDrop("STG"), ckWait("LINE", WaitKindStation), ckPick("LINE"),
		ckDrop("OSTG"), ckPick("STG"), ckDrop("LINE"), ckPick("OSTG"), ckDrop("OUT")}
	carry := []resolvedStep{ckPick("LINE"), ckDrop("STG"), ckWait("STG", WaitKindStation), ckPick("STG"), ckDrop("LINE")}
	bare := []resolvedStep{ckPick("SRC"), ckDrop("STG"), {Action: protocol.ActionWait}, ckPick("STG"), ckDrop("LINE")}
	for _, tc := range []struct {
		name  string
		plan  []resolvedStep
		start int
		seg   []resolvedStep // nil: plan[start:]
		rows  []domain.Cart
		want  []string
	}{
		{name: "two-robot swap, supply's create: the lift and the wait in front of the relay's lift",
			plan: supply, seg: supply[:3], rows: []domain.Cart{ckRow(ckSmall, 0, 1), ckRow(ckSmall, 3, 4)},
			want: []string{"jackload_3230", "", "wait_3230"}},
		{name: "two-robot swap, supply's append", plan: supply, start: 3,
			rows: []domain.Cart{ckRow(ckSmall, 0, 1), ckRow(ckSmall, 3, 4)}, want: []string{"jackload_3230", ""}},
		{name: "two-robot swap, evac's create: the wait in front of the line cart's lift",
			plan: evac, seg: evac[:1], rows: []domain.Cart{ckRow(ckBig, 1, 2)}, want: []string{"wait_4548"}},
		{name: "two-robot swap, evac's append", plan: evac, start: 1,
			rows: []domain.Cart{ckRow(ckBig, 1, 2)}, want: []string{"jackload_4548", ""}},
		{name: "single-robot swap, two families: each lift by its own cart, the wait by the line cart",
			plan: single, rows: []domain.Cart{ckRow(ckSmall, 0, 1), ckRow(ckBig, 3, 4), ckRow(ckSmall, 5, 6), ckRow(ckBig, 7, 8)},
			want: []string{"jackload_3230", "", "wait_4548", "jackload_4548", "", "jackload_3230", "", "jackload_4548", ""}},
		{name: "carryover round trip: one cart, two pick rows, both lifts and the wait before the second",
			plan: carry, rows: []domain.Cart{ckRow(ckBig, 0, 1), ckRow(ckBig, 3, 4)},
			want: []string{"jackload_4548", "", "wait_4548", "jackload_4548", ""}},
		{name: "the plan position, not the block number: a bare wait before the run", plan: bare, start: 3,
			rows: []domain.Cart{ckRow(ckBig, 0, 1), ckRow(ckBig, 3, 4)}, want: []string{"jackload_4548", ""}},
		{name: "a run read at the block number lands on nothing", plan: bare, start: 2, seg: bare[3:],
			rows: []domain.Cart{ckRow(ckBig, 0, 1), ckRow(ckBig, 3, 4)}, want: nil},
		{name: "a lift no row is for", plan: evac, start: 1, rows: []domain.Cart{ckRow(ckBig, 4, 5)}, want: nil},
		{name: "a cart with no family on record", plan: evac, rows: []domain.Cart{ckRow(ckLost, 1, 2)}, want: nil},
		{name: "a family that names no key", plan: evac, rows: []domain.Cart{ckRow(ckPlain, 1, 2)}, want: nil},
		{name: "a family that names only a wait key", plan: evac, rows: []domain.Cart{ckRow(ckWaitOnly, 1, 2)},
			want: []string{"wait_only", "", ""}},
		{name: "two carts lifted at one step, keys that disagree", plan: evac,
			rows: []domain.Cart{ckRow(ckBig, 1, 2), ckRow(ckSmall, 1, 2)}, want: nil},
		{name: "two carts lifted at one step, one keyed and one not", plan: evac,
			rows: []domain.Cart{ckRow(ckBig, 1, 2), ckRow(ckPlain, 1, 2)}, want: nil},
		{name: "two carts lifted at one step, of one family", plan: evac,
			rows: []domain.Cart{ckRow(ckBig, 1, 2), ckRow(ckBig, 1, 2)}, want: []string{"wait_4548", "jackload_4548", ""}},
		{name: "a carrying wait: the lane gate point a store waits at", rows: []domain.Cart{ckRow(ckBig, 0, 2)},
			plan: []resolvedStep{ckPick("LINE"), ckWait("GATE", WaitKindLane), ckDrop("SLOT")},
			want: []string{"jackload_4548", "", ""}},
		{name: "a gate point in front of a lift at another node", rows: []domain.Cart{ckRow(ckBig, 1, 2)},
			plan: []resolvedStep{ckWait("GATE", WaitKindLane), ckPick("SLOT"), ckDrop("LINE")},
			want: []string{"", "jackload_4548", ""}},
		{name: "a carrying wait: the dig dwell, before its drop is bound", rows: []domain.Cart{ckOpen(ckBig, 0)},
			plan: []resolvedStep{ckPick("S1"), ckWait("S1", WaitKindLane)}, want: []string{"jackload_4548", ""}},
		{name: "a carrying wait: the dig dwell, its drop bound", rows: []domain.Cart{ckRow(ckBig, 0, 2)},
			plan: []resolvedStep{ckPick("S1"), ckWait("S1", WaitKindLane), ckDrop("SHUF")}, want: []string{"jackload_4548", "", ""}},
		{name: "a carrying wait: the turn in front of a set-down", rows: []domain.Cart{ckRow(ckBig, 0, 2)},
			plan: []resolvedStep{ckPick("SRC"), ckWait("LINE", WaitKindTurn), ckDrop("LINE")}, want: []string{"jackload_4548", "", ""}},
		{name: "the turn in front of a lift stands at the cart", rows: []domain.Cart{ckRow(ckBig, 1, 2)},
			plan: []resolvedStep{ckWait("SRC", WaitKindTurn), ckPick("SRC"), ckDrop("LINE")},
			want: []string{"wait_4548", "jackload_4548", ""}},
		{name: "the run's own steps: a node the run re-points is read from the run", plan: evac,
			seg:  []resolvedStep{ckWait("LINE", WaitKindStation), ckPick("LINE2"), ckDrop("OUT")},
			rows: []domain.Cart{ckRow(ckBig, 1, 2)}, want: []string{"", "jackload_4548", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seg := tc.seg
			if seg == nil {
				seg = tc.plan[tc.start:]
			}
			if got := carrierKeyNames(tc.plan, tc.start, seg, tc.rows, ckFamilyOf, ckKeys); !slices.Equal(got, tc.want) {
				t.Errorf("names %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCarrierKeyNames_AsksAFamilyOnlyForALiftedCart: a run with no lift and no
// wait in front of one never asks a cart's family (at an append, the first ask
// is the read of the order's carts).
func TestCarrierKeyNames_AsksAFamilyOnlyForALiftedCart(t *testing.T) {
	t.Parallel()
	asked := 0
	familyOf := func(bin int64) (int64, bool) { asked++; return ckFamilyOf(bin) }
	plan := []resolvedStep{ckPick("SRC"), ckWait("GATE", WaitKindLane), ckDrop("SLOT")}
	rows := []domain.Cart{ckRow(ckBig, 0, 2)}
	if got := carrierKeyNames(plan, 1, plan[1:], rows, familyOf, ckKeys); got != nil || asked != 0 {
		t.Errorf("a run of a carrying wait and a set-down: names %q, %d famil(ies) asked, want none and 0", got, asked)
	}
	if got := carrierKeyNames(plan, 0, plan, rows, familyOf, ckKeys); got == nil || asked != 1 {
		t.Errorf("a run that lifts: names %q, %d famil(ies) asked, want the lift sized after 1", got, asked)
	}
}
