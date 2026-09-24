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

// A ROW THAT NAMES ITS CARRIER round-trips, and an old Edge's row decodes with
// no carrier. Lane A of the memory build adds bin_id, bin_epoch and
// flushed_seq; they cost 48 bytes on this row (a 5-digit bin id, a 4-digit
// seq) and nothing on one without.
func TestLinesideLevelEntry_CarrierKeysRoundTrip(t *testing.T) {
	t.Parallel()
	bin := int64(12345)
	want := LinesideLevelEntry{CoreNodeName: "ALN_001", PayloadCode: "PART-A", BinCount: 1, BinUOP: 46, BucketQty: 7,
		BinID: &bin, BinEpoch: 3, FlushedSeq: 1234}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const wire = `{"core_node_name":"ALN_001","payload_code":"PART-A","bin_count":1,"bin_uop":46,"bucket_qty":7,"bin_id":12345,"bin_epoch":3,"flushed_seq":1234}`
	if string(b) != wire {
		t.Errorf("wire bytes = %s\nwant          %s", b, wire)
	}
	var got LinesideLevelEntry
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	t.Logf("bytes per row with a carrier: %d (+%d)", len(b), len(b)-len(oldEdgeEntryJSON))

	var old LinesideLevelEntry
	if err := json.Unmarshal([]byte(oldEdgeEntryJSON), &old); err != nil {
		t.Fatalf("unmarshal old: %v", err)
	}
	if old.BinID != nil || old.BinEpoch != 0 || old.FlushedSeq != 0 {
		t.Errorf("an old Edge's row decoded with a carrier: %+v", old)
	}
}
