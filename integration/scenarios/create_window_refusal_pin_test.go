// The station's view of a fleet that refuses an order's create, pinned end to
// end as it behaves: a real Core engine and a real Edge engine over the Bus, the
// Core's fleet the simulator wrapped so the test stands inside every create and
// chooses its answer.
//
// Inside each refused create, Core's hand has already written `dispatched` and
// told the station, before the fleet was asked. Pumped through, the Edge's row
// reads `dispatched`, so a changeover START pressed there is refused as if a
// robot carried the order (ChangeoverStartActionFor: block), and every surface
// that reads the row shows a robot coming. The park after the refusal tells
// `queued` again. Each refused pass writes two rows of Edge history.
//
// A FLEET pin: it flips when `dispatched` is written at the fleet's yes. The
// Edge then holds `queued` with its cause through every refusal, START cancels
// the order, and a refused pass after the first writes no Edge history
// (the dispatch-claim pin list, P11).
//
//go:build docker

package scenarios

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/router"

	coreconfig "shingocore/config"
	coreengine "shingocore/engine"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	coremessaging "shingocore/messaging"
	coreharness "shingocore/testharness"

	edgeorders "shingoedge/orders"
	edgeharness "shingoedge/testharness"
)

const cwrStation = "edge.cwr"

// cwrFleet is the simulator with the test standing inside every create: inside
// runs before the simulator answers, and a non-nil return refuses the create.
type cwrFleet struct {
	*simulator.SimulatorBackend
	mu     sync.Mutex
	inside func(n int) error
	asked  int
}

func (f *cwrFleet) CreateOrder(req fleet.CreateOrderRequest) (fleet.TransportOrderResult, error) {
	f.mu.Lock()
	f.asked++
	n, inside := f.asked, f.inside
	f.mu.Unlock()
	if inside != nil {
		if err := inside(n); err != nil {
			return fleet.TransportOrderResult{}, err
		}
	}
	return f.SimulatorBackend.CreateOrder(req)
}

func (f *cwrFleet) set(inside func(n int) error) {
	f.mu.Lock()
	f.inside = inside
	f.mu.Unlock()
}

func TestRefusalLoop_TheStationBlocksStartInsideEachCreate(t *testing.T) {
	t.Parallel()
	refused := errors.New("RDS error 50001: robot system refused the order")
	f := &cwrFleet{SimulatorBackend: simulator.New()}
	f.set(func(int) error { return refused })

	db := coreharness.OpenDB(t)
	cfg := coreconfig.Defaults()
	cfg.Messaging.StationID = "core"
	eng := coreengine.New(coreengine.Config{AppConfig: cfg, DB: db, Fleet: f, LogFunc: t.Logf})
	eng.Start()
	t.Cleanup(eng.Stop)
	std := coreharness.SetupStandardData(t, db)
	mustNil(t, db.SetPayloadBinTypes(std.Payload.ID, []int64{std.BinType.ID}), "carrier rule")
	coreharness.CreateBinAtNode(t, db, std.Payload.Code, std.StorageNode.ID, "CWR-BIN")

	edge := edgeharness.NewEdge(t, cwrStation)
	coreHandler := coremessaging.NewCoreHandler(db, nil, "core", "shingo.dispatch", eng.Dispatcher())
	coreIngestor := protocol.NewIngestor(nil)
	coreRouter := router.New[string]()
	router.Register(coreRouter, protocol.TypeOrderRequest, coreHandler.HandleOrderRequest)
	router.Register(coreRouter, protocol.TypeOrderCancel, coreHandler.HandleOrderCancel)
	router.Register(coreRouter, protocol.TypeOrderReceipt, coreHandler.HandleOrderReceipt)
	coreIngestor.Dispatch = func(env *protocol.Envelope) { coreRouter.Dispatch(env, env.Type) }
	bus := harness.NewBus(t,
		harness.EdgeSide{EdgeStore: edge.DB, EdgeIngestor: edge.Ingestor},
		harness.CoreSide{CoreStore: db, CoreIngestor: coreIngestor},
	)
	settle := func() {
		for i := 0; i < 20; i++ {
			if bus.PumpAll() == 0 {
				time.Sleep(50 * time.Millisecond)
				if bus.PumpAll() == 0 {
					return
				}
			}
		}
	}
	settle()

	// The station calls a bin; the intake's own acquire is the first refusal.
	o, err := edge.Engine.OrderManager().CreateRetrieveOrder(edgeorders.RetrieveSpec{
		Delivery: std.LineNode.Name, Source: std.StorageNode.Name, Payload: std.Payload.Code,
		Quantity: 1, Origin: edgeorders.NoDemand(),
	})
	mustNil(t, err, "the call")
	settle()
	edgeRow := func() (protocol.Status, int) {
		t.Helper()
		row, err := edge.DB.GetOrder(o.ID)
		mustNil(t, err, "edge order")
		hist, err := edge.DB.ListOrderHistory(o.ID)
		mustNil(t, err, "edge history")
		return row.Status, len(hist)
	}
	status, rows := edgeRow()
	if status != protocol.StatusQueued {
		t.Fatalf("after the first refusal the Edge reads %s, want queued", status)
	}

	// Three more refused passes. Inside each create the test pumps Core's tells
	// through to the Edge and reads what the station would act on.
	var inside []string
	var histPerPass []int
	f.set(func(n int) error {
		settle()
		s, _ := edgeRow()
		inside = append(inside, fmt.Sprintf("%s/%s", s, protocol.ChangeoverStartActionFor(s)))
		return refused
	})
	for pass := 0; pass < 3; pass++ {
		eng.RunFulfillmentScan()
		settle()
		s, r := edgeRow()
		if s != protocol.StatusQueued {
			t.Errorf("after refused pass %d the Edge reads %s, want queued", pass+1, s)
		}
		histPerPass = append(histPerPass, r-rows)
		rows = r
	}
	if want := "dispatched/block, dispatched/block, dispatched/block"; strings.Join(inside, ", ") != want {
		t.Errorf("inside each refused create the Edge reads %v, want %s as built", inside, want)
	}
	if fmt.Sprint(histPerPass) != "[2 2 2]" {
		t.Errorf("Edge history rows per refused pass = %v, want [2 2 2] as built (dispatched, then queued)", histPerPass)
	}

	// The fleet comes back: one more pass, and the station hears the dispatch.
	f.set(nil)
	eng.RunFulfillmentScan()
	settle()
	if s, _ := edgeRow(); s != protocol.StatusDispatched {
		t.Errorf("after the fleet took the order the Edge reads %s, want dispatched", s)
	}
}
