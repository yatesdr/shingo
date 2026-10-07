//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol/testutil"
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
//   - apiBinAction x all 16 verbs: input -> stored row -> response. The answer
//     is the bare {"status":"ok"} for every verb: it carries no bin (LC12 adds
//     the history-free detail to it).
//   - apiBulkBinAction: {"results":[{id, ok, error}]} and nothing about the bins.
//   - apiBinDetail: the key set, and that `audit` is the bin's whole history.
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

// pinOKBody is the one answer every successful verb gives today.
const pinOKBody = "{\"status\":\"ok\"}\n"

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
// response. The response is the bare ok for all 16 (LC12 moves it).
func TestPinBins_ActionEveryVerb(t *testing.T) {
	t.Parallel()

	type row struct {
		verb    string
		prep    func(t *testing.T, db *store.DB, sd *testdb.StandardData, b *bins.Bin) int64
		params  func(sd *testdb.StandardData) any
		stored  func(t *testing.T, db *store.DB, sd *testdb.StandardData, id int64)
		comment string
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
			code, body, raw := pinBinPost(t, h, id, r.verb, params)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", code, raw)
			}
			// TODAY: the bare ok, no bin. LC12 moves this (the answer carries the
			// bin's history-free detail).
			if raw != pinOKBody {
				t.Errorf("body = %q, want %q", raw, pinOKBody)
			}
			if _, ok := body["bin"]; ok {
				t.Error("answer carries a bin today; the pin says it does not")
			}
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
	// A verb's own refusal: release of a bin that is not staged.
	code, _, raw = pinBinPost(t, h, b.ID, "release", nil)
	want := fmt.Sprintf("{\"error\":\"bin %d is not staged\"}\n", b.ID)
	if code != http.StatusBadRequest || raw != want {
		t.Errorf("release of an available bin = %d %q, want 400 %q", code, raw, want)
	}
	if got := testdb.RequireBin(t, db, b.ID); string(got.Status) != "available" {
		t.Errorf("status after refused release = %q, want available", got.Status)
	}
}

// TestPinBins_BulkAction pins apiBulkBinAction's answer: one result per id,
// {id, ok} or {id, error}, and nothing about the bins themselves (LC12 adds the
// history-free detail per bin). A locked bin is refused per row unless the verb
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
	want := fmt.Sprintf(`{"results":[{"id":%d,"ok":true},{"id":%d,"ok":false,"error":"locked by pin-holder"},{"id":999999,"ok":false,"error":"not found"}]}`+"\n",
		b.ID, locked.ID)
	if rec.Body.String() != want {
		t.Errorf("bulk body =\n  %s\nwant\n  %s", rec.Body.String(), want)
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
		if code != http.StatusOK || raw != pinOKBody {
			t.Fatalf("typed = %d %q, want 200 ok", code, raw)
		}
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
		if code != http.StatusOK || raw != pinOKBody {
			t.Fatalf("stale = %d %q, want 200 ok (today the server takes a stale match)", code, raw)
		}
		if got := testdb.RequireBin(t, db, b.ID); got.UOPRemaining != 100 {
			t.Errorf("stored uop = %d, want 100 (the stale page value overwrote 80)", got.UOPRemaining)
		}
		requireAudit(t, db, b.ID, "counted", "80", "100", "cc")
		entries, _ := db.ListEntityAudit("bin", b.ID)
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
