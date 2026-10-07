//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/engine"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// bins_pins_test.go — P0 pins for the Core Bins page's server side (lane F's
// Bins items and LC12), taken at 5c0beb74 before either lane changes source.
//
// Each pin states what the server answers TODAY. The predicted post-change
// value of every case is in the evidence folder's predictions/p0-bins-f.md;
// a case that moves must move under its label (LC12, or the cycle-count bug),
// never for any other reason.
//
// What these pin, in one place:
//   - apiBinAction x all 16 verbs: input -> stored row -> response. Pinned bare
//     {"status":"ok"} at 5c0beb74; LC12 moved it: the answer keeps
//     "status":"ok" and carries the bin's history-free detail (no audit).
//   - apiBulkBinAction: {"results":[{id, ok, error}]}; LC12 adds each existing
//     bin's history-free detail to its result.
//   - apiBinDetail: the key set, and that `audit` is the bin's whole history
//     (unchanged by LC12); ?history=0 is the same answer without the audit.
//   - record_count: a typed value, a stale value (the count moved after the page
//     loaded) and a refused bin.

// pinBinPost drives apiBinAction and returns the status code, the decoded body
// and the raw body.
func pinBinPost(t *testing.T, h *Handlers, id int64, action string, params any) (int, map[string]any, string) {
	t.Helper()
	body := map[string]any{"id": id, "action": action}
	if params != nil {
		body["params"] = params
	}
	rec := postJSON(t, h.apiBinAction, "/api/bins/action", body)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%s: decode body %q: %v", action, rec.Body.String(), err)
	}
	return rec.Code, got, rec.Body.String()
}

// pinBinDetail drives apiBinDetail for one bin and returns the decoded body.
func pinBinDetail(t *testing.T, h *Handlers, id int64) map[string]json.RawMessage {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/bins/detail?id="+strconv.FormatInt(id, 10), nil)
	rec := httptest.NewRecorder()
	h.apiBinDetail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: status %d, body %s", rec.Code, rec.Body.String())
	}
	var got map[string]json.RawMessage
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &got), "decode detail")
	return got
}

func pinKeys(m map[string]json.RawMessage) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// pinEchoTable reads ECHOES from static/pages/bins-echo.js: how many
// bin-update events the page expects a successful verb to emit for its bin.
// The page drops exactly that many as the echo of its own action, so the table
// must match what the handlers really emit (TestPinBins_ActionEveryVerb).
func pinEchoTable(t *testing.T) map[string]int {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("static", "pages", "bins-echo.js"))
	testutil.MustNoErr(t, err, "read bins-echo.js")
	block := regexp.MustCompile(`(?s)export var ECHOES = \{(.*?)\};`).FindSubmatch(src)
	if block == nil {
		t.Fatal("bins-echo.js: no ECHOES table")
	}
	out := map[string]int{}
	for _, m := range regexp.MustCompile(`(\w+):\s*(\d+)`).FindAllSubmatch(block[1], -1) {
		n, err := strconv.Atoi(string(m[2]))
		out[string(m[1])] = testutil.Must(t, n, err, "bins-echo.js: echo count "+string(m[1]))
	}
	return out
}

// pinCountBinUpdates counts the bin-update events emitted for bin id while fn
// runs. The bus is synchronous (eventbus.Emit), so an emit inside the handler
// is counted before fn returns.
func pinCountBinUpdates(h *Handlers, id int64, fn func()) int {
	var mu sync.Mutex
	n := 0
	sub := h.engine.EventBus().SubscribeTypes(func(evt engine.Event) {
		if ev, ok := evt.Payload.(engine.BinUpdatedEvent); ok && ev.BinID == id {
			mu.Lock()
			n++
			mu.Unlock()
		}
	}, engine.EventBinUpdated)
	fn()
	h.engine.EventBus().Unsubscribe(sub)
	mu.Lock()
	defer mu.Unlock()
	return n
}

// pinHistoryFreeKeys is the key set of a history-free answer for a bin with a
// payload and no claim (LC12): the detail's keys without `audit`.
const pinHistoryFreeKeys = "bin,manifest,recent_orders,template"

