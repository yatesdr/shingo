//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/audit"
	"shingocore/store/bins"
	"shingocore/store/orders"
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
