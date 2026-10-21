package www

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/engine"
	"shingoedge/store"
	"shingoedge/uop"
)

// runtimeRow is a home's whole runtime row, column by column: count, bin
// pointer, stamp, identity and both order pointers.
func runtimeRow(t *testing.T, db *store.DB, nodeID int64) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM process_node_runtime_states WHERE process_node_id = ?`, nodeID)
	testutil.MustNoErr(t, err, "read runtime row")
	defer rows.Close()
	cols, err := rows.Columns()
	testutil.MustNoErr(t, err, "runtime columns")
	out := map[string]string{}
	n := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		testutil.MustNoErr(t, rows.Scan(ptrs...), "scan runtime row")
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			out[c] = fmt.Sprintf("%v", vals[i])
		}
		n++
	}
	testutil.MustNoErr(t, rows.Err(), "runtime rows")
	if n != 1 {
		t.Fatalf("home %d has %d runtime rows, want 1", nodeID, n)
	}
	return out
}

// everyOrder is the whole orders table: each row's id, status, ends, pending
// intent and node.
func everyOrder(t *testing.T, db *store.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT id, status, source_node, delivery_node, pending_intent,
		COALESCE(process_node_id, 0) FROM orders ORDER BY id`)
	testutil.MustNoErr(t, err, "read orders")
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, node int64
		var status, src, dst, intent string
		testutil.MustNoErr(t, rows.Scan(&id, &status, &src, &dst, &intent, &node), "scan order")
		fmt.Fprintf(&b, "order %d %s %s->%s intent=%q node=%d\n", id, status, src, dst, intent, node)
	}
	testutil.MustNoErr(t, rows.Err(), "order rows")
	return b.String()
}

