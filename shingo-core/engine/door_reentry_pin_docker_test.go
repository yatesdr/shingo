//go:build docker

package engine

import (
	"net"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store/demands"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// loaderUnderItsLevel is a started engine with the real scanner wired and one
// shared-window loader whose part reads 0 against a reorder level of 50: no
// refill outstanding, the debounce open. It returns the part and the window.
func loaderUnderItsLevel(t *testing.T, prefix string) (*Engine, string, *nodes.Node) {
	t.Helper()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	station, part := "edge."+prefix, "P-"+prefix
	testutil.MustNoErr(t, db.CreatePayload(&payloads.Payload{Code: part, UOPCapacity: 10}), "part")
	empties, window := node(t, eng, prefix+"-EMPTIES"), node(t, eng, prefix+"-W1")
	testdb.CreateBinAtNode(t, db, "", empties.ID, prefix+"-EMPTY")

	svc := service.NewLoaderService(db, nil)
	loaderID, err := svc.CreateLoader(service.LoaderCreate{
		Name: prefix + "-LOADER", Role: loaders.RoleProduce,
		Replenishment: loaders.ReplenishmentThreshold, InboundSource: empties.Name,
	})
	testutil.MustNoErr(t, err, "loader")
	testutil.MustNoErr(t, svc.SetHome(loaderID, window.ID, "", "", 0), "window")
	testutil.MustNoErr(t, svc.SetPayload(loaderID, part, 50), "part at the loader")
	_, err = db.SyncDemandRegistry(station, []demands.RegistryEntry{{
		StationID: station, CoreNodeName: window.Name, LoaderID: loaderID,
		Role: protocol.ClaimRoleProduce, PayloadCode: part, ReplenishUOPThreshold: 50,
	}})
	testutil.MustNoErr(t, err, "registry")
	return eng, part, window
}

func node(t *testing.T, eng *Engine, name string) *nodes.Node {
	t.Helper()
	n := &nodes.Node{Name: name, Enabled: true}
	testutil.MustNoErr(t, eng.db.CreateNode(n), name)
	return n
}

// returnsWithin fails the test when fn has not returned in 10 s: Core froze.
func returnsWithin(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return in 10 s: Core froze on its own scan lock", what)
	}
}

// refillAcquired waits for the window's refill to be born and asked for its
// hand (dispatched, or queued with its cause), never left pending.
func refillAcquired(t *testing.T, eng *Engine, window *nodes.Node) {
	t.Helper()
	var id int64
	var status string
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := eng.db.QueryRow(`SELECT id, status FROM orders WHERE order_type = 'retrieve_empty' AND delivery_node = $1`,
			window.Name).Scan(&id, &status)
		if err == nil && status != string(protocol.StatusPending) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no refill acquired for %s (order %d, status %q, err %v)", window.Name, id, status, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDoorPin_ASwapRequestUnderTheLoaderLevelDoesNotFreezeCore is the freeze of
// carry-edge2 (RULINGS-carry-and-core-freeze §2), with the real scanner wired.
// A complex order arrives for a part under its loader's reorder level, with no
// refill outstanding and the debounce open. Its door emits OrderReceived; the
// threshold monitor's recheck fires the loader's refill, whose acquire needs the
// scan lock the door is holding.
//
// The intake must return, and the refill must still be born and asked for its
// hand once the door has let go. RED at 545b8b57: the intake never returns.
func TestDoorPin_ASwapRequestUnderTheLoaderLevelDoesNotFreezeCore(t *testing.T) {
	t.Parallel()
	eng, part, window := loaderUnderItsLevel(t, "DOOR-PIN")
	from, to := node(t, eng, "DOOR-PIN-FROM"), node(t, eng, "DOOR-PIN-TO")
	returnsWithin(t, "the complex intake", func() {
		eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
			OrderUUID: "door-pin-swap", PayloadCode: part, Quantity: 1,
			Steps: []protocol.ComplexOrderStep{
				{Action: "pickup", Node: from.Name},
				{Action: "dropoff", Node: to.Name},
			},
		})
	})
	refillAcquired(t, eng, window)
}

// TestDoorPin_ABinUpdateInsideTheScanLockDoesNotFreezeCore is the census's F3
// (LOCKS.md): a terminal event under the scan lock infers a stranded bin's
// place and evicts a ghost, and the eviction emits BinUpdated. Its threshold
// subscriber fires the refill, whose acquire needs the scan lock. The emit is
// forced inside the lock here; the path that reaches it at a plant needs a
// stranded bin on _TRANSIT and a ghost to evict. RED at 545b8b57.
func TestDoorPin_ABinUpdateInsideTheScanLockDoesNotFreezeCore(t *testing.T) {
	t.Parallel()
	eng, part, window := loaderUnderItsLevel(t, "BINUPD-PIN")
	returnsWithin(t, "a BinUpdated emitted under the scan lock", func() {
		eng.fulfillment.Door(func() {
			eng.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{PayloadCode: part, Action: BinActionMoved}})
		})
	})
	refillAcquired(t, eng, window)
}

// TestDoorPin_AResumeInsideTheScanLockDoesNotFreezeCore is the census's F4: a
// lane gate evaluated under the scan lock re-drives a held compound, a race
// empties its legs, and its chapter end resumes the parent. OrderResumed's
// subscriber asks for the parent's acquire, which needs the scan lock. The emit
// is forced inside the lock here (the order id is one no row has: the lock is
// what is under test). RED at 545b8b57.
func TestDoorPin_AResumeInsideTheScanLockDoesNotFreezeCore(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	returnsWithin(t, "an OrderResumed emitted under the scan lock", func() {
		eng.fulfillment.Door(func() {
			eng.Events.Emit(Event{Type: EventOrderResumed, Payload: OrderResumedEvent{OrderID: 1 << 40}})
		})
	})
}

// TestDoorPin_AFailureEmailDoesNotHoldTheScanLock is the census's F9: a fail
// under the scan lock (the scanner's failFn, a door's failOrder) emits
// OrderFailed, and its alert subscriber spoke SMTP on the emitter's goroutine
// over a dial with no deadline. A mail server that accepts and never answers held
// the scan lock for as long as it stayed silent: no deadlock, and the same
// frozen Core. RED at 545b8b57.
func TestDoorPin_AFailureEmailDoesNotHoldTheScanLock(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	testutil.MustNoErr(t, err, "silent mail server")
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() }) // accepts, and says nothing
		}
	}()

	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	eng.notifier.Reconfigure(&config.NotificationsConfig{
		Enabled: true, SMTPHost: "127.0.0.1", SMTPPort: ln.Addr().(*net.TCPAddr).Port,
		FromAddress: "core@example.invalid", Recipients: []string{"ops@example.invalid"},
	})
	returnsWithin(t, "an OrderFailed emitted under the scan lock", func() {
		eng.fulfillment.Door(func() {
			eng.Events.Emit(Event{Type: EventOrderFailed, Payload: OrderFailedEvent{OrderID: 1 << 40, ErrorCode: "pin"}})
		})
	})
}
