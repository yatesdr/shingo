package engine

import (
	"testing"

	"shingo/protocol"
)

// Core's carrier rule, asked over the catalog: a part's listed carriers only;
// any carrier for a part with none listed; no judgement without a catalog or
// without the bin's carrier.
func TestPartPermitsCarrier(t *testing.T) {
	t.Parallel()
	catalog := []protocol.PayloadBinTypeInfo{
		{PayloadCode: "P-ONE", BinTypeCode: "TYPE-A"},
		{PayloadCode: "P-TWO", BinTypeCode: "TYPE-A"},
		{PayloadCode: "P-TWO", BinTypeCode: "TYPE-B"},
	}
	for _, c := range []struct {
		name          string
		catalog       []protocol.PayloadBinTypeInfo
		part, carrier string
		want          bool
	}{
		{"a part's own carrier", catalog, "P-ONE", "TYPE-A", true},
		{"a carrier the part does not ride", catalog, "P-ONE", "TYPE-B", false},
		{"a part with two carriers, the second", catalog, "P-TWO", "TYPE-B", true},
		{"a part with none listed rides anything", catalog, "P-NONE", "TYPE-B", true},
		{"no catalog yet: no judgement", nil, "P-ONE", "TYPE-B", true},
		{"a bin whose carrier was not named: no judgement", catalog, "P-ONE", "", true},
	} {
		if got := partPermitsCarrier(c.catalog, c.part, c.carrier); got != c.want {
			t.Errorf("%s: partPermitsCarrier(%s, %s) = %v, want %v", c.name, c.part, c.carrier, got, c.want)
		}
	}
}
