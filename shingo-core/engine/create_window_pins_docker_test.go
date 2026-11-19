//go:build docker

package engine

// THE CREATE WINDOW AT THE ENGINE, PINNED AS IT STANDS: a Core that dies inside
// a create, a fleet that refuses for half an hour, and a create whose answer was
// lost. Each runs a real engine on testdb.CreateWindowFleet, which stands the
// test inside every create and takes its answer from it
// (the dispatch-claim pin list).

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reconciliation"
)

// cwRetrieve sends one plain retrieve through the intake door, whose own
// acquire makes the first create. It runs on a goroutine of its own and waits
// for it, so a create that dies (testdb.TookItAndDied, DiedBeforeAnswer) ends
// that goroutine and not the test.
func cwRetrieve(t *testing.T, eng *Engine, sd *testdb.StandardData, uuid string) *orders.Order {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
			OrderUUID: uuid, OrderType: dispatch.OrderTypeRetrieve, PayloadCode: sd.Payload.Code,
			SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name, Quantity: 1,
		})
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the intake door did not return in 30 s")
	}
	return testdb.RequireOrder(t, eng.db, uuid)
}

// cwAge moves every history row and the birth of the order back by d: time
// passing for the stuck flag, which ages an order by its newest history row
// (store/reconciliation stuckOrderQuery), without moving the process clock
// other tests read.
func cwAge(t *testing.T, db *store.DB, orderID int64, d time.Duration) {
	t.Helper()
	secs := int(d.Seconds())
	for _, q := range []string{
		`UPDATE order_history SET created_at = created_at - make_interval(secs => $2) WHERE order_id = $1`,
		`UPDATE orders SET created_at = created_at - make_interval(secs => $2),
		                   updated_at = updated_at - make_interval(secs => $2) WHERE id = $1`,
	} {
		if _, err := db.Exec(q, orderID, secs); err != nil {
			t.Fatalf("age order %d by %s: %v", orderID, d, err)
		}
	}
}

// cwAnomalies is every anomaly naming orderID, by issue.
func cwAnomalies(t *testing.T, db *store.DB, orderID int64) []string {
	t.Helper()
	all, err := reconciliation.ListAnomalies(db.DB)
	if err != nil {
		t.Fatalf("anomalies: %v", err)
	}
	var out []string
	for _, a := range all {
		if a.OrderID != nil && *a.OrderID == orderID {
			out = append(out, a.Issue)
		}
	}
	return out
}

// TestCrashInsideACreate_IsFoundOnlyByTheStuckFlag: Core dies inside an order's
// create, once before the fleet answers and once after the fleet took the job.
// Either way the row is what the hand committed: its waiting status (`pending`
// here), the ask naming the job it asked for, no vendor id, the bin claimed. A
// restarted Core does not track it (no vendor id), does not scan it (an ask is
// open, so it is not acquiring), does not ask the fleet again, and names it in
// no anomaly; only the 30-minute stuck flag finds it. After the fleet took the
// job, the fleet holds a job Core cannot name but by the ask. No history row
// names the asked id: `dispatched` is the fleet's yes, which never came.
//
// It also asks the one reader the reviews missed, isParkedOnDeckOrder, inside
// the create: it reads the order as in flight.
//
// As built before, the row read `dispatched` with no vendor id and only the
// hand's history sentence carried the asked id. It flips when boot lists an
// open ask as an anomaly naming the asked id.
func TestCrashInsideACreate_IsFoundOnlyByTheStuckFlag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		death error
	}{
		{"before the fleet answered", testdb.DiedBeforeAnswer},
		{"after the fleet took the job", testdb.TookItAndDied},
	} {
		death := tc.death
		t.Run(tc.name, func(t *testing.T) {
			testdb.ExpectAskLeftOpen(t, "Core dies inside the create: its ask is the one record of the job")
			db := testdb.Open(t)
			sd := testdb.SetupStandardData(t, db)
			bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "CW-CRASH-BIN")
			f := testdb.NewCreateWindowFleet()
			parkedInside := true
			f.Inside = func(n int, req fleet.CreateOrderRequest) error {
				o, err := db.GetOrderByVendorID(req.OrderID)
				if err == nil && o != nil {
					t.Errorf("inside the create the row already names %s", req.OrderID)
				}
				if row, err := db.GetOrderByUUID("cw-crash"); err == nil {
					parkedInside = isParkedOnDeckOrder(row)
				}
				return death
			}
			eng := newTestEngine(t, db, f)
			o := cwRetrieve(t, eng, sd, "cw-crash")
			if parkedInside {
				t.Errorf("inside the create isParkedOnDeckOrder read the order as parked; a Recover press would cancel it")
			}
			asked := f.Asked()
			if len(asked) != 1 {
				t.Fatalf("the fleet was asked %d time(s) before the death, want 1", len(asked))
			}
			if o.Status != protocol.StatusPending || o.VendorOrderID != "" || o.AskedVendorOrderID != asked[0] {
				t.Fatalf("after the death the row reads %s vendor %q ask %q; want pending, no vendor id, asking %s",
					o.Status, o.VendorOrderID, o.AskedVendorOrderID, asked[0])
			}
			if b, err := db.GetBin(bin.ID); err != nil || b.ClaimedBy == nil || *b.ClaimedBy != o.ID {
				t.Errorf("the dead hand's bin claim is gone (bin %+v, err %v); a crash keeps its holds", b, err)
			}

			// A restarted Core: a fresh engine on the same database, its boot,
			// two passes. The fleet is healthy now.
			f.Inside = nil
			eng2 := newTestEngine(t, db, f)
			eng2.RunFulfillmentScan()
			eng2.RunFulfillmentScan()

			o = testdb.RequireOrder(t, db, "cw-crash")
			if o.Status != protocol.StatusPending || o.VendorOrderID != "" || o.AskedVendorOrderID != asked[0] {
				t.Errorf("after the restart the row reads %s vendor %q ask %q, want it untouched",
					o.Status, o.VendorOrderID, o.AskedVendorOrderID)
			}
			if got := f.Asked(); len(got) != 1 {
				t.Errorf("the restarted Core asked the fleet again: %v", got)
			}
			tracked, err := db.ListTrackedVendorOrderIDs()
			if err != nil {
				t.Fatalf("tracked: %v", err)
			}
			if len(tracked) != 0 {
				t.Errorf("Core tracks %q; as built nothing names the job", tracked)
			}
			wantHeld := 0
			if death == testdb.TookItAndDied {
				wantHeld = 1
			}
			if got := len(f.CreateRequests()); got != wantHeld {
				t.Errorf("the fleet holds %d job(s), want %d", got, wantHeld)
			}
			hist, err := db.ListOrderHistory(o.ID)
			if err != nil {
				t.Fatalf("history: %v", err)
			}
			for _, h := range hist {
				if h.Status == protocol.StatusDispatched {
					t.Errorf("a `dispatched` history row (%q) for a job the fleet never confirmed", h.Detail)
				}
			}
			if got := cwAnomalies(t, db, o.ID); len(got) != 0 {
				t.Errorf("anomalies naming the order straight after the restart = %v, want none as built", got)
			}
			cwAge(t, db, o.ID, 31*time.Minute)
			if got := cwAnomalies(t, db, o.ID); strings.Join(got, ",") != "active_order_stuck" {
				t.Errorf("anomalies naming the order 31 minutes on = %v, want the stuck flag alone", got)
			}
		})
	}
}

