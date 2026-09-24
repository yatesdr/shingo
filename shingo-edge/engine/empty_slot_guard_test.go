package engine

import (
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/orders"
	ordertestutil "shingoedge/orders/testutil"
	"shingoedge/service"
	"shingoedge/store"
)

// empty_slot_guard_test.go — what the empty-slot guard costs. The slot
// remembers the bin it last held and that bin's stamp, written by the same
// statement that empties it, and the empty-slot bind is refused in the same
// statement that would have bound. So neither path gains a statement.

// newCountingCoverageEngine is newCoverageEngine on a store that counts every
// statement it sends.
func newCountingCoverageEngine(t *testing.T) (*Engine, *store.QueryCounter) {
	t.Helper()
	db, counter, err := store.OpenCounting(filepath.Join(t.TempDir(), "engine-count.db"))
	if err != nil {
		t.Fatalf("open counting store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.Config{Namespace: "test-ns", LineID: "line-1"}
	eng := &Engine{
		cfg:      cfg,
		db:       db,
		Events:   NewEventBus(),
		stopChan: make(chan struct{}),
		logFn:    func(string, ...any) {},
	}
	eng.coreClient = NewCoreClient("")
	eng.reconciliation = newReconciliationService(eng.db)
	eng.coreSync = newCoreSyncService(eng)
	eng.orderMgr = orders.NewManager(db, ordertestutil.NoOpOrderEmitter{}, cfg.StationID())
	eng.stationService = service.NewStationService(db)
	eng.changeoverService = service.NewChangeoverService(db)
	return eng, counter
}

// TestEmptySlotGuard_CostsNoStatement pins the statements on both halves of
// the guard at their base counts: emptying a slot through a Released
// adjustment, and a correction arriving at the emptied slot.
//
// Base: Released = 4 statements (node lookup, runtime read, clear the bin
// pointer, blank the count); late correction = 3 (node lookup, runtime read,
// the bind write). The guard adds a column write to the clearing statement and
// a predicate to the bind statement, and neither count moves.
func TestEmptySlotGuard_CostsNoStatement(t *testing.T) {
	t.Parallel()
	eng, counter := newCountingCoverageEngine(t)
	const binX = int64(7201)
	boundNodeFixture(t, eng, "EPOCH-COST", binX, 4)
	eng.HandleBinEpochRefresh(protocol.BinEpochRefresh{BinID: binX, CoreNodeName: "EPOCH-COST", Epoch: 5})

	counter.Reset()
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID: binX, CoreNodeName: "EPOCH-COST", Released: true, Epoch: 5, Actor: "admin-under-test",
	})
	if got := counter.Count(); got != 4 {
		t.Errorf("Released adjustment issued %d statements, want 4 — remembering the departed "+
			"bin must ride the statement that clears the pointer", got)
	}

	counter.Reset()
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID: binX, CoreNodeName: "EPOCH-COST", NewRemaining: 11, Epoch: 4, Actor: "admin-under-test",
	})
	if got := counter.Count(); got != 3 {
		t.Errorf("late correction at the empty slot issued %d statements, want 3 — the refusal "+
			"must ride the bind statement, not a read before it", got)
	}
}
