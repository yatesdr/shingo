package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/store/orders"
)

// TestParkedOnDeck_AnOrderBeingAskedForIsNotParked: a Recover press cancels a
// parked on-deck order as parked (cancelParkedOnDeckOrder). An order Core is
// asking the fleet for is its own door's create in flight, and reads not
// parked, as today's in-create `dispatched` row does; the same row at rest
// reads parked.
func TestParkedOnDeck_AnOrderBeingAskedForIsNotParked(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		o    orders.Order
		want bool
	}{
		{"today, inside the create", orders.Order{Status: protocol.StatusDispatched}, false},
		{"queued with an ask open", orders.Order{Status: protocol.StatusQueued, AskedVendorOrderID: "sg-1-ask"}, false},
		{"queued at rest", orders.Order{Status: protocol.StatusQueued}, true},
	}
	for _, c := range cases {
		if got := isParkedOnDeckOrder(&c.o); got != c.want {
			t.Errorf("%s: isParkedOnDeckOrder = %v, want %v", c.name, got, c.want)
		}
	}
}
