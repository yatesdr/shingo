package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
)

// TestPressIndexPair_StepsGolden pins the press-index pair's two legs as the
// bytes that go on the wire, for each shape the builder makes: unflipped and
// flipped, two positions and three. TestBuildPressIndexSwapSteps_FlippedShapes
// and _UnflippedIsUnchanged pin the action@node trace; this pins every field
// of every step (the empty flags, the wait's kind and purpose, and that no
// other key is sent), so a field added to a step moves a byte here.
//
// Each leg is rendered twice and the two compared before the golden.
func TestPressIndexPair_StepsGolden(t *testing.T) {
	t.Parallel()
	const (
		waitPress = `{"action":"wait","node":"PRESS","wait_kind":"station","purpose":"swap"}`
		waitB     = `{"action":"wait","node":"INDEX-B","wait_kind":"station","purpose":"swap"}`
		liftPress = `{"action":"pickup","node":"PRESS"}`
		toMarket  = `{"action":"dropoff","node":"MARKET"}`
		fetch     = `{"action":"pickup","node":"MARKET-EMPTIES","empty":true}`
		liftB     = `{"action":"pickup","node":"INDEX-B","empty":true}`
		liftC     = `{"action":"pickup","node":"INDEX-C","empty":true}`
		toPress   = `{"action":"dropoff","node":"PRESS"}`
		toB       = `{"action":"dropoff","node":"INDEX-B"}`
		toC       = `{"action":"dropoff","node":"INDEX-C"}`
	)
	leg := func(steps ...string) string {
		out := "["
		for i, s := range steps {
			if i > 0 {
				out += ","
			}
			out += s
		}
		return out + "]"
	}
	for _, tc := range []struct {
		name           string
		secondPaired   string
		flipped        bool
		wantR1, wantR2 string
	}{
		{"two positions, unflipped", "", false,
			leg(waitPress, liftPress, toMarket, fetch, toB),
			leg(waitB, liftB, toPress)},
		{"two positions, flipped", "", true,
			leg(waitPress, liftPress, toMarket),
			leg(waitB, liftB, toPress, fetch, toB)},
		{"three positions, unflipped", "INDEX-C", false,
			leg(waitPress, liftPress, toMarket, fetch, toC),
			leg(waitB, liftB, toPress, liftC, toB)},
		{"three positions, flipped", "INDEX-C", true,
			leg(waitPress, liftPress, toMarket),
			leg(waitB, liftB, toPress, liftC, toB, fetch, toC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			render := func() (string, string) {
				r1, r2 := BuildTwoRobotPressIndexSwapSteps(
					flipClaim(protocol.ClaimRoleProduce, tc.secondPaired, tc.flipped))
				a, err := json.Marshal(r1)
				if err != nil {
					t.Fatalf("marshal R1: %v", err)
				}
				b, err := json.Marshal(r2)
				if err != nil {
					t.Fatalf("marshal R2: %v", err)
				}
				return string(a), string(b)
			}
			r1, r2 := render()
			r1Again, r2Again := render()
			if r1 != r1Again || r2 != r2Again {
				t.Fatalf("two renderings differ:\n  %s\n  %s\n  %s\n  %s", r1, r1Again, r2, r2Again)
			}
			if r1 != tc.wantR1 {
				t.Errorf("R1 =\n  %s\nwant\n  %s", r1, tc.wantR1)
			}
			if r2 != tc.wantR2 {
				t.Errorf("R2 =\n  %s\nwant\n  %s", r2, tc.wantR2)
			}
		})
	}
}
