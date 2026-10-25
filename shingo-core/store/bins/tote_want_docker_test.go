//go:build docker

package bins_test

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// claimRow mirrors one plant claim, with no swap mode: the shape test must not
// need one.
func claimRow(t *testing.T, db *store.DB, process, style, core, payload, allowed, paired string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO style_claims
		(process_id, style_id, core_node_name, role, swap_mode, payload_code, allowed_payload_codes, paired_core_node)
		VALUES ($1, $2, $3, 'consume', '', $4, $5, $6)`, process, style, core, payload, allowed, paired)
	testutil.MustNoErr(t, err, "claim at "+core)
}

func plainNode(t *testing.T, db *store.DB, name string, parent *int64) *nodes.Node {
	t.Helper()
	n := &nodes.Node{Name: name, Enabled: true, ParentID: parent}
	testutil.MustNoErr(t, nodes.Create(db.DB, n), "node "+name)
	return n
}

func layersText(h domain.Height) string {
	if n, ok := h.Stated(); ok {
		return fmt.Sprint(n)
	}
	return "none"
}

// TestToteWant_TheShape: the height an empty is wanted at, read with the node
// it is set down on (store.DB.GetSetDownNode). 0 at the press of a cell fed from
// behind whose claim carries the part; the part's layers at its outside
// position, at either press of a sequential pair, and anywhere unclaimed; none
// for a part that states no layers.
func TestToteWant_TheShape(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, err := db.Exec(`INSERT INTO payloads (code, tote_layers, uop_per_tote) VALUES
		('TW-PART', 5, 10), ('TW-ALLOWED', 4, 10), ('TW-KD', NULL, NULL)`)
	testutil.MustNoErr(t, err, "payloads")

	parent := plainNode(t, db, "TW-AREA", nil)
	plainNode(t, db, "TW-PRESS", nil)
	plainNode(t, db, "TW-OUT", nil)
	plainNode(t, db, "TW-SEQ-A", nil)
	plainNode(t, db, "TW-SEQ-B", nil)
	plainNode(t, db, "TW-FREE", nil)
	plainNode(t, db, "TW-CHILD", &parent.ID)
	// A press fed from behind: its paired position is no core node of the cell.
	claimRow(t, db, "TW-P1", "S1", "TW-PRESS", "TW-PART", `["TW-ALLOWED"]`, "TW-OUT")
	// A sequential pair: two presses of one process and style, each the other's
	// paired position.
	claimRow(t, db, "TW-P2", "S1", "TW-SEQ-A", "TW-PART", "[]", "TW-SEQ-B")
	claimRow(t, db, "TW-P2", "S1", "TW-SEQ-B", "TW-PART", "[]", "TW-SEQ-A")
	// The same press under another process with a feeder that IS a core node
	// there does not make TW-PRESS a sequential press for TW-P1.
	claimRow(t, db, "TW-P3", "S1", "TW-OUT", "TW-OTHER", "[]", "")

	for _, tc := range []struct {
		name, node, payload string
		want                domain.Height
	}{
		{"a fed press, its claim's part", "TW-PRESS", "TW-PART", protocol.Layers(0)},
		{"a fed press, a part its claim allows", "TW-PRESS", "TW-ALLOWED", protocol.Layers(0)},
		{"its outside position", "TW-OUT", "TW-PART", protocol.Layers(5)},
		{"a sequential pair's first press", "TW-SEQ-A", "TW-PART", protocol.Layers(5)},
		{"a sequential pair's second press", "TW-SEQ-B", "TW-PART", protocol.Layers(5)},
		{"an unclaimed node", "TW-FREE", "TW-PART", protocol.Layers(5)},
		{"a dotted name", "TW-AREA.TW-CHILD", "TW-PART", protocol.Layers(5)},
		{"a part with no layers at a fed press", "TW-PRESS", "TW-KD", domain.Height{}},
		{"a part with no layers anywhere", "TW-FREE", "TW-KD", domain.Height{}},
		{"no part", "TW-PRESS", "", domain.Height{}},
	} {
		n, got, err := db.GetSetDownNode(tc.node, tc.payload)
		testutil.MustNoErr(t, err, tc.name)
		want, werr := nodes.GetByDotName(db.DB, tc.node)
		testutil.MustNoErr(t, werr, tc.name+": the node by name")
		if n.ID != want.ID || n.Name != want.Name || n.ParentName != want.ParentName {
			t.Errorf("%s: read node %+v, want %+v", tc.name, n, want)
		}
		if layersText(got) != layersText(tc.want) {
			t.Errorf("%s: wanted %s layers, want %s", tc.name, layersText(got), layersText(tc.want))
		}
	}

	if _, _, err := db.GetSetDownNode("TW-NO-SUCH", "TW-PART"); err == nil {
		t.Error("a name that matches no node read a node")
	}
}
