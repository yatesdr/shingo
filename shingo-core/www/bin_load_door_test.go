//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"

	"shingocore/internal/testdb"
	"shingocore/messaging"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/audit"
	"shingocore/store/bins"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// bin_load_door_test.go — the bin-load telemetry door answers like its
// bin-count and bin-clear siblings (I2).
//
// It took binList[0] on a node holding several carriers, loaded an unknown
// payload at uop 0 (a write that looks like a measurement of an empty bin),
// and ran the manifest write and the confirm as two transactions with the
// confirm's failure logged and swallowed — a set-but-unconfirmed bin reported
// "ok". The confirm is the gate that makes a full bin a drain source, so that
// was a bin the Edge thought it had loaded and kanban could never see.

func TestApiBinLoad_AmbiguousMultipleBins_Returns409(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	a := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LD-AMB-1")
	b := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LD-AMB-2")

	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", map[string]any{
		"node_name": sd.StorageNode.Name, "payload_code": sd.Payload.Code,
		"manifest": []map[string]any{{"part_number": "P1"}},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "specify bin_id")
	for _, id := range []int64{a.ID, b.ID} {
		if got, _ := db.GetBin(id); got.PayloadCode != "" {
			t.Errorf("bin %d loaded by a refused request: payload %q", id, got.PayloadCode)
		}
	}
}

