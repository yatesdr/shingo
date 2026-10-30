//go:build docker

package www

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// Pins for the /inventory redesign (lane D, 2026-10-29), committed green at the
// base before the page changed. Predictions are in the work folder
// (PREDICTIONS-inventory-page-pins-2026-10-29.md).

// jsonKeys returns an object's keys, sorted.
func jsonKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pinLoaderHome makes a threshold-mode dedicated loader with one home for
// payload at a registered station, through the service door the page saves
// through, and returns the loader id and the home's node.
func pinLoaderHome(t *testing.T, h *Handlers, db *store.DB, station, nodeName, payload string, threshold int) (int64, *nodes.Node) {
	t.Helper()
	_, err := db.EnrollEdge(station, "", station)
	testutil.MustNoErr(t, err, "enroll edge")
	_, err = db.RegisterEdge(station, "pin-host", "pin-inst", "test", "", sql.NullInt64{})
	testutil.MustNoErr(t, err, "register edge")
	id, err := db.CreateLoader(loaders.Loader{Name: nodeName + "-LOADER", Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold})
	testutil.MustNoErr(t, err, "create loader")
	home := &nodes.Node{Name: nodeName, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(home), "create home node")
	testutil.MustNoErr(t, h.engine.LoaderService().SetHome(id, home.ID, payload, "", threshold), "set home")
	return id, home
}

// TestInventoryPin_ResponseShapes pins every key /inventory reads. At the base
// (a6fbd09a) that was two Core-computed reads: the per-payload rollup
// (monitor-totals: bin_uop, bindings{core_node_name, loader_id, station_id,
// threshold}, bucket_uop, description, ledger_negative, monitored, on_hand,
// payload_code, swap_contradiction, threshold) and the count ledger
// (ledger-exceptions). The page now reads one, /api/inventory/loop; the count
// ledger moved to /inventory/count-history without its daily series.
func TestInventoryPin_ResponseShapes(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-SHAPE")
	pinLoaderHome(t, h, db, "ST-SHAPE", "SHAPE-HOME", sd.Payload.Code, 50)
	_, err := db.Exec(`INSERT INTO process_styles (process_id, style_id, is_active) VALUES ('LINE-SHAPE', 'S1', true)`)
	testutil.MustNoErr(t, err, "seed a line's style")
	_, err = db.Exec(`INSERT INTO style_claims (process_id, style_id, core_node_name, role, swap_mode, payload_code, uop_capacity, reorder_point)
		VALUES ('LINE-SHAPE', 'S1', 'ALN_SHAPE', 'consume', 'single_robot', $1, 100, 20)`, sd.Payload.Code)
	testutil.MustNoErr(t, err, "seed a line's claim")

	rec := getPlain(t, h.apiInventoryLoop, "/api/inventory/loop")
	if rec.Code != http.StatusOK {
		t.Fatalf("loop: %d %s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &view), "decode loop")
	if got := strings.Join(jsonKeys(view), ","); got != "doubt,lead_days,rows,safety" {
		t.Errorf("loop keys = %s", got)
	}
	rows, _ := view["rows"].([]any)
	byKind := map[string]map[string]any{}
	for _, r := range rows {
		if m := r.(map[string]any); m["payload_code"] == sd.Payload.Code {
			byKind[m["kind"].(string)] = m
		}
	}
	if byKind["loader"] == nil || byKind["line"] == nil {
		t.Fatalf("want a loader row and a line row for %s: %s", sd.Payload.Code, rec.Body.String())
	}
	gotRow := strings.Join(jsonKeys(byKind["loader"]), ",")
	// confidence and inputs ride only when there is one (no suggestion here:
	// the window holds no orders); off_home because the seeded bin stands at
	// STORAGE-A1, not the home.
	wantRow := "beyond,bin_uop,bucket_uop,coming,completed_l1,cover_seconds,cycle_seconds,description," +
		"group,home_bins,home_capacity,in_loop,key,kind,layout,lead_seconds,loader_id,loader_name,look," +
		"node,off_home,pack,payload_code,position_node_id,provenance,reorder_point,state," +
		"suggested,threshold"
	t.Logf("PIN inventory/loop row keys=%s", gotRow)
	if gotRow != wantRow {
		t.Errorf("loop row keys = %s, want %s", gotRow, wantRow)
	}
	// A line row: role always; in_bin only with a bin of the part at the line
	// (none here), station only when the mirror names one.
	gotLine := strings.Join(jsonKeys(byKind["line"]), ",")
	wantLine := "beyond,bin_uop,bucket_uop,coming,completed_l1,cover_seconds,cycle_seconds,group,home_bins," +
		"home_capacity,key,kind,lead_seconds,look,node,pack,payload_code,provenance,reorder_point,role,state," +
		"suggested,threshold"
	t.Logf("PIN inventory-4/line row keys=%s", gotLine)
	if gotLine != wantLine {
		t.Errorf("line row keys = %s, want %s", gotLine, wantLine)
	}

	rec = getPlain(t, h.apiInventoryLedgerExceptions, "/api/inventory/ledger-exceptions")
	if rec.Code != http.StatusOK {
		t.Fatalf("ledger-exceptions: %d %s", rec.Code, rec.Body.String())
	}
	var lx map[string]any
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &lx), "decode ledger-exceptions")
	gotLx := strings.Join(jsonKeys(lx), ",")
	wantLx := "declared_full_stamps,delta_integrity,excursions,hold_drop_runs," +
		"negative_payloads,open_bins,report_divergences,since"
	t.Logf("PIN inventory/ledger-exceptions keys=%s", gotLx)
	if gotLx != wantLx {
		t.Errorf("ledger-exceptions keys = %s, want %s", gotLx, wantLx)
	}
}

