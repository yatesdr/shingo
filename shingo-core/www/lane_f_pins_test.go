//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// lane_f_pins_test.go — P0 pins for lane F's Core handler items, taken at
// 5c0beb74. Predicted values after the lane lands are in the evidence folder's
// predictions/p0-bins-f.md.

// TestPinNodes_UpdateWithoutZoneKeepsIt: templates/nodes.html posts no zone
// field. Before lane F, handleNodeUpdate wrote r.FormValue("zone") and every
// save from the page blanked the zone. AFTER (lane F, node zone bug): a save
// that does not send zone leaves it.
func TestPinNodes_UpdateWithoutZoneKeepsIt(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	node := &nodes.Node{Name: "PIN-ZONE-1", Zone: "A", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create node")

	form := url.Values{}
	form.Set("id", strconv.FormatInt(node.ID, 10))
	form.Set("name", "PIN-ZONE-1")
	form.Set("enabled", "on")
	rec := postForm(t, h.handleNodeUpdate, "/nodes/update", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body %s", rec.Code, rec.Body.String())
	}
	got, err := db.GetNode(node.ID)
	testutil.MustNoErr(t, err, "get node")
	if got.Zone != "A" {
		t.Errorf("stored zone = %q, want \"A\" (a save without zone leaves it)", got.Zone)
	}
}

// pinSeedMissionOrder makes one order at a station, carried by a robot, with
// history rows at fixed instants: in_transit at t0, faulted at t0+10s,
// in_transit again at t0+40s, delivered at t0+dur.
func pinSeedMissionOrder(t *testing.T, db *store.DB, station, robot string, t0 time.Time, dur time.Duration) {
	t.Helper()
	o := testdb.CreateOrder(t, db, func(o *orders.Order) { o.StationID = station })
	_, err := db.Exec(`UPDATE orders SET robot_id = $1 WHERE id = $2`, robot, o.ID)
	testutil.MustNoErr(t, err, "robot")
	for _, r := range []struct {
		status string
		at     time.Time
	}{
		{"in_transit", t0},
		{"faulted", t0.Add(10 * time.Second)},
		{"in_transit", t0.Add(40 * time.Second)},
		{"delivered", t0.Add(dur)},
	} {
		_, err := db.Exec(`INSERT INTO order_history (order_id, status, detail, created_at) VALUES ($1, $2, '', $3)`,
			o.ID, r.status, r.at)
		testutil.MustNoErr(t, err, "history")
	}
}

func pinGet(t *testing.T, handler http.HandlerFunc, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestPinMissions_DwellAndFaultsHonourStationAndRobot: before lane F,
// apiMissionDwell and apiMissionFaults parsed the filter but passed neither
// station_id nor robot_id on, so a filtered answer was byte for byte the
// unfiltered one. AFTER (lane F, Missions filters bug): two orders, one per
// station and robot; unfiltered counts both, each of ?station_id= /
// ?robot_id= / both counts the one order that matches.
func TestPinMissions_DwellAndFaultsHonourStationAndRobot(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	t0 := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	pinSeedMissionOrder(t, db, "pin-line-a", "PIN-R1", t0, 60*time.Second)
	pinSeedMissionOrder(t, db, "pin-line-b", "PIN-R2", t0.Add(time.Hour), 120*time.Second)

	dwellTransit := func(body string) int64 {
		var resp struct {
			Rows []struct {
				Key   string `json:"key"`
				Count int64  `json:"count"`
			} `json:"rows"`
		}
		testutil.MustNoErr(t, json.Unmarshal([]byte(body), &resp), "decode dwell")
		for _, r := range resp.Rows {
			if r.Key == "transit" {
				return r.Count
			}
		}
		t.Errorf("dwell has no transit row: %s", body)
		return -1
	}
	faults := func(body string) (n int64, robots int) {
		var resp struct {
			Stats struct {
				Outcomes []struct {
					Count int64 `json:"count"`
				} `json:"outcomes"`
				ByRobot []json.RawMessage `json:"by_robot"`
			} `json:"stats"`
		}
		testutil.MustNoErr(t, json.Unmarshal([]byte(body), &resp), "decode faults")
		for _, o := range resp.Stats.Outcomes {
			n += o.Count
		}
		return n, len(resp.Stats.ByRobot)
	}

	const win = "since=2026-01-01&until=2026-01-31"
	for _, c := range []struct {
		filter string
		want   int64
	}{
		{"", 2},
		{"&station_id=pin-line-a", 1},
		{"&robot_id=PIN-R1", 1},
		{"&station_id=pin-line-a&robot_id=PIN-R1", 1},
		{"&station_id=pin-line-a&robot_id=PIN-R2", 0},
	} {
		if got := dwellTransit(pinGet(t, h.apiMissionDwell, "/api/missions/dwell?"+win+c.filter)); got != c.want {
			t.Errorf("dwell%s transit count = %d, want %d", c.filter, got, c.want)
		}
		n, robots := faults(pinGet(t, h.apiMissionFaults, "/api/missions/faults?"+win+c.filter))
		if n != c.want || int64(robots) != c.want {
			t.Errorf("faults%s = %d faults over %d robots, want %d over %d", c.filter, n, robots, c.want, c.want)
		}
	}
}

// TestPinTestOrders_ReleaseOfUnknownOrderAnswersNotFound: before lane F,
// apiDirectOrderRelease called the dispatcher, which refused on the wire (an
// outbox order.error), and then wrote 200 "released" regardless, so the page
// toasted "Released" for an order Core does not have. AFTER (lane F, Test
// Orders release bug): 404 {"error":"order not found"}; the page shows it.
func TestPinTestOrders_ReleaseOfUnknownOrderAnswersNotFound(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiDirectOrderRelease, "/api/test-orders/direct/release",
		map[string]any{"order_uuid": "pin-no-such-order"})
	want := "{\"error\":\"order not found\"}\n"
	if rec.Code != http.StatusNotFound || rec.Body.String() != want {
		t.Errorf("release of an unknown order = %d %q, want 404 %q", rec.Code, rec.Body.String(), want)
	}
}

// TestPinTestOrders_ComplexSubmitWithNoRowAnswersFailure: before lane F, the
// direct complex submit answered 200 with the uuids it made up whether or not
// an order was created. AFTER (lane F, complex submit bug): a sequential submit
// naming nodes that do not exist creates no row and answers 409 with the
// dispatcher's refusal ("not created: sequential: <detail>").
func TestPinTestOrders_ComplexSubmitWithNoRowAnswersFailure(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	rec := postJSON(t, h.apiDirectComplexOrderSubmit, "/api/test-orders/direct/complex", map[string]any{
		"cycle_mode": "sequential", "location": "PIN-NO-SUCH-NODE", "payload_code": "PIN-NO-SUCH-PAYLOAD",
		"inbound_source": "PIN-NO-SUCH-SRC", "outbound_destination": "PIN-NO-SUCH-DEST",
	})
	t.Logf("answer: %d %s", rec.Code, rec.Body.String())
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode")
	const prefix = "not created: sequential: "
	if !strings.HasPrefix(resp.Error, prefix) || len(resp.Error) == len(prefix) || resp.Error == prefix+"no order was created" {
		t.Errorf("error = %q, want %q followed by the dispatcher's refusal", resp.Error, prefix)
	}
	// Still no row: the fix is the answer, not the intake.
	if n, err := db.ListOrdersByStation("core-direct", 50); err != nil || len(n) != 0 {
		t.Errorf("core-direct orders = %d (%v), want 0", len(n), err)
	}
}

// TestPinOrders_TerminateFailureAnswers: the Orders row Cancel posts
// /api/orders/terminate (orders.js:85-93) and only console.errors a failure.
// The server answers the failure; it is the row that hides it. TODAY: an unknown
// order and a finished one are each a 500 with the reason.
func TestPinOrders_TerminateFailureAnswers(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	rec := postJSON(t, h.apiTerminateOrder, "/api/orders/terminate", map[string]any{"order_id": 999999})
	if rec.Code != http.StatusInternalServerError || rec.Body.String() != "{\"error\":\"order not found\"}\n" {
		t.Errorf("unknown order = %d %q, want 500 order not found", rec.Code, rec.Body.String())
	}

	done := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "confirmed" })
	rec = postJSON(t, h.apiTerminateOrder, "/api/orders/terminate", map[string]any{"order_id": done.ID})
	want := "{\"error\":\"cannot terminate order in status \\\"confirmed\\\"\"}\n"
	if rec.Code != http.StatusInternalServerError || rec.Body.String() != want {
		t.Errorf("confirmed order = %d %q, want 500 %q", rec.Code, rec.Body.String(), want)
	}
}