// TestRefusingFleet_TheStuckFlagAgesItFromTheFirstPark: a fleet that refuses an
// order on every pass for more than half an hour. The first refusal parks it
// `queued`; a refusal after that, under the same wait, writes no history row,
// and the stuck flag ages an order by its newest history row, so it finds the
// order 30 minutes after its first park. A FLEET pin: as built before, each pass
// wrote the hand's `dispatched` row and the park's `queued` row, and the flag
// never found an order the robot system had refused all along.
func TestRefusingFleet_TheStuckFlagAgesItFromTheFirstPark(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "CW-STUCK-BIN")
	f := testdb.NewCreateWindowFleet()
	f.Inside = func(int, fleet.CreateOrderRequest) error { return fmt.Errorf("RDS error 50001: robot system refused") }
	eng := newTestEngine(t, db, f)
	o := cwRetrieve(t, eng, sd, "cw-stuck")
	if o.Status != protocol.StatusQueued || o.QueueCode != string(protocol.QueueFleetUnavailable) {
		t.Fatalf("fixture: the first refusal left %s/%s, want queued under fleet_unavailable", o.Status, o.QueueCode)
	}
	// Four passes ten minutes apart: forty minutes of refusals.
	for pass := 0; pass < 4; pass++ {
		cwAge(t, db, o.ID, 10*time.Minute)
		eng.RunFulfillmentScan()
	}
	if got := len(f.Asked()); got < 5 {
		t.Fatalf("the fleet was asked %d times, want a create every pass (5)", got)
	}
	if got := cwAnomalies(t, db, o.ID); strings.Join(got, ",") != "active_order_stuck" {
		t.Errorf("anomalies naming an order refused for forty minutes = %v, want the stuck flag", got)
	}
}

// TestUnansweredCreate_IsAskedAgainUnderANewID: the fleet takes the job and
// Core's call times out. Core reads the timeout as a refusal, the next pass
// asks under a new id, and the fleet then holds two jobs for one order: Core
// names and tracks the second, and nothing names, tracks or cancels the first.
// It stays as it is in this build (the brief: the next brief splits a create
// that was sent and got no answer from one the fleet refused). Pinned so the
// change does not move it.
func TestUnansweredCreate_IsAskedAgainUnderANewID(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "CW-LOST-BIN")
	f := testdb.NewCreateWindowFleet()
	f.Inside = func(n int, _ fleet.CreateOrderRequest) error {
		if n == 1 {
			return testdb.TookItAndLostTheAnswer
		}
		return nil
	}
	eng := newTestEngine(t, db, f)
	o := cwRetrieve(t, eng, sd, "cw-lost")
	if o.Status != protocol.StatusQueued || o.VendorOrderID != "" {
		t.Fatalf("after the lost answer the row reads %s vendor %q, want queued with no vendor id", o.Status, o.VendorOrderID)
	}
	eng.RunFulfillmentScan()

	o = testdb.RequireOrder(t, db, "cw-lost")
	asked, held := f.Asked(), f.CreateRequests()
	if len(asked) != 2 || asked[0] == asked[1] || len(held) != 2 {
		t.Fatalf("the fleet was asked %v and holds %d job(s); want two asks under two ids, both held", asked, len(held))
	}
	if o.VendorOrderID != asked[1] {
		t.Errorf("Core names %q, want the second job %q", o.VendorOrderID, asked[1])
	}
	tracked, err := db.ListTrackedVendorOrderIDs()
	if err != nil {
		t.Fatalf("tracked: %v", err)
	}
	if strings.Join(tracked, ",") != asked[1] {
		t.Errorf("Core tracks %v, want only %s", tracked, asked[1])
	}
	for _, id := range f.CancelRequests() {
		if id == asked[0] {
			t.Errorf("Core cancelled the first job %s; as built nothing does", id)
		}
	}
}