// pinRequireBinAnswer is what every successful verb answers since LC12: the
// "status":"ok" it always carried, plus the bin as it now is, without history.
// The answer's bin must be the stored row (id, status, count, label).
func pinRequireBinAnswer(t *testing.T, db *store.DB, id int64, raw string) {
	t.Helper()
	var got map[string]json.RawMessage
	testutil.MustNoErr(t, json.Unmarshal([]byte(raw), &got), "decode answer")
	if string(got["status"]) != `"ok"` {
		t.Errorf("answer status = %s, want \"ok\"; body %s", got["status"], raw)
	}
	if _, ok := got["audit"]; ok {
		t.Error("answer carries the audit; LC12's answer is history-free")
	}
	for _, k := range []string{"bin", "manifest", "recent_orders"} {
		if _, ok := got[k]; !ok {
			t.Errorf("answer has no %q; body %s", k, raw)
		}
	}
	var b struct {
		ID           int64  `json:"id"`
		Status       string `json:"status"`
		UOPRemaining int    `json:"uop_remaining"`
		Label        string `json:"label"`
	}
	testutil.MustNoErr(t, json.Unmarshal(got["bin"], &b), "decode answer bin")
	stored := testdb.RequireBin(t, db, id)
	if b.ID != id || b.Status != string(stored.Status) || b.UOPRemaining != stored.UOPRemaining || b.Label != stored.Label {
		t.Errorf("answer bin = %+v, stored = id %d status %q uop %d label %q", b, id, stored.Status, stored.UOPRemaining, stored.Label)
	}
}

// pinEmptyBin creates a carrier with no payload and no manifest at the node.
func pinEmptyBin(t *testing.T, db *store.DB, nodeID int64, label string) *bins.Bin {
	t.Helper()
	bt, err := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, err, "default bin type")
	b := &bins.Bin{BinTypeID: bt.ID, Label: label, NodeID: &nodeID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(b), "create empty bin")
	got, err := db.GetBin(b.ID)
	testutil.MustNoErr(t, err, "read empty bin")
	return got
}

