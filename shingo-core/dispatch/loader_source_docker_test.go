//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// makeLoaderBin creates an available, manifest-confirmed bin of payloadCode at
// nodeID with the given uop_remaining, then backdates loaded_at for deterministic
// FIFO. uop == capacity is a full; 0 < uop < cap a partial.
func makeLoaderBin(t *testing.T, db *store.DB, payloadCode string, nodeID int64, label string, uop int, loadedAt time.Time) *bins.Bin {
	t.Helper()
	bt, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		t.Fatalf("get DEFAULT bin type: %v", err)
	}
	b := &bins.Bin{BinTypeID: bt.ID, Label: label, NodeID: &nodeID, Status: "available"}
	if err := db.CreateBin(b); err != nil {
		t.Fatalf("create bin %s: %v", label, err)
	}
	if err := db.SetBinManifest(b.ID, `{"items":[]}`, payloadCode, uop); err != nil {
		t.Fatalf("set manifest %s: %v", label, err)
	}
	if err := db.ConfirmBinManifest(b.ID, ""); err != nil {
		t.Fatalf("confirm manifest %s: %v", label, err)
	}
	if _, err := db.DB.Exec(`UPDATE bins SET loaded_at=$1 WHERE id=$2`, loadedAt, b.ID); err != nil {
		t.Fatalf("backdate loaded_at %s: %v", label, err)
	}
	got, err := db.GetBin(b.ID)
	if err != nil {
		t.Fatalf("reload bin %s: %v", label, err)
	}
	return got
}

// makeEmptyBin creates an available bin with no manifest (payload "") — a fungible
// empty — at nodeID.
func makeEmptyBin(t *testing.T, db *store.DB, nodeID int64, label string) *bins.Bin {
	t.Helper()
	bt, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		t.Fatalf("get DEFAULT bin type: %v", err)
	}
	b := &bins.Bin{BinTypeID: bt.ID, Label: label, NodeID: &nodeID, Status: "available"}
	if err := db.CreateBin(b); err != nil {
		t.Fatalf("create empty bin %s: %v", label, err)
	}
	got, err := db.GetBin(b.ID)
	if err != nil {
		t.Fatalf("reload empty bin %s: %v", label, err)
	}
	return got
}

// dedicatedLoaderFixture wires a dedicated loader of the given role for PART-X
// with one payload-pinned home position and one explicit BUFFER slot
// (home_kind=buffer, no payload) — no separate buffer node group. Returns the
// position + buffer. The cell binds to the home position; the buffer holds kept
// partials. (An UNPINNED home — kind=home, blank payload — would be inert; the
// buffer is sourced only because it is marked as such.)
func dedicatedLoaderFixture(t *testing.T, db *store.DB, role string) (pos *nodes.Node, buffer *nodes.Node) {
	t.Helper()
	if err := db.CreatePayload(&payloads.Payload{Code: "PART-X", Description: "X", UOPCapacity: 10}); err != nil {
		t.Fatalf("create payload PART-X: %v", err)
	}
	pos = &nodes.Node{Name: "LX-P1", Enabled: true}
	if err := db.CreateNode(pos); err != nil {
		t.Fatalf("create position: %v", err)
	}
	buffer = &nodes.Node{Name: "LX-B1", Enabled: true}
	if err := db.CreateNode(buffer); err != nil {
		t.Fatalf("create buffer: %v", err)
	}
	loaderID, err := db.CreateLoader(store.Loader{
		Name: "LX", Role: role, Layout: "dedicated_positions", Replenishment: "operator",
	})
	if err != nil {
		t.Fatalf("create loader: %v", err)
	}
	// Pinned home position (payload X) + an explicit BUFFER slot (home_kind=buffer,
	// no payload). The buffer is a real pool member; an unpinned home would be inert.
	if err := db.UpsertLoaderHome(store.LoaderHome{LoaderID: loaderID, PositionNodeID: pos.ID, PayloadCode: "PART-X", Kind: loaders.HomeKindHome}); err != nil {
		t.Fatalf("upsert pinned home: %v", err)
	}
	if err := db.UpsertLoaderHome(store.LoaderHome{LoaderID: loaderID, PositionNodeID: buffer.ID, Kind: loaders.HomeKindBuffer}); err != nil {
		t.Fatalf("upsert buffer: %v", err)
	}
	return pos, buffer
}

