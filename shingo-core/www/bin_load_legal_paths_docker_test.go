//go:build docker

package www

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/engine"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/audit"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// bin_load_legal_paths_docker_test.go — the loads the held-carrier check must
// leave alone, and the boundary it draws (owner, 2026-10-18): a carrier is not
// loadable while its order is active before delivery (dispatched, in transit,
// held); from delivered onward it is loadable, and that load confirms a loader
// order exactly as before the check.
//
// The check keys on the carrier's hold (refuseHeldCarrierTx). These pins are
// what makes that the same boundary as the order's status: the hold is given up
// at the delivery itself (handleOrderDelivered → applyBinArrivalForOrder →
// ApplyArrival, a handoff: helpers.PlaceBinTx clears claimed_by and releases the
// reservation in the arrival's transaction), so a delivered pull's carrier has
// no hold left to refuse.

// loadEffect renders what a LOAD left on a bin: its contents and the ledger ops
// it wrote, with nothing that differs between two bins (ids, labels, times).
func loadEffect(t *testing.T, db *store.DB, binID int64) string {
	t.Helper()
	var code, manifest string
	var uop int
	var confirmed bool
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COALESCE(payload_code,''), COALESCE(manifest::text,'<NULL>'),
		uop_remaining, manifest_confirmed FROM bins WHERE id=$1`, binID).Scan(&code, &manifest, &uop, &confirmed),
		"read the loaded bin")
	rows, err := audit.ListBinUOPByBin(db.DB, binID, 100, 0)
	testutil.MustNoErr(t, err, "read the bin's ledger")
	var ops []string
	for _, r := range rows {
		ops = append(ops, r.Op)
	}
	sort.Strings(ops)
	return fmt.Sprintf("payload=%s uop=%d confirmed=%v manifest=%s ops=%s", code, uop, confirmed, manifest,
		strings.Join(ops, ","))
}

// pageLoadRefusal is the bins page's reason to withhold Load Payload for a
// bin: the history-free detail's load_refusal, "" when absent.
func pageLoadRefusal(t *testing.T, h *Handlers, binID int64) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.apiBinDetail(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/bins/detail?id=%d&history=0", binID), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		LoadRefusal string `json:"load_refusal"`
	}
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &detail), "decode the bin detail")
	return detail.LoadRefusal
}

// TestLoad_ADeliveredPullsCarrierLoadsAsARestingOneDoes — the loader's LOAD on
// the carrier an empty pull has just delivered: the delivery gave up the hold,
// the LOAD lands exactly as it does on a carrier nobody ever held, and the
// order then confirms as before. Before delivery, at dispatched and in transit,
// the same POST is refused with the one sentence and the carrier is untouched.
func TestLoad_ADeliveredPullsCarrierLoadsAsARestingOneDoes(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)
	d := h.engine.Dispatcher()
	empty := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LEGAL-PULL")

	post := func(node string, binID int64) (int, string) {
		rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", map[string]any{
			"node_name": node, "bin_id": binID, "payload_code": sd.Payload.Code,
			"uop_count": 30, "manifest": []map[string]any{{"part_number": "P1"}},
		})
		return rec.Code, rec.Body.String()
	}

	d.HandleOrderRequest(testdb.Envelope(), &protocol.OrderRequest{
		OrderUUID: "legal-pull", OrderType: dispatch.OrderTypeRetrieveEmpty, PayloadCode: sd.Payload.Code,
		DeliveryNode: sd.LineNode.Name, Quantity: 1,
	})
	// Intake plans; the scanner claims and dispatches (the claim-move contract).
	eng, ok := h.engine.(*engine.Engine)
	if !ok {
		t.Fatal("precondition: the handlers' engine is not *engine.Engine")
	}
	eng.RunFulfillmentScan()
	o := testdb.RequireOrderStatus(t, db, "legal-pull", dispatch.StatusDispatched)
	if o.BinID == nil || *o.BinID != empty.ID {
		t.Fatalf("precondition: the pull carries bin %v, want the empty %d", o.BinID, empty.ID)
	}
	before := loadEffect(t, db, empty.ID)

	// Before delivery the carrier is the order's: a direct POST is refused.
	sentence := heldRefusalSentence(o.ID)
	for _, step := range []struct {
		drive  string
		status protocol.Status
	}{{"", dispatch.StatusDispatched}, {"RUNNING", protocol.StatusInTransit}} {
		if step.drive != "" {
			sim.DriveState(o.VendorOrderID, step.drive)
			testdb.RequireOrderStatus(t, db, "legal-pull", step.status)
		}
		at := testdb.RequireBin(t, db, empty.ID)
		code, body := post(at.NodeName, empty.ID)
		if code != http.StatusConflict || !strings.Contains(body, sentence) {
			t.Errorf("%s: LOAD answered %d %s, want 409 with %q", step.status, code, body, sentence)
		}
		if got := loadEffect(t, db, empty.ID); got != before {
			t.Errorf("%s: a refused LOAD changed the carrier:\n got %s\nwant %s", step.status, got, before)
		}
		// The bins page withholds Load Payload with the same sentence.
		if got := pageLoadRefusal(t, h, empty.ID); got != sentence {
			t.Errorf("%s: the bins page says %q, want %q", step.status, got, sentence)
		}
	}

	// Delivered: the hold went with the arrival.
	sim.DriveState(o.VendorOrderID, "FINISHED")
	testdb.RequireOrderStatus(t, db, "legal-pull", protocol.StatusDelivered)
	testdb.AssertBinAtNode(t, db, empty.ID, sd.LineNode.ID)
	testdb.AssertBinUnclaimed(t, db, empty.ID)
	// The soft half of the hold, asked as the check asks it.
	var spoken sql.NullInt64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT `+reservations.BinSpokenForByOrderSQL+` FROM bins b WHERE b.id=$1`,
		empty.ID).Scan(&spoken), "read the carrier's pending reservation")
	if spoken.Valid {
		t.Fatalf("delivered: order %d still holds the carrier by a pending reservation", spoken.Int64)
	}

	if got := pageLoadRefusal(t, h, empty.ID); got != "" {
		t.Errorf("delivered: the bins page still withholds Load Payload: %q", got)
	}
	if code, body := post(sd.LineNode.Name, empty.ID); code != http.StatusOK {
		t.Fatalf("delivered: LOAD answered %d %s, want 200", code, body)
	}
	// A carrier nobody ever held, beside it, loaded the same way.
	resting := testdb.CreateBinAtNode(t, db, "", sd.LineNode.ID, "BIN-LEGAL-REST")
	if code, body := post(sd.LineNode.Name, resting.ID); code != http.StatusOK {
		t.Fatalf("resting: LOAD answered %d %s, want 200", code, body)
	}
	if got, want := loadEffect(t, db, empty.ID), loadEffect(t, db, resting.ID); got != want {
		t.Errorf("the delivered carrier's load differs from a resting carrier's:\n got %s\nwant %s", got, want)
	}

	// The load is followed by the order's confirm, unchanged.
	d.HandleOrderReceipt(testdb.Envelope(), &protocol.OrderReceipt{
		OrderUUID: "legal-pull", ReceiptType: "confirmed", FinalCount: 30,
	})
	testdb.RequireOrderStatus(t, db, "legal-pull", protocol.StatusConfirmed)
	if got := testdb.RequireBin(t, db, empty.ID); got.PayloadCode != sd.Payload.Code || got.UOPRemaining != 30 {
		t.Errorf("after the confirm: payload %q uop %d, want %q 30", got.PayloadCode, got.UOPRemaining, sd.Payload.Code)
	}
}

