//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// run_twice_docker_test.go — docket B11's crash half: each order-channel handler
// run a second time on the same envelope (the orc's note,
// work/loc-census-2026-11-20/NOTE-orc-b11-crash-half-2026-11-24.md).
//
// Once the inbox row is written after the handler (middleware.NewInboxDedup), a
// first run cut by a crash leaves no row, Kafka delivers the message again, and
// the redelivery reaches the handler, which meets whatever the first run left.
// So every handler must be safe to run again. Each handler has two cases:
//
//   - run, cut, run again: the first run's goroutine ends at the named point
//     (runtime.Goexit, as the process would end there), then the redelivery
//     runs whole.
//   - run, finish, run again: a whole first run, then the same envelope again,
//     as a crash between the handler's end and its inbox row delivers it. It
//     should do nothing new and send nothing new.
//
// and what its station is told on a first run, pinned byte for byte: the changes
// leave it as it is. The handlers are driven directly; the dedup's own half is
// TestCoreHandler_AnOrderCutInHalfIsCreatedOnceAfterRedelivery (messaging).
//
// A dispatcher here has no acquirer (door.go), so an order its door births stays
// `pending` for the scanner's next pass, which is where an order cut before its
// apply waits too.
//
// Its own file rather than seven siblings': one docket item across the
// order-channel handlers, on one cut rig (cutEmitter, cutRun, toldSince) that
// no existing file has.

// b11KnownRed skips a case that is red until its handler's change lands, unless
// SHINGO_KNOWN_RED=1.
func b11KnownRed(t *testing.T, until string) {
	t.Helper()
	if os.Getenv("SHINGO_KNOWN_RED") == "" {
		t.Skipf("docket B11 red pin, red until %s; run with SHINGO_KNOWN_RED=1", until)
	}
}

// cutEmitter is the mock emitter with a cut: cut, when set, is called at each
// event below, and a first run ends where it calls runtime.Goexit. It keeps the
// projections, which reach a station as a message, and the status each
// cancelled event names (cancelledFrom): the engine's cancellation notice
// carries it to the station as PreviousStatus.
type cutEmitter struct {
	*mockEmitter
	cut           func(event string, orderID int64)
	projections   []string
	cancelledFrom []string
}

func (e *cutEmitter) at(event string, orderID int64) {
	if e.cut != nil {
		e.cut(event, orderID)
	}
}

func (e *cutEmitter) EmitOrderReceived(orderID int64, edgeUUID, stationID string, orderType protocol.OrderType, payloadCode, deliveryNode string) {
	e.mockEmitter.EmitOrderReceived(orderID, edgeUUID, stationID, orderType, payloadCode, deliveryNode)
	e.at("received", orderID)
}

func (e *cutEmitter) EmitOrderDispatched(orderID int64, vendorOrderID, sourceNode, destNode string) {
	e.mockEmitter.EmitOrderDispatched(orderID, vendorOrderID, sourceNode, destNode)
	e.at("dispatched", orderID)
}

func (e *cutEmitter) EmitOrderCancelled(orderID int64, edgeUUID, stationID, reason, previousStatus string, recovers *int64) {
	e.mockEmitter.EmitOrderCancelled(orderID, edgeUUID, stationID, reason, previousStatus, recovers)
	e.cancelledFrom = append(e.cancelledFrom, previousStatus)
	e.at("cancelled", orderID)
}

func (e *cutEmitter) EmitOrderCompleted(orderID int64, edgeUUID, stationID string, recovers *int64) {
	e.mockEmitter.EmitOrderCompleted(orderID, edgeUUID, stationID, recovers)
	e.at("completed", orderID)
}

func (e *cutEmitter) ProjectOrder(stationID string, p protocol.OrderProjection) {
	e.mockEmitter.ProjectOrder(stationID, p)
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	e.projections = append(e.projections, "projection to "+stationID+": "+normalizeTold(b))
}

// twiceRig is one cell's database, with station tells on as the engine names
// them, and a dispatcher on a fleet that takes every order.
type twiceRig struct {
	db      *store.DB
	d       *Dispatcher
	e       *cutEmitter
	backend *testdb.MockBackend
}

func newTwiceRig(t *testing.T) twiceRig {
	t.Helper()
	db := testDB(t)
	tellToStations(db)
	backend := testdb.NewSuccessBackend()
	e := &cutEmitter{mockEmitter: &mockEmitter{}}
	d := NewDispatcher(db, backend, e, "core", "shingo.dispatch", nil)
	d.DebugLog = testDebugLog(t)
	return twiceRig{db: db, d: d, e: e, backend: backend}
}

// cutRun runs a first run on its own goroutine and waits for it to end, whole
// or where its cut calls runtime.Goexit.
func cutRun(t *testing.T, run func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		run()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the first run did not end")
	}
}

// outboxMark is the id of the outbox's last row, 0 for none.
func outboxMark(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM outbox`).Scan(&id), "outbox mark")
	return id
}

// toldSince lists every message queued after outbox row mark, oldest first, as
// "type to station: body": what Core told its stations. The body is the
// envelope's own payload (normalizeTold).
func toldSince(t *testing.T, db *store.DB, mark int64) []string {
	t.Helper()
	msgs, err := db.ListPendingOutbox(500)
	testutil.MustNoErr(t, err, "list outbox")
	var out []string
	for _, m := range msgs {
		if m.ID <= mark {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "unmarshal envelope")
		out = append(out, fmt.Sprintf("%s to %s: %s", m.MsgType, m.DstStation, normalizeTold(env.Payload)))
	}
	return out
}

var (
	toldUUID   = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	toldVendor = regexp.MustCompile(`\bsg-[0-9]+-[0-9a-f]{8}\b`)
)

// normalizeTold renders a message body with what differs between two runs of
// one cell written as a placeholder: an RFC 3339 clock reading as <t>, a minted
// uuid as <uuid>, a minted fleet job id as <job>. Keys in encoding/json's sorted
// order.
func normalizeTold(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(normalizeToldValue(v))
	if err != nil {
		return string(raw)
	}
	return toldVendor.ReplaceAllString(toldUUID.ReplaceAllString(string(b), "<uuid>"), "<job>")
}

func normalizeToldValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = normalizeToldValue(e)
		}
	case []any:
		for i, e := range x {
			x[i] = normalizeToldValue(e)
		}
	case string:
		if _, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return "<t>"
		}
	}
	return v
}

// assertTold fails unless got is want, line for line.
func assertTold(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got  %d line(s):\n  %s\n want %d line(s):\n  %s", what,
			len(got), strings.Join(got, "\n  "), len(want), strings.Join(want, "\n  "))
	}
}

// ordersWithUUID counts the orders Core holds under one edge uuid.
func ordersWithUUID(t *testing.T, db *store.DB, uuid string) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM orders WHERE edge_uuid=$1`, uuid).Scan(&n), "count orders")
	return n
}

