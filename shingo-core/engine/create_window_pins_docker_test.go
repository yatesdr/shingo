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
	"shingocore/config"
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

// cwAnomalies is every anomaly naming orderID, by issue, as the engine lists
// them to the diagnostics page: the store's findings and the asks named at boot.
func cwAnomalies(t *testing.T, eng *Engine, orderID int64) []string {
	t.Helper()
	var out []string
	for _, a := range cwAnomaliesFor(t, eng, orderID) {
		out = append(out, a.Issue)
	}
	return out
}

// cwAnomaliesFor is every anomaly naming orderID, whole.
func cwAnomaliesFor(t *testing.T, eng *Engine, orderID int64) []*reconciliation.Anomaly {
	t.Helper()
	all, err := eng.Reconciliation().ListAnomalies()
	if err != nil {
		t.Fatalf("anomalies: %v", err)
	}
	var out []*reconciliation.Anomaly
	for _, a := range all {
		if a.OrderID != nil && *a.OrderID == orderID {
			out = append(out, a)
		}
	}
	return out
}

// TestCrashInsideACreate_IsNamedAtBootAndAgedByTheStuckFlag: Core dies inside an
// order's create, once before the fleet answers and once after the fleet took
// the job. Either way the row is what the hand committed: its waiting status
// (`pending` here), the ask naming the job it asked for, no vendor id, the bin
// claimed. A restarted Core does not track it (no vendor id), does not scan it
// (an ask is open, so it is not acquiring), and does not ask the fleet again.
// Its boot names it: one anomaly, ask_open_at_boot, naming the asked id, from
// the first moment, and the 30-minute stuck flag beside it later. After the
// fleet took the job, the fleet holds a job Core names only by the ask. No
// history row names the asked id: `dispatched` is the fleet's yes, which never
// came.
//
// It also asks the one reader the reviews missed, isParkedOnDeckOrder, inside
// the create: it reads the order as in flight.
//
// D3's flip: the restarted Core named the order in no anomaly until the stuck
// flag found it 30 minutes on (it was TestCrashInsideACreate_IsFoundOnlyByTheStuckFlag).
//
// MUTATION (fires): drop the TakeBootAsks call from Engine.Start → no anomaly
// names the order straight after the restart.
func TestCrashInsideACreate_IsNamedAtBootAndAgedByTheStuckFlag(t *testing.T) {
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
			named := cwAnomaliesFor(t, eng2, o.ID)
			if len(named) != 1 || named[0].Issue != "ask_open_at_boot" || !strings.Contains(named[0].Detail, asked[0]) {
				t.Errorf("anomalies naming the order straight after the restart = %v, want ask_open_at_boot naming %s",
					cwAnomalies(t, eng2, o.ID), asked[0])
			}
			if sum, err := eng2.Reconciliation().Summary(); err != nil || sum.Status == "ok" || sum.TotalAnomalies < 1 {
				t.Errorf("the restarted Core's summary reads %+v (%v); an ask a crash left open is an anomaly", sum, err)
			}
			cwAge(t, db, o.ID, 31*time.Minute)
			if got := strings.Join(cwAnomalies(t, eng2, o.ID), ","); got != "active_order_stuck,ask_open_at_boot" {
				t.Errorf("anomalies naming the order 31 minutes on = %v, want the stuck flag beside the boot's", got)
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
	if got := cwAnomalies(t, eng, o.ID); strings.Join(got, ",") != "active_order_stuck" {
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

// cwAskedRow is an order resting in status with ask open on it, as a Core that
// died inside its create leaves one.
func cwAskedRow(t *testing.T, db *store.DB, sd *testdb.StandardData, uuid string, status protocol.Status, ask string) *orders.Order {
	t.Helper()
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.OrderType = dispatch.OrderTypeRetrieve
		o.Status = status
		o.PayloadCode = sd.Payload.Code
		o.SourceNode = sd.StorageNode.Name
		o.DeliveryNode = sd.LineNode.Name
	})
	if _, err := db.Exec(`UPDATE orders SET asked_vendor_order_id = $2 WHERE id = $1`, o.ID, ask); err != nil {
		t.Fatalf("open the ask on order %d: %v", o.ID, err)
	}
	return testdb.RequireOrder(t, db, uuid)
}

// TestBootAsk_AnEndedRowsAskGoesToTheTerminateChase: an order that ended while
// its create was in flight, and then Core died, carries the ask on its ended
// row: the fleet may run that job for an order Core has given up on. Boot
// hands it to the terminate chase, the same move the handover makes when the
// fleet does not take the cancel (orders.ChaseEndedAsk): the job named on the
// ended row as CREATED, the ask cleared. A terminal row carries no ask, and no
// anomaly names the order (its holds went with its ending).
//
// MUTATION (fires): TakeBootAsks lists an ended row like a live one → the row
// keeps its ask, the chase never sees the job, and the anomaly names it.
func TestBootAsk_AnEndedRowsAskGoesToTheTerminateChase(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	const job = "sg-ended-crash"
	o := cwAskedRow(t, db, sd, "cw-ended-ask", protocol.StatusCancelled, job)

	eng := newTestEngine(t, db, testdb.NewCreateWindowFleet())

	after := testdb.RequireOrder(t, db, "cw-ended-ask")
	if after.AskedVendorOrderID != "" || after.VendorOrderID != job || after.VendorState != "CREATED" {
		t.Errorf("the ended row reads ask %q vendor %q state %q after boot; want the job %s named for the "+
			"terminate chase (CREATED) and no ask", after.AskedVendorOrderID, after.VendorOrderID, after.VendorState, job)
	}
	now := time.Now().UTC()
	ended, err := db.ListEndedWithLiveVendorState(now.Add(-time.Hour), now.Add(time.Minute), 50)
	if err != nil {
		t.Fatalf("the chase's list: %v", err)
	}
	found := false
	for _, e := range ended {
		found = found || e.ID == o.ID
	}
	if !found {
		t.Errorf("the terminate chase does not list order %d; the job it asked for is chased by nothing", o.ID)
	}
	if got := cwAnomalies(t, eng, o.ID); len(got) != 0 {
		t.Errorf("anomalies naming the ended order = %v, want none: its job is with the chase", got)
	}
}

// TestBootAsk_AnAskThisCoreOpensIsNotListed: only the asks a previous Core left
// are named. Inside a create this Core is making, the order's ask is open and
// no anomaly names it, while the boot's anomaly names the crash's.
//
// MUTATION (fires): list every open ask, not the ones named at boot → the order
// inside its create is listed as ask_open_at_boot.
func TestBootAsk_AnAskThisCoreOpensIsNotListed(t *testing.T) {
	t.Parallel()
	testdb.ExpectAskLeftOpen(t, "the crash's ask stays open: a person ends it")
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	crash := cwAskedRow(t, db, sd, "cw-boot-crash", protocol.StatusPending, "sg-boot-crash")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "CW-LIVE-BIN")
	f := testdb.NewCreateWindowFleet()
	var inside []string
	var eng *Engine
	f.Inside = func(_ int, req fleet.CreateOrderRequest) error {
		if row, err := db.GetOrderByUUID("cw-live"); err == nil && row.AskedVendorOrderID == req.OrderID {
			inside = cwAnomalies(t, eng, row.ID)
		} else {
			t.Errorf("inside the create the order does not hold the ask %s", req.OrderID)
		}
		return nil
	}
	eng = newTestEngine(t, db, f)
	cwRetrieve(t, eng, sd, "cw-live")

	if len(inside) != 0 {
		t.Errorf("inside this Core's own create the order is listed %v; an ask this Core opened is not a crash's", inside)
	}
	if got := cwAnomalies(t, eng, crash.ID); strings.Join(got, ",") != "ask_open_at_boot" {
		t.Errorf("the crash's ask is listed %v, want ask_open_at_boot", got)
	}
}

