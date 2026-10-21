//go:build docker

package www

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// TestNodeBins_Golden pins the node-bins answer (GET /api/telemetry/node-bins)
// byte for byte, for the four rows it can give: a full, a bare empty, a node
// with no bin, and a node Core does not know. The ids and generations come from
// the rows the fixture made; every key, its order and its presence is literal.
//
// The handler is called twice and the two bodies compared before the golden. A
// field added to protocol.NodeBinInfo later is omitempty, so a plant that sets
// none of it must still get exactly these bytes (the struct's own golden is
// protocol's TestNodeBinInfo_Golden).
func TestNodeBins_Golden(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	fullAt := &nodes.Node{Name: "NBG-FULL", Enabled: true}
	bareAt := &nodes.Node{Name: "NBG-BARE", Enabled: true}
	emptyAt := &nodes.Node{Name: "NBG-NONE", Enabled: true}
	for _, n := range []*nodes.Node{fullAt, bareAt, emptyAt} {
		testutil.MustNoErr(t, db.CreateNode(n), "create "+n.Name)
	}
	full := testdb.CreateBinAtNode(t, db, sd.Payload.Code, fullAt.ID, "NBG-TOTE-1")

	marker := mintBareType(t, db, "NBG-TOTE")
	bare := &bins.Bin{BinTypeID: marker.ID, Label: "NBG-TOTE-2", NodeID: &bareAt.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bare), "create the bare cart")
	bare, err := db.GetBin(bare.ID)
	testutil.MustNoErr(t, err, "read the bare cart")

	path := "/api/telemetry/node-bins?nodes=NBG-FULL,NBG-BARE,NBG-NONE,NBG-UNKNOWN"
	render := func() string {
		rec := getPlain(t, h.apiTelemetryNodeBins, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("node-bins status %d; body=%s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	first, second := render(), render()
	if first != second {
		t.Fatalf("two answers differ:\n  %s\n  %s", first, second)
	}

	want := "[" + strings.Join([]string{
		fmt.Sprintf(`{"node_name":"NBG-FULL","bin_id":%d,"bin_label":"NBG-TOTE-1","bin_type_code":"DEFAULT",`+
			`"payload_code":%q,"uop_remaining":100,"delta_epoch":%d,"manifest":"{\"items\": []}",`+
			`"manifest_confirmed":true,"occupied":true,"uop_capacity":%d}`, full.ID, sd.Payload.Code, full.DeltaEpoch,
			sd.Payload.UOPCapacity),
		fmt.Sprintf(`{"node_name":"NBG-BARE","bin_id":%d,"bin_label":"NBG-TOTE-2","bin_type_code":"NBG-TOTE-BARE",`+
			`"bare":true,"uop_remaining":0,"delta_epoch":%d,"manifest_confirmed":false,"occupied":true}`,
			bare.ID, bare.DeltaEpoch),
		`{"node_name":"NBG-NONE","uop_remaining":0,"delta_epoch":0,"manifest_confirmed":false,"occupied":false}`,
		`{"node_name":"NBG-UNKNOWN","uop_remaining":0,"delta_epoch":0,"manifest_confirmed":false,"occupied":false}`,
	}, ",") + "]\n"
	if first != want {
		t.Errorf("node-bins answer =\n  %s\nwant\n  %s", first, want)
	}
}