// ── order request ───────────────────────────────────────────────────────────

// TestRunTwice_OrderRequest: a retrieve for a part with no stock. Its first run is
// born `pending`, decides a wait and parks `queued`, its station told once by the
// park's own write (TestHandleOrderRequest_Retrieve_NoSource). The cut is the
// post-birth hook: the row is written and nothing after it has run.
func TestRunTwice_OrderRequest(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, uuid string) (twiceRig, *protocol.Envelope, *protocol.OrderRequest) {
		t.Helper()
		r := newTwiceRig(t)
		_, line, bp := testdb.SetupTestData(t, r.db)
		env := testEnvelope()
		env.ID, env.Type = "env-"+uuid, protocol.TypeOrderRequest
		return r, env, &protocol.OrderRequest{OrderUUID: uuid, OrderType: OrderTypeRetrieve,
			PayloadCode: bp.Code, DeliveryNode: line.Name, Quantity: 1}
	}

	t.Run("the station is told on a first run", func(t *testing.T) {
		t.Parallel()
		r, env, req := setup(t, "rt-or-told")
		mark := outboxMark(t, r.db)
		r.d.HandleOrderRequest(env, req)
		assertTold(t, "what a first run tells", append(r.e.projections, toldSince(t, r.db, mark)...), []string{
			`projection to line-1: {"delivery_node":"LINE1-IN","order_type":"retrieve","order_uuid":"rt-or-told","origin_class":"orphan","payload_code":"PART-A","quantity":1,"status":"pending","version":1}`,
			`order.update to line-1: {"detail":"Waiting for material: PART-A","order_uuid":"rt-or-told","queue_code":"waiting_for_material","queue_reason":"Waiting for material: PART-A","status":"queued","version":2}`,
		})
	})

	t.Run("run, cut, run again", func(t *testing.T) {
		t.Parallel()
		r, env, req := setup(t, "rt-or-cut")
		cutting := true
		r.d.SetPostBirthHook(func(int64) {
			if cutting {
				runtime.Goexit()
			}
		})
		cutRun(t, func() { r.d.HandleOrderRequest(env, req) })
		cutting = false
		mark := outboxMark(t, r.db)
		r.d.HandleOrderRequest(env, req)

		if n := ordersWithUUID(t, r.db, req.OrderUUID); n != 1 {
			t.Fatalf("%d orders under %s after the cut and the redelivery, want 1", n, req.OrderUUID)
		}
		assertTold(t, "the redelivery told the station (a refusal ends the Edge's row while Core holds the order)",
			toldSince(t, r.db, mark), nil)
		testdb.AssertOrderStatus(t, r.db, req.OrderUUID, StatusPending) // as the cut left it, for the next pass
		if len(r.e.received) != 1 {
			t.Errorf("order received announced %d times, want 1", len(r.e.received))
		}
	})

	t.Run("run, finish, run again", func(t *testing.T) {
		t.Parallel()
		r, env, req := setup(t, "rt-or-finish")
		r.d.HandleOrderRequest(env, req)
		mark := outboxMark(t, r.db)
		r.d.HandleOrderRequest(env, req)

		if n := ordersWithUUID(t, r.db, req.OrderUUID); n != 1 {
			t.Fatalf("%d orders under %s after the repeat, want 1", n, req.OrderUUID)
		}
		assertTold(t, "the repeat told the station", toldSince(t, r.db, mark), nil)
		testdb.AssertOrderStatus(t, r.db, req.OrderUUID, StatusQueued)
		if len(r.e.received) != 1 || len(r.e.projected) != 1 {
			t.Errorf("received %d, projected %d after the repeat, want 1 and 1", len(r.e.received), len(r.e.projected))
		}
	})

	// The note's open question: the same station and uuid meeting a TERMINAL order.
	// A move with no pickup is born and failed by its first run, which tells its
	// station the failure (the terminal write's tell and the order.error). The
	// repeat returns as for a live order: the station already holds Core's word.
	t.Run("a first run that failed the order, then the repeat", func(t *testing.T) {
		t.Parallel()
		r, env, req := setup(t, "rt-or-failed")
		req.OrderType, req.SourceNode = OrderTypeMove, ""
		r.d.HandleOrderRequest(env, req)
		testdb.AssertOrderStatus(t, r.db, req.OrderUUID, StatusFailed)
		mark := outboxMark(t, r.db)
		r.d.HandleOrderRequest(env, req)

		if n := ordersWithUUID(t, r.db, req.OrderUUID); n != 1 {
			t.Fatalf("%d orders under %s after the repeat, want 1", n, req.OrderUUID)
		}
		assertTold(t, "the repeat told the station", toldSince(t, r.db, mark), nil)
		if len(r.e.failed) != 1 {
			t.Errorf("order failed announced %d times, want 1", len(r.e.failed))
		}
	})
}

// ── complex request ─────────────────────────────────────────────────────────

