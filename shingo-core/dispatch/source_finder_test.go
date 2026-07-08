package dispatch

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// These tests pin the SourceFinder tier cascade — the one seam BOTH intake
// planning and the scanner replay route through, so "intake and replay agree" is
// structural, not coincidental. Each test asserts the finder's outcome AND that
// the plant-wide fallbacks (FindSourceBinFIFO / FindEmptyCompatibleBin) are NOT
// consulted when a scoped tier applies — the exact drift the collapse fixes.

// ── fake FinderDB ────────────────────────────────────────────────────────────

type fakeFinderDB struct {
	nodesByID   map[int64]*nodes.Node
	nodesByName map[string]*nodes.Node
	binsByNode  map[int64][]*bins.Bin

	homes     []loaders.Home
	loaders   map[int64]*loaders.Loader
	homeByPos map[int64]*loaders.Home

	fifoBin     *bins.Bin
	globalEmpty *bins.Bin
	groupEmpty  *bins.Bin
	accessible  map[int64]bool // slot -> accessible; absent = accessible

	fifoCalls        int
	globalEmptyCalls int
	groupEmptyCalls  int
}

func newFakeFinderDB() *fakeFinderDB {
	return &fakeFinderDB{
		nodesByID:   map[int64]*nodes.Node{},
		nodesByName: map[string]*nodes.Node{},
		binsByNode:  map[int64][]*bins.Bin{},
		loaders:     map[int64]*loaders.Loader{},
		homeByPos:   map[int64]*loaders.Home{},
	}
}

func (f *fakeFinderDB) addNode(n *nodes.Node) {
	f.nodesByID[n.ID] = n
	f.nodesByName[n.Name] = n
}

func (f *fakeFinderDB) addBin(b *bins.Bin) {
	if b.NodeID != nil {
		f.binsByNode[*b.NodeID] = append(f.binsByNode[*b.NodeID], b)
	}
}

// addDedicatedLoader registers a dedicated_positions loader with one pinned home
// position (InSourcePool → true) at positionNodeID.
func (f *fakeFinderDB) addDedicatedLoader(loaderID, positionNodeID int64, pinnedPayload string) {
	f.loaders[loaderID] = &loaders.Loader{ID: loaderID, Layout: loaders.LayoutDedicatedPositions}
	h := loaders.Home{LoaderID: loaderID, PositionNodeID: positionNodeID, PayloadCode: pinnedPayload}
	f.homes = append(f.homes, h)
	f.homeByPos[positionNodeID] = &f.homes[len(f.homes)-1]
}

func (f *fakeFinderDB) GetNodeByDotName(name string) (*nodes.Node, error) {
	if n, ok := f.nodesByName[name]; ok {
		return n, nil
	}
	return nil, errors.New("node not found: " + name)
}

func (f *fakeFinderDB) GetNode(id int64) (*nodes.Node, error) {
	if n, ok := f.nodesByID[id]; ok {
		return n, nil
	}
	return nil, errors.New("node not found")
}

func (f *fakeFinderDB) ListBinsByNode(nodeID int64) ([]*bins.Bin, error) {
	return f.binsByNode[nodeID], nil
}

func (f *fakeFinderDB) ListBinsByNodes(ids []int64) ([]*bins.Bin, error) {
	var out []*bins.Bin
	for _, id := range ids {
		out = append(out, f.binsByNode[id]...)
	}
	return out, nil
}

func (f *fakeFinderDB) FindSourceBinFIFO(_ string, _ int64) (*bins.Bin, error) {
	f.fifoCalls++
	if f.fifoBin == nil {
		return nil, errors.New("no source bin")
	}
	return f.fifoBin, nil
}

func (f *fakeFinderDB) FindEmptyCompatibleBin(_, _ string, _ int64) (*bins.Bin, error) {
	f.globalEmptyCalls++
	if f.globalEmpty == nil {
		return nil, errors.New("no empty")
	}
	return f.globalEmpty, nil
}

func (f *fakeFinderDB) FindEmptyCompatibleBinInGroup(_ string, _, _ int64) (*bins.Bin, error) {
	f.groupEmptyCalls++
	if f.groupEmpty == nil {
		return nil, errors.New("no empty in group")
	}
	return f.groupEmpty, nil
}

func (f *fakeFinderDB) IsSlotAccessible(id int64) (bool, error) {
	if f.accessible == nil {
		return true, nil
	}
	acc, ok := f.accessible[id]
	if !ok {
		return true, nil
	}
	return acc, nil
}

