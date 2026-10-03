package service

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store"
)

func TestBinReturnLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		r    store.BinReturn
		want string
	}{
		{store.BinReturn{BinLabel: "B1", State: protocol.BinReturnReturning, Destination: "SMN_001"}, "Bin B1 returning to SMN_001"},
		{store.BinReturn{BinLabel: "B1", State: protocol.BinReturnReturned, Destination: "SMN_001"}, "Bin B1 returned to SMN_001"},
		{store.BinReturn{BinLabel: "B1", State: protocol.BinReturnHeld, Reason: "no empty slot"}, "Bin B1 held on the robot: no empty slot"},
		{store.BinReturn{State: protocol.BinReturnReturned}, "Bin returned to storage"},
		{store.BinReturn{BinLabel: "B1", State: "bogus"}, ""},
	}
	for _, c := range cases {
		if got := BinReturnLine(c.r); got != c.want {
			t.Errorf("BinReturnLine(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}