// TestRunTwice_ComplexRequest: a two-step complex order. Its first run issues the
// row `pending` and acks the station. The cut is the order-received event: the
// row is written, the ack is not.
func TestRunTwice_ComplexRequest(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, uuid string) (twiceRig, *protocol.Envelope, *protocol.ComplexOrderRequest) {
		t.Helper()
		r := newTwiceRig(t)
		storage, line, bp := testdb.SetupTestData(t, r.db)
		env := testEnvelope()
		env.ID, env.Type = "env-"+uuid, protocol.TypeComplexOrderRequest
		return r, env, &protocol.ComplexOrderRequest{OrderUUID: uuid, PayloadCode: bp.Code, Quantity: 1,
			Steps: []protocol.ComplexOrderStep{
				{Action: protocol.ActionPickup, Node: storage.Name},
				{Action: protocol.ActionDropoff, Node: line.Name},
			}}
	}

	t.Run("the station is told on a first run", func(t *testing.T) {
		t.Parallel()
		r, env, req := setup(t, "rt-cx-told")
		mark := outboxMark(t, r.db)
		r.d.HandleComplexOrderRequest(env, req)
		assertTold(t, "what a first run tells", append(r.e.projections, toldSince(t, r.db, mark)...), []string{
			`order.ack to line-1: {"order_uuid":"rt-cx-told","shingo_order_id":1,"source_node":"STORAGE-A1"}`,
		})
	})

	t.Run("run, cut, run again", func(t *testing.T) {
		t.Parallel()
		r, env, req := setup(t, "rt-cx-cut")
		r.e.cut = func(event string, _ int64) {
			if event == "received" {
				runtime.Goexit()
			}
		}
		cutRun(t, func() { r.d.HandleComplexOrderRequest(env, req) })
		r.e.cut = nil
		mark := outboxMark(t, r.db)
		r.d.HandleComplexOrderRequest(env, req)

		if n := ordersWithUUID(t, r.db, req.OrderUUID); n != 1 {
			t.Fatalf("%d orders under %s after the cut and the redelivery, want 1", n, req.OrderUUID)
		}
		assertTold(t, "the redelivery told the station (a refusal ends the Edge's row while Core holds the order)",
			toldSince(t, r.db, mark), nil)
		testdb.AssertOrderStatus(t, r.db, req.OrderUUID, StatusPending)
	})

	t.Run("run, finish, run again", func(t *testing.T) {
		t.Parallel()
		r, env, req := setup(t, "rt-cx-finish")
		r.d.HandleComplexOrderRequest(env, req)
		mark := outboxMark(t, r.db)
		r.d.HandleComplexOrderRequest(env, req)

		if n := ordersWithUUID(t, r.db, req.OrderUUID); n != 1 {
			t.Fatalf("%d orders under %s after the repeat, want 1", n, req.OrderUUID)
		}
		assertTold(t, "the repeat told the station", toldSince(t, r.db, mark), nil)
		if len(r.e.received) != 1 {
			t.Errorf("order received announced %d times, want 1", len(r.e.received))
		}
	})
}

// ── redirect ────────────────────────────────────────────────────────────────

// TestRunTwice_Redirect: a pre-dispatch move redirected to another line. Its first
// run re-aims the row, hands it to the fleet (`dispatched`, told by that write)
// and acks the station. The cut is the order-dispatched event: the fleet has the
// job and the row says so, the ack is not sent.
func TestRunTwice_Redirect(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, uuid string) (twiceRig, *protocol.Envelope, *protocol.OrderRedirect) {
		t.Helper()
		r := newTwiceRig(t)
		from := &nodes.Node{Name: uuid + "-LINE-1", Enabled: true}
		to := &nodes.Node{Name: uuid + "-LINE-2", Enabled: true}
		testutil.MustNoErr(t, r.db.CreateNode(from), "create the line")
		testutil.MustNoErr(t, r.db.CreateNode(to), "create the redirect's line")
		testutil.MustNoErr(t, r.db.CreateOrder(&orders.Order{
			EdgeUUID: uuid, StationUID: testEnvelope().Src.Station, OrderType: OrderTypeMove,
			Status: StatusQueued, Quantity: 1, SourceNode: from.Name, DeliveryNode: from.Name,
		}), "create the order")
		env := testEnvelope()
		env.ID, env.Type = "env-"+uuid, protocol.TypeOrderRedirect
		return r, env, &protocol.OrderRedirect{OrderUUID: uuid, NewDeliveryNode: to.Name}
	}

	// Each case has its own database, so one uuid serves them all and a whole
	// first run's lines compare to a cut one's.
	var whole []string
	t.Run("the station is told on a first run", func(t *testing.T) {
		r, env, req := setup(t, "rt-rd")
		mark := outboxMark(t, r.db)
		r.d.HandleOrderRedirect(env, req)
		whole = toldSince(t, r.db, mark)
		assertTold(t, "what a first run tells", append(r.e.projections, whole...), []string{
			`order.update to line-1: {"detail":"vendor order <job> created","order_uuid":"rt-rd","status":"dispatched","version":2}`,
			`order.ack to line-1: {"order_uuid":"rt-rd","shingo_order_id":1,"source_node":"rt-rd-LINE-1"}`,
		})
	})

	t.Run("run, cut, run again", func(t *testing.T) {
		r, env, req := setup(t, "rt-rd")
		r.e.cut = func(event string, _ int64) {
			if event == "dispatched" {
				runtime.Goexit()
			}
		}
		mark := outboxMark(t, r.db)
		cutRun(t, func() { r.d.HandleOrderRedirect(env, req) })
		r.e.cut = nil
		r.d.HandleOrderRedirect(env, req)

		if n := len(r.backend.CreateRequests()); n != 1 {
			t.Errorf("the fleet was handed %d jobs, want 1", n)
		}
		if n := len(r.backend.CancelRequests()); n != 0 {
			t.Errorf("the fleet was told to cancel %d jobs, want none", n)
		}
		o := testdb.AssertOrderStatus(t, r.db, req.OrderUUID, StatusDispatched)
		if o != nil && o.DeliveryNode != req.NewDeliveryNode {
			t.Errorf("delivery node %q, want %q", o.DeliveryNode, req.NewDeliveryNode)
		}
		// The cut run and the redelivery tell the station what one whole first
		// run tells it.
		assertTold(t, "the cut run and the redelivery told the station", toldSince(t, r.db, mark), whole)
	})

	t.Run("run, finish, run again", func(t *testing.T) {
		r, env, req := setup(t, "rt-rd")
		mark := outboxMark(t, r.db)
		r.d.HandleOrderRedirect(env, req)
		first := toldSince(t, r.db, mark)
		mark = outboxMark(t, r.db)
		r.d.HandleOrderRedirect(env, req)

		if n := len(r.backend.CreateRequests()); n != 1 {
			t.Errorf("the fleet was handed %d jobs, want 1", n)
		}
		if n := len(r.backend.CancelRequests()); n != 0 {
			t.Errorf("the fleet was told to cancel %d jobs, want none", n)
		}
		// The repeat answers with the first run's answer, the ack, and nothing else.
		var ack []string
		for _, l := range first {
			if strings.HasPrefix(l, protocol.TypeOrderAck+" ") {
				ack = append(ack, l)
			}
		}
		assertTold(t, "the repeat told the station", toldSince(t, r.db, mark), ack)
		testdb.AssertOrderStatus(t, r.db, req.OrderUUID, StatusDispatched)
	})
}

