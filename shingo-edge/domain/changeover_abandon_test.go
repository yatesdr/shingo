package domain

import (
	"errors"
	"testing"

	"shingo/protocol"
)

// TestAbandonRefusal is the one rule the abandon refuses on and the station
// offers ABANDON - SKIP THIS STEP by: only a node waiting for material, only
// while its delivery is still queued, and never while the partner evac is with
// the fleet — faulted included, because a faulted robot's state is unknown.
func TestAbandonRefusal(t *testing.T) {
	t.Parallel()
	supplyID := int64(10)
	waiting := NodeTask{NodeName: "ALN_011", State: NodeTaskAwaitingMaterial, NextMaterialOrderID: &supplyID}
	order := func(s protocol.Status) *Order { return &Order{ID: 10, Status: s} }

	for _, tc := range []struct {
		name       string
		task       NodeTask
		supply     *Order
		evac       *Order
		acceptHalf bool
		ok         bool
		inFlight   bool
	}{
		{"queued supply, no evac", waiting, order(protocol.StatusQueued), nil, false, true, false},
		{"queued supply, queued evac", waiting, order(protocol.StatusQueued), order(protocol.StatusQueued), false, true, false},
		{"terminal supply settles the task", waiting, order(protocol.StatusCancelled), nil, false, true, false},
		{"supply gone from the board", waiting, nil, nil, false, true, false},
		{"supply moving", waiting, order(protocol.StatusInTransit), nil, false, false, false},
		{"evac staged", waiting, order(protocol.StatusQueued), order(protocol.StatusStaged), false, false, true},
		{"evac faulted", waiting, order(protocol.StatusQueued), order(protocol.StatusFaulted), false, false, true},
		{"accept half keeps a moving evac", waiting, order(protocol.StatusQueued), order(protocol.StatusInTransit), true, true, false},
		{"not waiting for material", NodeTask{NodeName: "ALN_003", State: NodeTaskStagingRequested, NextMaterialOrderID: &supplyID},
			order(protocol.StatusQueued), nil, false, false, false},
		{"no supply order", NodeTask{NodeName: "ALN_011", State: NodeTaskAwaitingMaterial}, nil, nil, false, false, false},
	} {
		err := AbandonRefusal(tc.task, tc.supply, tc.evac, tc.acceptHalf)
		if (err == nil) != tc.ok {
			t.Errorf("%s: refusal = %v, want ok=%v", tc.name, err, tc.ok)
		}
		if tc.inFlight && !errors.Is(err, ErrPartnerInFlight) {
			t.Errorf("%s: refusal = %v, want ErrPartnerInFlight", tc.name, err)
		}
	}
}
