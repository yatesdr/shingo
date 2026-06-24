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