// ── stamped ingest ──────────────────────────────────────────────────────────

// TestRunTwice_StampedIngest: a release-time produce ingest stamped with the
// epoch the Edge counted under. Its first run books the count and bumps the
// bin's epoch in one transaction (recordProduced). The cut is the apply's debug
// line, after that commit: what is left is a finished run's state, since a cut
// inside the transaction rolls it back and the redelivery applies it once.
func TestRunTwice_StampedIngest(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, uuid string) (*Dispatcher, *protocol.Envelope, *protocol.OrderIngestRequest, *bool) {
		t.Helper()
		d, bin, bp := s1bIngestFixture(t)
		tellToStations(d.db)
		cutting := new(bool)
		log := testDebugLog(t)
		d.DebugLog = func(format string, args ...any) {
			log(format, args...)
			if *cutting && strings.HasPrefix(format, "ingest: manifest recorded") {
				runtime.Goexit()
			}
		}
		env := testEnvelope()
		env.ID, env.Type = "env-"+uuid, protocol.TypeOrderIngest
		return d, env, &protocol.OrderIngestRequest{OrderUUID: uuid, PayloadCode: bp.Code, BinID: bin.ID,
			BinEpoch: bin.DeltaEpoch, SourceNode: "SYN-PRESS-S1B", Quantity: 47}, cutting
	}
	// NOT BUILT (lane b11crash, STOP): the stamp alone cannot tell a stamp this
	// ingest applied from one a reset made stale, both one generation behind;
	// the ruling picks how the handler tells them apart.
	const until = "a stamp already applied to the bin is answered ok, nothing written (awaiting a ruling)"
	refusals := func(t *testing.T, d *Dispatcher, binID int64) int {
		t.Helper()
		var n int
		testutil.MustNoErr(t, d.db.QueryRow(`SELECT count(*) FROM bin_uop_ledger WHERE bin_id=$1 AND op=$2`,
			binID, "ingest_stale_epoch_refused").Scan(&n), "refusal rows")
		return n
	}

	// No station claims this press node, so the epoch bump's announcement
	// reaches nobody: what is pinned is that the ingest itself answers nothing on
	// success (manifest-only, no reply).
	t.Run("the station is told on a first run", func(t *testing.T) {
		t.Parallel()
		d, env, req, _ := setup(t, "rt-in-told")
		mark := outboxMark(t, d.db)
		d.HandleOrderIngest(env, req)
		assertTold(t, "what a first run tells", toldSince(t, d.db, mark), nil)
	})

	for _, c := range []struct {
		name string
		cut  bool
	}{{"run, cut, run again", true}, {"run, finish, run again", false}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			b11KnownRed(t, until)
			d, env, req, cutting := setup(t, "rt-in-"+map[bool]string{true: "cut", false: "finish"}[c.cut])
			*cutting = c.cut
			cutRun(t, func() { d.HandleOrderIngest(env, req) })
			*cutting = false
			applied, err := d.db.GetBin(req.BinID)
			testutil.MustNoErr(t, err, "read the bin after the first run")
			mark := outboxMark(t, d.db)
			d.HandleOrderIngest(env, req)

			assertTold(t, "the repeat told the station", toldSince(t, d.db, mark), nil)
			if n := refusals(t, d, req.BinID); n != 0 {
				t.Errorf("%d ingest_stale_epoch_refused row(s) for an ingest that applied, want none", n)
			}
			got, err := d.db.GetBin(req.BinID)
			testutil.MustNoErr(t, err, "read the bin after the repeat")
			if got.UOPRemaining != 47 || got.DeltaEpoch != applied.DeltaEpoch || got.DeltaEpoch != req.BinEpoch+1 {
				t.Errorf("bin after the repeat: uop %d epoch %d, want 47 at epoch %d (booked once)",
					got.UOPRemaining, got.DeltaEpoch, req.BinEpoch+1)
			}
		})
	}
}

// ── cancel, cut inside the cascade ──────────────────────────────────────────