// TestLoad_AStagedCarrierAtRestLoads — a carrier a delivery left standing
// staged (every arrival off a storage slot is staged, resolveNodeStaging; a
// keep-staged spare on its spot is one) is at rest: no order holds it, and both
// LOAD doors load it.
func TestLoad_AStagedCarrierAtRestLoads(t *testing.T) {
	t.Parallel()
	for _, door := range []string{"loader LOAD", "admin Load Payload"} {
		t.Run(door, func(t *testing.T) {
			t.Parallel()
			h, db := testHandlers(t)
			sd := testdb.SetupStandardData(t, db)
			bin := testdb.CreateBinAtNode(t, db, "", sd.LineNode.ID, "BIN-LEGAL-STAGED")
			o := testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.Status = protocol.StatusDispatched
				o.DeliveryNode = sd.LineNode.Name
			})
			testdb.ClaimBinForTest(t, db, bin.ID, o.ID)
			// The arrival: staged, the claim and the reservation given up.
			_, err := h.engine.BinService().ApplyArrival(bin.ID, sd.LineNode.ID, true, nil, o.ID)
			testutil.MustNoErr(t, err, "the delivery's arrival")
			testdb.SeedOrderStatus(t, db, o.ID, string(protocol.StatusDelivered), "")
			if got := testdb.RequireBin(t, db, bin.ID); got.Status != "staged" || got.ClaimedBy != nil {
				t.Fatalf("precondition: status %q claimed_by %v, want staged and unclaimed", got.Status, got.ClaimedBy)
			}

			var code int
			var body string
			if door == "loader LOAD" {
				rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", map[string]any{
					"node_name": sd.LineNode.Name, "bin_id": bin.ID, "payload_code": sd.Payload.Code,
					"uop_count": 30, "manifest": []map[string]any{{"part_number": "P1"}},
				})
				code, body = rec.Code, rec.Body.String()
			} else {
				rec := postJSON(t, h.apiBinAction, "/api/bins/action", map[string]any{
					"id": bin.ID, "action": "load_payload",
					"params": map[string]any{"payload_code": sd.Payload.Code, "uop_override": 30},
				})
				code, body = rec.Code, rec.Body.String()
			}
			if code != http.StatusOK {
				t.Fatalf("%s on a staged carrier at rest: %d %s, want 200", door, code, body)
			}
			if got := testdb.RequireBin(t, db, bin.ID); got.PayloadCode != sd.Payload.Code || got.UOPRemaining != 30 {
				t.Errorf("%s: payload %q uop %d, want %q 30", door, got.PayloadCode, got.UOPRemaining, sd.Payload.Code)
			}
		})
	}
}