// TestPlanRetrieve_DedicatedLoaderPool_DrainsBufferedPartialOldestFirst is the
// open problem end to end: a cell bound to home P1 demands PART-X. A fresh FULL
// sits at P1 and an OLDER partial sits in a buffer. The retrieve must source the
// loader's whole pool and claim the OLDER buffer partial — proving the cell sees
// the buffer via the pool and drains oldest-first.
func TestPlanRetrieve_DedicatedLoaderPool_DrainsBufferedPartialOldestFirst(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	pos, buffer := dedicatedLoaderFixture(t, db, "consume")

	now := time.Now().UTC()
	full := makeLoaderBin(t, db, "PART-X", pos.ID, "full-fresh", 10, now)
	partial := makeLoaderBin(t, db, "PART-X", buffer.ID, "partial-old", 5, now.Add(-2*time.Hour))

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "loader-drain-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-X",
		DeliveryNode: lineNode.Name,
		SourceNode:   pos.Name,
		Quantity:     1.0,
	})

	order := dispatchSimpleViaScanner(t, d, db, "loader-drain-1")
	if order.BinID == nil {
		t.Fatalf("order claimed no bin (status=%s) — expected the buffer partial", order.Status)
	}
	if *order.BinID != partial.ID {
		t.Fatalf("claimed bin %d, want older buffer partial %d (fresh full was %d)", *order.BinID, partial.ID, full.ID)
	}
}

// TestPlanMove_DedicatedLoaderPool_DrainsBufferedPartialOldestFirst: same proof
// for a MOVE order (swap-mode consume cells move instead of retrieve).
func TestPlanMove_DedicatedLoaderPool_DrainsBufferedPartialOldestFirst(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	pos, buffer := dedicatedLoaderFixture(t, db, "consume")

	now := time.Now().UTC()
	full := makeLoaderBin(t, db, "PART-X", pos.ID, "mv-full-fresh", 10, now)
	partial := makeLoaderBin(t, db, "PART-X", buffer.ID, "mv-partial-old", 5, now.Add(-2*time.Hour))

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "loader-move-1",
		OrderType:    OrderTypeMove,
		PayloadCode:  "PART-X",
		DeliveryNode: lineNode.Name,
		SourceNode:   pos.Name,
		Quantity:     1.0,
	})

	order := dispatchSimpleViaScanner(t, d, db, "loader-move-1")
	if order.BinID == nil {
		t.Fatalf("move claimed no bin (status=%s) — expected the buffer partial", order.Status)
	}
	if *order.BinID != partial.ID {
		t.Fatalf("move claimed bin %d, want older buffer partial %d (fresh full was %d)", *order.BinID, partial.ID, full.ID)
	}
}

// TestPlanMove_DedicatedLoaderPosition_EmptyPayload_ClaimsPhysicalBin pins the
// manual-empty-move fix: a MOVE with NO payload from a dedicated home position is a
// direct relocation of the (true-empty) carrier parked there, NOT a part-keyed pool
// drain. It must claim the physical bin at the position via the concrete-node path
// and dispatch — never route through the payload-keyed loader pool, which can't
// resolve an empty and would queue the order, after which the fulfiller's
// empty-payload guard hard-fails it. (Live repro: Springfield manual "Move" of the
// empty CARRIERs sitting on SMN_014..016 → "order has empty payload_code; cannot
// match a source bin".)
func TestPlanMove_DedicatedLoaderPosition_EmptyPayload_ClaimsPhysicalBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	pos, _ := dedicatedLoaderFixture(t, db, "produce")

	// A true-empty carrier sits on the home position (payload "", 0 UOP).
	empty := makeEmptyBin(t, db, pos.ID, "mv-empty-carrier")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "loader-move-empty-1",
		OrderType:    OrderTypeMove,
		PayloadCode:  "", // --none-- : a payload-less relocation
		DeliveryNode: lineNode.Name,
		SourceNode:   pos.Name,
		Quantity:     1.0,
	})

	order := dispatchSimpleViaScanner(t, d, db, "loader-move-empty-1")
	if order.Status == "failed" {
		t.Fatalf("empty-payload move FAILED (%s) — the loader-pool path must not swallow a payload-less relocation", order.Status)
	}
	if order.BinID == nil {
		t.Fatalf("empty-payload move claimed no bin (status=%s) — expected the physical empty carrier at the position", order.Status)
	}
	if *order.BinID != empty.ID {
		t.Fatalf("claimed bin %d, want the empty carrier %d physically at the position", *order.BinID, empty.ID)
	}
}

