package protocol

import (
	"encoding/json"
	"testing"
)

// The node-bins row and the CLEAR request, byte for byte as they serialise
// today. Fields added to either later are omitempty, so a plant that sets none
// of them must still send exactly these bytes; a byte that moves here is a wire
// change at every plant, not only the ones that use the new field.
//
// Each value is rendered twice and the two renderings compared before the
// golden, so a non-deterministic encoding fails on its own line rather than as
// a golden mismatch.

func renderTwice(t *testing.T, v any) string {
	t.Helper()
	first, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal again: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("two renderings differ:\n  %s\n  %s", first, second)
	}
	return string(first)
}

func TestNodeBinInfo_Golden(t *testing.T) {
	t.Parallel()
	manifest := `{"items":[{"part_number":"PART-A","quantity":40}]}`
	for _, tc := range []struct {
		name string
		row  NodeBinInfo
		want string
	}{
		{
			name: "no bin at the node",
			row:  NodeBinInfo{NodeName: "SMN_A1"},
			want: `{"node_name":"SMN_A1","uop_remaining":0,"delta_epoch":0,"manifest_confirmed":false,"occupied":false}`,
		},
		{
			name: "a full",
			row: NodeBinInfo{
				NodeName: "SMN_A1", BinID: 27, BinLabel: "TOTE-1", BinTypeCode: "TOTE",
				PayloadCode: "PART-A", UOPRemaining: 40, DeltaEpoch: 3,
				Manifest: &manifest, ManifestConfirmed: true, Occupied: true,
			},
			want: `{"node_name":"SMN_A1","bin_id":27,"bin_label":"TOTE-1","bin_type_code":"TOTE",` +
				`"payload_code":"PART-A","uop_remaining":40,"delta_epoch":3,` +
				`"manifest":"{\"items\":[{\"part_number\":\"PART-A\",\"quantity\":40}]}",` +
				`"manifest_confirmed":true,"occupied":true}`,
		},
		{
			name: "a bare empty",
			row: NodeBinInfo{
				NodeName: "SMN_A1", BinID: 28, BinLabel: "TOTE-2", BinTypeCode: "TOTE-BARE",
				Bare: true, DeltaEpoch: 4, Occupied: true,
			},
			want: `{"node_name":"SMN_A1","bin_id":28,"bin_label":"TOTE-2","bin_type_code":"TOTE-BARE",` +
				`"bare":true,"uop_remaining":0,"delta_epoch":4,"manifest_confirmed":false,"occupied":true}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := renderTwice(t, tc.row); got != tc.want {
				t.Errorf("NodeBinInfo =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

func TestBinClearRequest_Golden(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		req  BinClearRequest
		want string
	}{
		{
			name: "the one bin, its own type",
			req:  BinClearRequest{NodeName: "SMN_A1"},
			want: `{"node_name":"SMN_A1"}`,
		},
		{
			name: "a named bin, re-typed",
			req:  BinClearRequest{NodeName: "SMN_A1", BinID: 27, BinTypeCode: "KD-1"},
			want: `{"node_name":"SMN_A1","bin_id":27,"bin_type_code":"KD-1"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := renderTwice(t, tc.req); got != tc.want {
				t.Errorf("BinClearRequest =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}