// TestRunTwice_CancelCutInsideTheCascade: a station cancels a digging order. Its
// first run cancels the parent, then its legs, then answers the station
// (CancelOrderWithCascade, HandleOrderCancel). The cut is the parent's cancelled
// event: the parent is cancelled, its legs are not.
func TestRunTwice_CancelCutInsideTheCascade(t *testing.T) {
	t.Parallel()
	// lifted: the dig's first leg has lifted its bin before the cancel, so the
	// cascade keeps it (TestCancelCascade_ALiftedLegFinishesItsStore).
	setup := func(t *testing.T, prefix string, lifted bool) (twiceRig, *orders.Order, *protocol.Envelope, *protocol.OrderCancel) {
		t.Helper()
		r := newTwiceRig(t)
		parent, _, slots, legs := planDigToDissolve(t, r.db, r.d, prefix, 1)
		if lifted {
			transit, err := r.db.GetNodeByDotName("_TRANSIT")
			testutil.MustNoErr(t, err, "resolve _TRANSIT")
			liftBin(t, r.db, r.d, legs[0], slots[0], transit)
		}
		env := &protocol.Envelope{
			ID: "env-" + prefix, Type: protocol.TypeOrderCancel,
			Src: protocol.Address{Role: protocol.RoleEdge, Station: parent.Requester()},
			Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"},
		}
		return r, parent, env, &protocol.OrderCancel{OrderUUID: parent.EdgeUUID, Reason: "operator abort"}
	}
	liveLegs := func(t *testing.T, db *store.DB, parentID int64) []string {
		t.Helper()
		var live []string
		for _, leg := range legsOf(t, db, parentID) {
			if !protocol.IsTerminal(leg.Status) {
				live = append(live, fmt.Sprintf("%d:%s", leg.ID, leg.Status))
			}
		}
		return live
	}

	// Each case has its own database, so one prefix serves them all and a whole
	// first run's lines compare to a cut one's.
	var whole []string
	t.Run("the station is told on a first run", func(t *testing.T) {
		r, parent, env, req := setup(t, "RT-CC", false)
		mark := outboxMark(t, r.db)
		r.d.HandleOrderCancel(env, req)
		whole = toldSince(t, r.db, mark)
		assertTold(t, "what a first run tells", append(r.e.projections, whole...), []string{
			`order.update to test: {"detail":"operator abort","order_uuid":"RT-CC-parent","status":"cancelled","version":3}`,
			`order.update to test: {"detail":"parent order cancelled: operator abort","order_uuid":"<uuid>","status":"cancelled","version":4}`,
			`order.update to test: {"detail":"parent order cancelled: operator abort","order_uuid":"<uuid>","status":"cancelled","version":2}`,
			`order.cancelled to test: {"order_uuid":"RT-CC-parent","reason":"operator abort"}`,
		})
		if live := liveLegs(t, r.db, parent.ID); len(live) != 0 {
			t.Errorf("legs live after a whole cancel: %v", live)
		}
	})

	t.Run("run, cut, run again", func(t *testing.T) {
		r, parent, env, req := setup(t, "RT-CC", false)
		r.e.cut = func(event string, orderID int64) {
			if event == "cancelled" && orderID == parent.ID {
				runtime.Goexit()
			}
		}
		mark := outboxMark(t, r.db)
		cutRun(t, func() { r.d.HandleOrderCancel(env, req) })
		r.e.cut = nil
		testdb.AssertOrderStatus(t, r.db, req.OrderUUID, StatusCancelled)
		if live := liveLegs(t, r.db, parent.ID); len(live) == 0 {
			t.Fatal("fixture: the cut left no live leg")
		}
		r.d.HandleOrderCancel(env, req)

		if live := liveLegs(t, r.db, parent.ID); len(live) != 0 {
			t.Errorf("legs live under a cancelled parent after the redelivery: %v", live)
		}
		// The cut run and the redelivery tell the station what one whole first
		// run tells it.
		assertTold(t, "the cut run and the redelivery told the station", toldSince(t, r.db, mark), whole)
	})

	// A cancel that asks (final brief 5b) is answered by the tells alone, and
	// the redelivery that finishes its cascade sends no order.cancelled either:
	// the cut run and the redelivery tell the station a whole run's tells.
	t.Run("run, cut, run again, asking", func(t *testing.T) {
		if len(whole) != 4 {
			t.Fatalf("a whole first run told %d lines, want the three tells and the answer", len(whole))
		}
		r, parent, env, req := setup(t, "RT-CC", false)
		req.Intent = true
		r.e.cut = func(event string, orderID int64) {
			if event == "cancelled" && orderID == parent.ID {
				runtime.Goexit()
			}
		}
		mark := outboxMark(t, r.db)
		cutRun(t, func() { r.d.HandleOrderCancel(env, req) })
		r.e.cut = nil
		if live := liveLegs(t, r.db, parent.ID); len(live) == 0 {
			t.Fatal("fixture: the cut left no live leg")
		}
		r.d.HandleOrderCancel(env, req)

		if live := liveLegs(t, r.db, parent.ID); len(live) != 0 {
			t.Errorf("legs live under a cancelled parent after the redelivery: %v", live)
		}
		assertTold(t, "the cut run and the redelivery told the station", toldSince(t, r.db, mark), whole[:3])
	})

	t.Run("run, finish, run again", func(t *testing.T) {
		r, parent, env, req := setup(t, "RT-CC", false)
		r.d.HandleOrderCancel(env, req)
		creates, cancels := len(r.backend.CreateRequests()), len(r.backend.CancelRequests())
		mark := outboxMark(t, r.db)
		r.d.HandleOrderCancel(env, req)

		assertTold(t, "the repeat told the station", toldSince(t, r.db, mark), nil)
		if c, k := len(r.backend.CreateRequests()), len(r.backend.CancelRequests()); c != creates || k != cancels {
			t.Errorf("the repeat reached the fleet: creates %d -> %d, cancels %d -> %d", creates, c, cancels, k)
		}
		if live := liveLegs(t, r.db, parent.ID); len(live) != 0 {
			t.Errorf("legs live after the repeat: %v", live)
		}
	})

	// A whole cancel keeps a leg carrying its bin, so its parent is cancelled
	// with a live leg the cascade has nothing to say to. The repeat has nothing
	// to cancel: no second cascade, no second answer.
	t.Run("run, finish, run again, a leg carrying its bin kept", func(t *testing.T) {
		r, parent, env, req := setup(t, "RT-CK", true)
		r.d.HandleOrderCancel(env, req)
		kept := liveLegs(t, r.db, parent.ID)
		if len(kept) != 1 {
			t.Fatalf("fixture: legs live after the whole cancel %v, want the one carrying its bin", kept)
		}
		creates, cancels := len(r.backend.CreateRequests()), len(r.backend.CancelRequests())
		mark := outboxMark(t, r.db)
		r.d.HandleOrderCancel(env, req)

		assertTold(t, "the repeat told the station", toldSince(t, r.db, mark), nil)
		if c, k := len(r.backend.CreateRequests()), len(r.backend.CancelRequests()); c != creates || k != cancels {
			t.Errorf("the repeat reached the fleet: creates %d -> %d, cancels %d -> %d", creates, c, cancels, k)
		}
		if live := liveLegs(t, r.db, parent.ID); strings.Join(live, ",") != strings.Join(kept, ",") {
			t.Errorf("legs live after the repeat %v, want the kept one %v", live, kept)
		}
	})
}

