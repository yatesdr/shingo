package service

import (
	"reflect"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/orders"
)

func iptr(v int) *int { return &v }

// The board's buttons come from the rows the view holds (SHAPE 3.8). Each case
// is a node's live legs; the want is what the tile renders.
func TestReleasePurposes(t *testing.T) {
	t.Parallel()
	co := `{"purposes":["ready","tooling_done"]}`
	swap := `{"purposes":["swap"]}`
	leg := func(status protocol.Status, wait *int, kind, facts, intent string) orders.Order {
		return orders.Order{Status: status, StationWait: wait, WaitKind: kind, ReleaseFacts: facts, ReleaseIntent: intent}
	}
	st := protocol.WaitKindStation
	for _, c := range []struct {
		name string
		legs []orders.Order
		want []domain.ReleasePurpose
	}{
		{"a changeover evac parked at ready", []orders.Order{leg(protocol.StatusStaged, iptr(0), st, co, "")},
			[]domain.ReleasePurpose{{Purpose: "ready", Ready: true}}},
		{"parked at tooling done: the button says so", []orders.Order{leg(protocol.StatusStaged, iptr(1), st, co, "")},
			[]domain.ReleasePurpose{{Purpose: "tooling_done", Ready: true}}},
		{"released at ready and driving on to tooling done: present, not ready",
			[]orders.Order{leg(protocol.StatusInTransit, iptr(0), st, co, `{"station_wait":0,"sent_at":"x"}`)},
			[]domain.ReleasePurpose{{Purpose: "tooling_done"}}},
		{"at a lane wait: present, not ready", []orders.Order{leg(protocol.StatusStaged, nil, protocol.WaitKindLane, swap, "")},
			[]domain.ReleasePurpose{{Purpose: "swap"}}},
		{"not yet moving: present, not ready", []orders.Order{leg(protocol.StatusDispatched, nil, "", swap, "")},
			[]domain.ReleasePurpose{{Purpose: "swap"}}},
		{"two purposes at one node, in the order owed", []orders.Order{
			leg(protocol.StatusStaged, iptr(0), st, swap, ""), leg(protocol.StatusStaged, iptr(1), st, co, "")},
			[]domain.ReleasePurpose{{Purpose: "tooling_done", Ready: true}, {Purpose: "swap", Ready: true}}},
		{"past its last wait, terminal, or no stored facts: nothing", []orders.Order{
			leg(protocol.StatusInTransit, iptr(0), st, swap, `{"station_wait":0,"sent_at":"x"}`),
			leg(protocol.StatusConfirmed, iptr(0), st, swap, ""),
			leg(protocol.StatusStaged, iptr(0), st, "", "")}, nil},
	} {
		if got := releasePurposes(c.legs); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// L9: the chip reads every live leg's sentence at the node, not only the
// runtime's active order's.
func TestReleaseHeld(t *testing.T) {
	t.Parallel()
	got := releaseHeld([]orders.Order{
		{Status: protocol.StatusStaged, ReleaseHeld: "waiting for the bin on SYN-PRESS to be lifted"},
		{Status: protocol.StatusStaged, ReleaseHeld: "Core rejected the release: invalid_state"},
		{Status: protocol.StatusStaged, ReleaseHeld: "waiting for the bin on SYN-PRESS to be lifted"},
		{Status: protocol.StatusCancelled, ReleaseHeld: "an ended leg says nothing"},
		{Status: protocol.StatusInTransit},
	})
	want := "waiting for the bin on SYN-PRESS to be lifted; Core rejected the release: invalid_state"
	if got != want {
		t.Errorf("chip = %q, want %q", got, want)
	}
}