// TestPinBins_ActionEveryVerb is the 16-verb table. Each row prepares the bin,
// posts the verb through the HTTP door, and pins the stored row and the
// response. The response was the bare ok for all 16; LC12 moved it to ok plus
// the bin's history-free detail.
func TestPinBins_ActionEveryVerb(t *testing.T) {
	t.Parallel()

	type row struct {
		verb   string
		prep   func(t *testing.T, db *store.DB, sd *testdb.StandardData, b *bins.Bin) int64
		params func(sd *testdb.StandardData) any
		stored func(t *testing.T, db *store.DB, sd *testdb.StandardData, id int64)
	}
	same := func(_ *testing.T, _ *store.DB, _ *testdb.StandardData, b *bins.Bin) int64 { return b.ID }
	status := func(want string) func(*testing.T, *store.DB, *testdb.StandardData, int64) {
		return func(t *testing.T, db *store.DB, _ *testdb.StandardData, id int64) {
			got := testdb.RequireBin(t, db, id)
			if string(got.Status) != want {
				t.Errorf("stored status = %q, want %q", got.Status, want)
			}
		}
	}

	rows := []row{
		{verb: "activate",
			prep: func(t *testing.T, db *store.DB, _ *testdb.StandardData, b *bins.Bin) int64 {
				testutil.MustNoErr(t, db.UpdateBinStatus(b.ID, "flagged"), "flag first")
				return b.ID
			},
			stored: status("available")},
		{verb: "flag", prep: same, stored: status("flagged")},
		{verb: "maintenance", prep: same, stored: status("maintenance")},
		{verb: "retire", prep: same,
			stored: func(t *testing.T, db *store.DB, _ *testdb.StandardData, id int64) {
				got := testdb.RequireBin(t, db, id)
				if string(got.Status) != "retired" || got.NodeID != nil {
					t.Errorf("stored = status %q node %v, want retired and no node", got.Status, got.NodeID)
				}
			}},
		{verb: "release",
			prep: func(t *testing.T, db *store.DB, _ *testdb.StandardData, b *bins.Bin) int64 {
				testutil.MustNoErr(t, db.StageBin(b.ID, nil), "stage first")
				return b.ID
			},
			stored: status("available")},
		{verb: "stage", prep: same, stored: status("staged")},
		{verb: "lock", prep: same,
			params: func(*testdb.StandardData) any { return map[string]any{"actor": "pin-locker"} },
			stored: func(t *testing.T, db *store.DB, _ *testdb.StandardData, id int64) {
				got := testdb.RequireBin(t, db, id)
				if !got.Locked || got.LockedBy != "pin-locker" {
					t.Errorf("stored = locked %v by %q, want locked by pin-locker", got.Locked, got.LockedBy)
				}
			}},
		{verb: "unlock",
			prep: func(t *testing.T, db *store.DB, _ *testdb.StandardData, b *bins.Bin) int64 {
				testutil.MustNoErr(t, db.LockBin(b.ID, "someone"), "lock first")
				return b.ID
			},
			stored: func(t *testing.T, db *store.DB, _ *testdb.StandardData, id int64) {
				got := testdb.RequireBin(t, db, id)
				if got.Locked || got.LockedBy != "" {
					t.Errorf("stored = locked %v by %q, want unlocked", got.Locked, got.LockedBy)
				}
			}},
		{verb: "load_payload",
			prep: func(t *testing.T, db *store.DB, sd *testdb.StandardData, _ *bins.Bin) int64 {
				return pinEmptyBin(t, db, sd.LineNode.ID, "PIN-LOAD").ID
			},
			params: func(sd *testdb.StandardData) any {
				return map[string]any{"payload_code": sd.Payload.Code, "uop_override": 40}
			},
			stored: func(t *testing.T, db *store.DB, sd *testdb.StandardData, id int64) {
				got := testdb.RequireBin(t, db, id)
				if got.PayloadCode != sd.Payload.Code || got.UOPRemaining != 40 {
					t.Errorf("stored = payload %q uop %d, want %q 40", got.PayloadCode, got.UOPRemaining, sd.Payload.Code)
				}
			}},
		{verb: "clear", prep: same,
			stored: func(t *testing.T, db *store.DB, _ *testdb.StandardData, id int64) {
				got := testdb.RequireBin(t, db, id)
				if got.PayloadCode != "" || got.UOPRemaining != 0 {
					t.Errorf("stored = payload %q uop %d, want empty and 0", got.PayloadCode, got.UOPRemaining)
				}
			}},
		{verb: "confirm_manifest",
			prep: func(t *testing.T, db *store.DB, _ *testdb.StandardData, b *bins.Bin) int64 {
				testutil.MustNoErr(t, db.UnconfirmBinManifest(b.ID), "unconfirm first")
				return b.ID
			},
			stored: func(t *testing.T, db *store.DB, _ *testdb.StandardData, id int64) {
				if !testdb.RequireBin(t, db, id).ManifestConfirmed {
					t.Error("stored manifest_confirmed = false, want true")
				}
			}},
		{verb: "unconfirm_manifest", prep: same,
			stored: func(t *testing.T, db *store.DB, _ *testdb.StandardData, id int64) {
				if testdb.RequireBin(t, db, id).ManifestConfirmed {
					t.Error("stored manifest_confirmed = true, want false")
				}
			}},
		{verb: "move", prep: same,
			params: func(sd *testdb.StandardData) any { return map[string]any{"node_id": sd.LineNode.ID} },
			stored: func(t *testing.T, db *store.DB, sd *testdb.StandardData, id int64) {
				testdb.RequireBinAtNode(t, db, id, sd.LineNode.ID)
			}},
		{verb: "record_count", prep: same,
			params: func(*testdb.StandardData) any { return map[string]any{"actual_uop": 95, "actor": "pin-counter"} },
			stored: func(t *testing.T, db *store.DB, _ *testdb.StandardData, id int64) {
				got := testdb.RequireBin(t, db, id)
				if got.UOPRemaining != 95 || got.LastCountedBy != "pin-counter" {
					t.Errorf("stored = uop %d by %q, want 95 by pin-counter", got.UOPRemaining, got.LastCountedBy)
				}
			}},
		{verb: "add_note", prep: same,
			params: func(*testdb.StandardData) any {
				return map[string]any{"note_type": "general", "message": "pin note", "actor": "pin-noter"}
			},
			stored: func(t *testing.T, db *store.DB, _ *testdb.StandardData, id int64) {
				entries, err := db.ListEntityAudit("bin", id)
				testutil.MustNoErr(t, err, "audit")
				for _, e := range entries {
					if strings.HasPrefix(e.Action, "note") && strings.Contains(e.NewValue, "pin note") {
						return
					}
				}
				t.Errorf("no note audit row carrying the message; got %v", auditActions(entries))
			}},
		{verb: "update", prep: same,
			params: func(*testdb.StandardData) any {
				return map[string]any{"label": "PIN-RELABEL", "description": "pin desc"}
			},
			stored: func(t *testing.T, db *store.DB, _ *testdb.StandardData, id int64) {
				got := testdb.RequireBin(t, db, id)
				if got.Label != "PIN-RELABEL" || got.Description != "pin desc" {
					t.Errorf("stored = label %q desc %q, want PIN-RELABEL / pin desc", got.Label, got.Description)
				}
			}},
	}
	if len(rows) != 16 {
		t.Fatalf("table has %d verbs, bin_actions.go:20-35 has 16", len(rows))
	}
	echoes := pinEchoTable(t)
	if len(echoes) != 16 {
		t.Fatalf("bins-echo.js ECHOES has %d verbs, want 16: %v", len(echoes), echoes)
	}

	for _, r := range rows {
		r := r
		t.Run(r.verb, func(t *testing.T) {
			t.Parallel()
			h, db, sd, b := setupBinForAction(t)
			id := r.prep(t, db, sd, b)
			var params any
			if r.params != nil {
				params = r.params(sd)
			}
			var code int
			var body map[string]any
			var raw string
			emitted := pinCountBinUpdates(h, id, func() { code, body, raw = pinBinPost(t, h, id, r.verb, params) })
			// The page counts this verb's echoes (bins-echo.js ECHOES); the
			// count must be what the handler emits.
			if want, ok := echoes[r.verb]; !ok || emitted != want {
				t.Errorf("%s emitted %d bin-update events for its bin; bins-echo.js ECHOES says %d (present %v)", r.verb, emitted, want, ok)
			}
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", code, raw)
			}
			// LC12: was the bare ok with no bin; now ok plus the bin as stored,
			// without history.
			if body["status"] != "ok" {
				t.Errorf("status = %v, want ok", body["status"])
			}
			pinRequireBinAnswer(t, db, id, raw)
			r.stored(t, db, sd, id)
		})
	}
}