// TestPlanRetrieveEmpty_DedicatedLoaderPool_FillPrefersPartialOverEmpty is the
// produce/Fill path: a fresh EMPTY sits at the home position and a PARTIAL of X
// sits in a buffer. retrieve_empty must claim the PARTIAL to top up (not the
// empty), and the plain claim keeps the partial's manifest intact.
func TestPlanRetrieveEmpty_DedicatedLoaderPool_FillPrefersPartialOverEmpty(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	pos, buffer := dedicatedLoaderFixture(t, db, "produce")

	emptyBin := makeEmptyBin(t, db, pos.ID, "fill-empty-fresh")
	partial := makeLoaderBin(t, db, "PART-X", buffer.ID, "fill-partial", 5, time.Now().UTC().Add(-1*time.Hour))

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "loader-fill-1",
		OrderType:    OrderTypeRetrieveEmpty,
		PayloadCode:  "PART-X",
		DeliveryNode: lineNode.Name,
		SourceNode:   pos.Name,
		Quantity:     1.0,
	})

	order := dispatchSimpleViaScanner(t, d, db, "loader-fill-1")
	if order.BinID == nil || *order.BinID != partial.ID {
		t.Fatalf("fill claimed %v, want the partial of X %d (fresh empty was %d)", order.BinID, partial.ID, emptyBin.ID)
	}
	got, err := db.GetBin(partial.ID)
	if err != nil {
		t.Fatalf("reload partial: %v", err)
	}
	if got.PayloadCode != "PART-X" || got.UOPRemaining != 5 {
		t.Fatalf("partial manifest changed by claim: payload=%q uop=%d, want PART-X/5", got.PayloadCode, got.UOPRemaining)
	}
}

// TestPlanRetrieveEmpty_DedicatedLoaderPool_FillFallsBackToEmpty: no partial of X
// in the pool → Fill claims a fungible empty.
func TestPlanRetrieveEmpty_DedicatedLoaderPool_FillFallsBackToEmpty(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	pos, buffer := dedicatedLoaderFixture(t, db, "produce")

	empty := makeEmptyBin(t, db, buffer.ID, "fill-only-empty")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "loader-fill-empty-1",
		OrderType:    OrderTypeRetrieveEmpty,
		PayloadCode:  "PART-X",
		DeliveryNode: lineNode.Name,
		SourceNode:   pos.Name,
		Quantity:     1.0,
	})

	order := dispatchSimpleViaScanner(t, d, db, "loader-fill-empty-1")
	if order.BinID == nil || *order.BinID != empty.ID {
		t.Fatalf("fill claimed %v, want the empty %d", order.BinID, empty.ID)
	}
}

// TestPlanRetrieve_DedicatedLoaderPool_NoPartInPoolQueues guards the concrete-
// node bug fix: a loader position with no PART-X in its pool must QUEUE, not fall
// through to the global FIFO scan and pull an unrelated PART-X parked elsewhere.
func TestPlanRetrieve_DedicatedLoaderPool_NoPartInPoolQueues(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	pos, _ := dedicatedLoaderFixture(t, db, "consume")

	// A PART-X bin sits at an unrelated storage node — NOT in the loader pool.
	stray := makeLoaderBin(t, db, "PART-X", storageNode.ID, "stray-global", 10, time.Now().UTC())

	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "loader-empty-pool-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-X",
		DeliveryNode: lineNode.Name,
		SourceNode:   pos.Name,
		Quantity:     1.0,
	})

	if len(emitter.queued) != 1 {
		t.Fatalf("queued events = %d, want 1 (loader pool empty must queue, not pull globally)", len(emitter.queued))
	}
	order := dispatchSimpleViaScanner(t, d, db, "loader-empty-pool-1")
	if order.BinID != nil {
		t.Fatalf("order claimed bin %d (stray global %d) — must not fall through to the global scan", *order.BinID, stray.ID)
	}
}