// ── cancel, a plain order ───────────────────────────────────────────────────

// plainCancelOrder is a plain retrieve of testEnvelope's station, storage to
// line, carrying one cart, where a cancel can find it (final brief 1b's three
// answers, then the ways an order ends):
//   - "not lifted": the fleet has the job; the cart is claimed at its pickup.
//   - "carried": the fleet runs it; the cart is on the robot (_TRANSIT), claimed.
//   - "landed": the cart is set down at the line and its claim released, and
//     the order still reads in_transit: the landing, before the delivered write.
//   - "delivered", then "confirmed" (the station's receipt).
//   - "failed" (the fleet failed it), "skipped" (queued, its work gone, no
//     cart) and "cancelled" (Core stopped it).
//   - "unreadable": as not lifted, its cart unclaimed and its stored plan cut
//     short, so where its cart is cannot be read (store.DB.OrderTrips errors).
//
// Each status is reached through the lifecycle's own writers, so each has its
// history row and its tell, before the test's outbox mark. Then its clocks are
// pinned (cancelClocks), so a refusal's sentence is a literal.
func plainCancelOrder(t *testing.T, r twiceRig, uuid, shape string) *orders.Order {
	t.Helper()
	o := buildCancelShape(t, r, uuid, shape)
	for _, q := range []string{
		`UPDATE order_history SET created_at = CASE
			WHEN status = 'delivered' THEN '` + cancelClocks.delivered + `'::timestamptz
			WHEN status IN ('confirmed', 'failed', 'skipped', 'cancelled') THEN '` + cancelClocks.ended + `'::timestamptz
			ELSE created_at END WHERE order_id = $1`,
		`UPDATE bins SET updated_at = '` + cancelClocks.landed + `'::timestamptz
			WHERE id IN (SELECT bin_id FROM order_bins WHERE order_id = $1)`,
		`UPDATE orders SET updated_at = '` + cancelClocks.rowWrite + `'::timestamptz,
			completed_at = CASE WHEN completed_at IS NULL THEN NULL ELSE '` + cancelClocks.rowWrite + `'::timestamptz END
			WHERE id = $1`,
	} {
		_, err := r.db.Exec(q, o.ID)
		testutil.MustNoErr(t, err, "pin the order's clocks")
	}
	return reloadOrder(t, r.db, o.ID)
}

// cancelClocks are plainCancelOrder's pinned times, each source its own, so a
// sentence shows which one it read: the delivered history row; the ending's
// history row; the landing's write of the cart; and a later write of the
// order's own row, which no sentence should read. In America/Chicago (CST in
// November) they read 08:32, 08:35, 08:31 and 08:50.
var cancelClocks = struct{ delivered, ended, landed, rowWrite string }{
	delivered: "2026-11-25T14:32:00Z",
	ended:     "2026-11-25T14:35:00Z",
	landed:    "2026-11-25T14:31:00Z",
	rowWrite:  "2026-11-25T14:50:00Z",
}

// plantZone is the zone the cancel pins give the dispatcher, so a sentence's
// time is shown to be the plant's: America/Chicago.
func plantZone(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	testutil.MustNoErr(t, err, "load America/Chicago")
	return loc
}

func buildCancelShape(t *testing.T, r twiceRig, uuid, shape string) *orders.Order {
	t.Helper()
	storage, line, bp := testdb.SetupTestData(t, r.db)
	station := testEnvelope().Src.Station
	if shape == "skipped" {
		o := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
			o.EdgeUUID, o.StationUID = uuid, station
			o.SourceNode, o.DeliveryNode, o.PayloadCode = storage.Name, line.Name, bp.Code
		})
		testutil.MustNoErr(t, r.d.lifecycle.Skip(o, station, "no_source_bin", "no bins at any pickup node"),
			"skip the order")
		return o
	}
	o := testdb.CreateOrder(t, r.db, testdb.FleetHas("job-"+uuid), func(o *orders.Order) {
		o.EdgeUUID, o.StationUID = uuid, station
		o.SourceNode, o.DeliveryNode, o.PayloadCode = storage.Name, line.Name, bp.Code
	})
	bin := testdb.CreateBinAtNode(t, r.db, bp.Code, storage.ID, uuid+"-BIN")
	testdb.SeedCartRow(t, r.db, o.ID, bin.ID, 0, 1)
	if shape == "unreadable" {
		_, err := r.db.Exec(`UPDATE orders SET steps_json = '[' WHERE id = $1`, o.ID)
		testutil.MustNoErr(t, err, "cut the stored plan short")
		if _, err := r.db.OrderTrips([]*orders.Order{reloadOrder(t, r.db, o.ID)}); err == nil {
			t.Fatal("fixture: where the unreadable order's cart is was read")
		}
		return o
	}
	testdb.ClaimBinForTest(t, r.db, bin.ID, o.ID)
	step := func(what string, write func(o *orders.Order) error) {
		t.Helper()
		testutil.MustNoErr(t, write(reloadOrder(t, r.db, o.ID)), what)
	}
	switch shape {
	case "not lifted":
		return o
	case "failed":
		step("the fleet fails the order", func(o *orders.Order) error {
			return r.d.lifecycle.Fail(o, station, "fleet_failed", "the fleet failed the order")
		})
		return o
	case "cancelled":
		r.d.lifecycle.CancelOrder(reloadOrder(t, r.db, o.ID), station, "fleet order stopped", CancelCause{})
		return o
	}
	step("the fleet reports the order running", func(o *orders.Order) error {
		return r.d.lifecycle.MarkInTransit(o, "", "fleet", protocol.OrderUpdate{})
	})
	transit, err := r.db.GetNodeByDotName("_TRANSIT")
	testutil.MustNoErr(t, err, "resolve _TRANSIT")
	testutil.MustNoErr(t, r.db.MoveBinToTransit(bin.ID, transit.ID), "the robot lifts the cart")
	if shape == "carried" {
		return o
	}
	_, err = r.db.ApplyMultiBinArrival(o.ID, []orders.BinArrivalInstruction{{BinID: bin.ID, ToNodeID: line.ID}}, nil)
	testutil.MustNoErr(t, err, "the robot sets the cart down at the line")
	if shape == "landed" {
		return o
	}
	step("the fleet reports the order delivered", func(o *orders.Order) error {
		return r.d.lifecycle.MarkDelivered(o, "fleet", protocol.OrderUpdate{Detail: "fleet state: FINISHED"})
	})
	switch shape {
	case "delivered":
	case "confirmed":
		step("the station confirms it", func(o *orders.Order) error {
			_, err := r.d.lifecycle.ConfirmReceipt(o, station, "confirmed", 1)
			return err
		})
	default:
		t.Fatalf("plainCancelOrder: no shape %q", shape)
	}
	return o
}

