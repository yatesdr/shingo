//go:build docker

package engine

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/router"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fulfillment"
	"shingocore/internal/testdb"
	"shingocore/messaging"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/orders"
)

// scan_reader_docker_test.go: what the wait of the one inbound reader on the
// scan lock does to what is read behind it.

// slowPassStore is the real store with a pass that takes passTime, the first
// one held until release closes.
type slowPassStore struct {
	*store.DB
	passTime time.Duration
	passes   atomic.Int64
	held     chan struct{}
	release  chan struct{}
}

func (s *slowPassStore) ListAcquiringOrders() ([]*orders.Order, error) {
	if s.passes.Add(1) == 1 {
		close(s.held)
		<-s.release
	}
	time.Sleep(s.passTime)
	return s.DB.ListAcquiringOrders()
}

// TestHeartbeatOffset_CarriesAtMostTwoPassesOfTheScanLock: /edges reads the
// clock of an Edge off its heartbeat as ts minus the clock of Core when Core
// handles it, so whatever the reader waited is in it. Behind a burst of triggers
// during a pass that wait is two passes at most. The path is the one the reader
// drives: envelopes as bytes, one goroutine, the ingestor, the routed handlers
// (the door of an order request, then the heartbeat) and the registry row.
//
// RED at 6bd55616: the door waited out every triggered pass, and the stored
// offset said the Edge was five seconds behind.
func TestHeartbeatOffset_CarriesAtMostTwoPassesOfTheScanLock(t *testing.T) {
	t.Parallel()
	const (
		station   = "edge.offset"
		triggers  = 100
		passTime  = 50 * time.Millisecond
		allowance = time.Second // the work of the two handlers, and the hold
	)
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	registerActiveEdge(t, db, station)

	eng := newUnstartedEngine(t, db, testdb.NewTrackingBackend())
	resolver := &dispatch.DefaultResolver{DB: db}
	eng.dispatcher = eng.newDispatcher(&dispatchEmitter{bus: eng.Events, engine: eng}, resolver)
	slow := &slowPassStore{DB: db, passTime: passTime, held: make(chan struct{}), release: make(chan struct{})}
	eng.fulfillment = fulfillment.NewScanner(slow, eng.dispatcher, eng.dispatcher.Lifecycle(),
		dispatch.NewSourceFinder(db, resolver, nil), eng.binManifest, eng.sendToEdge, eng.failOrderAndEmit, eng.logFn, nil)
	eng.dispatcher.SetAcquirer(eng.fulfillment)
	eng.wireEventHandlers()
	eng.fulfillment.StartPeriodicSweep(time.Hour)
	t.Cleanup(eng.Stop)
	var once sync.Once
	release := func() { once.Do(func() { close(slow.release) }) }
	t.Cleanup(release)

	h := messaging.NewCoreHandler(db, nil, "test-core", "shingo.dispatch", eng.dispatcher)
	data := messaging.NewCoreDataService(db, h, service.EpochAnnounce{Topic: "shingo.dispatch", CoreStation: "test-core"})
	var before int64
	subjects := router.NewSubject()
	router.RegisterSubject(subjects, protocol.SubjectEdgeHeartbeat, func(env *protocol.Envelope, p *protocol.EdgeHeartbeat) {
		before = slow.passes.Load()
		data.HandleEdgeHeartbeat(env, p)
	})
	r := router.New[string]()
	router.Register(r, protocol.TypeOrderRequest, h.HandleOrderRequest)
	router.Register(r, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { subjects.Dispatch(env, p) })
	in := protocol.NewIngestor(nil)
	in.Dispatch = func(env *protocol.Envelope) { r.Dispatch(env, env.Type) }

	trigger := func() {
		eng.Events.Emit(Event{Type: EventBinEnteredTransit, Payload: BinEnteredTransitEvent{}})
	}
	trigger()
	select {
	case <-slow.held:
	case <-time.After(10 * time.Second):
		t.Fatal("fixture: no first pass in 10 s")
	}
	for range triggers {
		trigger()
	}
	time.Sleep(20 * time.Millisecond) // whatever the triggers queue is at the lock

	edge := protocol.Address{Role: protocol.RoleEdge, Station: station}
	core := protocol.Address{Role: protocol.RoleCore}
	req, err := protocol.NewEnvelope(protocol.TypeOrderRequest, edge, core, &protocol.OrderRequest{
		OrderUUID: "offset-ahead", OrderType: protocol.OrderTypeRetrieve, Quantity: 1,
		PayloadCode: sd.Payload.Code, DeliveryNode: sd.LineNode.Name,
	})
	testutil.MustNoErr(t, err, "the order request ahead")
	hb, err := protocol.NewDataEnvelope(protocol.SubjectEdgeHeartbeat, edge, core, &protocol.EdgeHeartbeat{})
	testutil.MustNoErr(t, err, "the heartbeat")
	var inbound [][]byte
	for _, env := range []*protocol.Envelope{req, hb} {
		raw, err := env.Encode()
		testutil.MustNoErr(t, err, "encode")
		inbound = append(inbound, raw)
	}

	read := make(chan struct{})
	go func() { // the reader: one message after another
		defer close(read)
		for _, raw := range inbound {
			in.HandleRaw(raw)
		}
	}()
	time.Sleep(20 * time.Millisecond) // and it is at the lock
	release()
	select {
	case <-read:
	case <-time.After(60 * time.Second):
		t.Fatal("the reader did not get through two messages in 60 s")
	}

	var ms int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT clock_offset_ms FROM edge_registry WHERE station_uid = $1`, station).Scan(&ms),
		"the offset /edges shows")
	var born int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM orders WHERE edge_uuid = 'offset-ahead'`).Scan(&born), "the order")
	if born != 1 {
		t.Fatal("fixture: the order request took no door, so nothing waited on the scan lock ahead of the heartbeat")
	}
	t.Logf("MEASURED: clock offset %d ms behind %d pass(es) of %v, %d triggered", ms, before, passTime, triggers)
	if before > 2 {
		t.Errorf("the heartbeat was read behind %d passes, want at most 2", before)
	}
	if least := -(2*passTime + allowance); time.Duration(ms)*time.Millisecond < least {
		t.Errorf("the stored clock offset is %d ms for an Edge on the clock of Core, want no lower than %v: "+
			"two passes and the work of the handlers", ms, least)
	}
}
