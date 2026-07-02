package fulfillment

import (
	"errors"

	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// fakeStore is an in-memory Store used by the fulfillment scanner
// unit tests. It models only the surface the scanner actually exercises
// on the non-dispatch paths — i.e. the branches in tryFulfill that
// return false before reaching s.dispatcher.DispatchDirect.
//
// Mirrors the material package's fakeStore pattern: map-backed reads,
// call-log for mutations, per-method error toggles for the specific
// failure scenarios the tests want to provoke. If a new test needs
// behaviour the fake doesn't model, extend the struct rather than
// reaching past it.
type fakeStore struct {
	// Seed data.
	queued     []*orders.Order
	ordersByID map[int64]*orders.Order
	nodesByDot map[string]*nodes.Node
	nodesByID  map[int64]*nodes.Node
	emptyBin   *bins.Bin
	sourceBin  *bins.Bin
	inFlightAt map[string]int
	binsAtNode map[int64]int

	// Error toggles.
	errListQueued        error
	errGetOrder          error
	errCountInFlight     error
	errCountBinsByNode   error
	errFindEmptyBin      error
	errFindSourceBinFIFO error
	errClaimBin          error
	errGetNode           error

	// getNodeByDotNameFn, when non-nil, replaces the default map
	// lookup for GetNodeByDotName. Used by the DestNodeLookupFails
	// scenario to succeed on the first call (bin-occupancy check)
	// and fail on the second (final destination resolution).
	getNodeByDotNameFn func(name string) (*nodes.Node, error)

	// onListQueuedOrders, when non-nil, is invoked on every
	// ListQueuedOrders call before results are returned. Used by
	// the Trigger-during-scan coalescing test to set pending=true
	// while a scan is in flight.
	onListQueuedOrders func()

	// Recorded mutations — every test asserts on these.
	claimedBins        [][2]int64 // (binID, orderID)
	unclaimedOrderIDs  []int64
	binIDUpdates       [][2]int64 // (orderID, binID)
	sourceNodeUpdates  []sourceNodeUpdate
	statusUpdates      []statusUpdate
	failedAtomically   []failCall
	findEmptyPrefZones []findEmptyCall
	queueReasons       []queueReasonUpdate
	childrenByParent   map[int64][]*nodes.Node
}

type queueReasonUpdate struct {
	OrderID int64
	Reason  string
}

type sourceNodeUpdate struct {
	OrderID    int64
	SourceNode string
}

type statusUpdate struct {
	OrderID int64
	Status  string
	Detail  string
}

type failCall struct {
	OrderID int64
	Detail  string
}

type findEmptyCall struct {
	PayloadCode string
	PreferZone  string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		ordersByID: map[int64]*orders.Order{},
		nodesByDot: map[string]*nodes.Node{},
		nodesByID:  map[int64]*nodes.Node{},
		inFlightAt: map[string]int{},
		binsAtNode: map[int64]int{},
	}
}

// --- Store interface ---------------------------------------------

func (f *fakeStore) ListQueuedOrders() ([]*orders.Order, error) {
	if f.onListQueuedOrders != nil {
		f.onListQueuedOrders()
	}
	if f.errListQueued != nil {
		return nil, f.errListQueued
	}
	return f.queued, nil
}

func (f *fakeStore) GetOrder(id int64) (*orders.Order, error) {
	if f.errGetOrder != nil {
		return nil, f.errGetOrder
	}
	o, ok := f.ordersByID[id]
	if !ok {
		return nil, errors.New("order not found")
	}
	return o, nil
}

func (f *fakeStore) CountInFlightOrdersByDeliveryNode(deliveryNode string) (int, error) {
	if f.errCountInFlight != nil {
		return 0, f.errCountInFlight
	}
	return f.inFlightAt[deliveryNode], nil
}

func (f *fakeStore) CountInFlightOrdersByDeliveryNodeExcluding(deliveryNode string, excludeID int64) (int, error) {
	if f.errCountInFlight != nil {
		return 0, f.errCountInFlight
	}
	// Test fakes don't track per-order, so excludeID is informational
	// only — the same total count applies. Real *store.DB does the
	// SQL exclusion via WHERE id != $excludeID.
	return f.inFlightAt[deliveryNode], nil
}

