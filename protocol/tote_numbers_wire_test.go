package protocol

import "testing"

// The tote numbers on the wire, for a plant that sets them. A count of 0 is a
// base and is sent; unknown is an absent key. The bytes for a plant that sets
// none are TestNodeBinInfo_Golden's and TestBinClearRequest_Golden's, and do not
// move.

func TestNodeBinInfo_ToteNumbers(t *testing.T) {
	t.Parallel()
	zero, sixteen, eight := 0, 16, 8
	for _, tc := range []struct {
		name string
		row  NodeBinInfo
		want string
	}{
		{
			name: "a base: zero totes is sent",
			row: NodeBinInfo{NodeName: "SMN_A1", BinID: 28, BinLabel: "TOTE-2", BinTypeCode: "TOTE",
				DeltaEpoch: 4, Occupied: true, TotesOnCart: &zero, TotesPerLayer: &eight},
			want: `{"node_name":"SMN_A1","bin_id":28,"bin_label":"TOTE-2","bin_type_code":"TOTE",` +
				`"uop_remaining":0,"delta_epoch":4,"manifest_confirmed":false,"occupied":true,` +
				`"totes_on_cart":0,"totes_per_layer":8}`,
		},
		{
			name: "two layers of eight",
			row: NodeBinInfo{NodeName: "SMN_A1", BinID: 28, BinLabel: "TOTE-2", BinTypeCode: "TOTE",
				DeltaEpoch: 4, Occupied: true, TotesOnCart: &sixteen, TotesPerLayer: &eight},
			want: `{"node_name":"SMN_A1","bin_id":28,"bin_label":"TOTE-2","bin_type_code":"TOTE",` +
				`"uop_remaining":0,"delta_epoch":4,"manifest_confirmed":false,"occupied":true,` +
				`"totes_on_cart":16,"totes_per_layer":8}`,
		},
		{
			name: "a tote carrier whose count is unknown",
			row: NodeBinInfo{NodeName: "SMN_A1", BinID: 28, BinLabel: "TOTE-2", BinTypeCode: "TOTE",
				DeltaEpoch: 4, Occupied: true, TotesPerLayer: &eight},
			want: `{"node_name":"SMN_A1","bin_id":28,"bin_label":"TOTE-2","bin_type_code":"TOTE",` +
				`"uop_remaining":0,"delta_epoch":4,"manifest_confirmed":false,"occupied":true,` +
				`"totes_per_layer":8}`,
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

func TestBinClearRequest_Layers(t *testing.T) {
	t.Parallel()
	zero, two := 0, 2
	for _, tc := range []struct {
		name string
		req  BinClearRequest
		want string
	}{
		{"a base is declared", BinClearRequest{NodeName: "SMN_A1", Layers: &zero},
			`{"node_name":"SMN_A1","layers":0}`},
		{"two layers, re-typed", BinClearRequest{NodeName: "SMN_A1", BinID: 27, BinTypeCode: "TOTE", Layers: &two},
			`{"node_name":"SMN_A1","bin_id":27,"bin_type_code":"TOTE","layers":2}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := renderTwice(t, tc.req); got != tc.want {
				t.Errorf("BinClearRequest =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}
