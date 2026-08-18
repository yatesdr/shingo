package dispatch

import (
	"database/sql"
	"errors"
	"testing"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// These tests pin the SourceFinder tier cascade — the one seam BOTH intake
// planning and the scanner replay route through, so "intake and replay agree" is
// structural, not coincidental. Each test asserts the finder's outcome AND that
// the plant-wide fallbacks (FindSourceBinFIFO / FindEmptyCompatibleBin) are NOT
// consulted when a scoped tier applies — the exact drift the collapse fixes.

// ── fake FinderDB ────────────────────────────────────────────────────────────
//
// EVERY FINDER HERE RETURNS sql.ErrNoRows FOR NONE-FOUND, because that is what
// the real store returns — ScanBin propagates it straight off row.Scan, and a
// wrapped error means the query failed. Until MG3-1a these returned
// errors.New("no empty"), which was a lie the call sites could not catch
// because they read any error as "none found" anyway. Now that the cascade
// separates the two, a fake that kept lying would send every none-found path
// down the unreadable arm and no test could tell.

type fakeFinderDB struct {
	nodesByID   map[int64]*nodes.Node
	nodesByName map[string]*nodes.Node
	binsByNode  map[int64][]*bins.Bin

	homes     []loaders.Home
	loaders   map[int64]*loaders.Loader
	homeByPos map[int64]*loaders.Home

	// Declared carrier mix: quota per loader, capability per window.
	quotas       map[int64][]loaders.Quota
	homeBinTypes map[int64]map[int64][]string
	typedEmpty   map[string]*bins.Bin

	// maintainedType stands for the open maintained-group episodes: origin id →
	// the carrier type that episode is short of. An origin absent from the map is
	// not a maintain episode, which is every other order in the plant.
	maintainedType map[string]string
	// maintainedGroup is the other half of the same episode: origin id → the
	// group node name. It is what the finder turns into a subtree exclusion so a
	// top-off ask cannot source out of the group it is filling.
	maintainedGroup map[string]string
	// lastFence records the maintained-group fence the plant-wide finders were
	// actually handed. Recorded rather than ignored because the fence IS the
	// behaviour under test; it replaced lastExcludedSubtree when MG3-1 absorbed
	// MG2-11's subtree exclusion into the fence's second rule.
	lastFence bins.EmptyFence
	// lastAsker records the dig asker the finders were handed. MG3-0 pinned that
	// this was the ZERO value on the simple path; MG3-1b makes it real, so the
	// record is what the inverted pin asserts against.
	lastAsker reservations.DigAsker
	// fencedGroups are the group node ids that turn every asker away, and
	// fencedGroupErr makes the fence read FAIL. Both stand for a strict
	// maintained group the need is not supported at.
	fencedGroups   map[int64]bool
	fencedGroupErr error

	// supportingGroups: process node name -> the maintained groups serving it.
	// nodeUnder: bin node id -> the group it sits under, if any.
	supportingGroups    map[string][]int64
	supportingGroupsErr error
	nodeUnder           map[int64]int64
	// maintainedTypeErr makes the episode read FAIL rather than answer, so the
	// "a read failure returns no type rather than guessing" arm is exercisable.
	maintainedTypeErr error
	// searchErr makes EVERY empty/full finder fail with a real error rather than
	// sql.ErrNoRows — the "the query did not run" case MG3-1a separates from
	// "nothing matched". One field for all of them because they share one
	// disposition at the caller, and one of them exercising it is enough.
	searchErr error
	// nodeBinsErr makes the node-local candidate read FAIL. Until MG3-1a that
	// error was discarded outright — `candidates, _ :=` — so a failed read
	// parked the order under "the node holds nothing usable", which is a claim
	// about the plant nobody had checked.
	nodeBinsErr error

	fifoBin     *bins.Bin
	globalEmpty *bins.Bin
	groupEmpty  *bins.Bin
	accessible  map[int64]bool // slot -> accessible; absent = accessible

	// accessibilityErr makes the reachability read FAIL rather than answer, which
	// is the only way to exercise the fail-closed disposition (D2). Set, it wins
	// over `accessible`.
	accessibilityErr error
	// nodeErr makes GetNode fail for one id — the second and third reads in the
	// same tier-6 block, which used to fall through to OutcomeFound just as
	// silently as the first.
	nodeErr map[int64]error
	// loaderHomesErr makes the loader-pool membership read FAIL. It stands for all
	// five reads sourceFromDedicatedLoader can fail on; they share one disposition
	// and one arm at the caller, so one of them exercises it.
	loaderHomesErr error

	fifoCalls        int
	globalEmptyCalls int
	groupEmptyCalls  int
	typedGroupCalls  int
	// typedGlobalCalls counts the TYPED plant-wide empty search — the tier-5
	// twin of globalEmptyCalls. Absent until MG3-0, which is why the golden
	// matrix could not assert "and no neighbouring finder was consulted" on the
	// one tier where the typed and untyped arms sit side by side.
	typedGlobalCalls int
}

func newFakeFinderDB() *fakeFinderDB {
	return &fakeFinderDB{
		nodesByID:    map[int64]*nodes.Node{},
		nodesByName:  map[string]*nodes.Node{},
		binsByNode:   map[int64][]*bins.Bin{},
		loaders:      map[int64]*loaders.Loader{},
		homeByPos:    map[int64]*loaders.Home{},
		quotas:       map[int64][]loaders.Quota{},
		homeBinTypes: map[int64]map[int64][]string{},
		typedEmpty:   map[string]*bins.Bin{},
		// Both episode maps are made here rather than lazily by each test. They
		// were nil until MG3-0, so a test that stated a maintain origin panicked
		// on assignment instead of exercising the arm — which is a fixture that
		// can only be used by someone who already knows it is broken.
		maintainedType:   map[string]string{},
		maintainedGroup:  map[string]string{},
		fencedGroups:     map[int64]bool{},
		supportingGroups: map[string][]int64{},
		nodeUnder:        map[int64]int64{},
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
	if err, ok := f.nodeErr[id]; ok {
		return nil, err
	}
	if n, ok := f.nodesByID[id]; ok {
		return n, nil
	}
	return nil, errors.New("node not found")
}

func (f *fakeFinderDB) ListBinsByNode(nodeID int64) ([]*bins.Bin, error) {
	if f.nodeBinsErr != nil {
		return nil, f.nodeBinsErr
	}
	return f.binsByNode[nodeID], nil
}

func (f *fakeFinderDB) ListBinsByNodes(ids []int64) ([]*bins.Bin, error) {
	var out []*bins.Bin
	for _, id := range ids {
		out = append(out, f.binsByNode[id]...)
	}
	return out, nil
}

func (f *fakeFinderDB) ListLoaderQuotas(loaderID int64) ([]loaders.Quota, error) {
	return f.quotas[loaderID], nil
}

func (f *fakeFinderDB) ListLoaderHomeBinTypes(loaderID int64) (map[int64][]string, error) {
	return f.homeBinTypes[loaderID], nil
}

// FindEmptyBinOfType RECORDS THE FENCE IT WAS HANDED. The fence is the whole
// behaviour under test on this path, so a fake that dropped the argument would
// let it be removed with every test still green — the same reason its
// predecessor recorded the subtree it was told to exclude.
func (f *fakeFinderDB) FindEmptyBinOfType(binTypeCode, _ string, _ int64, fence bins.EmptyFence, asker reservations.DigAsker) (*bins.Bin, error) {
	f.typedGlobalCalls++
	f.lastFence = fence
	f.lastAsker = asker
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if b, ok := f.typedEmpty[binTypeCode]; ok {
		return b, nil
	}
	return nil, sql.ErrNoRows
}

// supportingGroups / nodeUnder stand for the MG3-5 audit's two reads. Recorded
// rather than stubbed to nothing so the audit's SILENCE is testable: the line
// must not fire for a press no group serves, and must not fire when the carrier
// came from inside one that does.
func (f *fakeFinderDB) MaintainedGroupsSupporting(processNode string) ([]int64, error) {
	if f.supportingGroupsErr != nil {
		return nil, f.supportingGroupsErr
	}
	return f.supportingGroups[processNode], nil
}

func (f *fakeFinderDB) NodeIsUnderAny(nodeID int64, roots []int64) (bool, error) {
	for _, r := range roots {
		if f.nodeUnder[nodeID] == r {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeFinderDB) GroupFencesAsker(groupNodeID int64, _ string) (bool, error) {
	if f.fencedGroupErr != nil {
		return false, f.fencedGroupErr
	}
	return f.fencedGroups[groupNodeID], nil
}

func (f *fakeFinderDB) MaintainedEpisodeForOrigin(originID string) (string, string, error) {
	if f.maintainedTypeErr != nil {
		return "", "", f.maintainedTypeErr
	}
	if originID == "" {
		// The real one returns without querying: origin_id is a UUID column and
		// "" is a cast error, not an empty result.
		return "", "", nil
	}
	return f.maintainedGroup[originID], f.maintainedType[originID], nil
}

func (f *fakeFinderDB) FindEmptyBinOfTypeInGroup(binTypeCode string, _, _ int64, asker reservations.DigAsker) (*bins.Bin, error) {
	f.typedGroupCalls++
	f.lastAsker = asker
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if b, ok := f.typedEmpty[binTypeCode]; ok {
		return b, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakeFinderDB) FindSourceBinFIFO(_ string, _ int64) (*bins.Bin, error) {
	f.fifoCalls++
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if f.fifoBin == nil {
		return nil, sql.ErrNoRows
	}
	return f.fifoBin, nil
}

func (f *fakeFinderDB) FindEmptyCompatibleBin(_, _ string, _ int64, fence bins.EmptyFence, asker reservations.DigAsker) (*bins.Bin, error) {
	f.globalEmptyCalls++
	f.lastFence = fence
	f.lastAsker = asker
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if f.globalEmpty == nil {
		return nil, sql.ErrNoRows
	}
	return f.globalEmpty, nil
}

func (f *fakeFinderDB) FindEmptyCompatibleBinInGroup(_ string, _, _ int64, asker reservations.DigAsker) (*bins.Bin, error) {
	f.groupEmptyCalls++
	f.lastAsker = asker
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if f.groupEmpty == nil {
		return nil, sql.ErrNoRows
	}
	return f.groupEmpty, nil
}

func (f *fakeFinderDB) IsSlotAccessible(id int64) (bool, error) {
	if f.accessibilityErr != nil {
		return false, f.accessibilityErr // fails closed: false AND the error
	}
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
	if f.loaderHomesErr != nil {
		return nil, f.loaderHomesErr
	}
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

func (r *fakeResolver) Resolve(_ *nodes.Node, _ binresolver.ResolveMode, _ string, _ *int64, _ reservations.DigAsker) (*ResolveResult, error) {
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

// ── MG2-4: the typed ask, pinned at mint ──────────────────────────────────
//
// An ask carrying an origin that names an open maintained-group episode sources
// THAT type and never re-derives one. These pin the routing consequence: the
// need must reach the TYPED group finder, not the compatible one, and a group
// that has no carrier of the wanted type must WAIT rather than substitute.

func TestFindSource_MaintainOriginPinsTheType(t *testing.T) {
	db := newFakeFinderDB()
	groupID := int64(100)
	db.addNode(&nodes.Node{ID: groupID, Name: "PRESS-EMPTIES", IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP})
	// The keeper pre-resolves, so its DeliveryNode is a concrete child slot —
	// NOT a loader window. The loader derivation below the new arm would find no
	// home here and return "", which is exactly why the arm goes first.
	slot := int64(101)
	db.addNode(&nodes.Node{ID: slot, Name: "PEB_001"})

	const origin = "11111111-2222-3333-4444-555555555555"
	db.maintainedType = map[string]string{origin: "45x58x32"}

	// Both types are physically present in the group. Only the pinned one may
	// be taken.
	wanted := int64(102)
	db.addNode(&nodes.Node{ID: wanted, Name: "PEB_002"})
	db.typedEmpty = map[string]*bins.Bin{
		"45x58x32": {ID: 501, PayloadCode: "", NodeID: &wanted},
	}
	db.groupEmpty = &bins.Bin{ID: 999, PayloadCode: "", NodeID: &wanted} // the wrong-type trap

	finder := NewSourceFinder(db, nil, nil)
	order := &orders.Order{
		ID: 7, OrderType: OrderTypeRetrieveEmpty,
		SourceNode: "PRESS-EMPTIES", DeliveryNode: "PEB_001",
		OriginID: origin,
	}

	res := finder.FindSource(order, IntentEmpty)
	if res.Outcome != OutcomeFound {
		t.Fatalf("outcome = %v, want OutcomeFound", res.Outcome)
	}
	if res.Bin == nil || res.Bin.ID != 501 {
		t.Fatalf("bin = %v, want the 45x58x32 carrier (501)", res.Bin)
	}
	if db.typedGroupCalls == 0 {
		t.Error("the typed group finder was never called — the pinned type did not reach the tier")
	}
	if db.groupEmptyCalls != 0 {
		t.Errorf("the COMPATIBLE group finder was consulted %d time(s); a pinned ask must never "+
			"fall through to any-compatible, which is how the mix drifts", db.groupEmptyCalls)
	}
	if db.globalEmptyCalls != 0 {
		t.Errorf("plant-wide finder consulted %d time(s) while group-scoped", db.globalEmptyCalls)
	}
}

// HONOURED, NOT APPROXIMATED. The group holds carriers — just not of the pinned
// type. The ask WAITS under the type-specific cause rather than taking what is
// there: substituting would deliver a carrier the keeper is not counting, so the
// level never converges and nothing reports an error.
func TestFindSource_MaintainOriginWaitsRatherThanSubstitute(t *testing.T) {
	db := newFakeFinderDB()
	groupID := int64(100)
	db.addNode(&nodes.Node{ID: groupID, Name: "PRESS-EMPTIES", IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP})
	db.addNode(&nodes.Node{ID: 101, Name: "PEB_001"})

	const origin = "11111111-2222-3333-4444-555555555555"
	db.maintainedType = map[string]string{origin: "45x58x32"}

	// No 45x58x32 anywhere — but a compatible empty IS in the group, and one is
	// also available plant-wide. Neither may be taken.
	other := int64(102)
	db.addNode(&nodes.Node{ID: other, Name: "PEB_002"})
	db.groupEmpty = &bins.Bin{ID: 601, PayloadCode: "", NodeID: &other}
	db.globalEmpty = &bins.Bin{ID: 602, PayloadCode: "", NodeID: &other}

	finder := NewSourceFinder(db, nil, nil)
	order := &orders.Order{
		ID: 8, OrderType: OrderTypeRetrieveEmpty,
		SourceNode: "PRESS-EMPTIES", DeliveryNode: "PEB_001",
		OriginID: origin,
	}

	res := finder.FindSource(order, IntentEmpty)
	if res.Outcome != OutcomeWait {
		t.Fatalf("outcome = %v, want OutcomeWait — a pinned type is honoured, not approximated", res.Outcome)
	}
	if res.QueueCause != CauseFinderNoEmptyOfType {
		t.Errorf("cause = %q, want %q", res.QueueCause, CauseFinderNoEmptyOfType)
	}
	if db.groupEmptyCalls != 0 || db.globalEmptyCalls != 0 {
		t.Errorf("substitution attempted: compatible-in-group=%d plant-wide=%d — both must be zero",
			db.groupEmptyCalls, db.globalEmptyCalls)
	}
}

// A blank origin is every other order in the plant, and must behave exactly as
// it did before the arm existed: fall through to the loader derivation.
func TestFindSource_BlankOriginUnchanged(t *testing.T) {
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 100, Name: "GROUP-A", IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP})
	db.addNode(&nodes.Node{ID: 99, Name: "DEST"})
	at := int64(101)
	db.addNode(&nodes.Node{ID: at, Name: "GROUP-A-LANE-1"})
	db.groupEmpty = &bins.Bin{ID: 701, PayloadCode: "", NodeID: &at}

	finder := NewSourceFinder(db, nil, nil)
	order := &orders.Order{ID: 9, OrderType: OrderTypeRetrieveEmpty, SourceNode: "GROUP-A", DeliveryNode: "DEST"}

	res := finder.FindSource(order, IntentEmpty)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 701 {
		t.Fatalf("blank-origin empty pull changed shape: outcome=%v bin=%v", res.Outcome, res.Bin)
	}
	if db.typedGroupCalls != 0 {
		t.Errorf("the typed finder was consulted for an untyped ask: %d calls", db.typedGroupCalls)
	}
}

// A FAILED episode read returns no type rather than a guess. The ask falls
// through untyped — one carrier the keeper will not count, retried next tick —
// which is strictly better than confidently sourcing the wrong type.
func TestFindSource_MaintainedTypeReadFailureDoesNotGuess(t *testing.T) {
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 100, Name: "GROUP-A", IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP})
	db.addNode(&nodes.Node{ID: 99, Name: "DEST"})
	at := int64(101)
	db.addNode(&nodes.Node{ID: at, Name: "GROUP-A-LANE-1"})
	db.groupEmpty = &bins.Bin{ID: 801, PayloadCode: "", NodeID: &at}
	db.maintainedTypeErr = errors.New("episode read failed")

	finder := NewSourceFinder(db, nil, nil)
	order := &orders.Order{
		ID: 10, OrderType: OrderTypeRetrieveEmpty,
		SourceNode: "GROUP-A", DeliveryNode: "DEST",
		OriginID: "11111111-2222-3333-4444-555555555555",
	}

	res := finder.FindSource(order, IntentEmpty)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 801 {
		t.Fatalf("a failed episode read must fall through untyped, not fail the ask: outcome=%v bin=%v",
			res.Outcome, res.Bin)
	}
	if db.typedGroupCalls != 0 {
		t.Errorf("typed finder called despite an unreadable episode: %d calls", db.typedGroupCalls)
	}
}