func (f *fakeStore) ListChildNodes(parentID int64) ([]*nodes.Node, error) {
	return f.childrenByParent[parentID], nil
}

func (f *fakeStore) GetNode(id int64) (*nodes.Node, error) {
	if f.errGetNode != nil {
		return nil, f.errGetNode
	}
	n, ok := f.nodesByID[id]
	if !ok {
		return nil, errors.New("node not found")
	}
	return n, nil
}

func (f *fakeStore) GetNodeByDotName(name string) (*nodes.Node, error) {
	if f.getNodeByDotNameFn != nil {
		return f.getNodeByDotNameFn(name)
	}
	n, ok := f.nodesByDot[name]
	if !ok {
		return nil, errors.New("node not found")
	}
	return n, nil
}

func (f *fakeStore) CountBinsByNode(nodeID int64) (int, error) {
	if f.errCountBinsByNode != nil {
		return 0, f.errCountBinsByNode
	}
	return f.binsAtNode[nodeID], nil
}

func (f *fakeStore) FindEmptyCompatibleBin(payloadCode, preferZone string, excludeNodeID int64) (*bins.Bin, error) {
	f.findEmptyPrefZones = append(f.findEmptyPrefZones, findEmptyCall{
		PayloadCode: payloadCode,
		PreferZone:  preferZone,
	})
	if f.errFindEmptyBin != nil {
		return nil, f.errFindEmptyBin
	}
	return f.emptyBin, nil
}

func (f *fakeStore) FindSourceBinFIFO(payloadCode string, excludeNodeID int64) (*bins.Bin, error) {
	if f.errFindSourceBinFIFO != nil {
		return nil, f.errFindSourceBinFIFO
	}
	return f.sourceBin, nil
}

func (f *fakeStore) ClaimBin(binID, orderID int64) error {
	if f.errClaimBin != nil {
		return f.errClaimBin
	}
	f.claimedBins = append(f.claimedBins, [2]int64{binID, orderID})
	return nil
}

// ClaimForDispatch mirrors ClaimBin for the reserve-then-claim path the scanner
// now uses — records to the same claimedBins signal and honors errClaimBin, so
// existing claim/no-claim assertions and error-injection tests keep working. The
// fakeStore doubles as the fulfillment.Claimer in the test scanner constructors.
func (f *fakeStore) ClaimForDispatch(binID, orderID int64, _ *int) error {
	if f.errClaimBin != nil {
		return f.errClaimBin
	}
	f.claimedBins = append(f.claimedBins, [2]int64{binID, orderID})
	return nil
}

func (f *fakeStore) UnclaimOrderBins(orderID int64) {
	f.unclaimedOrderIDs = append(f.unclaimedOrderIDs, orderID)
}

// ReleaseClaimByOrder is the coupled rollback; record it under the same signal
// so rollback assertions on unclaimedOrderIDs keep working after the re-route.
func (f *fakeStore) ReleaseClaimByOrder(orderID int64) error {
	f.unclaimedOrderIDs = append(f.unclaimedOrderIDs, orderID)
	return nil
}

func (f *fakeStore) UpdateOrderBinID(orderID, binID int64) error {
	f.binIDUpdates = append(f.binIDUpdates, [2]int64{orderID, binID})
	return nil
}

func (f *fakeStore) UpdateOrderSourceNode(id int64, sourceNode string) error {
	f.sourceNodeUpdates = append(f.sourceNodeUpdates, sourceNodeUpdate{
		OrderID: id, SourceNode: sourceNode,
	})
	return nil
}

func (f *fakeStore) UpdateOrderStatus(id int64, status, detail string) error {
	f.statusUpdates = append(f.statusUpdates, statusUpdate{
		OrderID: id, Status: status, Detail: detail,
	})
	return nil
}

func (f *fakeStore) FailOrderAtomic(orderID int64, detail string) error {
	f.failedAtomically = append(f.failedAtomically, failCall{
		OrderID: orderID, Detail: detail,
	})
	return nil
}

func (f *fakeStore) SetOrderQueueReason(id int64, reason string) error {
	f.queueReasons = append(f.queueReasons, queueReasonUpdate{OrderID: id, Reason: reason})
	return nil
}
