//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

// TestPinNodes_UpdateWithoutZoneBlanksIt: handleNodeUpdate writes
// r.FormValue("zone") (handlers_nodes.go:231) and templates/nodes.html posts no
// zone field, so every save from the page blanks the zone. TODAY: stored "".
func TestPinNodes_UpdateWithoutZoneBlanksIt(t *testing.T) {
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
	if got.Zone != "" {
		t.Errorf("stored zone = %q, want \"\" (today a save without zone blanks it)", got.Zone)
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

// TestPinMissions_DwellAndFaultsIgnoreStationAndRobot: apiMissionDwell
// (handlers_missions.go:297-316) and apiMissionFaults (:329-345) parse the
// filter but pass neither station_id nor robot_id on. Two orders, one per
// station and robot: TODAY the answer with ?station_id= / ?robot_id= is byte
// for byte the unfiltered one, and counts both orders.
func TestPinMissions_DwellAndFaultsIgnoreStationAndRobot(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	t0 := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	pinSeedMissionOrder(t, db, "pin-line-a", "PIN-R1", t0, 60*time.Second)
	pinSeedMissionOrder(t, db, "pin-line-b", "PIN-R2", t0.Add(time.Hour), 120*time.Second)

	const win = "since=2026-01-01&until=2026-01-31"
	for _, door := range []struct {
		name    string
		handler http.HandlerFunc
		path    string
	}{
		{"dwell", h.apiMissionDwell, "/api/missions/dwell"},
		{"faults", h.apiMissionFaults, "/api/missions/faults"},
	} {
		base := pinGet(t, door.handler, door.path+"?"+win)
		for _, f := range []string{"&station_id=pin-line-a", "&robot_id=PIN-R1", "&station_id=pin-line-a&robot_id=PIN-R1"} {
			if got := pinGet(t, door.handler, door.path+"?"+win+f); got != base {
				t.Errorf("%s%s differs from the unfiltered answer (today the filters are ignored):\n  %s\n  %s",
					door.name, f, got, base)
			}
		}
		switch door.name {
		case "dwell":
			var resp struct {
				Rows []struct {
					Key   string `json:"key"`
					Count int64  `json:"count"`
				} `json:"rows"`
			}
			testutil.MustNoErr(t, json.Unmarshal([]byte(base), &resp), "decode dwell")
			found := false
			for _, r := range resp.Rows {
				if r.Key == "transit" {
					found = true
					if r.Count != 2 {
						t.Errorf("dwell transit count = %d, want 2 (both stations)", r.Count)
					}
				}
			}
			if !found {
				t.Errorf("dwell has no transit row: %s", base)
			}
		case "faults":
			var resp struct {
				Stats struct {
					Outcomes []struct {
						Count int64 `json:"count"`
					} `json:"outcomes"`
					ByRobot []json.RawMessage `json:"by_robot"`
				} `json:"stats"`
			}
			testutil.MustNoErr(t, json.Unmarshal([]byte(base), &resp), "decode faults")
			var n int64
			for _, o := range resp.Stats.Outcomes {
				n += o.Count
			}
			if n != 2 || len(resp.Stats.ByRobot) != 2 {
				t.Errorf("faults = %d faults over %d robots, want 2 over 2: %s", n, len(resp.Stats.ByRobot), base)
			}
		}
	}
}

// TestPinTestOrders_ReleaseAnswersReleasedWhateverHappened: apiDirectOrderRelease
// (handlers_test_orders_direct.go:189) calls the dispatcher, which answers on the
// wire (an outbox order.error), and then writes 200 "released" regardless. The
// page reads res.ok (test-orders.js:398-412), so it toasts "Released" for an
// order Core does not have. TODAY: 200 {"order_uuid":…,"status":"released"}.
func TestPinTestOrders_ReleaseAnswersReleasedWhateverHappened(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiDirectOrderRelease, "/api/test-orders/direct/release",
		map[string]any{"order_uuid": "pin-no-such-order"})
	want := "{\"order_uuid\":\"pin-no-such-order\",\"status\":\"released\"}\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("release of an unknown order = %d %q, want 200 %q", rec.Code, rec.Body.String(), want)
	}
}

// TestPinTestOrders_ComplexSubmitAnswersOKWhateverHappened: the direct complex
// submit (handlers_test_orders_direct.go:106) hands the request to the
// dispatcher and answers 200 with the uuids it made up, whether or not an order
// was created. TODAY: a sequential submit naming nodes that do not exist is 200
// with one order uuid.
func TestPinTestOrders_ComplexSubmitAnswersOKWhateverHappened(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	rec := postJSON(t, h.apiDirectComplexOrderSubmit, "/api/test-orders/direct/complex", map[string]any{
		"cycle_mode": "sequential", "location": "PIN-NO-SUCH-NODE", "payload_code": "PIN-NO-SUCH-PAYLOAD",
		"inbound_source": "PIN-NO-SUCH-SRC", "outbound_destination": "PIN-NO-SUCH-DEST",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		CycleMode string `json:"cycle_mode"`
		Orders    []struct {
			Role      string `json:"role"`
			OrderUUID string `json:"order_uuid"`
		} `json:"orders"`
	}
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode")
	if resp.CycleMode != "sequential" || len(resp.Orders) != 1 || resp.Orders[0].OrderUUID == "" {
		t.Fatalf("body = %s, want sequential with one order uuid", rec.Body.String())
	}
	// TODAY: the answered uuid names no order at all — the dispatcher refused it
	// on the wire and the page toasts "Complex order created".
	if o, err := db.GetOrderByUUID(resp.Orders[0].OrderUUID); err == nil {
		t.Errorf("an order row exists for the answered uuid (status %q); today there is none", o.Status)
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
