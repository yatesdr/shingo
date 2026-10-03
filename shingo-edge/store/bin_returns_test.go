package store

import (
	"testing"

	"shingo/protocol"
)

// TestBinReturnSupersedes pins the precedence rule as a table: returned and
// held are final, a late returning never overwrites either, held never
// overwrites returned, returned overwrites held, and an unknown state never
// wins.
func TestBinReturnSupersedes(t *testing.T) {
	t.Parallel()
	const (
		ing = protocol.BinReturnReturning
		ed  = protocol.BinReturnReturned
		hd  = protocol.BinReturnHeld
	)
	cases := []struct {
		stored, incoming string
		want             bool
	}{
		{"", ing, true},
		{"", ed, true},
		{"", hd, true},
		{ing, ing, true},
		{ing, ed, true},
		{ing, hd, true},
		{ed, ing, false},
		{hd, ing, false},
		{ed, hd, false},
		{hd, ed, true},
		{ed, ed, true},
		{hd, hd, true},
		{ing, "bogus", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := BinReturnSupersedes(c.stored, c.incoming); got != c.want {
			t.Errorf("BinReturnSupersedes(%q, %q) = %v, want %v", c.stored, c.incoming, got, c.want)
		}
	}
}

// TestUpsertBinReturn_OutOfOrderArrivalKeepsTheFinalState drives the rule
// through the table: a returned that lands before its returning stays
// returned, and the later held does not displace it either.
func TestUpsertBinReturn_OutOfOrderArrivalKeepsTheFinalState(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	put := func(state, dest string) bool {
		t.Helper()
		ok, err := db.UpsertBinReturn(BinReturn{OrderUUID: "edge-cancelled-1", BinLabel: "BIN-7",
			State: state, Destination: dest})
		if err != nil {
			t.Fatalf("upsert %s: %v", state, err)
		}
		return ok
	}
	if !put(protocol.BinReturnReturned, "SMN_001") {
		t.Fatal("first notice was not written")
	}
	if put(protocol.BinReturnReturning, "SMN_002") {
		t.Error("a late returning overwrote returned")
	}
	if put(protocol.BinReturnHeld, "") {
		t.Error("a held overwrote returned")
	}

	got, err := db.BinReturnsForOrders([]string{"edge-cancelled-1", "unknown"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	r, ok := got["edge-cancelled-1"]
	if !ok || r.State != protocol.BinReturnReturned || r.Destination != "SMN_001" || r.BinLabel != "BIN-7" {
		t.Errorf("stored row = %+v, want returned to SMN_001", r)
	}
	if _, ok := got["unknown"]; ok {
		t.Error("an order with no notice came back with a row")
	}
	if _, err := db.UpsertBinReturn(BinReturn{OrderUUID: "x", State: "bogus"}); err == nil {
		t.Error("an unknown state was accepted")
	}
}