// TestInventoryPin_ThresholdSave pins the save path the page drives: the
// set-home door writes bin_loader_homes, the re-derive carries the value into
// demand_registry, and the save leaves one audit row (I3; none at the base),
// which the page's provenance reads back. The monitor's immediate evaluation
// on a changed threshold is pinned by engine
// TestThresholdEngage_ThresholdChangeEvaluates.
func TestInventoryPin_ThresholdSave(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	id, home := pinLoaderHome(t, h, db, "ST-SAVE", "SAVE-HOME", sd.Payload.Code, 0)

	rec := postJSON(t, h.apiSetLoaderHome, "/api/loader/set-home", map[string]any{
		"loader_id": id, "position_node_id": home.ID, "payload_code": sd.Payload.Code, "uop_threshold": 120,
		"source": "calculated",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("set-home: %d %s", rec.Code, rec.Body.String())
	}
	var stored, derived, audit int
	testutil.MustNoErr(t, db.QueryRow(`SELECT uop_threshold FROM bin_loader_homes WHERE loader_id=$1 AND position_node_id=$2`,
		id, home.ID).Scan(&stored), "read home")
	testutil.MustNoErr(t, db.QueryRow(`SELECT replenish_uop_threshold FROM demand_registry
		WHERE station_id='ST-SAVE' AND core_node_name=$1 AND payload_code=$2`, home.Name, sd.Payload.Code).Scan(&derived),
		"read registry")
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM audit_log WHERE entity_type='loader_home' AND entity_id=$1`,
		home.ID).Scan(&audit), "count audit")
	t.Logf("PIN inventory/threshold-save home=%d registry=%d audit_rows=%d", stored, derived, audit)
	if stored != 120 || derived != 120 || audit != 1 {
		t.Errorf("save 0->120: home %d, registry %d, audit rows %d; want 120, 120, 1", stored, derived, audit)
	}
	var action, oldV, newV, actor string
	testutil.MustNoErr(t, db.QueryRow(`SELECT action, old_value, new_value, actor FROM audit_log
		WHERE entity_type='loader_home' AND entity_id=$1`, home.ID).Scan(&action, &oldV, &newV, &actor), "read audit")
	if action != "uop_threshold" || oldV != sd.Payload.Code+" 0" || newV != sd.Payload.Code+" 120 calculated" || actor != "system" {
		t.Errorf("audit row = %s %q -> %q by %q", action, oldV, newV, actor)
	}

	// The same value again writes nothing; the provenance reads the row back.
	rec = postJSON(t, h.apiSetLoaderHome, "/api/loader/set-home", map[string]any{
		"loader_id": id, "position_node_id": home.ID, "payload_code": sd.Payload.Code, "uop_threshold": 120,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("set-home again: %d %s", rec.Code, rec.Body.String())
	}
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM audit_log WHERE entity_type='loader_home' AND entity_id=$1`,
		home.ID).Scan(&audit), "count audit again")
	if audit != 1 {
		t.Errorf("an unchanged save wrote an audit row: %d rows", audit)
	}
	view := getPlain(t, h.apiInventoryLoop, "/api/inventory/loop")
	var body struct {
		Rows []struct {
			PayloadCode string `json:"payload_code"`
			Node        string `json:"node"`
			Provenance  *struct {
				Value   int    `json:"value"`
				Source  string `json:"source"`
				By      string `json:"by"`
				Current bool   `json:"current"`
			} `json:"provenance"`
		} `json:"rows"`
	}
	testutil.MustNoErr(t, json.Unmarshal(view.Body.Bytes(), &body), "decode loop")
	found := false
	for _, r := range body.Rows {
		if r.PayloadCode == sd.Payload.Code && r.Node == home.Name {
			found = true
			if r.Provenance == nil || r.Provenance.Value != 120 || r.Provenance.Source != "calculated" ||
				r.Provenance.By != "system" || !r.Provenance.Current {
				t.Errorf("provenance = %+v, want calculated 120 by system, current", r.Provenance)
			}
		}
	}
	if !found {
		t.Errorf("no loop row for %s at %s: %s", sd.Payload.Code, home.Name, view.Body.String())
	}
}