// TestPinBins_ActionRefusals pins the two refusals the door itself makes and
// the shape of a verb's own refusal.
func TestPinBins_ActionRefusals(t *testing.T) {
	t.Parallel()
	h, db, _, b := setupBinForAction(t)

	code, _, raw := pinBinPost(t, h, 999999, "flag", nil)
	if code != http.StatusNotFound || raw != "{\"error\":\"bin not found\"}\n" {
		t.Errorf("unknown bin = %d %q, want 404 bin not found", code, raw)
	}
	code, _, raw = pinBinPost(t, h, b.ID, "bogus", nil)
	if code != http.StatusBadRequest || raw != "{\"error\":\"unknown action: bogus\"}\n" {
		t.Errorf("unknown verb = %d %q, want 400 unknown action: bogus", code, raw)
	}
	// A verb's own refusal: release of a bin that is not staged. A refused
	// verb emits no bin-update (the page settles a refusal as zero echoes).
	emitted := pinCountBinUpdates(h, b.ID, func() { code, _, raw = pinBinPost(t, h, b.ID, "release", nil) })
	if emitted != 0 {
		t.Errorf("refused release emitted %d bin-update events, want 0", emitted)
	}
	want := fmt.Sprintf("{\"error\":\"bin %d is not staged\"}\n", b.ID)
	if code != http.StatusBadRequest || raw != want {
		t.Errorf("release of an available bin = %d %q, want 400 %q", code, raw, want)
	}
	if got := testdb.RequireBin(t, db, b.ID); string(got.Status) != "available" {
		t.Errorf("status after refused release = %q, want available", got.Status)
	}
}

