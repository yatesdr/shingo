package domain

import (
	"errors"
	"testing"

	"shingo/protocol"
)

// TestStepNode_FollowsTheRoutesPlan: an order's carts and its route read one
// plan. Wherever the route reads stored steps (stepPlan), StepNode names each
// step's node, else its group; where the route reads the endpoints
// (endpointPlan), StepNode names the lift at the source and the drop at the
// delivery.
func TestStepNode_FollowsTheRoutesPlan(t *testing.T) {
	for _, o := range []*Order{
		{SourceNode: "SRC", DeliveryNode: "DST"},
		{SourceNode: "SRC", DeliveryNode: "DST", ProcessNode: "LINE"},
		{StepsJSON: `[{"action":"pickup","node":"P"},{"action":"dropoff","node":"W"},{"action":"pickup","node":"S"},{"action":"dropoff","node":"P"}]`},
		{StepsJSON: `[{"action":"wait","node":"P"},{"action":"pickup","group":"GRP"},{"action":"dropoff","node":"LINE"}]`},
	} {
		p := planOf(o)
		for i, s := range p.steps {
			want := s.Node
			if want == "" {
				want = s.Group
			}
			if got := StepNode(o, i); got != want {
				t.Errorf("order %+v step %d: StepNode %q, the route's plan %q", o, i, got, want)
			}
		}
		if got := StepNode(o, len(p.steps)); got != "" {
			t.Errorf("order %+v: a step past the plan names %q", o, got)
		}
	}
}

// TestLifts_OneRowPerLift walks the plans the carts were defined on.
func TestLifts_OneRowPerLift(t *testing.T) {
	pick, drop, wait := protocol.ActionPickup, protocol.ActionDropoff, protocol.ActionWait
	type lift struct {
		bin        int64
		pick, drop int
	}
	for _, c := range []struct {
		name    string
		steps   []PlanStep
		claimed map[int]int64
		want    []lift
	}{
		{"the press-index swap: A at P to W, B at S to P",
			[]PlanStep{{wait, "P", ""}, {pick, "P", ""}, {drop, "W", ""}, {pick, "S", ""}, {drop, "P", ""}},
			map[int]int64{1: 10, 3: 20},
			[]lift{{10, 1, 2}, {20, 3, 4}}},
		{"the single_robot swap: F relayed through IS, O through OS",
			[]PlanStep{{pick, "S", ""}, {drop, "IS", ""}, {wait, "P", ""}, {pick, "P", ""}, {drop, "OS", ""}, {pick, "IS", ""}, {drop, "P", ""}, {pick, "OS", ""}, {drop, "OD", ""}},
			map[int]int64{0: 1, 3: 2},
			[]lift{{1, 0, 1}, {2, 3, 4}, {1, 5, 6}, {2, 7, 8}}},
		{"a plain order's implied plan",
			[]PlanStep{{pick, "SRC", ""}, {drop, "DST", ""}},
			map[int]int64{0: 7},
			[]lift{{7, 0, 1}}},
		{"a pickup that lifts nothing and a drop with nothing in hand",
			[]PlanStep{{pick, "X", ""}, {drop, "Y", ""}, {pick, "S", ""}, {drop, "P", ""}},
			map[int]int64{2: 5},
			[]lift{{5, 2, 3}}},
		{"one cart staged and re-lifted: two lifts of one cart",
			[]PlanStep{{pick, "S", ""}, {drop, "ST", ""}, {pick, "ST", ""}, {drop, "L", ""}},
			map[int]int64{0: 3},
			[]lift{{3, 0, 1}, {3, 2, 3}}},
		{"a drive with nothing in hand after the set-down",
			[]PlanStep{{pick, "S", ""}, {drop, "ST", ""}, {drop, "L", ""}},
			map[int]int64{0: 4},
			[]lift{{4, 0, 1}}},
		{"a cart in hand when the plan starts",
			[]PlanStep{{drop, "D", ""}},
			map[int]int64{InHand: 6},
			[]lift{{6, InHand, 0}}},
		{"a cart lifted and not set down yet: an open lift",
			[]PlanStep{{pick, "S", ""}},
			map[int]int64{0: 5},
			[]lift{{5, 0, -1}}},
	} {
		got := Lifts(c.steps, c.claimed)
		if len(got) != len(c.want) {
			t.Errorf("%s: %d lifts, want %d (%+v)", c.name, len(got), len(c.want), got)
			continue
		}
		for i, w := range c.want {
			drop := -1
			if got[i].DropStep != nil {
				drop = *got[i].DropStep
			}
			if got[i].BinID != w.bin || *got[i].PickStep != w.pick || drop != w.drop {
				t.Errorf("%s: lift %d = (%d, %d, %d), want (%d, %d, %d)", c.name, i,
					got[i].BinID, *got[i].PickStep, drop, w.bin, w.pick, w.drop)
			}
		}
	}
}

// TestOnlyBin_RefusesAnOrderWithSeveralCarts: "the order's cart" is one cart or
// a loud refusal, never one of several. A relayed cart is one cart.
func TestOnlyBin_RefusesAnOrderWithSeveralCarts(t *testing.T) {
	step := func(i int) *int { return &i }
	none := &Order{ID: 1}
	if b, err := none.OnlyBin(); b != nil || err != nil {
		t.Errorf("an order with no carts: %v, %v; want nil, nil", b, err)
	}
	relayed := &Order{ID: 2, Carts: []Cart{{BinID: 7, PickStep: step(0), DropStep: step(1)}, {BinID: 7, PickStep: step(5), DropStep: step(6)}}}
	if b, err := relayed.OnlyBin(); err != nil || b == nil || *b != 7 {
		t.Errorf("a relayed cart: %v, %v; want 7", b, err)
	}
	two := &Order{ID: 3, Carts: []Cart{{BinID: 7, PickStep: step(1), DropStep: step(2)}, {BinID: 8, PickStep: step(3), DropStep: step(4)}}}
	if b, err := two.OnlyBin(); !errors.Is(err, ErrSeveralCarts) || b != nil {
		t.Errorf("two carts: %v, %v; want ErrSeveralCarts", b, err)
	}
}