func (f *fakeFinderDB) GetLoaderHomeByPositionNode(posID int64) (*loaders.Home, error) {
	return f.homeByPos[posID], nil
}

func (f *fakeFinderDB) GetLoader(id int64) (*loaders.Loader, error) {
	return f.loaders[id], nil
}

func (f *fakeFinderDB) ListLoaderHomes(loaderID int64) ([]loaders.Home, error) {
	var out []loaders.Home
	for _, h := range f.homes {
		if h.LoaderID == loaderID {
			out = append(out, h)
		}
	}
	return out, nil
}

// fakeResolver stubs NodeResolver for the NGRP tier.
type fakeResolver struct {
	result *ResolveResult
	err    error
}

func (r *fakeResolver) Resolve(_ *nodes.Node, _ protocol.OrderType, _ string, _ *int64) (*ResolveResult, error) {
	return r.result, r.err
}

// ── A1: dedicated-loader pool (Drain), no plant-wide fall-through ─────────────

func TestReplayUsesLoaderPool(t *testing.T) {
	db := newFakeFinderDB()
	posID := int64(51)
	db.addNode(&nodes.Node{ID: posID, Name: "L1"})
	db.addNode(&nodes.Node{ID: 99, Name: "DEST"})
	db.addDedicatedLoader(1, posID, "X")

	// A plant-wide FIFO bin exists — it must NEVER be chosen for a loader source.
	wrongNode := int64(60)
	db.addNode(&nodes.Node{ID: wrongNode, Name: "WRONG"})
	db.fifoBin = &bins.Bin{ID: 201, PayloadCode: "X", NodeID: &wrongNode}

	finder := NewSourceFinder(db, nil, nil)
	order := &orders.Order{ID: 1, OrderType: OrderTypeRetrieve, SourceNode: "L1", DeliveryNode: "DEST", PayloadCode: "X"}

	// Pool empty → Wait, and the plant-wide FIFO is NOT consulted.
	res := finder.FindSource(order, IntentFull)
	if res.Outcome != OutcomeWait {
		t.Fatalf("pool empty: got outcome %v, want OutcomeWait", res.Outcome)
	}
	if db.fifoCalls != 0 {
		t.Errorf("FindSourceBinFIFO must not be called while the loader pool is empty: %d calls", db.fifoCalls)
	}

	// Pool gets a partial of X → replay sources the pool bin, still no plant-wide FIFO.
	db.addBin(&bins.Bin{ID: 101, PayloadCode: "X", NodeID: &posID, UOPRemaining: 5, UOPCapacity: 10, Status: "available"})
	res = finder.FindSource(order, IntentFull)
	if res.Outcome != OutcomeFound {
		t.Fatalf("pool partial: got outcome %v, want OutcomeFound", res.Outcome)
	}
	if res.Bin == nil || res.Bin.ID != 101 {
		t.Errorf("bin: got %v, want loader pool bin 101", res.Bin)
	}
	if res.Node == nil || res.Node.Name != "L1" {
		t.Errorf("node: got %v, want L1", res.Node)
	}
	if db.fifoCalls != 0 {
		t.Errorf("FindSourceBinFIFO must not be called on loader replay: %d calls", db.fifoCalls)
	}
}

// ── A2: group/lane-scoped empty, no plant-wide fall-through ───────────────────

func TestReplayKeepsGroupScope(t *testing.T) {
	db := newFakeFinderDB()
	groupID := int64(100)
	db.addNode(&nodes.Node{ID: groupID, Name: "GROUP-A", IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP})
	db.addNode(&nodes.Node{ID: 99, Name: "DEST"})

	// A wrong-group empty exists globally — must never be picked while scoped.
	wrong := int64(200)
	db.addNode(&nodes.Node{ID: wrong, Name: "WRONG-SUPERMARKET"})
	db.globalEmpty = &bins.Bin{ID: 201, PayloadCode: "", NodeID: &wrong}

	scoped := int64(101)
	db.addNode(&nodes.Node{ID: scoped, Name: "GROUP-A-LANE-1"})
	db.groupEmpty = &bins.Bin{ID: 101, PayloadCode: "", NodeID: &scoped}

	finder := NewSourceFinder(db, nil, nil)
	order := &orders.Order{ID: 2, OrderType: OrderTypeRetrieveEmpty, SourceNode: "GROUP-A", DeliveryNode: "DEST"}

	res := finder.FindSource(order, IntentEmpty)
	if res.Outcome != OutcomeFound {
		t.Fatalf("got outcome %v, want OutcomeFound", res.Outcome)
	}
	if res.Bin == nil || res.Bin.ID != 101 {
		t.Errorf("bin: got %v, want scoped empty 101", res.Bin)
	}
	if db.globalEmptyCalls != 0 {
		t.Errorf("global empty finder must not be consulted while the group scope applies: %d calls", db.globalEmptyCalls)
	}

	// Group loses its empty → Wait, and STILL no plant-wide fall-through.
	db.groupEmpty = nil
	res = finder.FindSource(order, IntentEmpty)
	if res.Outcome != OutcomeWait {
		t.Fatalf("group empty gone: got outcome %v, want OutcomeWait", res.Outcome)
	}
	if db.globalEmptyCalls != 0 {
		t.Errorf("scoped empty must not fall through to the plant-wide finder: %d calls", db.globalEmptyCalls)
	}
}

