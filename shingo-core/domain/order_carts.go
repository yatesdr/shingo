package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"shingo/protocol"
)

// order_carts.go — an order's carts. A cart the order carries is an
// order_bins row per LIFT: the plan step that picks the cart up and the plan
// step that sets it down. The rows are written where the order comes to hold
// the cart and are never deleted when the order ends, so "the carts of this
// order" and "the last order of this cart" read the same table, live or ended.
//
// The row holds plan positions, never nodes: where a step happens is the
// plan's, read through StepNode (and its SQL twin, StepNodeSQL in
// store/internal/helpers, pinned to it by TestStepNodeSQL_AgreesWithStepNode).
// Which cart is in a robot's forks is a fact about the plant — the order's rows
// whose cart is claimed by the order and stands on _TRANSIT or a robot's
// carrier node — and is never stamped on the row.

// Cart is one lift of a cart an order carries: the cart (BinID) is picked up at
// plan step PickStep and set down at plan step DropStep. DropStep is nil on an
// open lift (picked, its set-down not planned yet: a dwelling dig leg). Both are
// nil only on a row carried over from orders.bin_id for an order that ended
// before the rows existed (v173): the link is true, the steps are not known.
type Cart struct {
	BinID    int64 `json:"bin_id"`
	PickStep *int  `json:"pick_step,omitempty"`
	DropStep *int  `json:"drop_step,omitempty"`
}

// PlanStep is one step of an order's plan as the carts read it: what it does
// and where. Node is the step's node, else its group while the node is not
// chosen yet.
type PlanStep struct {
	Action string
	Node   string
	// Group is the group the step names (its group stamp), beside a node the
	// plan resolved or alone while it is not resolved yet.
	Group string
}

// planStepJSON is the part of a stored plan step (dispatch/plan.Step) the
// carts read. Read here rather than through dispatch/plan, which domain does
// not import.
type planStepJSON struct {
	Action string `json:"action"`
	Node   string `json:"node,omitempty"`
	Group  string `json:"group,omitempty"`
}

// PlanSteps reads a stored plan (orders.steps_json) as carts read it: the
// step's node, else its group. ok=false when the text is not a non-empty JSON
// array of steps.
func PlanSteps(stepsJSON string) ([]PlanStep, bool) {
	if stepsJSON == "" || stepsJSON[0] != '[' {
		return nil, false
	}
	var raw []planStepJSON
	if err := json.Unmarshal([]byte(stepsJSON), &raw); err != nil || len(raw) == 0 {
		return nil, false
	}
	out := make([]PlanStep, len(raw))
	for i, s := range raw {
		node := s.Node
		if node == "" {
			node = s.Group
		}
		out[i] = PlanStep{Action: s.Action, Node: node, Group: s.Group}
	}
	return out, true
}

// OrderPlan is the order's plan: its stored steps, else the two steps its
// endpoints imply — the lift at source_node (step 0) and the drop at
// delivery_node (step 1). A plain order has no stored steps; every order that
// carries a cart has a plan its rows can point into.
func OrderPlan(o *Order) []PlanStep {
	if o == nil {
		return nil
	}
	if steps, ok := PlanSteps(o.StepsJSON); ok {
		return steps
	}
	return []PlanStep{
		{Action: protocol.ActionPickup, Node: o.SourceNode},
		{Action: protocol.ActionDropoff, Node: o.DeliveryNode},
	}
}

// StepNode is where plan step i of the order happens: the step's node, else its
// group; "" for a step that names neither or an index outside the plan. The
// one Go spelling of "where"; store/internal/helpers.StepNodeSQL is the SQL one
// and TestStepNodeSQL_AgreesWithStepNode holds them together.
func StepNode(o *Order, i int) string {
	steps := OrderPlan(o)
	if i < 0 || i >= len(steps) {
		return ""
	}
	return steps[i].Node
}

