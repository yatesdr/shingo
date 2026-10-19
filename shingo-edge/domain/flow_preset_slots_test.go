package domain

import (
	"reflect"
	"testing"

	"shingo/protocol"
)

func TestPayloadSlots_MutualPairIsOneSlotAndASatelliteHasNone(t *testing.T) {
	press := []FlowCell{
		{CoreNodeName: "P_LH", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobotPressIndex, PairedCoreNode: "P_LH_B"},
		{CoreNodeName: "P_RH", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobotPressIndex, PairedCoreNode: "P_RH_B"},
	}
	if got := PayloadSlots(press); len(got) != 2 || ProduceSlots(got) != 2 {
		t.Fatalf("LH/RH press slots = %+v, want two produce slots", got)
	}
	seq := []FlowCell{
		{CoreNodeName: "A", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSequential, PairedCoreNode: "B"},
		{CoreNodeName: "B", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSequential, PairedCoreNode: "A"},
	}
	if got := PayloadSlots(seq); !reflect.DeepEqual(got, []Slot{{Nodes: []string{"A", "B"}, Role: protocol.ClaimRoleProduce}}) {
		t.Fatalf("sequential pair slots = %+v, want one slot over A and B", got)
	}
	weld := []FlowCell{
		{CoreNodeName: "IN", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot},
		{CoreNodeName: "OUT", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobot},
	}
	if got := PayloadSlots(weld); len(got) != 2 || ProduceSlots(got) != 1 {
		t.Fatalf("weld slots = %+v, want two slots, one produce (brief O-12)", got)
	}
}

// R-D (owner, 2026-10-17).
func TestMintedStyleName(t *testing.T) {
	p, c := protocol.ClaimRoleProduce, protocol.ClaimRoleConsume
	for _, tc := range []struct {
		codes []string
		roles []protocol.ClaimRole
		want  string
	}{
		{[]string{"1234-567"}, []protocol.ClaimRole{p}, "1234-567"},
		{[]string{"1234-567", "1234-568"}, []protocol.ClaimRole{p, p}, "1234-567/8"},
		{[]string{"PANEL-A", "PANEL-C"}, []protocol.ClaimRole{p, p}, "PANEL-A/C"},
		{[]string{"ABC", "XYZ"}, []protocol.ClaimRole{p, p}, "ABC/XYZ"},
		{[]string{"IN-1", "IN-2", "OUT-9"}, []protocol.ClaimRole{c, c, p}, "OUT-9"},
		{[]string{"IN-1", "OUT-8", "OUT-9"}, []protocol.ClaimRole{c, p, p}, "OUT-8/9"},
		// A one-character pair reads as the plant's part lists write it, and
		// the same whichever half is on the first slot.
		{[]string{"63144-6TA0A.06", "63145-6TA0A.06"}, []protocol.ClaimRole{p, p}, "63144/5-6TA0A.06"},
		{[]string{"63145-6TA0A.06", "63144-6TA0A.06"}, []protocol.ClaimRole{p, p}, "63144/5-6TA0A.06"},
		{[]string{"748B5-6RR2A.06", "748B5-6RR3A.06"}, []protocol.ClaimRole{p, p}, "748B5-6RR2/3A.06"},
	} {
		if got := MintedStyleName(tc.codes, tc.roles); got != tc.want {
			t.Errorf("MintedStyleName(%v) = %q, want %q", tc.codes, got, tc.want)
		}
	}
}