// TestPinBins_BulkAction pins apiBulkBinAction's answer: one result per id,
// {id, ok} or {id, error}; since LC12 each result of a bin that exists also
// carries that bin's history-free detail (an unknown id carries none). A locked bin is refused per row unless the verb
// is unlock; an unknown id is "not found"; more than 100 ids is refused whole.
func TestPinBins_BulkAction(t *testing.T) {
	t.Parallel()
	h, db, sd, b := setupBinForAction(t)
	locked := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "PIN-BULK-LOCKED")
	testutil.MustNoErr(t, db.LockBin(locked.ID, "pin-holder"), "lock")

	rec := postJSON(t, h.apiBulkBinAction, "/api/bins/bulk-action", map[string]any{
		"ids": []int64{b.ID, locked.ID, 999999}, "action": "flag",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	// Was exactly {"results":[{"id":A,"ok":true},{"id":B,"ok":false,"error":
	// "locked by pin-holder"},{"id":999999,"ok":false,"error":"not found"}]}.
	// LC12: the same id/ok/error per row, in the same order, and the two bins
	// that exist carry their detail; the unknown id is byte-identical.
	var bulk struct {
		Results []map[string]json.RawMessage `json:"results"`
	}
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &bulk), "decode bulk")
	if len(bulk.Results) != 3 {
		t.Fatalf("bulk results = %d, want 3; body %s", len(bulk.Results), rec.Body.String())
	}
	type wantRow struct {
		id     int64
		ok     string
		errMsg string
		status string // the detail's bin status; "" = no detail
	}
	for i, w := range []wantRow{
		{b.ID, "true", "", "flagged"},
		{locked.ID, "false", `"locked by pin-holder"`, "available"},
		{999999, "false", `"not found"`, ""},
	} {
		r := bulk.Results[i]
		if string(r["id"]) != strconv.FormatInt(w.id, 10) || string(r["ok"]) != w.ok || string(r["error"]) != w.errMsg {
			t.Errorf("result %d = id %s ok %s error %s, want %d %s %s", i, r["id"], r["ok"], r["error"], w.id, w.ok, w.errMsg)
		}
		if _, ok := r["audit"]; ok {
			t.Errorf("result %d carries the audit; LC12's detail is history-free", i)
		}
		if w.status == "" {
			if len(r) != 3 {
				t.Errorf("unknown id result = %v, want only id, ok, error", r)
			}
			continue
		}
		var rb struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		}
		testutil.MustNoErr(t, json.Unmarshal(r["bin"], &rb), "decode result bin")
		if rb.ID != w.id || rb.Status != w.status {
			t.Errorf("result %d bin = %+v, want id %d status %s", i, rb, w.id, w.status)
		}
		if _, ok := r["recent_orders"]; !ok {
			t.Errorf("result %d has no recent_orders", i)
		}
	}
	if !strings.HasSuffix(rec.Body.String(), `{"id":999999,"ok":false,"error":"not found"}]}`+"\n") {
		t.Errorf("unknown id row moved: %s", rec.Body.String())
	}
	if got := testdb.RequireBin(t, db, b.ID); string(got.Status) != "flagged" {
		t.Errorf("bulk flag: stored %q, want flagged", got.Status)
	}
	if got := testdb.RequireBin(t, db, locked.ID); string(got.Status) != "available" {
		t.Errorf("bulk flag of a locked bin: stored %q, want available (refused)", got.Status)
	}

	ids := make([]int64, 101)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	rec = postJSON(t, h.apiBulkBinAction, "/api/bins/bulk-action", map[string]any{"ids": ids, "action": "flag"})
	if rec.Code != http.StatusBadRequest || rec.Body.String() != "{\"error\":\"ids must contain 1-100 entries\"}\n" {
		t.Errorf("101 ids = %d %q, want 400 ids must contain 1-100 entries", rec.Code, rec.Body.String())
	}
}