// ── A4: NGRP capacity error queues SCOPED (was the drift to plant-wide FIFO) ──

func TestReplayNGRPCapacityStaysScoped(t *testing.T) {
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 100, Name: "NGRP-A", IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP})
	db.addNode(&nodes.Node{ID: 99, Name: "DEST"})
	wrong := int64(300)
	db.addNode(&nodes.Node{ID: wrong, Name: "WRONG"})
	db.fifoBin = &bins.Bin{ID: 201, PayloadCode: "X", NodeID: &wrong}

	// The momentarily-empty-group error is untyped and classifies ResolutionCapacity.
	resolver := &fakeResolver{err: errors.New("no bin of requested payload in node group NGRP-A")}
	finder := NewSourceFinder(db, resolver, nil)
	order := &orders.Order{ID: 3, OrderType: OrderTypeRetrieve, SourceNode: "NGRP-A", DeliveryNode: "DEST", PayloadCode: "X"}

	res := finder.FindSource(order, IntentFull)
	if res.Outcome != OutcomeWait {
		t.Fatalf("saturated NGRP: got outcome %v, want OutcomeWait", res.Outcome)
	}
	if db.fifoCalls != 0 {
		t.Errorf("capacity error must not fall through to plant-wide FIFO: %d calls", db.fifoCalls)
	}

	// Group gets a bin → resolver returns it → Found.
	childID := int64(150)
	db.addNode(&nodes.Node{ID: childID, Name: "NGRP-A-CHILD"})
	groupBin := &bins.Bin{ID: 101, PayloadCode: "X", NodeID: &childID}
	resolver.err = nil
	resolver.result = &ResolveResult{Node: db.nodesByID[childID], Bin: groupBin}

	res = finder.FindSource(order, IntentFull)
	if res.Outcome != OutcomeFound {
		t.Fatalf("group got a bin: got outcome %v, want OutcomeFound", res.Outcome)
	}
	if res.Bin == nil || res.Bin.ID != 101 {
		t.Errorf("bin: got %v, want group bin 101", res.Bin)
	}
}

// NGRP structural error is terminal (both callers map it to their fail path).
func TestFindSourceNGRPStructuralTerminal(t *testing.T) {
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 100, Name: "NGRP-A", IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP})
	db.addNode(&nodes.Node{ID: 99, Name: "DEST"})
	resolver := &fakeResolver{err: &StructuralError{Group: "NGRP-A", Payload: "X", Reason: "no child node accepts this payload type"}}
	finder := NewSourceFinder(db, resolver, nil)
	order := &orders.Order{ID: 7, OrderType: OrderTypeRetrieve, SourceNode: "NGRP-A", DeliveryNode: "DEST", PayloadCode: "X"}

	res := finder.FindSource(order, IntentFull)
	if res.Outcome != OutcomeStructural {
		t.Fatalf("got outcome %v, want OutcomeStructural", res.Outcome)
	}
	if res.TermCode != codeStructural {
		t.Errorf("term code: got %q, want %q", res.TermCode, codeStructural)
	}
}

// ── A6: payload-less move sources node-locally, never structurally failed ─────

