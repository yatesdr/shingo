package protocol

import "testing"

// TestPlacesBinAt pins the step-shape predicate's definition directly, so the
// "no LATER pickup" half cannot be quietly dropped. The builder-driven table
// (the shapes the Edge's builders actually emit) stays on the Edge, in
// engine/supply_leg_classifier_test.go, beside the builders it calls.
func TestPlacesBinAt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		steps []ComplexOrderStep
		want  bool
	}{
		{
			name: "dropoff at node, nothing after",
			steps: []ComplexOrderStep{
				{Action: ActionPickup, Node: "SRC"},
				{Action: ActionDropoff, Node: "PRESS"},
			},
			want: true,
		},
		{
			name: "dropoff at node, then picked back up",
			steps: []ComplexOrderStep{
				{Action: ActionDropoff, Node: "PRESS"},
				{Action: ActionPickup, Node: "PRESS"},
			},
			want: false,
		},
		{
			name: "picked up first, then a fresh bin dropped — the single-robot shape",
			steps: []ComplexOrderStep{
				{Action: ActionPickup, Node: "PRESS"},
				{Action: ActionDropoff, Node: "OUT-STAGING"},
				{Action: ActionDropoff, Node: "PRESS"},
			},
			want: true,
		},
		{
			name: "only ever picks up from the node",
			steps: []ComplexOrderStep{
				{Action: ActionWait, Node: "PRESS"},
				{Action: ActionPickup, Node: "PRESS"},
				{Action: ActionDropoff, Node: "MARKET"},
			},
			want: false,
		},
		{
			name: "never touches the node",
			steps: []ComplexOrderStep{
				{Action: ActionPickup, Node: "A"},
				{Action: ActionDropoff, Node: "B"},
			},
			want: false,
		},
		{
			name:  "no steps",
			steps: nil,
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := PlacesBinAt(tc.steps, "PRESS"); got != tc.want {
				t.Errorf("PlacesBinAt = %v, want %v", got, tc.want)
			}
		})
	}
	if PlacesBinAt([]ComplexOrderStep{{Action: ActionDropoff, Node: ""}}, "") {
		t.Error("a blank node is never placed at")
	}
}

// TestOrderPlacesBinAt pins the two arms and the key between them: the order
// TYPE, never whether steps are present. A plain order carrying a stored
// transport plan (Core writes one at dispatch) still answers by its delivery
// node, and a complex order's delivery node (a display value) is never read.
func TestOrderPlacesBinAt(t *testing.T) {
	t.Parallel()
	r2 := []ComplexOrderStep{
		{Action: ActionWait, Node: "B"},
		{Action: ActionPickup, Node: "B"},
		{Action: ActionDropoff, Node: "PRESS"},
		{Action: ActionPickup, Node: "C"},
		{Action: ActionDropoff, Node: "B"},
	}
	plan := []ComplexOrderStep{
		{Action: ActionPickup, Node: "MARKET"},
		{Action: ActionDropoff, Node: "B"},
	}
	for _, tc := range []struct {
		name         string
		orderType    OrderType
		steps        []ComplexOrderStep
		deliveryNode string
		node         string
		want         bool
	}{
		{"complex: by its steps, mid-sequence press", OrderTypeComplex, r2, "B", "PRESS", true},
		{"complex: by its steps, the last drop", OrderTypeComplex, r2, "B", "B", true},
		{"complex: its delivery node is never read", OrderTypeComplex, r2, "ELSEWHERE", "ELSEWHERE", false},
		{"complex with no steps places nowhere", OrderTypeComplex, nil, "PRESS", "PRESS", false},
		{"retrieve: by its delivery node", OrderTypeRetrieve, nil, "B", "B", true},
		{"retrieve: another node", OrderTypeRetrieve, nil, "B", "PRESS", false},
		{"retrieve with a stored plan still answers by its delivery node", OrderTypeRetrieve, plan, "PRESS", "B", false},
		{"move: by its delivery node", OrderTypeMove, plan, "OUT", "OUT", true},
		{"a blank node is never placed at", OrderTypeRetrieve, nil, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := OrderPlacesBinAt(tc.orderType, tc.steps, tc.deliveryNode, tc.node); got != tc.want {
				t.Errorf("OrderPlacesBinAt(%s, …, %q, %q) = %v, want %v", tc.orderType, tc.deliveryNode, tc.node, got, tc.want)
			}
		})
	}
}