// TestPinBins_DetailKeysAndFullAudit pins apiBinDetail's JSON: the key set for
// an unclaimed bin with a payload, and a claimed one; and that `audit` is the
// bin's WHOLE history, no limit (handlers_bins.go:347, store/audit/audit.go:41).
// The default answer must not change under LC12 (GET /api/bins/detail is
// public); the history-free read is an added query parameter.
func TestPinBins_DetailKeysAndFullAudit(t *testing.T) {
	t.Parallel()
	h, db, sd, b := setupBinForAction(t)

	// 120 notes: well past any page a limit would cut at.
	for i := 0; i < 120; i++ {
		code, _, raw := pinBinPost(t, h, b.ID, "add_note",
			map[string]any{"note_type": "general", "message": fmt.Sprintf("n%d", i)})
		if code != http.StatusOK {
			t.Fatalf("note %d: %d %s", i, code, raw)
		}
	}
	all, err := db.ListEntityAudit("bin", b.ID)
	testutil.MustNoErr(t, err, "audit")
	if len(all) < 120 {
		t.Fatalf("seeded %d audit rows, want >= 120", len(all))
	}

	got := pinBinDetail(t, h, b.ID)
	if k := pinKeys(got); k != "audit,bin,manifest,recent_orders,template" {
		t.Errorf("detail keys (unclaimed) = %s", k)
	}
	var audit []json.RawMessage
	testutil.MustNoErr(t, json.Unmarshal(got["audit"], &audit), "audit")
	if len(audit) != len(all) {
		t.Errorf("detail audit = %d rows, the bin has %d: today it is the whole history", len(audit), len(all))
	}
	if string(got["recent_orders"]) != "[]" {
		t.Errorf("recent_orders = %s, want []", got["recent_orders"])
	}

	// Claimed: current_order appears, recent_orders lists it.
	o := testdb.CreateOrder(t, db)
	// Written straight to the row: the detail read only follows claimed_by, and
	// ClaimBin's liveness/reservation guards are not what this pins.
	_, err = db.Exec(`UPDATE bins SET claimed_by = $1 WHERE id = $2`, o.ID, b.ID)
	testutil.MustNoErr(t, err, "claim")
	got = pinBinDetail(t, h, b.ID)
	if k := pinKeys(got); k != "audit,bin,current_order,manifest,recent_orders,template" {
		t.Errorf("detail keys (claimed) = %s", k)
	}
	// Undo the hand-written claim: the cleanup wedge sweep (testdb.go) rightly
	// refuses a hard claim by a queued order holding no reservation.
	_, err = db.Exec(`UPDATE bins SET claimed_by = NULL WHERE id = $1`, b.ID)
	testutil.MustNoErr(t, err, "unclaim")

	// An empty carrier: no template; manifest parses to {"items":null}.
	empty := pinEmptyBin(t, db, sd.LineNode.ID, "PIN-DETAIL-EMPTY")
	got = pinBinDetail(t, h, empty.ID)
	if k := pinKeys(got); k != "audit,bin,manifest,recent_orders" {
		t.Errorf("detail keys (empty bin) = %s", k)
	}
	if string(got["manifest"]) != `{"items":null}` {
		t.Errorf("empty bin manifest = %s, want {\"items\":null}", got["manifest"])
	}
}

// TestPinBins_DetailHistoryFree pins LC12's added read: GET
// /api/bins/detail?id=N&history=0 is the default answer without `audit`, every
// other key byte-identical to the default answer's.
func TestPinBins_DetailHistoryFree(t *testing.T) {
	t.Parallel()
	h, _, _, b := setupBinForAction(t)
	for i := 0; i < 3; i++ {
		if code, _, raw := pinBinPost(t, h, b.ID, "add_note",
			map[string]any{"note_type": "general", "message": fmt.Sprintf("n%d", i)}); code != http.StatusOK {
			t.Fatalf("note %d: %d %s", i, code, raw)
		}
	}
	full := pinBinDetail(t, h, b.ID)

	req := httptest.NewRequest(http.MethodGet, "/api/bins/detail?id="+strconv.FormatInt(b.ID, 10)+"&history=0", nil)
	rec := httptest.NewRecorder()
	h.apiBinDetail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("history-free detail: status %d, body %s", rec.Code, rec.Body.String())
	}
	var lean map[string]json.RawMessage
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &lean), "decode history-free detail")
	if k := pinKeys(lean); k != pinHistoryFreeKeys {
		t.Errorf("history-free keys = %s, want %s", k, pinHistoryFreeKeys)
	}
	for k, v := range lean {
		if string(full[k]) != string(v) {
			t.Errorf("history-free %q = %s, default answer has %s", k, v, full[k])
		}
	}
	if _, ok := full["audit"]; !ok {
		t.Error("default answer lost its audit")
	}
}

