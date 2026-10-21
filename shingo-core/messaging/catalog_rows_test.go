package messaging

import (
	"reflect"
	"testing"

	"shingo/protocol"
	"shingocore/store/payloads"
)

// TestCatalogRows_CarryManifestLines: the catalog feed's row for a payload
// carries that payload's manifest template lines, in the order read, beside
// the fields it already had; a payload with no lines carries none (nil, so the
// row encodes as it did before). Only the lines' part, ratio and description
// cross: the line ids, the part id and the part's cat id stay on Core.
func TestCatalogRows_CarryManifestLines(t *testing.T) {
	t.Parallel()
	ps := []*payloads.Payload{
		{ID: 1, Code: "PC-A", Description: "synthetic A", UOPCapacity: 12},
		{ID: 2, Code: "PC-B", Description: "synthetic B", UOPCapacity: 24},
	}
	catids := map[int64]string{1: "CAT-A"}
	lines := map[int64][]*payloads.ManifestItem{
		1: {
			{ID: 7, PayloadID: 1, PartNumber: "PN-A2", PartID: 3, CATID: "CAT-A", PartsPerCycle: 2, Description: "left"},
			{ID: 9, PayloadID: 1, PartNumber: "PN-A1", PartsPerCycle: 1},
		},
	}
	got := catalogRows(ps, catids, lines)
	want := []protocol.CatalogPayloadInfo{
		{ID: 1, Name: "PC-A", Code: "PC-A", Description: "synthetic A", UOPCapacity: 12, CATID: "CAT-A",
			Manifest: []protocol.ManifestItem{
				{PartNumber: "PN-A2", PartsPerCycle: 2, Description: "left"},
				{PartNumber: "PN-A1", PartsPerCycle: 1},
			}},
		{ID: 2, Name: "PC-B", Code: "PC-B", Description: "synthetic B", UOPCapacity: 24},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("catalog rows = %+v\nwant %+v", got, want)
	}
}