func TestApiBinLoad_BinIDLoadsTheNamedBin(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	a := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LD-NAMED-1")
	b := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LD-NAMED-2")

	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", map[string]any{
		"node_name": sd.StorageNode.Name, "bin_id": b.ID, "payload_code": sd.Payload.Code,
		"uop_count": 12, "manifest": []map[string]any{{"part_number": "P1"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := db.GetBin(b.ID); got.PayloadCode != sd.Payload.Code || got.UOPRemaining != 12 || !got.ManifestConfirmed {
		t.Errorf("named bin: got payload %q uop %d confirmed %v", got.PayloadCode, got.UOPRemaining, got.ManifestConfirmed)
	}
	if got, _ := db.GetBin(a.ID); got.PayloadCode != "" {
		t.Errorf("the other bin was loaded: payload %q", got.PayloadCode)
	}

	// A bin that is not at the node is a 409, not a silent fall-back.
	rec = postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", map[string]any{
		"node_name": sd.StorageNode.Name, "bin_id": 987654321, "payload_code": sd.Payload.Code,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("foreign bin_id: status %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "is not at node")
}

// TestApiBinLoad_UnknownPayloadIsRefused: a code Core has no template for is
// refused like BinService.LoadPayload refuses it, declared count or not, and
// the bin is left as it was.
func TestApiBinLoad_UnknownPayloadIsRefused(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LD-UNKNOWN")

	for _, body := range []map[string]any{
		{"node_name": sd.StorageNode.Name, "payload_code": "PAYLOAD-CORE-HAS-NEVER-SEEN"},
		{"node_name": sd.StorageNode.Name, "payload_code": "PAYLOAD-CORE-HAS-NEVER-SEEN", "uop_count": 5},
	} {
		rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertJSONError(t, rec.Body.Bytes(), "not found")
	}
	if got, _ := db.GetBin(bin.ID); got.PayloadCode != bin.PayloadCode || got.UOPRemaining != bin.UOPRemaining {
		t.Errorf("refused load wrote the bin: payload %q uop %d, was %q uop %d",
			got.PayloadCode, got.UOPRemaining, bin.PayloadCode, bin.UOPRemaining)
	}
}

// TestApiBinLoad_WritesBothLedgerRowsOnce: the set and the confirm each leave
// their audit row, once, now from one transaction.
func TestApiBinLoad_WritesBothLedgerRowsOnce(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LD-LEDGER")

	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", map[string]any{
		"node_name": sd.StorageNode.Name, "payload_code": sd.Payload.Code, "uop_count": 3,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	rows, err := audit.ListBinUOPByBin(db.DB, bin.ID, 100, 0)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	count := map[string]int{}
	for _, r := range rows {
		count[r.Op]++
	}
	if count[audit.OpSetForProduction] != 1 || count[audit.OpManifestConfirmed] != 1 {
		t.Errorf("ledger ops = %v, want exactly one %s and one %s", count,
			audit.OpSetForProduction, audit.OpManifestConfirmed)
	}
}

// TestApiBinLoad_ACarrierThePartMayNotRideIsRefused — the loader window's LOAD
// is a person's declaration (D1 item 8): on a carrier the part may not ride it
// is refused, 409, with the carrier, the allowed types and what to do with the
// carrier in the sentence the Edge shows. The bin is left as it was: no payload,
// no finding. It used to land and flag, and the flagged carrier was then
// unsourceable for good while its parts held the loader's reorder level (D33).
func TestApiBinLoad_ACarrierThePartMayNotRideIsRefused(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	fits := &bins.BinType{Code: "LD-FITS"}
	foreign := &bins.BinType{Code: "LD-FOREIGN"}
	for _, bt := range []*bins.BinType{fits, foreign} {
		if err := db.CreateBinType(bt); err != nil {
			t.Fatalf("create bin type %s: %v", bt.Code, err)
		}
	}
	if err := db.SetPayloadBinTypes(sd.Payload.ID, []int64{fits.ID}); err != nil {
		t.Fatalf("the part rides LD-FITS only: %v", err)
	}
	bin := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LD-FOREIGN")
	if _, err := db.DB.Exec(`UPDATE bins SET bin_type_id=$1 WHERE id=$2`, foreign.ID, bin.ID); err != nil {
		t.Fatalf("put the bin on the foreign carrier: %v", err)
	}

	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", map[string]any{
		"node_name": sd.StorageNode.Name, "bin_id": bin.ID, "payload_code": sd.Payload.Code,
		"uop_count": 12, "manifest": []map[string]any{{"part_number": "P1"}},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	// The sentence names the carrier, the carriers the part may ride and that
	// nothing was loaded, and nothing else (owner, 2026-10-12).
	for _, want := range []string{"LD-FOREIGN", "LD-FITS", "Nothing was loaded"} {
		assertJSONError(t, rec.Body.Bytes(), want)
	}
	if body := rec.Body.String(); strings.Contains(body, "has to leave") || strings.Contains(body, "Orders page") {
		t.Errorf("the refusal tells the operator what to do with the carrier: %s", body)
	}
	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("re-read bin: %v", err)
	}
	if got.PayloadCode != bin.PayloadCode || got.UOPRemaining != bin.UOPRemaining {
		t.Errorf("a refused LOAD wrote the bin: payload %q uop %d, was %q uop %d",
			got.PayloadCode, got.UOPRemaining, bin.PayloadCode, bin.UOPRemaining)
	}
	var flagged bool
	if err := db.DB.QueryRow(`SELECT undeclared_carrier_at IS NOT NULL FROM bins WHERE id=$1`, bin.ID).Scan(&flagged); err != nil {
		t.Fatalf("read the finding: %v", err)
	}
	if flagged {
		t.Error("a refused LOAD flagged the carrier")
	}
}

// heldCase is one hold a carrier can be under when a LOAD reaches it, shared by
// every LOAD door's pin so the doors are asked the same question.
type heldCase struct {
	name    string
	hold    func(t *testing.T, db *store.DB, binID int64) int64 // returns the holder, 0 for none
	refused bool
}

// heldCases: refused under a live order's hard claim or soft hold; loaded when
// nobody holds the carrier, or its holder already went terminal (the claim left
// behind is not a hold).
var heldCases = []heldCase{
	{"claimed by a live order", func(t *testing.T, db *store.DB, binID int64) int64 {
		// At dispatched: a hard hold is taken for the fleet call.
		o := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusDispatched })
		testdb.ClaimBinForTest(t, db, binID, o.ID)
		return o.ID
	}, true},
	// The rest of the order's life before delivery holds the carrier the same
	// way: in transit, held at a wait (staged), acknowledged by the fleet, and
	// faulted short of its drop. Delivery gives the hold up (ApplyArrival);
	// TestLoad_ADeliveredPullsCarrierLoadsAsARestingOneDoes pins that side.
	claimedAt(protocol.StatusInTransit),
	claimedAt(protocol.StatusStaged),
	claimedAt(protocol.StatusAcknowledged),
	claimedAt(protocol.StatusFaulted),
	// A queued order's soft hold (CreateOrder's default status): no robot yet,
	// and the carrier is still promised to it.
	{"soft-held by a live order", func(t *testing.T, db *store.DB, binID int64) int64 {
		o := testdb.CreateOrder(t, db)
		testdb.ReserveBin(t, db, o.ID, binID)
		return o.ID
	}, true},
	{"claimed by a terminal order", func(t *testing.T, db *store.DB, binID int64) int64 {
		o := testdb.CreateOrder(t, db)
		testdb.ClaimBinForTest(t, db, binID, o.ID)
		testdb.SeedOrderStatus(t, db, o.ID, string(protocol.StatusConfirmed), "")
		return o.ID
	}, false},
	{"held by nobody", func(*testing.T, *store.DB, int64) int64 { return 0 }, false},
}

