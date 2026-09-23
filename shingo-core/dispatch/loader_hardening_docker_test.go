//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// dedicatedHome wires a dedicated_positions loader with the given (payload-pinned)
// home positions and no buffer, returning the loader id and the created position
// nodes in order. A nil/"" payload makes an UNPINNED home (kind=home, blank).
func dedicatedHome(t *testing.T, db *store.DB, role string, names []string, payloads_ []string) (int64, []*nodes.Node) {
	t.Helper()
	loaderID, err := db.CreateLoader(store.Loader{
		Name: "LD-" + role + "-" + names[0], Role: role,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: "operator",
	})
	if err != nil {
		t.Fatalf("create loader: %v", err)
	}
	var out []*nodes.Node
	for i, nm := range names {
		n := &nodes.Node{Name: nm, Enabled: true}
		if err := db.CreateNode(n); err != nil {
			t.Fatalf("create node %s: %v", nm, err)
		}
		if err := db.UpsertLoaderHome(store.LoaderHome{
			LoaderID: loaderID, PositionNodeID: n.ID, PayloadCode: payloads_[i], Kind: loaders.HomeKindHome,
		}); err != nil {
			t.Fatalf("upsert home %s: %v", nm, err)
		}
		out = append(out, n)
	}
	return loaderID, out
}

// TestPlanRetrieve_DedicatedLoaderPool_HomeFullNoBuffer_SourcesHome is the M6
// characterization / zero-regression proof: a dedicated home with a fresh full of
// X and NO buffer sources that home bin (parity with the legacy path, which also
// resolved the lone X). The only intended behavior change vs legacy is the
// concrete-node→queue fix, covered separately by ...NoPartInPoolQueues.
func TestPlanRetrieve_DedicatedLoaderPool_HomeFullNoBuffer_SourcesHome(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	if err := db.CreatePayload(&payloads.Payload{Code: "PART-X", Description: "X", UOPCapacity: 10}); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	_, pos := dedicatedHome(t, db, "consume", []string{"HX-P1"}, []string{"PART-X"})

	homeFull := makeLoaderBin(t, db, "PART-X", pos[0].ID, "home-full", 10, time.Now().UTC())

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "home-full-1", OrderType: OrderTypeRetrieve, PayloadCode: "PART-X",
		DeliveryNode: lineNode.Name, SourceNode: pos[0].Name, Quantity: 1.0,
	})

	order := dispatchSimpleViaScanner(t, d, db, "home-full-1")
	if order.BinID == nil || *order.BinID != homeFull.ID {
		t.Fatalf("sourced bin %v, want the home full %d", order.BinID, homeFull.ID)
	}
}

// TestPlanRetrieve_DedicatedLoaderPool_OlderFullBeatsNewerPartial pins the Drain
// "no partial-first tier" contract at the PLANNER level (not just the pure ranker):
// an OLDER full of X at the home outranks a NEWER partial of X in the buffer, so
// plain FIFO consumes the genuinely-older bin first. If a tier were ever
// reintroduced in the wiring, this would flip and fail.
func TestPlanRetrieve_DedicatedLoaderPool_OlderFullBeatsNewerPartial(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	pos, buffer := dedicatedLoaderFixture(t, db, "consume")

	now := time.Now().UTC()
	olderFull := makeLoaderBin(t, db, "PART-X", pos.ID, "older-full", 10, now.Add(-3*time.Hour))
	newerPartial := makeLoaderBin(t, db, "PART-X", buffer.ID, "newer-partial", 4, now)

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "older-full-1", OrderType: OrderTypeRetrieve, PayloadCode: "PART-X",
		DeliveryNode: lineNode.Name, SourceNode: pos.Name, Quantity: 1.0,
	})

	order := dispatchSimpleViaScanner(t, d, db, "older-full-1")
	if order.BinID == nil || *order.BinID != olderFull.ID {
		t.Fatalf("sourced bin %v, want the OLDER full %d (Drain is plain FIFO; newer partial %d must wait)",
			order.BinID, olderFull.ID, newerPartial.ID)
	}
}