// TestPinBins_RecordCount pins the cycle-count door (the wizard posts
// record_count through /api/bins/action, bins.js:654-675):
//   - typed: the typed value is stored, the audit says expected -> typed;
//   - stale: the page loaded at 100, the count then moved to 80, and the wizard
//     posts the page-load 100 (what ccConfirm sends, bins.js:656). TODAY the
//     server takes it: stored 100, expected read fresh (80), a +20 discrepancy
//     note, 200 ok. The fix is client-side (step 2 re-reads the bin), so the
//     server answer is predicted unchanged;
//   - refused: a bin with no payload is a 400 with the reason; nothing stored.
func TestPinBins_RecordCount(t *testing.T) {
	t.Parallel()

	t.Run("typed", func(t *testing.T) {
		t.Parallel()
		h, db, _, b := setupBinForAction(t)
		code, _, raw := pinBinPost(t, h, b.ID, "record_count", map[string]any{"actual_uop": 77, "actor": "cc"})
		if code != http.StatusOK {
			t.Fatalf("typed = %d %q, want 200 ok", code, raw)
		}
		pinRequireBinAnswer(t, db, b.ID, raw) // LC12: ok + the bin at 77
		if got := testdb.RequireBin(t, db, b.ID); got.UOPRemaining != 77 {
			t.Errorf("stored uop = %d, want 77", got.UOPRemaining)
		}
		requireAudit(t, db, b.ID, "counted", "100", "77", "cc")
	})

	t.Run("stale", func(t *testing.T) {
		t.Parallel()
		h, db, _, b := setupBinForAction(t)
		pageLoad := b.UOPRemaining // 100, what the wizard painted
		_, err := db.Exec(`UPDATE bins SET uop_remaining = 80 WHERE id = $1`, b.ID)
		testutil.MustNoErr(t, err, "move the count")

		code, _, raw := pinBinPost(t, h, b.ID, "record_count", map[string]any{"actual_uop": pageLoad, "actor": "cc"})
		if code != http.StatusOK {
			t.Fatalf("stale = %d %q, want 200 ok (the server still takes a stale match; the wizard no longer sends one)", code, raw)
		}
		pinRequireBinAnswer(t, db, b.ID, raw) // LC12: ok + the bin at 100
		if got := testdb.RequireBin(t, db, b.ID); got.UOPRemaining != 100 {
			t.Errorf("stored uop = %d, want 100 (the stale page value overwrote 80)", got.UOPRemaining)
		}
		requireAudit(t, db, b.ID, "counted", "80", "100", "cc")
		entries, err := db.ListEntityAudit("bin", b.ID)
		testutil.MustNoErr(t, err, "list bin audit")
		note := findAuditByAction(entries, "note:count")
		if note == nil || !strings.Contains(note.NewValue, "expected 80, actual 100 (+20)") {
			t.Errorf("discrepancy note = %+v, want one naming expected 80, actual 100 (+20)", note)
		}
	})

	t.Run("refused no payload", func(t *testing.T) {
		t.Parallel()
		h, db, sd, _ := setupBinForAction(t)
		empty := pinEmptyBin(t, db, sd.LineNode.ID, "PIN-CC-EMPTY")
		code, _, raw := pinBinPost(t, h, empty.ID, "record_count", map[string]any{"actual_uop": 5, "actor": "cc"})
		want := fmt.Sprintf("{\"error\":\"cannot validate UOP capacity — bin %d has no payload\"}\n", empty.ID)
		if code != http.StatusBadRequest || raw != want {
			t.Errorf("refused = %d %q, want 400 %q", code, raw, want)
		}
		got := testdb.RequireBin(t, db, empty.ID)
		if got.UOPRemaining != 0 || got.LastCountedBy != "" {
			t.Errorf("refused count stored something: uop %d by %q", got.UOPRemaining, got.LastCountedBy)
		}
	})

	t.Run("refused negative", func(t *testing.T) {
		t.Parallel()
		h, db, _, b := setupBinForAction(t)
		code, _, raw := pinBinPost(t, h, b.ID, "record_count", map[string]any{"actual_uop": -1, "actor": "cc"})
		if code != http.StatusBadRequest || raw != "{\"error\":\"actual UOP must be 0 or more\"}\n" {
			t.Errorf("negative = %d %q, want 400 actual UOP must be 0 or more", code, raw)
		}
		if got := testdb.RequireBin(t, db, b.ID); got.UOPRemaining != 100 {
			t.Errorf("stored uop = %d, want 100 unchanged", got.UOPRemaining)
		}
	})
}