// claimedAt is a heldCase for a carrier claimed by an order at a status short
// of delivery.
func claimedAt(status protocol.Status) heldCase {
	return heldCase{"claimed by an order " + string(status), func(t *testing.T, db *store.DB, binID int64) int64 {
		o := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = status })
		testdb.ClaimBinForTest(t, db, binID, o.ID)
		return o.ID
	}, true}
}

// heldRefusalSentence is the owner's sentence (2026-10-18), written out here
// rather than read from the service so a change to it fails these pins.
func heldRefusalSentence(order int64) string {
	return fmt.Sprintf("Order %d holds this bin (robot on its way) — cancel it first, or load after it leaves.", order)
}

// assertHeldAnswer checks one LOAD door's answer against a heldCase: refused
// with exactly the sentence and the bin untouched, or loaded with 30.
func assertHeldAnswer(t *testing.T, db *store.DB, tc heldCase, rec *httptest.ResponseRecorder,
	refusedCode int, bin *bins.Bin, holder int64, payload string) {
	t.Helper()
	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("re-read bin: %v", err)
	}
	if !tc.refused {
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if got.PayloadCode != payload || got.UOPRemaining != 30 {
			t.Errorf("the LOAD did not land: payload %q uop %d", got.PayloadCode, got.UOPRemaining)
		}
		return
	}
	if rec.Code != refusedCode {
		t.Fatalf("status: got %d, want %d; body=%s", rec.Code, refusedCode, rec.Body.String())
	}
	var env map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode the refusal: %v; body=%s", err, rec.Body.String())
	}
	if want := heldRefusalSentence(holder); env["error"] != want {
		t.Errorf("refusal:\n got %q\nwant %q", env["error"], want)
	}
	if got.PayloadCode != bin.PayloadCode || got.UOPRemaining != bin.UOPRemaining {
		t.Errorf("a refused LOAD wrote the bin: payload %q uop %d, was %q uop %d",
			got.PayloadCode, got.UOPRemaining, bin.PayloadCode, bin.UOPRemaining)
	}
}

// TestApiBinLoad_AHeldCarrierIsRefused — the loader's LOAD refuses a carrier a
// live order holds, by hard claim or by soft hold, 409 with the owner's sentence
// naming the order (2026-10-18: "Refuse a held bin"). The held-step clear's sim
// found it: Core's clear zeroed a home's carrier, a queued empty pull claimed it,
// and a LOAD half a second later set it back to 30 parts, so the pull carried a
// full away.
func TestApiBinLoad_AHeldCarrierIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range heldCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, db := testHandlers(t)
			sd := testdb.SetupStandardData(t, db)
			bin := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LD-HELD")
			holder := tc.hold(t, db, bin.ID)

			rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", map[string]any{
				"node_name": sd.StorageNode.Name, "bin_id": bin.ID, "payload_code": sd.Payload.Code,
				"uop_count": 30, "manifest": []map[string]any{{"part_number": "P1"}},
			})
			assertHeldAnswer(t, db, tc, rec, http.StatusConflict, bin, holder, sd.Payload.Code)
		})
	}
}