// Lifts walks a plan and returns one Cart per lift, in plan order.
//
// claimedAt names the cart the order holds for a pickup step (the allocator's
// claims, by plan position). A pickup with no claim re-lifts the cart this
// order set down at that node earlier in the plan (a relay: the single_robot
// swap parks its fresh cart at inbound staging and comes back for it). A
// pickup that lifts nothing (no claim, nothing set down there) and a dropoff
// with nothing in hand are not lifts. A cart still in hand at the plan's end is
// an OPEN lift — a pick step and no drop step yet: a dwelling dig leg lifts its
// blocker before the release binds where it goes (dispatch's bindDwellTail
// appends the drop and sets it on the row). claimedAt[InHand] is a cart already
// in the robot's hands when the plan starts (a recovery's unload-only plan): its
// lift's pick step is InHand.
//
// THE ONE WALK. The allocator writes rows from it, the issue writes a single
// cart's row from it, and v173 lists live orders from it; dispatch's
// resolvePerBinDestinations was the destination-only half of it.
func Lifts(steps []PlanStep, claimedAt map[int]int64) []Cart {
	var out []Cart
	carrying, pick := claimedAt[InHand], InHand
	setDown := map[string]int64{}
	for i, s := range steps {
		switch s.Action {
		case protocol.ActionPickup:
			if b, ok := claimedAt[i]; ok {
				carrying, pick = b, i
				continue
			}
			if b, ok := setDown[s.Node]; ok && s.Node != "" {
				carrying, pick = b, i
				delete(setDown, s.Node)
			}
		case protocol.ActionDropoff:
			if carrying == 0 {
				continue
			}
			p, d := pick, i
			out = append(out, Cart{BinID: carrying, PickStep: &p, DropStep: &d})
			if s.Node != "" {
				setDown[s.Node] = carrying
			}
			carrying, pick = 0, InHand
		}
	}
	if carrying != 0 {
		p := pick
		out = append(out, Cart{BinID: carrying, PickStep: &p})
	}
	return out
}

// InHand is the pick step of a cart already in the robot's hands when the
// order's plan starts: the lift happened before step 0 (a recovery's
// unload-only plan). StepNode of it is the empty string.
const InHand = -1

// FirstPickup is the plan position of the plan's first pickup, -1 for none:
// where a single-cart order (a plain order's implied plan, a named move, a
// leg) lifts the cart it holds.
func FirstPickup(steps []PlanStep) int {
	for i, s := range steps {
		if s.Action == protocol.ActionPickup {
			return i
		}
	}
	return -1
}

// HoldFromBirth gives an order born holding cart (a named cart, a dig leg's
// bin) its cart in memory: lifted at its plan's first pickup — in hand from the
// start when the plan has none — and set down where the walk says. Create
// writes it with the order.
func (o *Order) HoldFromBirth(cart int64) {
	steps := OrderPlan(o)
	pick := FirstPickup(steps)
	if pick < 0 {
		pick = InHand
	}
	o.Carts = Lifts(steps, map[int]int64{pick: cart})
}

// ErrSeveralCarts is OnlyBin refusing an order whose rows name more than one
// cart: the caller asked "the order's cart" of an order that has several.
var ErrSeveralCarts = errors.New("the order carries more than one cart")

// OnlyBin is the one cart the order carries, nil when it carries none. An order
// whose rows name more than one cart is refused with ErrSeveralCarts: a site
// that asked "the order's cart" because it only ever saw single-cart orders
// says so out loud, instead of answering with one of them. A relayed cart (two
// lifts of one cart) is one cart.
func (o *Order) OnlyBin() (*int64, error) {
	var one *int64
	for _, c := range o.Carts {
		switch {
		case one == nil:
			b := c.BinID
			one = &b
		case *one != c.BinID:
			return nil, fmt.Errorf("order %d: %w (bins %d and %d)", o.ID, ErrSeveralCarts, *one, c.BinID)
		}
	}
	return one, nil
}

// CartsSettingDownAt is the order's rows whose drop step happens at node, in
// row order.
func (o *Order) CartsSettingDownAt(node string) []Cart {
	var out []Cart
	for _, c := range o.Carts {
		if c.DropStep != nil && node != "" && strings.TrimSpace(StepNode(o, *c.DropStep)) == node {
			out = append(out, c)
		}
	}
	return out
}

// FinalDrops is, for each cart the order carries, the plan position of its last
// set-down — where the order leaves it — in the order the carts were first
// lifted. A row with no steps (carried over from an order that ended before the
// rows existed) has no drop and is not listed.
func (o *Order) FinalDrops() []Cart {
	last := map[int64]int{}
	var order []int64
	for _, c := range o.Carts {
		if c.DropStep == nil {
			continue
		}
		if prev, seen := last[c.BinID]; !seen {
			order = append(order, c.BinID)
			last[c.BinID] = *c.DropStep
		} else if *c.DropStep > prev {
			last[c.BinID] = *c.DropStep
		}
	}
	out := make([]Cart, 0, len(order))
	for _, b := range order {
		d := last[b]
		var pick *int
		for _, c := range o.Carts {
			if c.BinID == b && c.DropStep != nil && *c.DropStep == d {
				pick = c.PickStep
			}
		}
		out = append(out, Cart{BinID: b, PickStep: pick, DropStep: &d})
	}
	return out
}
