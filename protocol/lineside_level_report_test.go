package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
)

// lineside_level_report_test.go — the wire shape of one lineside report row.
//
// PIN, green before and after lane A of the memory build. Lane A adds the
// carrier's identity to the row (bin id, epoch, flushed seq) as OPTIONAL keys.
// An Edge built before that change sends exactly the bytes below, and a Core
// built after it must still read them; a row carrying no carrier identity must
// also still marshal to exactly these bytes, so the change costs nothing on the
// wire where there is nothing new to say. If this test goes red, the change
// made an old Edge's row mean something different.
const oldEdgeEntryJSON = `{"core_node_name":"ALN_001","payload_code":"PART-A","bin_count":1,"bin_uop":46,"bucket_qty":7}`

func TestLinesideLevelEntry_WireShapeRoundTrips(t *testing.T) {
	t.Parallel()
	want := LinesideLevelEntry{CoreNodeName: "ALN_001", PayloadCode: "PART-A", BinCount: 1, BinUOP: 46, BucketQty: 7}

	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != oldEdgeEntryJSON {
		t.Errorf("wire bytes = %s\nwant          %s", b, oldEdgeEntryJSON)
	}

	var got LinesideLevelEntry
	if err := json.Unmarshal([]byte(oldEdgeEntryJSON), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("an old Edge's row decoded as %+v, want %+v", got, want)
	}
	t.Logf("bytes per row (no carrier identity): %d", len(b))
}