// TestExecuteBinAction_LoadPayload_AHeldCarrierIsRefused — the admin Load
// Payload asks the same check and answers with the same sentence (owner,
// 2026-10-18): no admin bypass, the cancel is the audited override. Its refusal
// keeps the action door's existing shape (400 with the error, which the bins
// page toasts as "Error: <sentence>").
func TestExecuteBinAction_LoadPayload_AHeldCarrierIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range heldCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, db := testHandlers(t)
			sd := testdb.SetupStandardData(t, db)
			bin := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LP-HELD")
			holder := tc.hold(t, db, bin.ID)

			rec := postJSON(t, h.apiBinAction, "/api/bins/action", map[string]any{
				"id": bin.ID, "action": "load_payload",
				"params": map[string]any{"payload_code": sd.Payload.Code, "uop_override": 30},
			})
			assertHeldAnswer(t, db, tc, rec, http.StatusBadRequest, bin, holder, sd.Payload.Code)
		})
	}
}

// TestBinDetail_LoadRefusalAgreesWithTheLoadDoors — the bins page withholds
// Load Payload exactly where the write refuses it. The detail answer's
// load_refusal (service.LoadRefusalFor, over the bin row and its claimant,
// both read already) is asked for every heldCase and must be the door's own
// sentence where the door refuses, and absent where it loads.
func TestBinDetail_LoadRefusalAgreesWithTheLoadDoors(t *testing.T) {
	t.Parallel()
	for _, tc := range heldCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, db := testHandlers(t)
			sd := testdb.SetupStandardData(t, db)
			bin := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LR-HELD")
			holder := tc.hold(t, db, bin.ID)

			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/bins/detail?id=%d&history=0", bin.ID), nil)
			got := httptest.NewRecorder()
			h.apiBinDetail(got, req)
			if got.Code != http.StatusOK {
				t.Fatalf("detail: %d %s", got.Code, got.Body.String())
			}
			var detail struct {
				LoadRefusal *string `json:"load_refusal"`
			}
			if err := json.Unmarshal(got.Body.Bytes(), &detail); err != nil {
				t.Fatalf("decode detail: %v", err)
			}
			switch {
			case tc.refused && (detail.LoadRefusal == nil || *detail.LoadRefusal != heldRefusalSentence(holder)):
				t.Errorf("the page offers a load the door refuses: load_refusal %v, want %q",
					detail.LoadRefusal, heldRefusalSentence(holder))
			case !tc.refused && detail.LoadRefusal != nil:
				t.Errorf("the page withholds a load the door accepts: load_refusal %q", *detail.LoadRefusal)
			}

			rec := postJSON(t, h.apiBinAction, "/api/bins/action", map[string]any{
				"id": bin.ID, "action": "load_payload",
				"params": map[string]any{"payload_code": sd.Payload.Code, "uop_override": 30},
			})
			assertHeldAnswer(t, db, tc, rec, http.StatusBadRequest, bin, holder, sd.Payload.Code)
		})
	}
}

// doorFix is one door call's world: the handlers over a fresh database and the
// standard data (STORAGE-A1, LINE1-IN, PART-A of capacity 1000).
type doorFix struct {
	h  *Handlers
	db *store.DB
	sd *testdb.StandardData
}

func newDoorFix(t *testing.T) *doorFix {
	t.Helper()
	h, db := testHandlers(t)
	// The write doors' refusals carry Core's rows, read as the composition
	// root wires them (cmd/shingocore: SetBinsRowsFunc).
	h.engine.NodeService().SetBinsRowsFunc(messaging.NewCoreDataService(db, nil, service.EpochAnnounce{}).NodeBinsRows)
	return &doorFix{h: h, db: db, sd: testdb.SetupStandardData(t, db)}
}

// doorArm is one arm of a door: arrange shapes the world and returns the
// request (a string is sent raw) and the bins whose rows are watched; answer is
// the status and body wanted, read after the call (an accept's body carries
// the epoch it wrote). wrote is nil for a refusal, which must write nothing;
// otherwise it is the bin columns and table rows the call must write. after,
// when set, checks the values written.
type doorArm struct {
	name    string
	arrange func(t *testing.T, f *doorFix) (body any, watch []int64)
	answer  func(t *testing.T, f *doorFix) (int, string)
	wrote   func(f *doorFix) (map[int64][]string, map[string]int)
	after   func(t *testing.T, f *doorFix)
}