func TestMoveReplayNotStructurallyFailed(t *testing.T) {
	db := newFakeFinderDB()
	srcID := int64(600)
	db.addNode(&nodes.Node{ID: srcID, Name: "MOVE-SRC"})
	db.addNode(&nodes.Node{ID: 99, Name: "MOVE-DEST"})
	db.addBin(&bins.Bin{ID: 88, PayloadCode: "X", NodeID: &srcID, Status: "available"})

	finder := NewSourceFinder(db, nil, nil)
	// Payload-less move: relocates the physical bin AT the source node.
	order := &orders.Order{ID: 5, OrderType: OrderTypeMove, SourceIntent: SourceIntentLocal, SourceNode: "MOVE-SRC", DeliveryNode: "MOVE-DEST", PayloadCode: ""}

	res := finder.FindSource(order, IntentFull)
	if res.Outcome != OutcomeFound {
		t.Fatalf("payload-less move: got outcome %v, want OutcomeFound", res.Outcome)
	}
	if res.Bin == nil || res.Bin.ID != 88 {
		t.Errorf("bin: got %v, want MOVE-SRC bin 88", res.Bin)
	}
	if res.Node == nil || res.Node.Name != "MOVE-SRC" {
		t.Errorf("node: got %v, want MOVE-SRC", res.Node)
	}
	if db.fifoCalls != 0 {
		t.Errorf("a move sources node-locally and must not scan plant-wide: %d calls", db.fifoCalls)
	}

	// No bin at the source node → Wait (not a terminal structural fail).
	db.binsByNode[srcID] = nil
	res = finder.FindSource(order, IntentFull)
	if res.Outcome != OutcomeWait {
		t.Fatalf("empty move source: got outcome %v, want OutcomeWait", res.Outcome)
	}
	if db.fifoCalls != 0 {
		t.Errorf("move must not fall through to plant-wide FIFO: %d calls", db.fifoCalls)
	}
}

// Stage 4 re-homing: the move-shape decision is keyed on the SourceIntent data
// stamped at intake (SourceIntentLocal), NOT on OrderType. These two subcases
// carry the identical OrderTypeMove order shape and differ only in SourceIntent,
// so the field alone drives the outcome: WITH the intent the finder sources
// node-locally and never widens; WITHOUT it the same order is retrieve-shaped
// and falls through to the plant-wide FIFO scan. Before Stage 4
// (moveShaped := order.OrderType == OrderTypeMove) the second subcase would have
// stayed node-local and this test would fail — that is the red-before-green.
func TestFindSourceMoveShapeKeyedOnSourceIntent(t *testing.T) {
	// SourceIntentLocal → move-shaped: no bin at the source queues scoped and
	// never touches the plant-wide FIFO scan.
	t.Run("local_intent_sources_node_local", func(t *testing.T) {
		db := newFakeFinderDB()
		srcID := int64(700)
		db.addNode(&nodes.Node{ID: srcID, Name: "SRC"})
		db.addNode(&nodes.Node{ID: 99, Name: "DEST"})
		db.fifoBin = &bins.Bin{ID: 900, PayloadCode: "X"} // must never be chosen
		finder := NewSourceFinder(db, nil, nil)
		order := &orders.Order{ID: 1, OrderType: OrderTypeMove, SourceIntent: SourceIntentLocal, SourceNode: "SRC", DeliveryNode: "DEST", PayloadCode: "X"}
		res := finder.FindSource(order, IntentFull)
		if res.Outcome != OutcomeWait {
			t.Fatalf("no bin at source: got %v, want OutcomeWait", res.Outcome)
		}
		if db.fifoCalls != 0 {
			t.Errorf("move-shaped must not widen to plant-wide FIFO: %d calls", db.fifoCalls)
		}
	})

	// No SourceIntent → retrieve-shaped: the identical OrderTypeMove order now
	// falls through to the plant-wide FIFO scan. The type no longer decides.
	t.Run("no_intent_widens_to_plant_wide", func(t *testing.T) {
		db := newFakeFinderDB()
		srcID := int64(700)
		fifoNodeID := int64(800)
		db.addNode(&nodes.Node{ID: srcID, Name: "SRC"})
		db.addNode(&nodes.Node{ID: 99, Name: "DEST"})
		db.addNode(&nodes.Node{ID: fifoNodeID, Name: "FIFO-SLOT"})
		db.fifoBin = &bins.Bin{ID: 901, PayloadCode: "X", NodeID: &fifoNodeID}
		finder := NewSourceFinder(db, nil, nil)
		order := &orders.Order{ID: 2, OrderType: OrderTypeMove, SourceNode: "SRC", DeliveryNode: "DEST", PayloadCode: "X"}
		res := finder.FindSource(order, IntentFull)
		if res.Outcome != OutcomeFound {
			t.Fatalf("retrieve-shaped: got %v, want OutcomeFound", res.Outcome)
		}
		if db.fifoCalls == 0 {
			t.Errorf("without SourceIntentLocal the finder must widen to plant-wide FIFO")
		}
		if res.Bin == nil || res.Bin.ID != 901 {
			t.Errorf("bin: got %v, want plant-wide FIFO bin 901", res.Bin)
		}
	})
}

