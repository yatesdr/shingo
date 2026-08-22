package store

// Stage 2D delegate file: order CRUD and history live in store/orders/.
// Cross-aggregate methods (CreateCompoundChildren, FailOrderAtomic,
// CancelOrderAtomic) stay here because they mutate both the orders and
// bins tables in a single transaction.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

func (db *DB) CreateOrder(o *orders.Order) error { return orders.Create(db.DB, o) }

// CompoundChild describes a child order to create in a compound order
// transaction. Declared here (not in the orders sub-package) because
// CreateCompoundChildren is cross-aggregate.
type CompoundChild struct {
	Order *orders.Order
	BinID int64 // bin to claim for this child
}

// ErrBlockerClaimed is the sentinel behind BlockerClaimedError, for callers that
// only need to know WHICH refusal this was.
var ErrBlockerClaimed = errors.New("bin held by an order outside the compound")

// BlockerClaimedError is the claim CAS refusing a leg because the bin it needs
// is held by an order OUTSIDE this compound.
//
// It is typed because its disposition differs from every other error
// CreateCompoundChildren can return. The others are faults — a failed write, a
// broken row. This one is CONGESTION: the holder is an ordinary live order that
// will finish and drop the claim, and the commonest holder by a wide margin is a
// dispatched retrieve whose robot is at that moment driving the blocker out of
// the lane. The blocker is in the process of ceasing to be a blocker.
//
// Terminally failing the digger for it violates wait-not-fail on traffic as
// ordinary as two demands on one lane minutes apart, so the dispatch callers map
// this to a transient planning error and park — see
// dispatch/planning_service.go planBuriedReshuffle.
//
// HolderID is 0 when the holding order could not be read back; the refusal
// stands either way, only the operator-facing detail is poorer.
type BlockerClaimedError struct {
	BinID    int64 // the bin the leg wanted
	ChildID  int64 // the leg that wanted it
	ParentID int64 // the compound the leg belongs to
	HolderID int64 // the order actually holding the claim, or 0 if unreadable
}

func (e *BlockerClaimedError) Error() string {
	if e.HolderID != 0 {
		return fmt.Sprintf("claim bin %d for child %d: held by order %d, outside compound %d",
			e.BinID, e.ChildID, e.HolderID, e.ParentID)
	}
	return fmt.Sprintf("claim bin %d for child %d: held by an order outside compound %d",
		e.BinID, e.ChildID, e.ParentID)
}

func (e *BlockerClaimedError) Unwrap() error { return ErrBlockerClaimed }