// TestRunTwice_Cancel: a station cancels a plain order (one cart, no legs),
// and the envelope is handled twice. Today's door is a cancel that does not ask
// (an older Edge's, or Kafka Direct's: final brief 1f): its first run stops the
// fleet's job, ends the order cancelled (the write's own tell) and answers the
// station order.cancelled, an order whose cart is set down included, its
// finished job stopped. A cancel that asks (OrderCancel.Intent, final brief 5b)
// is answered by the tell alone when Core cancels, and by one order.error when
// it does not (cancelRefusal). The cut is the order's cancelled event: the
// ending and its tell are written, the answer is not.
func TestRunTwice_Cancel(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, uuid, shape string, intent bool) (twiceRig, *protocol.Envelope, *protocol.OrderCancel) {
		t.Helper()
		r := newTwiceRig(t)
		r.d.SetPlantZone(plantZone(t))
		plainCancelOrder(t, r, uuid, shape)
		env := testEnvelope()
		env.ID, env.Type = "env-"+uuid, protocol.TypeOrderCancel
		return r, env, &protocol.OrderCancel{OrderUUID: uuid, Reason: "cancelled by operator", Intent: intent}
	}
	lines := func(l ...string) func(twiceRig, string) []string {
		return func(twiceRig, string) []string { return l }
	}
	for _, c := range []struct {
		name, shape, uuid string
		intent            bool
		from              protocol.Status                        // what the order read when the cancel ended it; "" when refused
		told              func(r twiceRig, uuid string) []string // what a first run tells, byte for byte
	}{
		{"not lifted", "not lifted", "rt-cn-on-its-way", false, StatusDispatched, lines(
			`order.update to line-1: {"detail":"cancelled by operator","order_uuid":"rt-cn-on-its-way","status":"cancelled","version":2}`,
			`order.cancelled to line-1: {"order_uuid":"rt-cn-on-its-way","reason":"cancelled by operator"}`,
		)},
		{"delivered", "delivered", "rt-cn-set-down", false, StatusDelivered, lines(
			`order.update to line-1: {"detail":"cancelled by operator","order_uuid":"rt-cn-set-down","status":"cancelled","version":4}`,
			`order.cancelled to line-1: {"order_uuid":"rt-cn-set-down","reason":"cancelled by operator"}`,
		)},
		// Final brief 5b: the tell is the answer, and the dispatcher's own
		// order.cancelled is not sent.
		{"not lifted, asking", "not lifted", "rt-cn-asks-on-its-way", true, StatusDispatched, lines(
			`order.update to line-1: {"detail":"cancelled by operator","order_uuid":"rt-cn-asks-on-its-way","status":"cancelled","version":2}`,
		)},
		// Final brief 5b: set down is "cannot", one order.error, nothing written.
		{"delivered, asking", "delivered", "rt-cn-asks-set-down", true, "", lines(
			`order.error to line-1: {"detail":"Not cancelled: its cart was set down at LINE1-IN on Nov 25 at 08:32 CST. Confirm it at the station.","error_code":"cancel_refused_set_down","order_uuid":"rt-cn-asks-set-down"}`,
		)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			stops := 1
			if c.from == "" {
				stops = 0
			}
			t.Run("the station is told on a first run", func(t *testing.T) {
				t.Parallel()
				r, env, req := setup(t, c.uuid, c.shape, c.intent)
				before := testdb.RequireOrder(t, r.db, c.uuid)
				mark := outboxMark(t, r.db)
				r.d.HandleOrderCancel(env, req)
				assertTold(t, "what a first run tells", append(r.e.projections, toldSince(t, r.db, mark)...), c.told(r, c.uuid))
				if c.from == "" {
					testdb.AssertOrderStatus(t, r.db, req.OrderUUID, before.Status)
				} else {
					testdb.AssertOrderStatus(t, r.db, req.OrderUUID, StatusCancelled)
				}
				if got := r.backend.CancelRequests(); len(got) != stops || (stops == 1 && got[0] != "job-"+c.uuid) {
					t.Errorf("fleet stops %v, want the order's job %d time(s)", got, stops)
				}
				if got := strings.Join(r.e.cancelledFrom, ","); got != string(c.from) {
					t.Errorf("cancelled events from %q, want %q", got, c.from)
				}
			})

			// A cancel cut after its ending: the redelivery meets a cancelled
			// order with no legs and does nothing. Today's door therefore never
			// sends the answer the cut run did not send, and the station hears
			// the tell alone. Should be harmless: the tell ends the station's
			// row, and on a row Core versions the answer writes nothing
			// (shingo-edge orders.HandleCancelledNotice). A cancel that asks is
			// answered by the tell alone either way. A refusal writes nothing to
			// cut after.
			if c.from != "" {
				t.Run("run, cut, run again", func(t *testing.T) {
					t.Parallel()
					r, env, req := setup(t, c.uuid, c.shape, c.intent)
					r.e.cut = func(event string, _ int64) {
						if event == "cancelled" {
							runtime.Goexit()
						}
					}
					mark := outboxMark(t, r.db)
					cutRun(t, func() { r.d.HandleOrderCancel(env, req) })
					r.e.cut = nil
					testdb.AssertOrderStatus(t, r.db, req.OrderUUID, StatusCancelled)
					r.d.HandleOrderCancel(env, req)

					assertTold(t, "the cut run and the redelivery told the station", toldSince(t, r.db, mark),
						c.told(r, c.uuid)[:1])
					if n := len(r.backend.CancelRequests()); n != 1 {
						t.Errorf("the fleet was told to stop %d jobs, want 1", n)
					}
					if n := len(r.e.cancelledFrom); n != 1 {
						t.Errorf("cancelled announced %d times, want 1", n)
					}
				})
			}

			// The repeat of a whole run: a cancel meets its order cancelled and
			// says nothing; a refusal is decided again, and answers the first
			// run's answer again, as the redirect re-sends its ack: Core keeps no
			// record of a refusal to know it from a first, and the station's
			// copy of the refusal should match no unanswered ask the second time.
			t.Run("run, finish, run again", func(t *testing.T) {
				t.Parallel()
				r, env, req := setup(t, c.uuid, c.shape, c.intent)
				r.d.HandleOrderCancel(env, req)
				mark := outboxMark(t, r.db)
				r.d.HandleOrderCancel(env, req)

				var again []string
				if c.from == "" {
					again = c.told(r, c.uuid)
				}
				assertTold(t, "the repeat told the station", toldSince(t, r.db, mark), again)
				if n := len(r.backend.CancelRequests()); n != stops {
					t.Errorf("the fleet was told to stop %d jobs, want %d", n, stops)
				}
				if n := len(r.e.cancelledFrom); n != stops {
					t.Errorf("cancelled announced %d times, want %d", n, stops)
				}
			})
		})
	}
}

