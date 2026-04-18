package binresolver

import (
	"errors"

	"shingocore/store"
)

// fakeStore is an in-memory Store used by the algorithm unit tests.
// It is intentionally dumb: every method reads a map and returns.
// Complex lane queries (FindSourceBinInLane, FindStoreSlotInLane,
// FindOldestBuriedBin, FindBuriedBin) are driven by per-lane fixtures
// set up by each test — the fake does not re-implement slot-ordering
// logic, it just replays what the real DB would return.
//
// Kept tiny on purpose. If a test needs a condition the fake does not
// model (e.g. a partial error from a specific method), add it inline
// in that test rather than growing this struct.
type fakeStore struct {
	// Basic lookup tables.
	nodes            map[int64]*store.Node
	children         map[int64][]*store.Node // parentID -> children
	bins             map[int64][]*store.Bin  // nodeID -> bins at node
	props            map[int64]map[string]string
	binCounts        map[int64]int    // nodeID -> CountBinsByNode value
	activeByDelivery map[string]int   // node name -> CountActiveOrdersByDeliveryNode
	laneSlots        map[int64][]*store.Node
	laneBinCounts    map[int64]int
	effPayloads      map[int64][]*store.Payload
	effBinTypes      map[int64][]*store.BinType

	// Lane query fixtures.
	//
	// Nil entries mean "lane is empty / no slot / no match" — the fake
	// returns a sentinel error so the caller's "err != nil, continue"
	// branch is exercised.
	sourceInLane map[int64]*store.Bin // laneID -> source bin
	storeSlot    map[int64]*store.Node
	oldestBuried map[int64]laneBuried
	buriedAny    map[int64]laneBuried
}

type laneBuried struct {
	bin  *store.Bin
	slot *store.Node
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		nodes:            map[int64]*store.Node{},
		children:         map[int64][]*store.Node{},
		bins:             map[int64][]*store.Bin{},
		props:            map[int64]map[string]string{},
		binCounts:        map[int64]int{},
		activeByDelivery: map[string]int{},
		laneSlots:        map[int64][]*store.Node{},
		laneBinCounts:    map[int64]int{},
		effPayloads:      map[int64][]*store.Payload{},
		effBinTypes:      map[int64][]*store.BinType{},
		sourceInLane:     map[int64]*store.Bin{},
		storeSlot:        map[int64]*store.Node{},
		oldestBuried:     map[int64]laneBuried{},
		buriedAny:        map[int64]laneBuried{},
	}
}

// setProp is a small convenience used by tests to initialize node
// properties (retrieve_algorithm, store_algorithm, etc.).
func (f *fakeStore) setProp(nodeID int64, key, value string) {
	if f.props[nodeID] == nil {
		f.props[nodeID] = map[string]string{}
	}
	f.props[nodeID][key] = value
}

// --- Store interface ---------------------------------------------------

func (f *fakeStore) ListChildNodes(parentID int64) ([]*store.Node, error) {
	return f.children[parentID], nil
}

func (f *fakeStore) GetNode(id int64) (*store.Node, error) {
	n, ok := f.nodes[id]
	if !ok {
		return nil, errors.New("node not found")
	}
	return n, nil
}

func (f *fakeStore) GetNodeProperty(nodeID int64, key string) string {
	return f.props[nodeID][key]
}

func (f *fakeStore) ListBinsByNode(nodeID int64) ([]*store.Bin, error) {
	return f.bins[nodeID], nil
}

func (f *fakeStore) CountBinsByNode(nodeID int64) (int, error) {
	// Prefer the explicit override; fall back to len(bins[nodeID]).
	if c, ok := f.binCounts[nodeID]; ok {
		return c, nil
	}
	return len(f.bins[nodeID]), nil
}

func (f *fakeStore) CountActiveOrdersByDeliveryNode(nodeName string) (int, error) {
	return f.activeByDelivery[nodeName], nil
}

func (f *fakeStore) ListLaneSlots(laneID int64) ([]*store.Node, error) {
	return f.laneSlots[laneID], nil
}

func (f *fakeStore) CountBinsInLane(laneID int64) (int, error) {
	return f.laneBinCounts[laneID], nil
}

func (f *fakeStore) FindSourceBinInLane(laneID int64, payloadCode string) (*store.Bin, error) {
	b := f.sourceInLane[laneID]
	if b == nil {
		return nil, errors.New("no source bin")
	}
	if payloadCode != "" && b.PayloadCode != payloadCode {
		return nil, errors.New("payload mismatch")
	}
	return b, nil
}

func (f *fakeStore) FindStoreSlotInLane(laneID int64) (*store.Node, error) {
	s := f.storeSlot[laneID]
	if s == nil {
		return nil, errors.New("lane full")
	}
	return s, nil
}

func (f *fakeStore) FindOldestBuriedBin(laneID int64, payloadCode string) (*store.Bin, *store.Node, error) {
	lb, ok := f.oldestBuried[laneID]
	if !ok || lb.bin == nil {
		return nil, nil, nil
	}
	if payloadCode != "" && lb.bin.PayloadCode != payloadCode {
		return nil, nil, nil
	}
	return lb.bin, lb.slot, nil
}

func (f *fakeStore) FindBuriedBin(laneID int64, payloadCode string) (*store.Bin, *store.Node, error) {
	lb, ok := f.buriedAny[laneID]
	if !ok || lb.bin == nil {
		return nil, nil, nil
	}
	if payloadCode != "" && lb.bin.PayloadCode != payloadCode {
		return nil, nil, nil
	}
	return lb.bin, lb.slot, nil
}

func (f *fakeStore) GetEffectivePayloads(nodeID int64) ([]*store.Payload, error) {
	return f.effPayloads[nodeID], nil
}

func (f *fakeStore) GetEffectiveBinTypes(nodeID int64) ([]*store.BinType, error) {
	return f.effBinTypes[nodeID], nil
}

// Compile-time check: *fakeStore satisfies Store.
var _ Store = (*fakeStore)(nil)