// CreateCompoundChildren creates all child orders and claims their payloads
// in a single transaction. Cross-aggregate (orders ↔ bins).
func (db *DB) CreateCompoundChildren(children []CompoundChild) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	for _, c := range children {
		o := c.Order
		// One writer for the orders table. orders.Create runs inside this
		// transaction because it takes a QueryRower rather than a *sql.DB, so
		// children carry the same columns as every other order and cannot drift
		// from them. It also owns the clock.Now() timestamps and the o.ID
		// write-back that the code below depends on.
		if err := orders.Create(tx, o); err != nil {
			return fmt.Errorf("create child order (seq %d): %w", o.Sequence, err)
		}

		// Bin-centric claiming: if the child order has a bin, claim it.
		//
		// SIBLING-SCOPED COMPARE-AND-SET. This used to be an unconditional
		// overwrite, which meant a compound could take a bin out from under an
		// unrelated order that was already carrying it — silently, because the
		// UPDATE reported success either way.
		//
		// The predicate is deliberately NOT the one bins.Claim uses
		// (`claimed_by IS NULL OR claimed_by = $1`). A multi-step reshuffle plan
		// INTENTIONALLY overlaps bin claims: a bin that appears in several steps —
		// an unbury followed by a retrieve of the same bin — is claimed once per
		// step, and the last step's write is the one that stands. That is relied on
		// downstream (engine/wiring_completion.go skips the delivery-arrival
		// teleport guard for children precisely because of it), so refusing a
		// repeat claim would be a behaviour change, not a fence.
		//
		// THE PARENT IS IN THE SET, and that is the part worth getting right. Both
		// the plain and target-node planners emit a `retrieve` step carrying the
		// buried TARGET bin (dispatch/reshuffle.go), which is the same bin the
		// parent retrieve exists to fetch; and the documented multi-burial loop
		// re-plans a fresh compound after the parent has resumed and been
		// dispatched, by which point the parent can be holding that claim. Excluding
		// it would fail a claim that works today, which is the one direction a fence
		// must not fail in.
		//
		// The child's own id needs no arm: it was inserted in this transaction a few
		// lines above, so the sibling subquery already sees it and a re-claim by the
		// same child is idempotent.
		if o.BinID != nil {
			parentID := int64(0)
			if o.ParentOrderID != nil {
				parentID = *o.ParentOrderID
			}
			// THE STEAL, MADE EXPLICIT. A blocker is positional: the dig has no
			// choice about which bins are in its way, so it always wins, and a soft
			// reservation never stops it — the claim CAS below admits any bin whose
			// claimed_by is NULL, including one another order has softly promised
			// itself. That has always happened. What has not happened is the
			// bookkeeping.
			//
			// The holder's row used to survive the steal and get deleted much later,
			// at the dig leg's ARRIVAL (ReleaseByBin in ApplyArrival), which left a
			// live reservation pointing at a bin somebody else was carrying away for
			// the whole excavation. It worked by accident. Releasing it HERE — in
			// the same transaction that makes the steal a fact — is what makes the
			// books honest, and it is what lets the dig take a ledger row of its own
			// (below) without fighting uq_reservations_bin_active.
			//
			// THE HOLDER'S bin_id GOES WITH IT, and that is not incidental. bin_id is
			// stamped at SOFT-reserve time (fulfillment/scanner.go), so an order that
			// merely reserved a bin re-enters through dispatchHeldBin, which confirms
			// by id and never re-acquires — ConfirmClaim's seatbelt requires a pending
			// reservation. Releasing the row and leaving bin_id would wedge the holder
			// on claim_failed forever, which is the opposite of recalculating. Cleared
			// together, the holder re-enters through the finder and re-resolves: it
			// finds its bin at the shuffle slot the dig parked it in, or a better one.
			if err := stealSoftHold(tx, *o.BinID, o.ID, parentID); err != nil {
				return err
			}
			res, err := tx.Exec(`UPDATE bins SET claimed_by=$1
				WHERE id=$2
				  AND (claimed_by IS NULL
				       OR claimed_by = $3
				       OR claimed_by IN (SELECT id FROM orders WHERE parent_order_id = $3))`,
				o.ID, *o.BinID, parentID)
			if err != nil {
				return fmt.Errorf("claim bin %d for child %d: %w", *o.BinID, o.ID, err)
			}
			if n, rErr := res.RowsAffected(); rErr == nil && n == 0 {
				// Refused, not failed-to-write. The bin is held by an order outside
				// this compound, so the plan was built against a lane that has since
				// moved. Failing the whole transaction is right: a compound missing
				// one leg's bin is a reshuffle that strands mid-dig, and the callers
				// drop the lane lock on any error out of here.
				//
				// TYPED, because what the caller does next is not what it does with
				// the other errors: the holder is a live order, not a broken row, so
				// the digger waits for it rather than dying. Read the holder back so
				// the wait can say whose bin it is waiting on — the SELECT is safe
				// inside this transaction because nothing has errored, the UPDATE
				// simply matched no row.
				var holder sql.NullInt64
				if qErr := tx.QueryRow(`SELECT claimed_by FROM bins WHERE id=$1`, *o.BinID).Scan(&holder); qErr != nil {
					holder = sql.NullInt64{}
				}
				return &BlockerClaimedError{
					BinID:    *o.BinID,
					ChildID:  o.ID,
					ParentID: parentID,
					HolderID: holder.Int64,
				}
			}

			// THE LEDGER ROW — hold class 3 closed. A dig's claim was STAMPED with
			// no reservation behind it: a claimed_by pointing at nothing in the
			// books, the one-fact-two-mechanisms shape this codebase keeps finding.
			// It stayed open because writing an honest row needs an answer to "whose
			// entry wins when a dig and a holder both have books on one bin", and
			// the answer is the steal above: the dig's entry supersedes, at the
			// moment the claim lands.
			//
			// DEDUPE BY BIN, because a plan legitimately touches one bin twice — an
			// unbury followed by a retrieve of the same bin — and the index allows
			// one active row per bin. The delete-then-insert makes the row follow
			// the claim: last write wins for both, together, which is exactly the
			// property wiring_completion.go's teleport-guard skip already relies on
			// for claimed_by.
			if err := supersedeBinLedger(tx, *o.BinID, o.ID); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

// stealSoftHold releases a FOREIGN soft hold on a bin a dig is about to claim,
// and clears the holder's pointer to it, so the holder recalculates instead of
// following a plan that is no longer true.
//
// Foreign means: not this compound's. A row belonging to the parent or to a
// sibling is the same demand holding its own bin across steps, and
// supersedeBinLedger moves it rather than reporting a theft.
//
// THE LOG LINE IS THE POINT of the third return. A steal that leaves no trace is
// how this behaviour survived unexamined for so long: the holder limped after its
// bin by id, it usually worked, and nothing anywhere said a dig had taken
// somebody's bin. Now it says so, by name, once, at the moment it happens.
func stealSoftHold(tx *sql.Tx, binID, childID, parentID int64) error {
	var holder sql.NullInt64
	err := tx.QueryRow(`SELECT order_id FROM reservations
		WHERE bin_id=$1 AND resource_kind='bin' AND state IN ('pending','confirmed')
		  AND order_id <> $2
		  AND order_id <> $3
		  AND order_id NOT IN (SELECT id FROM orders WHERE parent_order_id = $3)`,
		binID, childID, parentID).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // nobody else's book on this bin — nothing to supersede
	}
	if err != nil {
		return fmt.Errorf("read soft hold on bin %d: %w", binID, err)
	}

	// THE ROW ITSELF IS NOT DELETED HERE. supersedeBinLedger, three statements
	// later in this same transaction, is the one writer that clears this bin's
	// reservations — deleting it here too would be two writers for one fact, which
	// is the shape this codebase keeps having to unpick. Either both land or the
	// transaction rolls back, so there is no window where the holder's pointer is
	// cleared and its row is not.
	//
	// Scoped to a holder still pointing AT THIS BIN: an order that has already
	// moved on to another bin must keep the one it moved to.
	//
	// updated_at IS STAMPED FROM THE INJECTED CLOCK, like every other writer of
	// this column. It was `NOW()` — the one Postgres-clock writer among ~20
	// Go-clock ones, so the rows a dig took a bin from carried a foreign stamp on
	// a column whose readers all assume the other domain. Under the rig's clamp
	// the two agree and nothing showed; the moment the sim clock genuinely runs
	// ahead they do not, and this row's staleness reads as fresh forever. Two
	// readers now care: ListAnomalies' runtime-stuck detector, and the stale-dig
	// disposition's liveness test.
	if _, err := tx.Exec(
		`UPDATE orders SET bin_id=NULL, updated_at=$3 WHERE id=$1 AND bin_id=$2`,
		holder.Int64, binID, clock.Now().UTC()); err != nil {
		return fmt.Errorf("clear bin %d off holder %d: %w", binID, holder.Int64, err)
	}

	log.Printf("dispatch: dig %d took bin %d from order %d — the dig always wins on a positional "+
		"blocker; order %d keeps its demand and re-resolves (the bin is findable at its new home)",
		parentID, binID, holder.Int64, holder.Int64)
	return nil
}

// supersedeBinLedger makes the reservation books say what the claim says: one
// active row on this bin, owned by the child that now holds it.
//
// Delete-then-insert rather than an upsert because the row may currently belong
// to a sibling or to the parent, and the target of uq_reservations_bin_active is
// the BIN — there is no (order, bin) row to update into place. Written CONFIRMED
// because the claim it records is already a hard claim, not a plan: a pending row
// would say the dig is still deciding.
func supersedeBinLedger(tx *sql.Tx, binID, childID int64) error {
	if _, err := tx.Exec(
		`DELETE FROM reservations WHERE bin_id=$1 AND resource_kind='bin'`, binID); err != nil {
		return fmt.Errorf("clear bin ledger for bin %d: %w", binID, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO reservations (order_id, resource_kind, bin_id, state, reserved_by)
		 VALUES ($1, 'bin', $2, 'confirmed', 'compound-child')`,
		childID, binID); err != nil {
		return fmt.Errorf("write bin ledger for bin %d child %d: %w", binID, childID, err)
	}
	return nil
}

// ListChildOrders returns all child orders for a parent order.
func (db *DB) ListChildOrders(parentOrderID int64) ([]*orders.Order, error) {
	return orders.ListChildren(db.DB, parentOrderID)
}

// RetireReshuffleRestoreOrders cancels any non-terminal reshuffle_restore
// housekeeping orders (and their non-terminal children) left over from the
// retired restore-blockers subsystem. One-shot and idempotent: a clean DB — or a
// second run — finds none and returns 0. Cancellation goes through the terminal
// chokepoint (TerminalizeOrder) so any holds are released the same way a normal
// cancel releases them; a raw status write would trip the state-machine guard.
// Returns the number of orders cancelled. Called once at boot.
func (db *DB) RetireReshuffleRestoreOrders() (int, error) {
	rows, err := db.Query(fmt.Sprintf(
		`SELECT id FROM orders WHERE order_type=$1 AND status IN (%s) ORDER BY id`,
		protocol.NonTerminalStatusSQLList()), string(protocol.OrderTypeReshuffleRestore))
	if err != nil {
		return 0, fmt.Errorf("retire reshuffle_restore: list: %w", err)
	}
	var parents []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("retire reshuffle_restore: scan: %w", err)
		}
		parents = append(parents, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	const detail = "retired: restore-blockers subsystem removed"
	cancelled := 0
	for _, pid := range parents {
		children, cErr := db.ListChildOrders(pid)
		if cErr != nil {
			return cancelled, fmt.Errorf("retire reshuffle_restore: children of %d: %w", pid, cErr)
		}
		for _, c := range children {
			if protocol.IsTerminal(c.Status) {
				continue
			}
			// TerminalizeOrder is a compare-and-swap on "still live" and reports
			// whether THIS call is the one that landed it. The IsTerminal check
			// above reads a snapshot, so an order can go terminal underneath us —
			// a concurrent operator cancel is the everyday case. A declined swap
			// is not an error and is not our cancellation, so it is not counted:
			// the returned tally means "orders this sweep terminated", which is
			// what makes the second run report 0.
			swapped, tErr := db.TerminalizeOrder(c.ID, protocol.StatusCancelled, detail)
			if tErr != nil {
				return cancelled, fmt.Errorf("retire reshuffle_restore: cancel child %d: %w", c.ID, tErr)
			}
			if swapped {
				cancelled++
			}
		}
		swapped, tErr := db.TerminalizeOrder(pid, protocol.StatusCancelled, detail)
		if tErr != nil {
			return cancelled, fmt.Errorf("retire reshuffle_restore: cancel parent %d: %w", pid, tErr)
		}
		if swapped {
			cancelled++
		}
	}
	return cancelled, nil
}

// GetNextChildOrder returns the next pending child order for a parent.
func (db *DB) GetNextChildOrder(parentOrderID int64) (*orders.Order, error) {
	return orders.GetNextChild(db.DB, parentOrderID)
}

// SetCompoundOpen is the one writer of a compound parent's sealedness — see
// orders.SetCompoundOpen. Sealed is !open; nothing derives it.
func (db *DB) SetCompoundOpen(parentOrderID int64, open bool) error {
	return orders.SetCompoundOpen(db.DB, parentOrderID, open)
}

func (db *DB) UpdateOrderStatus(id int64, status, detail string) error {
	return orders.UpdateStatus(db.DB, id, status, detail)
}

// UpdateOrderStatusFromWithReason is the CAS status write plus the typed
// reason for the history row (migration 55). The →queued row is the important
// caller: orders.queue_code is overwritten in place, so the history row is the
// only place a queue reason becomes a time series.
func (db *DB) UpdateOrderStatusFromWithReason(id int64, from, to, detail string, reason HistoryReason) (bool, error) {
	return orders.UpdateStatusFromWithReason(db.DB, id, from, to, detail, reason.Code, reason.Actor, reason.refJSON())
}

// UpdateOrderWaitIndex increments the wait_index for a complex order after
// releasing one wait segment.
func (db *DB) UpdateOrderWaitIndex(id int64, waitIndex int) error {
	return orders.UpdateWaitIndex(db.DB, id, waitIndex)
}

// SetOrderQueueDetail stores the blocking reason on a queued order — the
// generated sentence, its structured queue code, and the engineer-only cause —
// in one write. code is typed (protocol.QueueCode) so a caller cannot pass free
// text: the dispatch formatter generates the sentence and is the sole caller.
// Pass "" / empty code to clear (on successful dispatch).
func (db *DB) SetOrderQueueDetail(id int64, reason string, code protocol.QueueCode, cause string) error {
	return orders.SetQueueDetail(db.DB, id, reason, string(code), cause)
}

// LinkOrderSiblingsByEdgeUUID records a two-robot swap pairing keyed on
// edge UUID — see orders.LinkSiblingsByEdgeUUID. Returns rows updated.
func (db *DB) LinkOrderSiblingsByEdgeUUID(uuidA, uuidB string) (int64, error) {
	return orders.LinkSiblingsByEdgeUUID(db.DB, uuidA, uuidB)
}

// OrderSiblingUUID returns the order's two-robot swap sibling edge UUID, or "".
func (db *DB) OrderSiblingUUID(id int64) (string, error) {
	return orders.SiblingUUID(db.DB, id)
}

func (db *DB) UpdateOrderVendor(id int64, vendorOrderID, vendorState, robotID string) error {
	return orders.UpdateVendor(db.DB, id, vendorOrderID, vendorState, robotID)
}

func (db *DB) UpdateOrderSourceNode(id int64, sourceNode string) error {
	return orders.UpdateSourceNode(db.DB, id, sourceNode)
}

func (db *DB) UpdateOrderDeliveryNode(id int64, deliveryNode string) error {
	return orders.UpdateDeliveryNode(db.DB, id, deliveryNode)
}

func (db *DB) UpdateOrderRemainingUOP(id int64, remainingUOP *int) error {
	return orders.UpdateRemainingUOP(db.DB, id, remainingUOP)
}

func (db *DB) UpdateOrderStepsJSON(id int64, stepsJSON string) error {
	return orders.UpdateStepsJSON(db.DB, id, stepsJSON)
}

func (db *DB) CompleteOrder(id int64) error             { return orders.Complete(db.DB, id) }
func (db *DB) GetOrder(id int64) (*orders.Order, error) { return orders.Get(db.DB, id) }
func (db *DB) GetOrderByUUID(uuid string) (*orders.Order, error) {
	return orders.GetByUUID(db.DB, uuid)
}
func (db *DB) GetOrderByVendorID(vendorOrderID string) (*orders.Order, error) {
	return orders.GetByVendorID(db.DB, vendorOrderID)
}

func (db *DB) ListOrders(status string, limit int) ([]*orders.Order, error) {
	return orders.List(db.DB, status, limit)
}

func (db *DB) ListActiveOrders() ([]*orders.Order, error) { return orders.ListActive(db.DB) }

// ListActiveBoardOrdersFiltered scopes the board to a set of station IDs.
// Empty stations = plant-wide (same as ListActiveBoardOrders).
func (db *DB) ListActiveBoardOrdersFiltered(stations []string) ([]*orders.Order, error) {
	return orders.ListActiveBoardFiltered(db.DB, stations)
}

// ListOrderStations returns the distinct station IDs seen on orders.
func (db *DB) ListOrderStations() ([]string, error) {
	return orders.ListDistinctStations(db.DB)
}

func (db *DB) ListOrderHistory(orderID int64) ([]*orders.History, error) {
	return orders.ListHistory(db.DB, orderID)
}

// GetFaultStats computes the /missions Faults card — see orders.GetFaultStats.
func (db *DB) GetFaultStats(r orders.LeadTimeRange, noticeAfter time.Duration) (*orders.FaultStats, error) {
	return orders.GetFaultStats(db.DB, r, noticeAfter)
}

// LatestOrderHistoryForStatus returns the order's most recent row for a status,
// or nil if it never recorded one — see orders.LatestHistoryForStatus.
func (db *DB) LatestOrderHistoryForStatus(orderID int64, status protocol.Status) (*orders.History, error) {
	return orders.LatestHistoryForStatus(db.DB, orderID, status)
}

// LatestOrderHistoryTimesForStatus is the batch form: order id -> when each of
// those orders most recently reached the status, in one round trip. See
// orders.LatestHistoryTimesForStatus.
func (db *DB) LatestOrderHistoryTimesForStatus(orderIDs []int64, status protocol.Status) (map[int64]time.Time, error) {
	return orders.LatestHistoryTimesForStatus(db.DB, orderIDs, status)
}

// OrderEverReachedStatus reports whether the order ever recorded the status —
// see orders.EverReachedStatus.
func (db *DB) OrderEverReachedStatus(orderID int64, status string) (bool, error) {
	return orders.EverReachedStatus(db.DB, orderID, status)
}

func (db *DB) UpdateOrderPriority(id int64, priority int) error {
	return orders.UpdatePriority(db.DB, id, priority)
}

func (db *DB) ListOrdersByStation(stationID string, limit int) ([]*orders.Order, error) {
	return orders.ListByStation(db.DB, stationID, limit)
}

// ListActiveOrdersByStation returns the non-terminal orders for one station —
// the set the order reconcile compares an Edge's own list against.
func (db *DB) ListActiveOrdersByStation(stationID string) ([]*orders.Order, error) {
	return orders.ListActiveByStation(db.DB, stationID)
}

// CountActiveOrdersByDeliveryNode counts non-terminal orders targeting a
// specific delivery node.
func (db *DB) CountActiveOrdersByDeliveryNode(nodeName string) (int, error) {
	return orders.CountActiveByDeliveryNode(db.DB, nodeName)
}

// ActiveLaneStores returns non-terminal orders whose delivery_node is one of the
// given slot names — the active stores targeting a lane (tiered-entry gate input).
func (db *DB) ActiveLaneStores(slotNames []string) ([]*orders.Order, error) {
	return orders.ActiveByDeliveryNodes(db.DB, slotNames)
}

// ActiveGateCandidates returns non-terminal orders carrying a plan and a vendor
// order — the set the lane-gate evaluator filters down to its candidates by
// reading the wait step. See orders.ActiveGateCandidates for why the lane is not
// part of the query.
//
// It replaced ActiveLaneRetrieves (and the evaluator's use of ActiveLaneStores),
// which found candidates by matching an ENDPOINT column against lane slot names
// and therefore could not see an order whose lane entry is interior to its plan.
func (db *DB) ActiveGateCandidates() ([]*orders.Order, error) {
	return orders.ActiveGateCandidates(db.DB)
}

// CountActiveOrders returns the number of non-terminal orders (dashboard
// "in flight" KPI).
func (db *DB) CountActiveOrders() (int, error) {
	return orders.CountActive(db.DB)
}

// ListStalledChapters returns reshuffling parents with an open leg whose whole
// family has been quiet since the cutoff — see orders.ListStalledChapters.
func (db *DB) ListStalledChapters(since time.Time, limit int) ([]int64, error) {
	return orders.ListStalledChapters(db.DB, since, limit)
}

// ListTrackedVendorOrderIDs returns the vendor order IDs Core must keep watching.
// (The old comment here said "all non-terminal orders", which it never was.)
func (db *DB) ListTrackedVendorOrderIDs() ([]string, error) {
	return orders.ListTrackedVendorOrderIDs(db.DB)
}

// ListActiveOrdersBySourceRef returns orders in pre-dispatch states (pending,
// sourcing, queued) whose source_node matches any of the provided names.
func (db *DB) ListActiveOrdersBySourceRef(names []string) ([]*orders.Order, error) {
	return orders.ListActiveBySourceRef(db.DB, names)
}

// ListAcquiringOrders returns all orders in an acquiring status (queued or
// sourcing) — the fulfillment scanner's retry set — priority then FIFO.
func (db *DB) ListAcquiringOrders() ([]*orders.Order, error) { return orders.ListAcquiring(db.DB) }

// UpdateOrderPayloadCode sets the payload_code on an order.
func (db *DB) UpdateOrderPayloadCode(orderID int64, payloadCode string) error {
	return orders.UpdatePayloadCode(db.DB, orderID, payloadCode)
}

// UpdateOrderBinID sets the bin_id on an order. Kept as a delegate even
// though the function lives in orders/ because every outer caller expects
// this name.
func (db *DB) UpdateOrderBinID(orderID, binID int64) error {
	return orders.UpdateBinID(db.DB, orderID, binID)
}

// ListOrdersByBin returns recent orders involving a specific bin.
// Cross-aggregate entry point: the query lives in orders/ (returns *orders.Order)
// but callers reach it via the bins-side delegate name.
func (db *DB) ListOrdersByBin(binID int64, limit int) ([]*orders.Order, error) {
	return orders.ListByBinID(db.DB, binID, limit)
}

// ActiveOrdersByBin returns non-terminal orders referencing a bin via bin_id.
// Cross-aggregate delegate for BinService's orphan-order guard on retire/move.
func (db *DB) ActiveOrdersByBin(binID int64) ([]*orders.Order, error) {
	return orders.ActiveByBinID(db.DB, binID)
}

// TerminalizeOrder transitions an order to a terminal status and releases ALL of
// its holds — bin claims, destination-slot claims, order_bins junction rows, and
// reservations (pending and confirmed) — in a single transaction. It is the one
// chokepoint that makes "reaching a terminal status releases everything" a
// structural invariant rather than several divergent write paths; transition()
// routes every IsTerminal target here (including the success terminal
// 'confirmed', whose reservation previously leaked through UpdateOrderStatus and
// bricked the bin via the uq_reservations_bin_active partial unique index).
//
// Any bin still claimed by this order and parked at _TRANSIT when the order
// terminalizes never arrived anywhere, so it is stamped anomalous (anomaly_at,
// the signal operator recovery picks up via ListAnomalousTransitBins) regardless
// of which terminal we reached. In the happy path this matches ZERO rows: a
// delivered bin was moved out of _TRANSIT and unclaimed at delivery time. It
// fires only when an arrival failed or was skipped — including the confirmed
// case where the operator confirmed receipt but the engine's delivery-arrival
// write never landed (the completion safety-net can't recover it because this
// chokepoint has, correctly, already cleared claimed_by). error_detail is
// persisted for every terminal except the clean success 'confirmed' (which would
// otherwise surface receipt text as an "error"); order_history keeps the full
// detail regardless. Cross-aggregate.
// HistoryReason is the typed reason recorded on an order_history row —
// order_history.code / actor / ref (migration 55).
//
// Zero value means "uncoded", which is correct for a transition with no
// category: most of them. Never invent a code to fill it.
type HistoryReason struct {
	Code  string           // protocol.TermCode on a terminal, protocol.QueueCode on a →queued row
	Actor string           // who caused it
	Ref   protocol.TermRef // what it concerns: node / payload / peer
}

// refJSON renders Ref for the JSONB column, or nil when it carries nothing —
// so an empty reference is SQL NULL rather than the string "{}", and the
// partial indexes stay small.
func (r HistoryReason) refJSON() any {
	if r.Ref.Empty() {
		return nil
	}
	b, err := json.Marshal(r.Ref)
	if err != nil {
		return nil
	}
	return string(b)
}

func (db *DB) TerminalizeOrder(orderID int64, status protocol.Status, detail string) (bool, error) {
	return db.TerminalizeOrderWithReason(orderID, status, detail, HistoryReason{})
}

// TerminalizeOrderWithReason is TerminalizeOrder plus the typed reason for the
// history row. Event.ErrorCode and Event.Actor were being set at every Fail
// and Skip call site and dropped in transition(), which passed only the prose
// detail down here — so the categories existed in memory and never reached
// disk.
func (db *DB) TerminalizeOrderWithReason(orderID int64, status protocol.Status, detail string, reason HistoryReason) (bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// error_detail is suppressed only for the clean success 'confirmed' (which
	// would otherwise surface receipt text as an "error").
	errDetail := detail
	if status == protocol.StatusConfirmed {
		errDetail = ""
	}
	// COMPARE-AND-SWAP on "still live". Two callers can each hold a snapshot
	// showing a non-terminal order and both pass lifecycle's guard — an operator
	// cancel racing a scanner fail is the everyday pair. Unguarded, the second
	// write flipped an already-terminal order to a different terminal and fired a
	// second actionMap entry (fireCancelled AND fireFailed for one order).
	//
	// The predicate is "current status is not terminal" rather than
	// "current status = the caller's from". A cancel is operator intent that must
	// land from ANY live state, and an order legitimately moves non-terminally
	// (queued→sourcing) between snapshot and cancel; keying on `from` would
	// silently refuse that cancel, and CancelOrder returns no error to notice it
	// with. Terminal-absorbs-terminal is the property we actually want.
	//
	// The set is derived from the transition table (protocol.TerminalStatuses),
	// not hard-coded here, so adding a terminal status can't quietly bypass this.
	terminals := protocol.TerminalStatuses()
	terminalNames := make([]string, len(terminals))
	for i, t := range terminals {
		terminalNames[i] = string(t)
	}
	// updated_at from clock.Now(), not the database's NOW(): the daily
	// terminal-volume histogram buckets on COALESCE(completed_at, updated_at),
	// so a DB-clock stamp here puts a sim order's terminal in a different
	// (real-time) day from its creation.
	res, err := tx.Exec(`UPDATE orders SET status=$1, error_detail=$2, updated_at=$5
		WHERE id=$3 AND status <> ALL($4)`, string(status), errDetail, orderID, terminalNames, clock.Now().UTC())
	if err != nil {
		return false, err
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	won := moved > 0
	// History only for the winner — a losing terminalize must not add a second
	// terminal row to the order's audit trail.
	if won {
		if _, err := tx.Exec(
			`INSERT INTO order_history (order_id, status, detail, code, actor, ref, created_at)
			 VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), $6, $7)`,
			orderID, string(status), detail,
			reason.Code, reason.Actor, reason.refJSON(), clock.Now().UTC()); err != nil {
			return false, err
		}
	}
	// EVERYTHING BELOW RUNS FOR THE LOSER TOO, and commits.
	//
	// The loser must not assume the winner released this order's holds. Every
	// release here is keyed on the ORDER id and is idempotent by construction
	// (WHERE claimed_by=$1 matches zero rows the second time; the DELETE and the
	// reservation release are set-based), so running them twice is a no-op — but
	// running them ZERO times because we returned early would strand a claim on a
	// bin forever, which is the exact leak this chokepoint exists to prevent.
	// Idempotent release is an invariant of this function, not an accident.
	// Anomaly mark MUST run before claim release: the WHERE filters on
	// claimed_by=$orderID, which the next statement clears. COALESCE preserves an
	// earlier stamp. Unconditional across terminals — a bin still claimed by this
	// order and parked at _TRANSIT never arrived, whether the order failed, was
	// skipped, or was confirmed with a lost arrival write. Zero rows on the happy
	// path (a delivered bin already left _TRANSIT and dropped its claim).
	if _, err := tx.Exec(`
		UPDATE bins SET anomaly_at=COALESCE(anomaly_at, NOW()), updated_at=NOW()
		WHERE claimed_by=$1
		  AND node_id IN (SELECT id FROM nodes WHERE name='_TRANSIT')`, orderID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return false, err
	}
	// Release this order's destination-slot claims too (store dual of the bin
	// release above); ReleaseOrphanedClaims is the defense-in-depth backstop.
	if _, err := tx.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM order_bins WHERE order_id=$1`, orderID); err != nil {
		return false, err
	}
	// Release any reservations this order holds (pending or confirmed). Must run
	// in the same tx so no window exists where the order is terminal but its
	// reservation still blocks the bin. The owner-liveness reaper is the
	// defense-in-depth backstop for any row that leaks past this path.
	if err := reservations.ReleaseByOrder(tx, orderID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return won, nil
}

// FailOrderAtomic transitions an order to "failed" and releases all its holds.
// A failure marks _TRANSIT bins anomalous (a claim released mid-flight is a leak
// to investigate). Thin wrapper over TerminalizeOrder.
func (db *DB) FailOrderAtomic(orderID int64, detail string) error {
	_, err := db.TerminalizeOrder(orderID, protocol.StatusFailed, detail)
	return err
}

// ReleaseClaimForBin is the inverse of a single ClaimForDispatch: it clears the
// bin's claim AND releases its reservation in one transaction. Dispatch-failure
// rollbacks route through this instead of a bare UnclaimBin, which would clear
// claimed_by only and orphan the CONFIRMED reservation ClaimForDispatch leaves
// on success — bricking the bin via uq_reservations_bin_active. Owner-scoped
// (only clears claimed_by held by orderID) and bin-keyed on the reservation (the
// unique index guarantees at most one active row per bin, and this order owns
// it). Idempotent: a not-claimed / not-reserved bin is a harmless no-op.
func (db *DB) ReleaseClaimForBin(binID, orderID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE id=$1 AND claimed_by=$2`, binID, orderID); err != nil {
		return err
	}
	if err := reservations.ReleaseByBin(tx, binID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseClaimByOrder is the multi-resource inverse: it clears claimed_by for
// every bin AND every slot this order holds, deletes its order_bins junction rows,
// and releases all its reservations (both kinds) in one transaction. The coupled
// replacement for UnclaimOrderBins on rollback / re-queue paths that abandon an
// order's claims without going terminal (which would otherwise leak the confirmed
// reservations). Idempotent.
//
// Release-set unification: this now releases the SAME set as
// TerminalizeOrder (minus the terminal-only _TRANSIT anomaly stamp). Before the
// slot substrate it cleared only bins + reservations; once slots are
// reservation-backed and hard-claimed via ConfirmSlotClaim, dropping the bin
// claims without the slot claims + order_bins would strand them — the gap this
// commit makes real, so it closes it.
func (db *DB) ReleaseClaimByOrder(orderID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return err
	}
	// Slot claims too (store dual of the bin release above).
	if _, err := tx.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM order_bins WHERE order_id=$1`, orderID); err != nil {
		return err
	}
	// Both reservation kinds — ReleaseByOrder is order-keyed and kind-agnostic.
	if err := reservations.ReleaseByOrder(tx, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

// CountInFlightOrdersByDeliveryNodeExcluding counts in-flight orders for a
// delivery node, excluding a specific order ID (the caller's own row).
// Phase 4c of bin-transit-state: planning-time capacity gates need to
// avoid self-collision when checking from inside the order's own
// dispatch path.
func (db *DB) CountInFlightOrdersByDeliveryNodeExcluding(deliveryNode string, excludeID int64) (int, error) {
	return orders.CountInFlightByDeliveryNodeExcluding(db.DB, deliveryNode, excludeID)
}

// CountLiveOrdersByOrigin counts a demand episode's own non-terminal orders —
// unlike the two counts above, `queued` is included. The sizing half of the
// replenishment bound. See orders.CountLiveByOrigin.
func (db *DB) CountLiveOrdersByOrigin(originID string) (int, error) {
	return orders.CountLiveByOrigin(db.DB, originID)
}

// CountLiveRootsByOrigin counts an episode's own non-terminal ROOT orders — the
// asks it made, not the legs those asks grew. The level keeper's "asked" tally.
// See orders.CountLiveRootsByOrigin for why a dig child must not count.
func (db *DB) CountLiveRootsByOrigin(originID string) (int, error) {
	return orders.CountLiveRootsByOrigin(db.DB, originID)
}

// CountTypedInboundToGroup counts carriers of one type already on their way into
// a group that the level keeper did not ask for. See
// orders.CountTypedInboundToGroup.
func (db *DB) CountTypedInboundToGroup(groupNodeID int64, groupNodeName, binTypeCode string) (int, error) {
	return orders.CountTypedInboundToGroup(db.DB, groupNodeID, groupNodeName, binTypeCode)
}

// CountLiveCarrierRequestsByDeliveryNode counts every non-terminal order that
// asked for a CARRIER at a node — queued included, origin-blind, returns
// excluded. "Has a carrier already been asked for here". See
// orders.CountLiveCarrierRequestsByDeliveryNode.
func (db *DB) CountLiveCarrierRequestsByDeliveryNode(deliveryNode string) (int, error) {
	return orders.CountLiveCarrierRequestsByDeliveryNode(db.DB, deliveryNode)
}

func (db *DB) UpdateOrderRobotID(id int64, robotID string) error {
	return orders.UpdateRobotID(db.DB, id, robotID)
}

func (db *DB) ActiveOrderIDsByRobot() (map[string]int64, error) {
	return orders.ActiveIDsByRobot(db.DB)
}

// ListOrdersByOrigin returns a demand episode's own orders, oldest first, with a
// truncation flag. See orders.ListByOrigin.
func (db *DB) ListOrdersByOrigin(originID string, limit int) ([]*orders.Order, bool, error) {
	return orders.ListByOrigin(db.DB, originID, limit)
}
