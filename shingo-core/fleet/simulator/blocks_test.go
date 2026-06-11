package simulator

import (
	"sync"
	"testing"

	"shingocore/fleet"
)

type capturedBlock struct {
	orderID       int64
	vendorOrderID string
	blockID       string
	location      string
	binTask       string
}

// captureEmitter records EmitBlockCompleted calls (and order status changes,
// used by the driver determinism test) for assertions.
type captureEmitter struct {
	mu     sync.Mutex
	blocks []capturedBlock
	status []string // "vendorOrderID:newState", in emit order
}

func (c *captureEmitter) EmitOrderStatusChanged(_ int64, vendorOrderID, _, newStatus, _, _ string, _ *fleet.OrderSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = append(c.status, vendorOrderID+":"+newStatus)
}
func (c *captureEmitter) EmitGraceExpired(int64, string) {}
func (c *captureEmitter) EmitBlockCompleted(orderID int64, vendorOrderID, blockID, location, binTask string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocks = append(c.blocks, capturedBlock{orderID, vendorOrderID, blockID, location, binTask})
}

type fixedResolver struct{ id int64 }

func (r fixedResolver) ResolveVendorOrderID(string) (int64, error) { return r.id, nil }

// T2.2: CompleteBlock emits a routable EmitBlockCompleted per block, carrying
// the binTask the engine classifier keys on and the resolved ShinGo order ID.
func TestCompleteBlockEmitsRoutableEvents(t *testing.T) {
	s := New()
	emitter := &captureEmitter{}
	s.InitTracker(emitter, fixedResolver{id: 42})

	vid := mkTransport(t, s, "o1") // JackLoad@A, JackUnload@B
	blocks := s.BlocksForOrder(vid)
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(blocks))
	}
	for _, b := range blocks {
		if !s.CompleteBlock(vid, b.BlockID, b.Location, b.BinTask) {
			t.Fatalf("CompleteBlock returned false for %s", b.BlockID)
		}
	}

	if len(emitter.blocks) != 2 {
		t.Fatalf("want 2 block-completed events, got %d", len(emitter.blocks))
	}
	if emitter.blocks[0].binTask != "JackLoad" || emitter.blocks[1].binTask != "JackUnload" {
		t.Fatalf("binTask not carried through: %+v", emitter.blocks)
	}
	if emitter.blocks[0].location != "A" || emitter.blocks[1].location != "B" {
		t.Fatalf("location not carried through: %+v", emitter.blocks)
	}
	if emitter.blocks[0].orderID != 42 {
		t.Fatalf("order ID not resolved: got %d", emitter.blocks[0].orderID)
	}
	// Sanity: the engine's classifiers would route these as pickup then dropoff.
	if !looksPickup(emitter.blocks[0].binTask) || looksPickup(emitter.blocks[1].binTask) {
		t.Fatalf("binTask shapes wrong for routing: %+v", emitter.blocks)
	}
}

// looksPickup mirrors the engine's isPickupBlock just enough for the routing
// sanity check above (JackLoad → pickup, JackUnload → not).
func looksPickup(binTask string) bool {
	return binTask == "JackLoad"
}

func TestCompleteBlockNoEmitWithoutTracker(t *testing.T) {
	s := New()
	vid := mkTransport(t, s, "o1")
	if s.CompleteBlock(vid, "blk", "A", "JackLoad") {
		t.Fatal("CompleteBlock should be a no-op before InitTracker")
	}
}

func TestCompleteBlockUnknownOrder(t *testing.T) {
	s := New()
	s.InitTracker(&captureEmitter{}, fixedResolver{id: 1})
	if s.CompleteBlock("does-not-exist", "blk", "A", "JackLoad") {
		t.Fatal("CompleteBlock should be false for an unknown order")
	}
}
