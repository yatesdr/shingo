//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// half_loader_pins_docker_test.go — the Core doors the half loader reuses,
// pinned before the bare bin type existed and kept with the stage-1 type
// flagged bare: the doors do not read the flag.
//
// Stage 1 is an unloader window: CLEAR at a full carrier sends the loader's
// configured carrier type through apiBinClear's existing bin_type_code. Stage 2
// is another unloader window: PUSH AS <type> is the same CLEAR on a carrier that
// is already empty (714041ce). Both rest on apiBinClear stamping whatever
// existing type it is told, at a loader window, with no fence in the way.

// halfLoaderWindow builds a consume shared-window loader with one window and
// returns the window. The window's capability list and its node_bin_types are
// both set to DEFAULT only, so a stamp of any other type proves neither is a
// fence on the clear.
func halfLoaderWindow(t *testing.T, db *store.DB, sd *testdb.StandardData, name string) *nodes.Node {
	t.Helper()
	win := &nodes.Node{Name: name, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(win), "create window")
	id, err := db.CreateLoader(loaders.Loader{
		Name: name + "-UNLOADER", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator,
	})
	testutil.MustNoErr(t, err, "create loader")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: win.ID}), "window")
	testutil.MustNoErr(t, db.SetLoaderHomeBinTypes(id, win.ID, []int64{sd.BinType.ID}), "window capability")
	testutil.MustNoErr(t, db.SetNodeBinTypes(win.ID, []int64{sd.BinType.ID}), "node bin types")
	return win
}

func mintBinType(t *testing.T, db *store.DB, code string) *bins.BinType {
	t.Helper()
	bt := &bins.BinType{Code: code, Description: "half-loader pin"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type "+code)
	return bt
}

func mintBareType(t *testing.T, db *store.DB, code string) *bins.BinType {
	t.Helper()
	bt := &bins.BinType{Code: code, Description: "half-loader pin", Bare: true}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bare bin type "+code)
	return bt
}

// nodeBinsRow reads the node-bins row for one node, as the Edge does.
func nodeBinsRow(t *testing.T, h *Handlers, node string) map[string]any {
	t.Helper()
	rec := getPlain(t, h.apiTelemetryNodeBins, "/api/telemetry/node-bins?nodes="+url.QueryEscape(node))
	if rec.Code != http.StatusOK {
		t.Fatalf("node-bins status %d; body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&rows), "decode node-bins")
	if len(rows) != 1 {
		t.Fatalf("node-bins rows = %d, want 1", len(rows))
	}
	return rows[0]
}

// TestPinBinClear_DeclaredTypeOnAFullBinAtAConsumeLoaderWindow_StampsIt is the
// stage-1 CLEAR: a full carrier at an unloader window, cleared with a declared
// type that is neither the window's capability nor in its node_bin_types. The
// clear is accepted, the payload goes, and the declared type is stamped.
func TestPinBinClear_DeclaredTypeOnAFullBinAtAConsumeLoaderWindow_StampsIt(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	win := halfLoaderWindow(t, db, sd, "HLPIN-S1-W1")
	stage1 := mintBareType(t, db, "HLPIN-S1-CARRIER")
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, win.ID, "BIN-HLPIN-S1")

	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": win.Name, "bin_type_code": stage1.Code})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	after, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if after.BinTypeID != stage1.ID || after.BinTypeCode != stage1.Code {
		t.Errorf("bin type = %d/%s, want the declared %d/%s", after.BinTypeID, after.BinTypeCode, stage1.ID, stage1.Code)
	}
	if after.PayloadCode != "" || after.UOPRemaining != 0 {
		t.Errorf("payload %q uop %d, want the clear to empty it", after.PayloadCode, after.UOPRemaining)
	}
	// The row the Edge reads now says the carrier is bare, from the bt join.
	if row := nodeBinsRow(t, h, win.Name); row["bare"] != true || row["bin_type_code"] != stage1.Code {
		t.Errorf("node-bins row = %v, want bare=true and type %s", row, stage1.Code)
	}
}

// TestPinBinClear_ReClearOfAnEmptyCarrier_RestampsItsType is PUSH AS's Core
// half: a carrier already empty, standing at an unloader window, cleared with a
// declared type. It is re-stamped and its epoch moves on, exactly as a clear of
// a full carrier; nothing about the carrier's CURRENT type is consulted.
func TestPinBinClear_ReClearOfAnEmptyCarrier_RestampsItsType(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	win := halfLoaderWindow(t, db, sd, "HLPIN-S2-W1")
	stage1 := mintBareType(t, db, "HLPIN-S2-FROM")
	realType := mintBinType(t, db, "HLPIN-S2-REAL")
	bin := testdb.CreateBinAtNode(t, db, "", win.ID, "BIN-HLPIN-S2")
	_, err := db.Exec(`UPDATE bins SET bin_type_id=$1 WHERE id=$2`, stage1.ID, bin.ID)
	testutil.MustNoErr(t, err, "retype to stage-1 carrier")
	before, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin before")

	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": win.Name, "bin_type_code": realType.Code})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		DeltaEpoch int64 `json:"delta_epoch"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	after, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin after")
	if after.BinTypeCode != realType.Code {
		t.Errorf("bin type = %s, want the PUSH AS type %s", after.BinTypeCode, realType.Code)
	}
	if after.DeltaEpoch <= before.DeltaEpoch || resp.DeltaEpoch != after.DeltaEpoch {
		t.Errorf("epoch before %d after %d resp %d, want the clear to bump it and report it",
			before.DeltaEpoch, after.DeltaEpoch, resp.DeltaEpoch)
	}
	// Re-stamped, it is an ordinary carrier again: the row loses `bare`.
	if row := nodeBinsRow(t, h, win.Name); row["bare"] != nil {
		t.Errorf("node-bins row = %v, want no bare key once PUSH AS re-stamped a real type", row)
	}
}

