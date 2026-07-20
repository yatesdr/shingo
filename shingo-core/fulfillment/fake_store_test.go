package fulfillment

import (
	"errors"

	"shingo/protocol"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// fakeStore is an in-memory Store used by the fulfillment scanner unit tests. It
// models only the surface the scanner exercises before dispatch. Source finding
// is NOT modeled here — it moved to the shared dispatch.SourceFinder, which the
// scanner holds behind the BinFinder interface (tests inject a fakeFinder).
//
// Mirrors the material package's fakeStore pattern: map-backed reads, a call-log
// for mutations, per-method error toggles. If a new test needs behaviour the fake
// doesn't model, extend the struct rather than reaching past it.
type fakeStore struct {
	// Seed data.
	queued     []*orders.Order
	ordersByID map[int64]*orders.Order
	nodesByDot map[string]*nodes.Node
	inFlightAt map[string]int
	binsAtNode map[int64]int

	// Error toggles.
	errListQueued      error
	errGetOrder        error
	errCountInFlight   error
	errCountBinsByNode error
	errClaimBin        error
	errReserveBin      error // soft-acquire (ReserveForDispatch) failure
	errConfirmBin      error // confirm-at-dispatch (ConfirmClaim) failure

	// getNodeByDotNameFn, when non-nil, replaces the default map lookup for
	// GetNodeByDotName — lets a test make specific names resolve or fail.
	getNodeByDotNameFn func(name string) (*nodes.Node, error)

	// onListQueuedOrders, when non-nil, is invoked on every ListAcquiringOrders
	// call before results are returned. Used by the Trigger-during-scan
	// coalescing test to set pending=true while a scan is in flight.
	onListQueuedOrders func()

	// Recorded mutations — every test asserts on these.
	claimedBins          [][2]int64 // (binID, orderID) — hard claims via ClaimForDispatch
	reservedBins         [][2]int64 // (binID, orderID) — soft pending reservations (ReserveForDispatch)
	confirmedBins        [][2]int64 // (binID, orderID) — confirm-at-dispatch (ConfirmClaim)
	confirmedSlots       [][2]int64 // (nodeID, orderID) — confirm-at-dispatch slot claim (ConfirmSlotClaim)
	releasedReservations []int64    // orderIDs whose soft bin reservation was released
	unclaimedOrderIDs    []int64
	binIDUpdates         [][2]int64 // (orderID, binID)
	sourceNodeUpdates    []sourceNodeUpdate
	statusUpdates        []statusUpdate
	queueReasons         []queueReasonUpdate
	capacityExcludeIDs   []int64 // excludeID passed to each capacity-gate in-flight count
	childrenByParent     map[int64][]*nodes.Node
}

type queueReasonUpdate struct {
	OrderID int64
	Reason  string
	Code    string
	Cause   string
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

func newFakeStore() *fakeStore {
	return &fakeStore{
		ordersByID: map[int64]*orders.Order{},
		nodesByDot: map[string]*nodes.Node{},
		inFlightAt: map[string]int{},
		binsAtNode: map[int64]int{},
	}
}

// --- Store interface ---------------------------------------------

func (f *fakeStore) ListAcquiringOrders() ([]*orders.Order, error) {
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

func (f *fakeStore) CountInFlightOrdersByDeliveryNodeExcluding(deliveryNode string, excludeID int64) (int, error) {
	// Record the excludeID so A7 tests can assert the caller self-excludes
	// (passes order.ID, not 0). The fake doesn't model per-order in-flight, so
	// the count itself ignores excludeID — real *store.DB does the SQL exclusion
	// via WHERE id != $excludeID.
	f.capacityExcludeIDs = append(f.capacityExcludeIDs, excludeID)
	if f.errCountInFlight != nil {
		return 0, f.errCountInFlight
	}
	return f.inFlightAt[deliveryNode], nil
}

func (f *fakeStore) ListChildNodes(parentID int64) ([]*nodes.Node, error) {
	return f.childrenByParent[parentID], nil
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

// ClaimForDispatch is the legacy reserve-then-claim path. The scanner plain path
// no longer uses it (it soft-reserves then confirms at dispatch), but the synchronous
// direct/spot order paths still do during the transition, so the fake keeps modeling
// it — records to claimedBins and honors errClaimBin.
func (f *fakeStore) ClaimForDispatch(binID, orderID int64, _ *int) error {
	if f.errClaimBin != nil {
		return f.errClaimBin
	}
	f.claimedBins = append(f.claimedBins, [2]int64{binID, orderID})
	return nil
}

// ReserveForDispatch is the soft-acquire path — places a pending reservation
// (no hard claimed_by). The scanner plain path calls this once it has found a bin
// and secured the slot, BEFORE dispatch. Records to reservedBins and honors
// errReserveBin (inject ErrReservationConflict to simulate a lost race).
func (f *fakeStore) ReserveForDispatch(binID, orderID int64) error {
	if f.errReserveBin != nil {
		return f.errReserveBin
	}
	f.reservedBins = append(f.reservedBins, [2]int64{binID, orderID})
	return nil
}

// ConfirmClaim is the confirm-at-dispatch hard claim — flips the pending
// reservation to confirmed and records the bin as hard-claimed. Called by the
// scanner (and the direct/spot paths) immediately before the fleet dispatch.
// Records to confirmedBins and honors errConfirmBin (inject to simulate a reaped
// pending reservation / claim_failed).
func (f *fakeStore) ConfirmClaim(binID, orderID int64, _ *int) error {
	if f.errConfirmBin != nil {
		return f.errConfirmBin
	}
	f.confirmedBins = append(f.confirmedBins, [2]int64{binID, orderID})
	return nil
}

// ConfirmSlotClaim is the slot dual of ConfirmClaim — hard-claims the destination
// slot at dispatch. Only called for a concrete storage dropoff. Records to
// confirmedSlots.
func (f *fakeStore) ConfirmSlotClaim(nodeID, orderID int64) error {
	f.confirmedSlots = append(f.confirmedSlots, [2]int64{nodeID, orderID})
	return nil
}

// ReleaseReservation releases the order's soft pending bin reservation — the
// rollback path for a soft-acquired bin when the order requeues before dispatch
// (no hard claim exists yet to clear).
func (f *fakeStore) ReleaseReservation(orderID, _ int64) error {
	f.releasedReservations = append(f.releasedReservations, orderID)
	return nil
}

// ReleaseClaimByOrder is the coupled rollback; record it under the unclaim signal
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

// UpdateOrderStatus is not a Store method (the scanner transitions via
// Lifecycle); it's kept for the stubLifecycle test shim, which writes
// transitions through to statusUpdates.
func (f *fakeStore) UpdateOrderStatus(id int64, status, detail string) error {
	f.statusUpdates = append(f.statusUpdates, statusUpdate{
		OrderID: id, Status: status, Detail: detail,
	})
	return nil
}

func (f *fakeStore) SetOrderQueueDetail(id int64, reason string, code protocol.QueueCode, cause string) error {
	f.queueReasons = append(f.queueReasons, queueReasonUpdate{OrderID: id, Reason: reason, Code: string(code), Cause: cause})
	return nil
}