// TestBootAsk_AListedOrderAPersonEndsGoesToTheChase: the anomaly's advice is to
// cancel the order. An ending lands on an asked row and releases its holds, but
// leaves the ask; the next reconciliation pass (ReconciliationService.Loop, at
// a 50 ms interval here) hands the job to the terminate chase, as boot does an
// ended row's, and the anomaly is gone.
//
// MUTATION (fires): drop chaseEndedBootAsks from the reconciliation pass → the
// cancelled row keeps its ask and the anomaly stays.
func TestBootAsk_AListedOrderAPersonEndsGoesToTheChase(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	const job = "sg-boot-then-cancelled"
	o := cwAskedRow(t, db, sd, "cw-person-ends", protocol.StatusPending, job)
	eng := newUnstartedEngineWith(t, db, testdb.NewCreateWindowFleet(), func(c *config.Config) {
		c.Staging.SweepInterval = 50 * time.Millisecond
	})
	eng.Start()
	t.Cleanup(func() { eng.Stop() })
	if got := cwAnomalies(t, eng, o.ID); strings.Join(got, ",") != "ask_open_at_boot" {
		t.Fatalf("fixture: boot listed %v, want ask_open_at_boot", got)
	}

	if ok, err := db.TerminalizeOrder(o.ID, protocol.StatusCancelled, "a person cancels the listed order"); err != nil || !ok {
		t.Fatalf("cancel the listed order: ok=%v err=%v", ok, err)
	}
	after := testdb.RequireOrder(t, db, "cw-person-ends")
	for deadline := time.Now().Add(10 * time.Second); after.AskedVendorOrderID != "" && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
		after = testdb.RequireOrder(t, db, "cw-person-ends")
	}
	if after.AskedVendorOrderID != "" || after.VendorOrderID != job {
		t.Errorf("the cancelled row reads ask %q vendor %q after the pass; want the job %s with the chase",
			after.AskedVendorOrderID, after.VendorOrderID, job)
	}
	if got := cwAnomalies(t, eng, o.ID); len(got) != 0 {
		t.Errorf("anomalies naming the cancelled order = %v, want none", got)
	}
}