// TestPinNodeBins_RowKeys pins the node-bins row's key set for an ordinary
// carrier. The row gains `bare`, omitempty, so an ordinary carrier's row must
// stay byte-identical.
func TestPinNodeBins_RowKeys(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-HLPIN-KEYS")

	rec := getPlain(t, h.apiTelemetryNodeBins, "/api/telemetry/node-bins?nodes="+url.QueryEscape(sd.StorageNode.Name))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&rows), "decode")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	keys := make([]string, 0, len(rows[0]))
	for k := range rows[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"bin_id", "bin_label", "bin_type_code", "delta_epoch", "manifest", "manifest_confirmed",
		"node_name", "occupied", "uop_remaining"}
	if len(keys) != len(want) {
		t.Fatalf("row keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("row keys = %v, want %v", keys, want)
		}
	}
}

// TestPinBinTypeUpdate_AFieldTheFormOmitsIsOverwritten pins the hazard the
// bare checkbox's hidden companion field answers: the edit door is a
// full-record write, so a field the POST does not carry is written as its zero
// value. Here it is required_robot_group; an unchecked checkbox is the same
// shape, because a browser sends nothing for one.
func TestPinBinTypeUpdate_AFieldTheFormOmitsIsOverwritten(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	bt := &bins.BinType{Code: "HLPIN-RG", Description: "d", RequiredRobotGroup: "RG-HEAVY"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create")

	rec := postForm(t, h.handleBinTypeUpdate, "/bin-types/update", url.Values{
		"id": {strconv.FormatInt(bt.ID, 10)}, "code": {bt.Code}, "description": {bt.Description},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d; body=%s", rec.Code, rec.Body.String())
	}
	got, err := db.GetBinType(bt.ID)
	testutil.MustNoErr(t, err, "reread")
	if got.RequiredRobotGroup != "" {
		t.Errorf("required_robot_group = %q, want it blanked by the omitting POST (the full-record hazard)",
			got.RequiredRobotGroup)
	}
}

// TestPinLoaderUpdate_AnOmittedBooleanReadsFalse pins push 1's accept_partials
// contract on the JSON loader door: absent reads false. The loader's bare type
// is set through the same full-record update.
func TestPinLoaderUpdate_AnOmittedBooleanReadsFalse(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	id, err := db.CreateLoader(loaders.Loader{
		Name: "HLPIN-UNL", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator, AcceptPartials: true,
	})
	testutil.MustNoErr(t, err, "create loader")

	rec := postJSON(t, h.apiUpdateLoader, "/api/loader/update", map[string]any{"id": id, "name": "HLPIN-UNL"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rec.Code, rec.Body.String())
	}
	l, err := db.GetLoader(id)
	testutil.MustNoErr(t, err, "reread")
	if l.AcceptPartials {
		t.Error("accept_partials survived an update that omitted it; the door's contract is absent = false")
	}
}

// TestPinPayloadBinTypes_AnyTypeCanBeListedAndReachesTheCatalog pins, at base,
// that a payload rule may name any carrier type, and that the node list's
// payload→carrier catalog (the Edge's PUSH AS / dunnage picker source,
// ListPayloadBinTypeMappings) carries every rule row.
func TestPinPayloadBinTypes_AnyTypeCanBeListedAndReachesTheCatalog(t *testing.T) {
	t.Parallel()
	_, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	stage1 := mintBinType(t, db, "HLPIN-CAT")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(sd.Payload.ID, []int64{sd.BinType.ID, stage1.ID}), "set rules")

	pairs, err := db.ListPayloadBinTypeMappings()
	testutil.MustNoErr(t, err, "catalog")
	seen := map[string]bool{}
	for _, p := range pairs {
		if p[0] == sd.Payload.Code {
			seen[p[1]] = true
		}
	}
	if !seen[sd.BinType.Code] || !seen[stage1.Code] {
		t.Errorf("catalog for %s = %v, want both %s and %s", sd.Payload.Code, seen, sd.BinType.Code, stage1.Code)
	}
}

// TestPinListBinTypes_Keys pins the loader admin's carrier catalog row. It
// carries `bare`, which the unloader's bare-type select and the carrier-mix
// picker filter on.
func TestPinListBinTypes_Keys(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	mintBinType(t, db, "HLPIN-LIST")
	rec := getPlain(t, h.apiListBinTypes, "/api/bin-types")
	var resp struct {
		BinTypes []map[string]any `json:"bin_types"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	for _, row := range resp.BinTypes {
		if row["code"] != "HLPIN-LIST" {
			continue
		}
		if len(row) != 4 || row["id"] == nil || row["description"] == nil || row["bare"] != false {
			t.Errorf("row = %v, want exactly id, code, description, bare=false", row)
		}
		return
	}
	t.Fatalf("HLPIN-LIST not listed: %v", resp.BinTypes)
}
