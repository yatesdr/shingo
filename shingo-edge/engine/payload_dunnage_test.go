package engine

import (
	"slices"
	"testing"

	"shingo/protocol"
)

func TestPayloadDunnageCodes_SinglePayloadSingleType(t *testing.T) {
	catalog := []protocol.PayloadBinTypeInfo{
		{PayloadCode: "PART-A", BinTypeCode: "45x48-KD"},
	}
	got := payloadDunnageCodes(catalog, []string{"PART-A"})
	if len(got) != 1 || got[0] != "45x48-KD" {
		t.Errorf("got %v, want [45x48-KD]", got)
	}
}

func TestPayloadDunnageCodes_MultiPayloadDedupesDunnage(t *testing.T) {
	// Both PART-A and PART-B map to 45x48-KD; PART-B also maps to 45x48-TOTES.
	catalog := []protocol.PayloadBinTypeInfo{
		{PayloadCode: "PART-A", BinTypeCode: "45x48-KD"},
		{PayloadCode: "PART-B", BinTypeCode: "45x48-KD"},
		{PayloadCode: "PART-B", BinTypeCode: "45x48-TOTES"},
	}
	got := payloadDunnageCodes(catalog, []string{"PART-A", "PART-B"})
	if len(got) != 2 {
		t.Fatalf("got %v (len %d), want 2 distinct codes", got, len(got))
	}
	if !slices.Contains(got, "45x48-KD") || !slices.Contains(got, "45x48-TOTES") {
		t.Errorf("got %v, want [45x48-KD 45x48-TOTES]", got)
	}
}

func TestPayloadDunnageCodes_EmptyPayloadListReturnsAll(t *testing.T) {
	catalog := []protocol.PayloadBinTypeInfo{
		{PayloadCode: "PART-A", BinTypeCode: "45x48-KD"},
		{PayloadCode: "PART-B", BinTypeCode: "45x48-TOTES"},
	}
	got := payloadDunnageCodes(catalog, nil)
	if len(got) != 2 {
		t.Fatalf("got %v, want both dunnage codes", got)
	}
}

func TestPayloadDunnageCodes_EmptyCatalogReturnsNil(t *testing.T) {
	got := payloadDunnageCodes(nil, []string{"PART-A"})
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

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