func nodeAt(t *testing.T, db *store.DB, coreNode string) int64 {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT id FROM process_nodes WHERE core_node_name = ?`, coreNode).Scan(&id),
		"find node "+coreNode)
	return id
}

func viewETag(t *testing.T, rig *binsRig, code string) string {
	t.Helper()
	resp := doRequest(t, rig.router, "GET", "/api/operator-stations/"+itoa(rig.stations[code])+"/view", nil, nil)
	defer resp.Body.Close()
	assertStatus(t, resp, http.StatusOK)
	return resp.Header.Get("ETag")
}

type clearAnswer func(protocol.ClearHomeRequest) (int, protocol.ClearHomeResponse)

func clearChosen(arm string) clearAnswer {
	return func(protocol.ClearHomeRequest) (int, protocol.ClearHomeResponse) {
		r := protocol.ClearHomeResponse{Status: "ok", Arm: arm, BinID: binsRigRows["SMN_H1"].BinID, DeltaEpoch: clearEpoch}
		if arm == protocol.ClearArmNone {
			r.Detail = "the partial stays on its spot for now"
		} else {
			r.ParentUUID = "core-clear-1"
		}
		return http.StatusOK, r
	}
}

func clearRefusedBy(status int, sentence string) clearAnswer {
	return func(protocol.ClearHomeRequest) (int, protocol.ClearHomeResponse) {
		return status, protocol.ClearHomeResponse{Status: "error", Error: sentence}
	}
}

// TestClearLoaderHome_TheDoorPerCoresArm: the tap is one request to Core's
// clear door, naming the home and the station, and nothing on the Edge
// creates or arms anything. Per Core's answer:
//
//   - spot, demand, none: 200 with the board's one sentence for what Core is
//     doing (the none arm's is the tote-stacks tap's, engine.HomeClearStays),
//     and the zero recorded as a CLEAR records it — count 0, Core's new stamp
//     on the bound carrier, known-empty by the operator — and nothing else on
//     the runtime row; the board's view moves;
//   - Core's one-live-clear fence: 409 with Core's sentence (the confirm shows
//     it in place of its buttons), nothing recorded;
//   - a node Core cannot clear: 400 with Core's sentence (Core is the one
//     validator), nothing recorded.
func TestClearLoaderHome_TheDoorPerCoresArm(t *testing.T) {
	cases := []struct {
		name   string
		core   clearAnswer
		status int
		key    string // the answer's JSON key holding the sentence
		says   string
	}{
		{"spot", clearChosen(protocol.ClearArmSpot), http.StatusOK, "message",
			"Full recorded · Shingo takes the empty to a buffer spot and brings the partial home"},
		{"demand", clearChosen(protocol.ClearArmDemand), http.StatusOK, "message",
			"Full recorded · a waiting empty pull takes the bin and Shingo brings the partial home"},
		{"none", clearChosen(protocol.ClearArmNone), http.StatusOK, "message", engine.HomeClearStays},
		{"fence", clearRefusedBy(http.StatusConflict, "SMN_H1 is already being cleared"), http.StatusConflict,
			"error", "SMN_H1 is already being cleared"},
		{"not a home", clearRefusedBy(http.StatusBadRequest, "SMN_H1 has no bin with parts to clear"),
			http.StatusBadRequest, "error", "SMN_H1 has no bin with parts to clear"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rig := newBinsRig(t)
			rig.eng.SetInventoryDeltaSink(uop.New(rig.db, "test.station", rig.db, rig.db))
			rig.core.homeClear = c.core
			home := nodeAt(t, rig.db, "SMN_H1")
			_, err := rig.db.EnsureProcessNodeRuntime(home)
			testutil.MustNoErr(t, err, "ensure runtime")
			bin := binsRigRows["SMN_H1"].BinID
			testutil.MustNoErr(t, rig.db.SetProcessNodeRuntimeWithBinAndEpoch(home, nil, &bin, 3, 12), "seed the home's full bin")
			before := runtimeRow(t, rig.db, home)
			ordersBefore := everyOrder(t, rig.db)
			etagBefore := viewETag(t, rig, "LDR")

			rig.core.take()
			resp := doRequest(t, rig.router, "POST", "/api/process-nodes/"+itoa(home)+"/clear-loader-home", map[string]any{}, nil)
			assertStatus(t, resp, c.status)
			assertJSONPath(t, resp, c.key, c.says)

			if got, want := jsonOf(t, rig.core.take()), `{"/api/telemetry/clear-home":1}`; got != want {
				t.Errorf("Core calls per tap = %s, want %s: the tap is one request to Core's clear door", got, want)
			}
			if len(rig.core.homeClears) != 1 || rig.core.homeClears[0].HomeNode != "SMN_H1" || rig.core.homeClears[0].Station != rig.eng.AppConfig().StationID() {
				t.Errorf("Core was asked %+v, want one clear of SMN_H1 naming the station", rig.core.homeClears)
			}
			after := runtimeRow(t, rig.db, home)
			want := map[string]string{}
			if c.status == http.StatusOK {
				want = map[string]string{"remaining_uop_cached": "0", "active_bin_epoch": fmt.Sprint(clearEpoch),
					"lineside_payload_code": "", "lineside_payload_known": "1", "lineside_source": "operator"}
			}
			free := map[string]bool{"lineside_at": true, "updated_at": true}
			for col, v := range after {
				if w, ok := want[col]; ok {
					if v != w {
						t.Errorf("%s after the tap = %q, want %q (what a CLEAR records)", col, v, w)
					}
					continue
				}
				if !free[col] && v != before[col] {
					t.Errorf("%s changed from %q to %q", col, before[col], v)
				}
			}
			if got := everyOrder(t, rig.db); got != ordersBefore {
				t.Errorf("the tap changed the Edge's orders (Core authors the clear's):\nbefore:\n%safter:\n%s", ordersBefore, got)
			}
			if moved := viewETag(t, rig, "LDR") != etagBefore; moved != (c.status == http.StatusOK) {
				t.Errorf("the board's view moved=%v after a %d tap", moved, c.status)
			}
		})
	}
}

// TestClearLoaderHome_CoreUnreachableIs502: a failure reaching Core is not a
// refusal: 502 with the reason, and the door called the engine once.
func TestClearLoaderHome_CoreUnreachableIs502(t *testing.T) {
	h, router := newTestHandlers(t)
	stub := h.engine.(*stubEngine)
	stub.clearLoaderHomeErr = errors.New("clear-home request failed: connection refused")
	router.Post("/api/process-nodes/{id}/clear-loader-home", h.apiClearLoaderHome)
	resp := doRequest(t, router, "POST", "/api/process-nodes/7/clear-loader-home", map[string]string{}, nil)
	assertStatus(t, resp, http.StatusBadGateway)
	assertJSONPath(t, resp, "error", "clear-home request failed: connection refused")
	if stub.clearLoaderHomeCalls != 1 {
		t.Errorf("the door called the engine %d times, want once", stub.clearLoaderHomeCalls)
	}
}