// Empty-intent buried result routes to reshuffle (planRetrieveEmpty's :421 path).
func TestFindSourceEmptyBuriedReshuffles(t *testing.T) {
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 99, Name: "DEST"})
	laneID := int64(400)
	slotID := int64(401)
	db.addNode(&nodes.Node{ID: laneID, Name: "LANE-1", NodeTypeCode: protocol.NodeClassLANE})
	db.addNode(&nodes.Node{ID: slotID, Name: "LANE-1-SLOT-2", ParentID: &laneID})
	db.globalEmpty = &bins.Bin{ID: 77, PayloadCode: "", NodeID: &slotID}
	db.accessible = map[int64]bool{slotID: false} // buried

	finder := NewSourceFinder(db, nil, nil)
	order := &orders.Order{ID: 8, OrderType: OrderTypeRetrieveEmpty, DeliveryNode: "DEST"} // no source → global empty

	res := finder.FindSource(order, IntentEmpty)
	if res.Outcome != OutcomeReshuffle {
		t.Fatalf("buried empty: got outcome %v, want OutcomeReshuffle", res.Outcome)
	}
	if res.Buried == nil || res.Buried.Bin.ID != 77 || res.Buried.LaneID != laneID {
		t.Errorf("buried payload: got %+v, want bin 77 in lane %d", res.Buried, laneID)
	}
}

// ── the both-paths-through-one-finder contract, table form ────────────────────

func TestIntakeAndReplayAgree(t *testing.T) {
	// Both intake planning and the scanner replay call THIS finder, so the table
	// below is the shared contract they both honor — there is no second
	// implementation to drift from.
	cases := []struct {
		name        string
		build       func(db *fakeFinderDB) (*fakeResolver, *orders.Order, Intent)
		wantOutcome Outcome
		wantBinID   int64
		wantNoFIFO  bool
	}{
		{
			name: "retrieve_plant_wide_fifo",
			build: func(db *fakeFinderDB) (*fakeResolver, *orders.Order, Intent) {
				n := int64(10)
				db.addNode(&nodes.Node{ID: n, Name: "SM-SLOT"})
				db.fifoBin = &bins.Bin{ID: 4001, PayloadCode: "X", NodeID: &n}
				return nil, &orders.Order{ID: 10, OrderType: OrderTypeRetrieve, PayloadCode: "X"}, IntentFull
			},
			wantOutcome: OutcomeFound, wantBinID: 4001,
		},
		{
			name: "retrieve_empty_plant_wide",
			build: func(db *fakeFinderDB) (*fakeResolver, *orders.Order, Intent) {
				n := int64(11)
				db.addNode(&nodes.Node{ID: n, Name: "EMPTY-SLOT"})
				db.globalEmpty = &bins.Bin{ID: 3001, PayloadCode: "", NodeID: &n}
				return nil, &orders.Order{ID: 11, OrderType: OrderTypeRetrieveEmpty}, IntentEmpty
			},
			wantOutcome: OutcomeFound, wantBinID: 3001,
		},
		{
			name: "retrieve_no_source_waits",
			build: func(db *fakeFinderDB) (*fakeResolver, *orders.Order, Intent) {
				return nil, &orders.Order{ID: 12, OrderType: OrderTypeRetrieve, PayloadCode: "X"}, IntentFull
			},
			wantOutcome: OutcomeWait,
		},
		{
			name: "move_no_bin_waits_not_terminal",
			build: func(db *fakeFinderDB) (*fakeResolver, *orders.Order, Intent) {
				db.addNode(&nodes.Node{ID: 20, Name: "MSRC"})
				return nil, &orders.Order{ID: 13, OrderType: OrderTypeMove, SourceIntent: SourceIntentLocal, SourceNode: "MSRC", PayloadCode: "X"}, IntentFull
			},
			wantOutcome: OutcomeWait, wantNoFIFO: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := newFakeFinderDB()
			resolver, order, intent := c.build(db)
			var r NodeResolver
			if resolver != nil {
				r = resolver
			}
			finder := NewSourceFinder(db, r, nil)
			res := finder.FindSource(order, intent)
			if res.Outcome != c.wantOutcome {
				t.Fatalf("outcome: got %v, want %v", res.Outcome, c.wantOutcome)
			}
			if c.wantBinID != 0 && (res.Bin == nil || res.Bin.ID != c.wantBinID) {
				t.Errorf("bin: got %v, want %d", res.Bin, c.wantBinID)
			}
			if c.wantNoFIFO && db.fifoCalls != 0 {
				t.Errorf("expected no plant-wide FIFO scan, got %d calls", db.fifoCalls)
			}
		})
	}
}