// ── receipt and release: safe run twice today, characterised ────────────────

// TestRunTwice_Receipt: a delivered order confirmed by its station. A repeat
// after a whole first run returns at completed_at. Cut between the confirmed
// write and the completed_at write (the confirmed event), the repeat is refused
// confirmed -> confirmed at debug: nothing new, nothing sent, either way.
func TestRunTwice_Receipt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		cut  bool
	}{{"run, cut, run again", true}, {"run, finish, run again", false}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newTwiceRig(t)
			uuid := "rt-rc-" + map[bool]string{true: "cut", false: "finish"}[c.cut]
			testutil.MustNoErr(t, r.db.CreateOrder(&orders.Order{
				EdgeUUID: uuid, StationUID: testEnvelope().Src.Station, OrderType: OrderTypeRetrieve,
				Status: StatusDelivered, Quantity: 1, DeliveryNode: "LINE-1",
			}), "create the order")
			env := testEnvelope()
			env.ID, env.Type = "env-"+uuid, protocol.TypeOrderReceipt
			req := &protocol.OrderReceipt{OrderUUID: uuid, ReceiptType: "confirmed", FinalCount: 1}
			if c.cut {
				r.e.cut = func(event string, _ int64) {
					if event == "completed" {
						runtime.Goexit()
					}
				}
			}
			cutRun(t, func() { r.d.HandleOrderReceipt(env, req) })
			r.e.cut = nil
			testdb.AssertOrderStatus(t, r.db, uuid, StatusConfirmed)
			mark := outboxMark(t, r.db)
			r.d.HandleOrderReceipt(env, req)

			assertTold(t, "the repeat told the station", toldSince(t, r.db, mark), nil)
			if len(r.e.completed) != 1 {
				t.Errorf("order completed announced %d times, want 1", len(r.e.completed))
			}
			testdb.AssertOrderStatus(t, r.db, uuid, StatusConfirmed)
		})
	}
}

// TestRunTwice_Release: a staged changeover leg released at its first wait. The
// same envelope again appends nothing (the N-a repeat guard,
// TestReleaseLayer_Na_SecondReleaseWhileDrivingToToolingDone), and tells nothing.
func TestRunTwice_Release(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	tellToStations(r.db)
	rlLeg(t, r.db, "rt-rl-finish", StatusStaged, 0, string(mustJSON(t, rlChangeoverLeg())))
	env := r.d.syntheticEnvelope("line-1")
	env.ID, env.Type = "env-rt-rl-finish", protocol.TypeOrderRelease
	req := &protocol.OrderRelease{OrderUUID: "rt-rl-finish"}
	r.d.HandleOrderRelease(env, req)
	appends := len(r.backend.ReleaseCalls())
	first := prReloadUUID(t, r.db, req.OrderUUID)
	mark := outboxMark(t, r.db)
	r.d.HandleOrderRelease(env, req)

	if n := len(r.backend.ReleaseCalls()); n != appends || appends != 1 {
		t.Errorf("appends: %d after the first run, %d after the repeat, want 1 and 1", appends, n)
	}
	assertTold(t, "the repeat told the station", toldSince(t, r.db, mark), nil)
	again := prReloadUUID(t, r.db, req.OrderUUID)
	if again.Status != first.Status || again.WaitIndex != first.WaitIndex {
		t.Errorf("the repeat moved the leg: %s at wait %d -> %s at wait %d",
			first.Status, first.WaitIndex, again.Status, again.WaitIndex)
	}
}