// runDoorArms drives each arm through the door on a world of its own.
func runDoorArms(t *testing.T, door func(*Handlers) http.HandlerFunc, path string, arms []doorArm) {
	t.Helper()
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			f := newDoorFix(t)
			body, watch := arm.arrange(t, f)
			snap := snapDoor(t, f.db, watch...)
			var rec *httptest.ResponseRecorder
			if raw, ok := body.(string); ok {
				rec = postRaw(t, door(f.h), path, []byte(raw))
			} else {
				rec = postJSON(t, door(f.h), path, body)
			}
			code, want := arm.answer(t, f)
			assertDoor(t, rec, code, want)
			if arm.after != nil {
				arm.after(t, f)
			}
			if arm.wrote == nil {
				snap.assertNothingWritten(t, f.db)
				return
			}
			cols, added := arm.wrote(f)
			snap.assertWrote(t, f.db, cols, added)
		})
	}
}

// refusedWith is an answer that does not depend on what the call wrote.
func refusedWith(code int, msg string) func(*testing.T, *doorFix) (int, string) {
	return func(*testing.T, *doorFix) (int, string) { return code, doorErr(msg) }
}

// binByLabel re-reads a fixture bin by its label.
func binByLabel(t *testing.T, db *store.DB, label string) *bins.Bin {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT id FROM bins WHERE label=$1`, label).Scan(&id), "find bin "+label)
	b, err := db.GetBin(id)
	testutil.MustNoErr(t, err, "read bin "+label)
	return b
}

// binIDOf is a fixture bin's id by its label, for an arm's write set.
func binIDOf(f *doorFix, label string) int64 {
	var id int64
	_ = f.db.QueryRow(`SELECT id FROM bins WHERE label=$1`, label).Scan(&id)
	return id
}

// failBinWrites makes every UPDATE of the bin's column fail inside its
// transaction, as a database fault would: a door's server-failure arm.
func failBinWrites(t *testing.T, db *store.DB, binID int64, column string) {
	t.Helper()
	_, err := db.DB.Exec(fmt.Sprintf(`
		CREATE FUNCTION door_fault() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'door fault'; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER door_fault BEFORE UPDATE OF %s ON bins FOR EACH ROW
		  WHEN (OLD.id = %d) EXECUTE FUNCTION door_fault();`, column, binID))
	testutil.MustNoErr(t, err, "the fault")
}

// loadBody is a LOAD of PART-A at STORAGE-A1 with one manifest line, and the
// fields given.
func loadBody(f *doorFix, fields map[string]any) map[string]any {
	b := map[string]any{"node_name": f.sd.StorageNode.Name, "payload_code": f.sd.Payload.Code,
		"manifest": []map[string]any{{"part_number": "P1", "quantity": 4}}}
	for k, v := range fields {
		b[k] = v
	}
	return b
}

// TestBinLoadDoor_Arms pins every arm of POST /api/telemetry/bin-load
// (apiBinLoad over BinManifestService.LoadAtLine): the status, the body byte
// for byte, and what was written — nothing at all on a refusal. Every hold the
// held-carrier arm refuses is TestApiBinLoad_AHeldCarrierIsRefused; it is
// asked once here for the write set.
func TestBinLoadDoor_Arms(t *testing.T) {
	t.Parallel()
	storage := func(f *doorFix) string { return f.sd.StorageNode.Name }
	// onStorage is an empty carrier at STORAGE-A1, its manifest unconfirmed as a
	// clear leaves it, so a LOAD's confirm shows in what it writes.
	onStorage := func(t *testing.T, f *doorFix, label string) *bins.Bin {
		b := testdb.CreateBinAtNode(t, f.db, "", f.sd.StorageNode.ID, label)
		_, err := f.db.DB.Exec(`UPDATE bins SET manifest_confirmed=false WHERE id=$1`, b.ID)
		testutil.MustNoErr(t, err, "unconfirm "+label)
		return b
	}
	// rides makes PART-A ride LD-FITS only.
	rides := func(t *testing.T, f *doorFix) {
		fits := &bins.BinType{Code: "LD-FITS"}
		testutil.MustNoErr(t, f.db.CreateBinType(fits), "LD-FITS")
		testutil.MustNoErr(t, f.db.SetPayloadBinTypes(f.sd.Payload.ID, []int64{fits.ID}), "PART-A rides LD-FITS only")
	}
	// bare makes b a bare cart of its own type.
	bare := func(t *testing.T, f *doorFix, b *bins.Bin) {
		marker, err := f.db.EnsureBareMarker(b.BinTypeID)
		testutil.MustNoErr(t, err, "bare marker")
		_, err = f.db.DB.Exec(`UPDATE bins SET bin_type_id=$1, payload_code='', manifest=NULL, uop_remaining=0 WHERE id=$2`, marker, b.ID)
		testutil.MustNoErr(t, err, "a bare cart")
	}
	loaded := func(label, payload string, uop int) func(*testing.T, *doorFix) (int, string) {
		return func(t *testing.T, f *doorFix) (int, string) {
			b := binByLabel(t, f.db, label)
			return http.StatusOK, doorJSON(t, protocol.BinLoadResponse{Status: "ok", BinID: b.ID, BinLabel: label,
				PayloadCode: payload, UOPRemaining: uop, DeltaEpoch: b.DeltaEpoch})
		}
	}
	wroteLoad := func(label string, extra ...string) func(*doorFix) (map[int64][]string, map[string]int) {
		return func(f *doorFix) (map[int64][]string, map[string]int) {
			cols := append([]string{"delta_epoch", "loaded_at", "manifest", "manifest_confirmed", "payload_code",
				"uop_remaining", "updated_at"}, extra...)
			sort.Strings(cols)
			return map[int64][]string{binIDOf(f, label): cols}, map[string]int{"bin_uop_ledger": 2}
		}
	}
	const foreign = "PART-A may not ride a DEFAULT carrier (allowed: LD-FITS). Nothing was loaded."
	var holder int64
	runDoorArms(t, func(h *Handlers) http.HandlerFunc { return h.apiBinLoad }, "/api/telemetry/bin-load", []doorArm{
		{name: "an undecodable body is refused",
			arrange: func(*testing.T, *doorFix) (any, []int64) { return "not-json", nil },
			answer:  refusedWith(http.StatusBadRequest, "invalid character 'o' in literal null (expecting 'u')")},
		{name: "no node_name is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "LD-A-NONAME")
				return loadBody(f, map[string]any{"node_name": ""}), []int64{b.ID}
			},
			answer: refusedWith(http.StatusBadRequest, "node_name is required")},
		{name: "an unknown node is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				return loadBody(f, map[string]any{"node_name": "LD-NO-SUCH-NODE"}), nil
			},
			answer: refusedAt(atNode("LD-NO-SUCH-NODE"), http.StatusNotFound, `node "LD-NO-SUCH-NODE" not found`)},
		{name: "a node holding no bin is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				return loadBody(f, map[string]any{"node_name": f.sd.LineNode.Name}), nil
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return refusalAt(t, f, f.sd.LineNode.Name, http.StatusBadRequest, "no bin at node "+f.sd.LineNode.Name)
			}},
		{name: "a bin_id not at the node is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "LD-A-FOREIGNID")
				return loadBody(f, map[string]any{"bin_id": 987654321}), []int64{b.ID}
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return refusalAt(t, f, storage(f), http.StatusConflict, "bin 987654321 is not at node "+storage(f))
			}},
		{name: "two bins and no bin_id is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				a, b := onStorage(t, f, "LD-A-AMB-1"), onStorage(t, f, "LD-A-AMB-2")
				return loadBody(f, nil), []int64{a.ID, b.ID}
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return refusalAt(t, f, storage(f), http.StatusConflict, "node "+storage(f)+" holds 2 bins; specify bin_id to disambiguate")
			}},
		{name: "a carrier a live order holds is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "LD-A-HELD")
				o := testdb.CreateOrder(t, f.db, func(o *orders.Order) { o.Status = protocol.StatusDispatched })
				testdb.ClaimBinForTest(t, f.db, b.ID, o.ID)
				holder = o.ID
				return loadBody(f, map[string]any{"uop_count": 30}), []int64{b.ID}
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return refusalAt(t, f, storage(f), http.StatusConflict, heldRefusalSentence(holder))
			}},
		{name: "a carrier the part may not ride is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				rides(t, f)
				b := onStorage(t, f, "LD-A-FOREIGN")
				return loadBody(f, map[string]any{"uop_count": 12}), []int64{b.ID}
			},
			answer: refusedAt(storage, http.StatusConflict, foreign)},
		{name: "a bare cart the part may not ride is refused and keeps its marker",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				rides(t, f)
				b := onStorage(t, f, "LD-A-BARE-FOREIGN")
				bare(t, f, b)
				return loadBody(f, map[string]any{"uop_count": 12}), []int64{b.ID}
			},
			answer: refusedAt(storage, http.StatusConflict, foreign),
			// The carrier is stamped before the judgement and put back after
			// it, two statements outside the load's transaction: the type ends
			// where it began, and the row's updated_at has moved.
			wrote: func(f *doorFix) (map[int64][]string, map[string]int) {
				return map[int64][]string{binIDOf(f, "LD-A-BARE-FOREIGN"): {"updated_at"}}, map[string]int{}
			}},
		{name: "no count at a partial home of the part is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				ppHome(t, f.db, f.sd.StorageNode, "LD-PPH")
				b := onStorage(t, f, "LD-A-PPH")
				return loadBody(f, map[string]any{"payload_code": "LD-PPH"}), []int64{b.ID}
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return refusalAt(t, f, storage(f), http.StatusConflict, "a load here must say how many it holds: "+storage(f)+
					" is a partial home for LD-PPH — enter the count on the LOAD")
			}},
		{name: "an unknown payload is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "LD-A-UNKNOWN")
				return loadBody(f, map[string]any{"payload_code": "LD-NEVER-SEEN", "uop_count": 5}), []int64{b.ID}
			},
			answer: refusedAt(storage, http.StatusBadRequest, `unknown payload: payload template "LD-NEVER-SEEN" not found`)},
		{name: "a write the database refuses is a server failure",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "LD-A-FAULT")
				failBinWrites(t, f.db, b.ID, "payload_code")
				return loadBody(f, map[string]any{"uop_count": 5}), []int64{b.ID}
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return refusalAt(t, f, storage(f), http.StatusInternalServerError, fmt.Sprintf("set manifest bin %d: ERROR: door fault (SQLSTATE P0001)",
					binIDOf(f, "LD-A-FAULT")))
			}},
		{name: "a declared count loads the one bin",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "LD-A-OK")
				return loadBody(f, map[string]any{"uop_count": 12}), []int64{b.ID}
			},
			answer: loaded("LD-A-OK", "PART-A", 12),
			wrote:  wroteLoad("LD-A-OK")},
		{name: "no count loads the standard pack",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "LD-A-STD")
				return loadBody(f, nil), []int64{b.ID}
			},
			answer: loaded("LD-A-STD", "PART-A", 1000),
			wrote:  wroteLoad("LD-A-STD")},
		{name: "a bin_id names one of two bins",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				a, b := onStorage(t, f, "LD-A-TWO-1"), onStorage(t, f, "LD-A-TWO-2")
				return loadBody(f, map[string]any{"bin_id": b.ID, "uop_count": 7}), []int64{a.ID, b.ID}
			},
			answer: loaded("LD-A-TWO-2", "PART-A", 7),
			wrote:  wroteLoad("LD-A-TWO-2")},
		{name: "a bare cart loads onto its carrier",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "LD-A-BARE")
				bare(t, f, b)
				return loadBody(f, map[string]any{"uop_count": 9}), []int64{b.ID}
			},
			answer: loaded("LD-A-BARE", "PART-A", 9),
			wrote:  wroteLoad("LD-A-BARE", "bin_type_id")},
		{name: "a declared count at a partial home loads and holds the carrier",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				ppHome(t, f.db, f.sd.StorageNode, "LD-PPH")
				b := onStorage(t, f, "LD-A-PPH-OK")
				return loadBody(f, map[string]any{"payload_code": "LD-PPH", "uop_count": 6}), []int64{b.ID}
			},
			answer: loaded("LD-A-PPH-OK", "LD-PPH", 6),
			wrote:  wroteLoad("LD-A-PPH-OK", "held_since")},
		// What the sender read (epoch, expect): a fact that no longer holds is
		// refused with what the carrier now holds, and nothing is written.
		{name: "a carrier whose generation moved is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "LD-A-EPOCH")
				return loadBody(f, map[string]any{"uop_count": 5, "epoch": b.DeltaEpoch + 1, "expect": "bare"}), []int64{b.ID}
			},
			answer: refusedAt(storage, http.StatusConflict, "the carrier changed since you looked — it now holds LD-A-EPOCH (bare)")},
		{name: "a carrier read bare that holds a part is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := testdb.CreateBinAtNode(t, f.db, f.sd.Payload.Code, f.sd.StorageNode.ID, "LD-A-NOTBARE")
				return loadBody(f, map[string]any{"uop_count": 5, "epoch": b.DeltaEpoch, "expect": "bare"}), []int64{b.ID}
			},
			answer: refusedAt(storage, http.StatusConflict, "the carrier changed since you looked — it now holds LD-A-NOTBARE (PART-A)")},
		{name: "a carrier read partial that is bare is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "LD-A-NOTPART")
				return loadBody(f, map[string]any{"uop_count": 5, "expect": "partial"}), []int64{b.ID}
			},
			answer: refusedAt(storage, http.StatusConflict, "the carrier changed since you looked — it now holds LD-A-NOTPART (bare)")},
		{name: "a carrier read partial that holds another part is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				testutil.MustNoErr(t, f.db.CreatePayload(&payloads.Payload{Code: "LD-OTHER", UOPCapacity: 50}), "another part")
				b := testdb.CreateBinAtNode(t, f.db, "LD-OTHER", f.sd.StorageNode.ID, "LD-A-OTHERPART")
				return loadBody(f, map[string]any{"uop_count": 5, "expect": "partial"}), []int64{b.ID}
			},
			answer: refusedAt(storage, http.StatusConflict, "the carrier changed since you looked — it now holds LD-A-OTHERPART (LD-OTHER)")},
		{name: "an expect the door has no term for is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "LD-A-EMPTY")
				return loadBody(f, map[string]any{"uop_count": 5, "expect": "empty"}), []int64{b.ID}
			},
			answer: refusedAt(storage, http.StatusBadRequest, `expect "empty" is not a carrier the door knows: say bare or partial`)},
		{name: "the generation and a bare carrier as read load",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "LD-A-ASREAD")
				return loadBody(f, map[string]any{"uop_count": 12, "epoch": b.DeltaEpoch, "expect": "bare"}), []int64{b.ID}
			},
			answer: loaded("LD-A-ASREAD", "PART-A", 12),
			wrote:  wroteLoad("LD-A-ASREAD")},
		{name: "a partial of the part as read loads",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := testdb.CreateBinAtNode(t, f.db, f.sd.Payload.Code, f.sd.StorageNode.ID, "LD-A-PARTIAL")
				_, err := f.db.DB.Exec(`UPDATE bins SET manifest_confirmed=false WHERE id=$1`, b.ID)
				testutil.MustNoErr(t, err, "unconfirm LD-A-PARTIAL")
				return loadBody(f, map[string]any{"uop_count": 40, "epoch": b.DeltaEpoch, "expect": "partial"}), []int64{b.ID}
			},
			answer: loaded("LD-A-PARTIAL", "PART-A", 40),
			// The part was already PART-A, so payload_code does not move.
			wrote: func(f *doorFix) (map[int64][]string, map[string]int) {
				return map[int64][]string{binIDOf(f, "LD-A-PARTIAL"): {"delta_epoch", "loaded_at", "manifest",
					"manifest_confirmed", "uop_remaining", "updated_at"}}, map[string]int{"bin_uop_ledger": 2}
			}},
	})
}
