//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// TestFeedsPin_GetContainmentBody pins Core's GET /api/containment body, byte
// for byte, for a fixed fixture: one active flag, one cleared flag, one held
// bin.
//
// after (F2, R21): byte-identical. The flag and held-bin rows move into
// protocol with the same JSON tags and domain aliases them, so the route, the
// handler and every byte here stay as they are.
//
// The fixture is written with SQL so every timestamp is fixed. The driver hands
// a timestamptz back in the process's local zone, so the expected time text is
// the fixed instant marshalled in time.Local — the same encoding the handler's
// rows get.
func TestFeedsPin_GetContainmentBody(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForRendering(t)

	t1 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t2 := time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)
	t3 := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	t4 := time.Date(2026, 1, 2, 4, 0, 0, 0, time.UTC)

	_, err := db.Exec(`INSERT INTO payload_containment
		(payload_code, active, reason, activated_by, activated_at, deactivated_by, deactivated_at, updated_at)
		VALUES ('P-HOLD', TRUE, 'synthetic hold', 'qa-1', $1, '', NULL, $2),
		       ('P-OLD', FALSE, 'cleared', '', NULL, 'qa-2', $3, $3)`, t1, t2, t3)
	testutil.MustNoErr(t, err, "seed containment flags")

	bt := &bins.BinType{Code: "BT-FEEDSPIN", Description: "synthetic"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	n := &nodes.Node{Name: "LN-1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(n), "create node")
	b := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-1", NodeID: &n.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(b), "create bin")
	_, err = db.Exec(`UPDATE bins SET payload_code = 'P-HOLD', quality_hold = TRUE, hold_by = 'edge.test', hold_at = $2
		WHERE id = $1`, b.ID, t4)
	testutil.MustNoErr(t, err, "hold bin")

	ts := func(v time.Time) string {
		out, err := json.Marshal(v.In(time.Local))
		testutil.MustNoErr(t, err, "marshal time")
		return string(out)
	}
	want := `{"containment":[` +
		`{"payload_code":"P-HOLD","active":true,"reason":"synthetic hold","activated_by":"qa-1","activated_at":` + ts(t1) + `,"deactivated_by":"","deactivated_at":null},` +
		`{"payload_code":"P-OLD","active":false,"reason":"cleared","activated_by":"","activated_at":null,"deactivated_by":"qa-2","deactivated_at":` + ts(t3) + `}` +
		`],"held_bins":[` +
		`{"bin_id":` + strconv.FormatInt(b.ID, 10) + `,"label":"BIN-1","payload_code":"P-HOLD","node_name":"LN-1","hold_by":"edge.test","hold_at":` + ts(t4) + `}` +
		`]}` + "\n"

	rec := httptest.NewRecorder()
	h.apiGetContainment(rec, httptest.NewRequest(http.MethodGet, "/api/containment", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (after: same)", rec.Code)
	}
	if got := rec.Body.String(); got != want {
		t.Errorf("GET /api/containment body changed (after F2: byte-identical, R21)\n got: %s\nwant: %s", got, want)
	}
}