// TestPlanRetrieve_DedicatedLoaderPool_MultiHomeSamePayload sources the oldest X
// across TWO homes pinned to the same payload (the locked O2 config). The pool is
// the loader's whole member set, so a cell bound to one home consumes the oldest
// bin even when it sits at the sibling home.
func TestPlanRetrieve_DedicatedLoaderPool_MultiHomeSamePayload(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	if err := db.CreatePayload(&payloads.Payload{Code: "PART-X", Description: "X", UOPCapacity: 10}); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	_, pos := dedicatedHome(t, db, "consume", []string{"MX-P1", "MX-P2"}, []string{"PART-X", "PART-X"})

	now := time.Now().UTC()
	_ = makeLoaderBin(t, db, "PART-X", pos[0].ID, "p1-newer", 10, now)
	olderAtP2 := makeLoaderBin(t, db, "PART-X", pos[1].ID, "p2-older", 10, now.Add(-2*time.Hour))

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "multi-home-1", OrderType: OrderTypeRetrieve, PayloadCode: "PART-X",
		DeliveryNode: lineNode.Name, SourceNode: pos[0].Name, Quantity: 1.0,
	})

	order := dispatchSimpleViaScanner(t, d, db, "multi-home-1")
	if order.BinID == nil || *order.BinID != olderAtP2.ID {
		t.Fatalf("sourced bin %v, want the older bin %d at the sibling home", order.BinID, olderAtP2.ID)
	}
}

// TestPlanRetrieve_SharedWindowLoader_LayoutGatedFromPool is the M3 market-loader
// guard: a shared_window loader's window node ALSO lives in bin_loader_homes, but
// the layout gate keeps it out of the dedicated flat-pool ranker. A retrieve whose
// SourceNode is that window is sourced as the concrete node it is: the matching
// full standing ON the window, never the pool. So with an empty window and a
// global X present, the order waits on the window as finder-node-empty — a
// pool-sourced window would have queued finder-pool-empty instead. (It used to
// fall through to the global finder and take the global bin; a named concrete
// source no longer widens — TestNamedSourcePin_ConcreteSourceStaysOnTheNode.)
func TestPlanRetrieve_SharedWindowLoader_LayoutGatedFromPool(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	if err := db.CreatePayload(&payloads.Payload{Code: "PART-X", Description: "X", UOPCapacity: 10}); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	// shared_window loader with a window node registered in bin_loader_homes.
	loaderID, err := db.CreateLoader(store.Loader{
		Name: "SW-1", Role: "produce", Layout: loaders.LayoutSharedWindow, Replenishment: "operator",
	})
	if err != nil {
		t.Fatalf("create shared loader: %v", err)
	}
	window := &nodes.Node{Name: "SW-W1", Enabled: true}
	if err := db.CreateNode(window); err != nil {
		t.Fatalf("create window: %v", err)
	}
	if err := db.UpsertLoaderHome(store.LoaderHome{LoaderID: loaderID, PositionNodeID: window.ID, Kind: loaders.HomeKindHome}); err != nil {
		t.Fatalf("upsert window: %v", err)
	}
	// The only PART-X bin is at a global storage node (NOT at the window).
	globalFull := makeLoaderBin(t, db, "PART-X", storageNode.ID, "global-full", 10, time.Now().UTC())

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "shared-window-1", OrderType: OrderTypeRetrieve, PayloadCode: "PART-X",
		DeliveryNode: lineNode.Name, SourceNode: window.Name, Quantity: 1.0,
	})

	order := dispatchSimpleViaScanner(t, d, db, "shared-window-1")
	if order.BinID != nil {
		t.Fatalf("sourced bin %d (the global full is %d) — a retrieve naming the window stays on the window",
			*order.BinID, globalFull.ID)
	}
	if order.QueueCause != string(CauseFinderNodeEmpty) {
		t.Fatalf("queue_cause = %q, want %q — %q would mean the window entered the dedicated pool ranker",
			order.QueueCause, CauseFinderNodeEmpty, CauseFinderPoolEmpty)
	}

	// And the full that IS on the window is taken, as the concrete node's resident.
	atWindow := makeLoaderBin(t, db, "PART-X", window.ID, "window-full", 10, time.Now().UTC())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "shared-window-2", OrderType: OrderTypeRetrieve, PayloadCode: "PART-X",
		DeliveryNode: lineNode.Name, SourceNode: window.Name, Quantity: 1.0,
	})
	order = dispatchSimpleViaScanner(t, d, db, "shared-window-2")
	if order.BinID == nil || *order.BinID != atWindow.ID {
		t.Fatalf("sourced bin %v, want the full %d standing on the window", order.BinID, atWindow.ID)
	}
}
