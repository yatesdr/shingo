package orders

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingo/protocol/types"
	"shingoedge/store"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// DebugLogFunc is a nil-safe debug logging function.
type DebugLogFunc = types.DebugLogFunc

// Manager handles the order lifecycle state machine.
type Manager struct {
	db        *store.DB
	emitter   EventEmitter
	stationID string
	lifecycle *LifecycleService
	sender    *OrderSender

	DebugLog DebugLogFunc
}

// NewManager creates an order manager.
func NewManager(db *store.DB, emitter EventEmitter, stationID string) *Manager {
	return &Manager{
		db:        db,
		emitter:   emitter,
		stationID: stationID,
		lifecycle: newLifecycleService(db, emitter, nil),
		sender:    newOrderSender(db, stationID),
	}
}

func (m *Manager) enqueueEnvelope(env *protocol.Envelope) error {
	return m.sender.enqueue(env)
}

// lookupPayloadMeta returns description and payload code from the active style
// node claim for the given process node. If processNodeID is nil or the lookup
// fails, returns empty strings. If payloadCode is already set, it is preserved.
// During changeover, prefers the target style over the active style so that
// newly created orders resolve the correct (new) payload.
func (m *Manager) lookupPayloadMeta(processNodeID *int64, payloadCode string) (desc, code string) {
	if processNodeID == nil {
		return "", payloadCode
	}
	node, err := m.db.GetProcessNode(*processNodeID)
	if err != nil {
		m.DebugLog.Log("process-node lookup failed: id=%d err=%v", *processNodeID, err)
		return "", payloadCode
	}
	process, err := m.db.GetProcess(node.ProcessID)
	if err != nil || process.ActiveStyleID == nil {
		return "", payloadCode
	}
	// During changeover, prefer the target style for payload resolution.
	// Orders created during changeover are typically for the new style's material.
	styleID := *process.ActiveStyleID
	if process.TargetStyleID != nil {
		if _, err := m.db.GetStyleNodeClaimByNode(*process.TargetStyleID, node.CoreNodeName); err == nil {
			styleID = *process.TargetStyleID
		}
	}
	claim, err := m.db.GetStyleNodeClaimByNode(styleID, node.CoreNodeName)
	if err != nil {
		m.DebugLog.Log("style-node-claim lookup failed: node=%s err=%v", node.CoreNodeName, err)
		return "", payloadCode
	}
	if payloadCode == "" {
		payloadCode = claim.PayloadCode
	}
	return claim.PayloadCode, payloadCode
}

// enqueueAndAutoSubmit enqueues a protocol envelope and transitions the order
// to submitted. Used by order types that auto-submit at creation (retrieve,
// move, complex, ingest). Store orders do NOT auto-submit — they wait for
// count confirmation.
//
// If the envelope fails to build or enqueue, the order stays in pending so
// the operator sees an actionable state rather than a stuck "submitted" order
// that Core never received.
func (m *Manager) enqueueAndAutoSubmit(orderID int64, orderUUID string, env *protocol.Envelope, envErr error) {
	if envErr != nil {
		log.Printf("ERROR: build envelope for order %s: %v (order stays pending)", orderUUID, envErr)
		m.DebugLog.Log("enqueue failed: order %s envelope build error: %v", orderUUID, envErr)
		return
	}
	if err := m.enqueueEnvelope(env); err != nil {
		log.Printf("ERROR: enqueue order %s: %v (order stays pending)", orderUUID, err)
		m.DebugLog.Log("enqueue failed: order %s outbox write error: %v", orderUUID, err)
		return
	}
	if err := m.TransitionOrder(orderID, StatusSubmitted, "auto-submitted at creation"); err != nil {
		log.Printf("auto-submit order %s: %v", orderUUID, err)
	}
}

// CreateRetrieveOrder creates a new retrieve order and enqueues it to the outbox.
// If payloadCode is empty and payloadID is set, it is derived from the payload.
func (m *Manager) CreateRetrieveOrder(processNodeID *int64, retrieveEmpty bool, quantity int64, deliveryNode, stagingNode, loadType, payloadCode string, autoConfirm, skipAutoConfirm bool) (*orders.Order, error) {
	orderUUID := uuid.New().String()

	payloadDesc, payloadCode := m.lookupPayloadMeta(processNodeID, payloadCode)

	orderID, err := m.db.CreateOrder(orderUUID, TypeRetrieve,
		processNodeID, retrieveEmpty,
		quantity, deliveryNode, stagingNode, "", loadType, autoConfirm, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeOrderRequest, &protocol.OrderRequest{
		OrderUUID:     orderUUID,
		OrderType:     TypeRetrieve,
		PayloadDesc:   payloadDesc,
		PayloadCode:   payloadCode,
		RetrieveEmpty: retrieveEmpty,
		Quantity:      quantity,
		DeliveryNode:  deliveryNode,
		StagingNode:   stagingNode,
		LoadType:      loadType,
 		SkipAutoConfirm: skipAutoConfirm,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s delivery=%s", TypeRetrieve, orderID, orderUUID, deliveryNode)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeRetrieve, nil, processNodeID)

	return m.db.GetOrder(orderID)
}

// CreateStoreOrder creates a new store order (for returning payloads to warehouse).
func (m *Manager) CreateStoreOrder(processNodeID *int64, quantity int64, sourceNode string) (*orders.Order, error) {
	orderUUID := uuid.New().String()

	orderID, err := m.db.CreateOrder(orderUUID, TypeStore,
		processNodeID, false,
		quantity, "", "", sourceNode, "", false, "")
	if err != nil {
		return nil, fmt.Errorf("create store order: %w", err)
	}

	m.DebugLog.Log("create: type=%s id=%d uuid=%s source=%s", TypeStore, orderID, orderUUID, sourceNode)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeStore, nil, processNodeID)
	return m.db.GetOrder(orderID)
}

// SubmitStoreOrder sets the final count, marks count as confirmed, and submits
// the store order in one atomic operation. This is the correct way to submit a
// store order when the count is known at creation time (e.g., from API).
func (m *Manager) SubmitStoreOrder(orderID int64, finalCount int64) error {
	if err := m.db.UpdateOrderFinalCount(orderID, finalCount, true); err != nil {
		return fmt.Errorf("set final count: %w", err)
	}
	return m.SubmitOrder(orderID)
}

// CreateMoveOrder creates a new move order (e.g., quality hold).
// autoConfirm threads through to the order row so Manager.handleDelivered
// can self-confirm instead of stranding the order at "delivered" when no
// operator station is wired up to confirm manually.
func (m *Manager) CreateMoveOrder(processNodeID *int64, quantity int64, sourceNode, deliveryNode string, autoConfirm bool) (*orders.Order, error) {
	orderUUID := uuid.New().String()

	payloadDesc, payloadCode := m.lookupPayloadMeta(processNodeID, "")

	orderID, err := m.db.CreateOrder(orderUUID, TypeMove,
		processNodeID, false,
		quantity, deliveryNode, "", sourceNode, "", autoConfirm, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create move order: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeOrderRequest, &protocol.OrderRequest{
		OrderUUID:    orderUUID,
		OrderType:    TypeMove,
		PayloadDesc:  payloadDesc,
		PayloadCode:  payloadCode,
		Quantity:     quantity,
		DeliveryNode: deliveryNode,
		SourceNode:   sourceNode,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s source=%s delivery=%s", TypeMove, orderID, orderUUID, sourceNode, deliveryNode)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeMove, nil, processNodeID)
	return m.db.GetOrder(orderID)
}

// CreateMoveOrderWithPayloadCode is CreateMoveOrder with an explicit payload
// code instead of falling back to the active claim's primary payload. The
// manual_swap loader / unloader case needs this: a claim can list multiple
// allowed_payload_codes and the operator picks one at LoadBin time. Without
// threading that pick through to L2 / U2, the side-cycle move ends up tagged
// with claim.PayloadCode (the primary), and operator station tiles — which
// filter active orders by payload_code per card — show no in-transit state on
// the loaded payload's tile and may render unrelated tiles as queued via the
// no-payload-code fallback in operator-render.js / operator-modal.js.
func (m *Manager) CreateMoveOrderWithPayloadCode(processNodeID *int64, quantity int64, sourceNode, deliveryNode, payloadCode string, autoConfirm bool) (*orders.Order, error) {
	orderUUID := uuid.New().String()

	payloadDesc, payloadCode := m.lookupPayloadMeta(processNodeID, payloadCode)

	orderID, err := m.db.CreateOrder(orderUUID, TypeMove,
		processNodeID, false,
		quantity, deliveryNode, "", sourceNode, "", autoConfirm, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create move order: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeOrderRequest, &protocol.OrderRequest{
		OrderUUID:    orderUUID,
		OrderType:    TypeMove,
		PayloadDesc:  payloadDesc,
		PayloadCode:  payloadCode,
		Quantity:     quantity,
		DeliveryNode: deliveryNode,
		SourceNode:   sourceNode,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s source=%s delivery=%s payload=%s", TypeMove, orderID, orderUUID, sourceNode, deliveryNode, payloadCode)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeMove, nil, processNodeID)
	return m.db.GetOrder(orderID)
}

// CreateMoveOrderWithUOP creates a move order and threads remainingUOP into the
// protocol envelope so Core can atomically clear/sync the bin manifest on claim.
// autoConfirm mirrors CreateMoveOrder so operator-initiated moves at a
// manual_swap node can self-confirm on delivery.
func (m *Manager) CreateMoveOrderWithUOP(processNodeID *int64, quantity int64, sourceNode, deliveryNode string, remainingUOP *int, autoConfirm bool) (*orders.Order, error) {
	orderUUID := uuid.New().String()

	payloadDesc, payloadCode := m.lookupPayloadMeta(processNodeID, "")

	orderID, err := m.db.CreateOrder(orderUUID, TypeMove,
		processNodeID, false,
		quantity, deliveryNode, "", sourceNode, "", autoConfirm, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create move order: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeOrderRequest, &protocol.OrderRequest{
		OrderUUID:    orderUUID,
		OrderType:    TypeMove,
		PayloadDesc:  payloadDesc,
		PayloadCode:  payloadCode,
		Quantity:     quantity,
		DeliveryNode: deliveryNode,
		SourceNode:   sourceNode,
		RemainingUOP: remainingUOP,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s source=%s delivery=%s remainingUOP=%v", TypeMove, orderID, orderUUID, sourceNode, deliveryNode, remainingUOP)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeMove, nil, processNodeID)
	return m.db.GetOrder(orderID)
}

// CreateComplexOrder creates a new multi-step complex order and enqueues
// it to the outbox. The order is created with auto_confirm=false: it
// requires an operator HMI press to transition delivered → confirmed.
// Use this for deliveries whose destination is at the lineside, where an
// operator can inspect the bin. For deliveries to the supermarket /
// outbound staging (no operator present), use
// CreateComplexOrderWithAutoConfirm instead. deliveryNode is stored on
// the order for downstream logic (e.g., handleOrderCompleted uses it to
// determine which payload to reset on completion).
//
// processNodeName is the dot-name of the line node the order belongs to
// (typically claim.CoreNodeName). Threaded through to
// ComplexOrderRequest.ProcessNode so Core picks the line bin for
// order.BinID and targets it at release-time fallback. Pass "" when the
// order has no distinct line node — Core falls back to source-node
// behavior.
func (m *Manager) CreateComplexOrder(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, false, "")
}

// CreateComplexOrderWithAutoConfirm creates an auto-confirm complex order.
// Used for orders whose destination is the supermarket / outbound staging,
// where there is no operator to press CONFIRM. The order auto-transitions
// delivered → confirmed in handleDelivered the moment the fleet reports
// FINISHED, eliminating the FINISHED → CONFIRMED race window where the
// scanner can re-claim a delivered bin and the late confirm clobbers state
// (the SMN_001 / SMN_002 teleport bug, plant-test 2026-04-27).
func (m *Manager) CreateComplexOrderWithAutoConfirm(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, true, "")
}

func (m *Manager) CreateComplexOrderWithPayload(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, autoConfirm bool, payloadCode string) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, autoConfirm, payloadCode)
}

func (m *Manager) createComplexOrder(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, autoConfirm bool, payloadOverride string) (*orders.Order, error) {
	orderUUID := uuid.New().String()

	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return nil, fmt.Errorf("marshal steps: %w", err)
	}

	payloadDesc, payloadCode := m.lookupPayloadMeta(processNodeID, payloadOverride)

	orderID, err := m.db.CreateOrder(orderUUID, TypeComplex,
		processNodeID, false,
		quantity, deliveryNode, "", "", "", autoConfirm, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create complex order: %w", err)
	}

	if err := m.db.UpdateOrderStepsJSON(orderID, string(stepsJSON)); err != nil {
		return nil, fmt.Errorf("store steps: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeComplexOrderRequest, &protocol.ComplexOrderRequest{
		OrderUUID:   orderUUID,
		PayloadCode: payloadCode,
		PayloadDesc: payloadDesc,
		Quantity:    quantity,
		ProcessNode: processNodeName,
		Steps:       steps,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s steps=%d", TypeComplex, orderID, orderUUID, len(steps))
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeComplex, nil, processNodeID)

	return m.db.GetOrder(orderID)
}

// CreateIngestOrder creates a new ingest order for originating a payload on a bin at a produce station.
// producedAt is an RFC3339 timestamp from the Edge capturing the moment the operator finalized the bin.
func (m *Manager) CreateIngestOrder(processNodeID *int64, payloadCode, binLabel, sourceNode string, quantity int64, manifest []protocol.IngestManifestItem, autoConfirm bool, producedAt string) (*orders.Order, error) {
	orderUUID := uuid.New().String()

	orderID, err := m.db.CreateOrder(orderUUID, TypeIngest,
		processNodeID, false,
		quantity, "", "", sourceNode, "", autoConfirm, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create ingest order: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeOrderIngest, &protocol.OrderIngestRequest{
		OrderUUID:   orderUUID,
		PayloadCode: payloadCode,
		BinLabel:    binLabel,
		SourceNode:  sourceNode,
		Quantity:    quantity,
		Manifest:    manifest,
		ProducedAt:  producedAt,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s payload=%s bin=%s", TypeIngest, orderID, orderUUID, payloadCode, binLabel)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeIngest, nil, processNodeID)

	return m.db.GetOrder(orderID)
}

// ReleaseOrder sends a release message for a staged (dwelling) order.
//
// remainingUOP late-binds the bin's manifest at Core's release handler. Pass
// nil when no manifest change is intended (legacy/Order-A/produce paths). Pass
// &0 to mark the bin empty (NOTHING PULLED disposition). Pass &N (N>0) to
// preserve the manifest with a synced count (SEND PARTIAL BACK disposition).
// See protocol.OrderRelease and BinManifestService.SyncOrClearForReleased.
//
// calledBy carries the operator identity through to Core's bin audit so the
// "who released this bin" question is answerable from Core's audit_log
// table. Empty for system/internal paths (wiring fallbacks, restore); Core
// substitutes "system" in that case.
//
// Thin wrapper that ships no Disposition — used by every fallback / early-
// return release path. Callers that have the structured disposition (the
// main ReleaseOrderWithLineside path) call ReleaseOrderWithDisposition
// directly so Core gets the override-audit context.
func (m *Manager) ReleaseOrder(orderID int64, remainingUOP *int, calledBy string) error {
	return m.ReleaseOrderWithDisposition(orderID, remainingUOP, nil, calledBy)
}

// ReleaseOrderWithDisposition is the Phase 0b release path that carries
// the structured UOPDisposition (kind + operator-submitted vs system-
// suggested values) alongside the legacy RemainingUOP pointer. Core's
// HandleOrderRelease uses RemainingUOP for the manifest sync (unchanged
// behavior); Disposition.CountSuggested / CapturesSuggested drive the
// override audit log.
//
// disposition may be nil — callers without an override-aware body
// (legacy fallback paths) ship only the legacy pointer.
func (m *Manager) ReleaseOrderWithDisposition(orderID int64, remainingUOP *int, disposition *protocol.UOPDisposition, calledBy string) error {
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order: %w", err)
	}
	// Pre-2026-04-27 this required order.Status == StatusStaged. Relaxed to
	// "non-terminal" because the simplified consolidated-release path
	// (ReleaseStagedOrders, see operator_stations.go) fans out to both legs
	// of a two-robot swap regardless of where each leg is in its choreography.
	// The transition to in_transit below is idempotent (applyTransition no-ops
	// when old == new), and Core's HandleOrderRelease only needs the order to
	// have been dispatched (have a VendorOrderID) — which is true once any
	// status past "acknowledged" is reached. See shingo_todo.md for the
	// pre-dispatch edge case worth eventually guarding.
	if IsTerminal(order.Status) {
		return fmt.Errorf("order is terminal (%s), cannot release", order.Status)
	}
	// Pre-dispatch guard: if Core hasn't acknowledged the order yet (no
	// VendorOrderID, status is pending/submitted), there's nothing to release
	// against. Core's HandleOrderRelease would fail trying to send blocks to
	// an empty VendorOrderID. In practice this can't happen during a real
	// two-robot swap (Robot B reaching staged means both orders dispatched),
	// but the consolidated release fans out unconditionally so we guard here.
	// Silent no-op: log and return nil so the consolidated call site doesn't
	// abort over a pre-dispatch sibling. See shingo_todo.md.
	if order.Status == StatusPending || order.Status == StatusSubmitted {
		m.DebugLog.Log("release: id=%d uuid=%s status=%q is pre-dispatch — skipping (no VendorOrderID for Core to release against)",
			orderID, order.UUID, order.Status)
		return nil
	}

	if err := m.sender.Queue(protocol.TypeOrderRelease, &protocol.OrderRelease{
		OrderUUID:    order.UUID,
		RemainingUOP: remainingUOP,
		Disposition:  disposition,
		CalledBy:     calledBy,
	}); err != nil {
		return fmt.Errorf("enqueue release: %w", err)
	}

	// Transition Edge status to in_transit now that the robot is resuming.
	// Core won't send a dedicated in_transit message (TypeOrderUpdate ignores
	// status), so we transition locally to keep Edge in sync.
	if err := m.TransitionOrder(orderID, StatusInTransit, "released from staging"); err != nil {
		return fmt.Errorf("transition to in_transit: %w", err)
	}

	// Single log shape regardless of nil-ness — keeps log-parsing tools
	// from having to handle two different formats for the same event.
	// Nil prints as "<nil>" via %v.
	m.DebugLog.Log("release: id=%d uuid=%s remaining_uop=%v disposition=%v called_by=%q",
		orderID, order.UUID, remainingUOP, disposition, calledBy)
	return nil
}

// HandleDeliveredWithExpiry processes a delivered reply with optional
// staged expiry. binID captures Core's bin id at delivery so the PLC
// tick path can attribute deltas to the right bin; nil for multi-bin
// orders. Edge's runtime cache (reconciler-healed from Core) is the
// source of truth for bin UOP at completion time — the OrderDelivered
// envelope no longer carries a UOP snapshot.
func (m *Manager) HandleDeliveredWithExpiry(orderUUID, statusDetail string, stagedExpireAt *time.Time, binID *int64) error {
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return fmt.Errorf("order %s not found: %w", orderUUID, err)
	}
	return m.handleDelivered(order, statusDetail, stagedExpireAt, binID)
}

func (m *Manager) handleDelivered(order *orders.Order, statusDetail string, stagedExpireAt *time.Time, binID *int64) error {
	if err := m.lifecycle.HandleDelivered(order, statusDetail, stagedExpireAt, binID); err != nil {
		return err
	}
	if order.AutoConfirm {
		m.DebugLog.Log("auto-confirm: id=%d uuid=%s qty=%d", order.ID, order.UUID, order.Quantity)
		return m.ConfirmDelivery(order.ID, order.Quantity)
	}
	return nil
}

// TransitionOrder moves an order to a new status with validation.
func (m *Manager) TransitionOrder(orderID int64, newStatus protocol.Status, detail string) error {
	m.lifecycle.debug = m.DebugLog
	return m.lifecycle.Transition(orderID, newStatus, detail)
}

// AbortOrder cancels a non-terminal order and enqueues a cancel message.
// The cancel message is enqueued BEFORE the local transition so that Core
// is guaranteed to receive the cancellation — preventing a robot from
// continuing to execute a cancelled order on the floor.
func (m *Manager) AbortOrder(orderID int64) error {
	m.DebugLog.Log("abort: id=%d", orderID)
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order: %w", err)
	}
	if IsTerminal(order.Status) {
		return fmt.Errorf("order is already in terminal state: %s", order.Status)
	}

	// Build and enqueue cancel message BEFORE transitioning locally.
	// If enqueue fails, the order stays in its current state so the
	// operator can retry rather than having a locally-cancelled order
	// with a robot still executing on the floor.
	const abortReason = "aborted by operator"
	if err := m.sender.Queue(protocol.TypeOrderCancel, &protocol.OrderCancel{
		OrderUUID: order.UUID,
		Reason:    abortReason,
	}); err != nil {
		return fmt.Errorf("enqueue cancel message: %w", err)
	}

	if err := m.TransitionOrder(orderID, StatusCancelled, abortReason); err != nil {
		return err
	}
	return nil
}

// RedirectOrder changes the delivery node of a non-terminal order and enqueues a redirect message.
// The envelope is built and enqueued before updating the local DB so that
// Core receives the redirect. If enqueue fails, the error is returned.
func (m *Manager) RedirectOrder(orderID int64, newDeliveryNode string) (*orders.Order, error) {
	m.DebugLog.Log("redirect: id=%d new_delivery=%s", orderID, newDeliveryNode)
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return nil, fmt.Errorf("get order: %w", err)
	}
	if IsTerminal(order.Status) {
		return nil, fmt.Errorf("order is already in terminal state: %s", order.Status)
	}

	// Build and enqueue redirect message first. If this fails, don't
	// update local state — the operator can retry.
	if err := m.sender.Queue(protocol.TypeOrderRedirect, &protocol.OrderRedirect{
		OrderUUID:       order.UUID,
		NewDeliveryNode: newDeliveryNode,
	}); err != nil {
		return nil, fmt.Errorf("enqueue redirect: %w", err)
	}

	if err := m.db.UpdateOrderDeliveryNode(orderID, newDeliveryNode); err != nil {
		return nil, fmt.Errorf("update delivery node: %w", err)
	}

	return m.db.GetOrder(orderID)
}

// SubmitOrder transitions a pending order to submitted and enqueues it.
// For store orders, it also builds and enqueues the storage waybill.
func (m *Manager) SubmitOrder(orderID int64) error {
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return err
	}

	m.DebugLog.Log("submit: id=%d uuid=%s type=%s", orderID, order.UUID, order.OrderType)

	// For store orders, build and enqueue the waybill BEFORE transitioning.
	// If enqueue fails, the order stays pending so the operator can retry.
	if order.OrderType == TypeStore {
		var finalCount int64
		if order.FinalCount != nil {
			finalCount = *order.FinalCount
		}
		desc, _ := m.lookupPayloadMeta(order.ProcessNodeID, "")
		env, err := m.sender.build(protocol.TypeOrderStorageWaybill, &protocol.OrderStorageWaybill{
			OrderUUID:   order.UUID,
			OrderType:   TypeStore,
			PayloadDesc: desc,
			SourceNode:  order.SourceNode,
			FinalCount:  finalCount,
		})
		if err != nil {
			return fmt.Errorf("build storage waybill: %w", err)
		}
		if err := m.enqueueEnvelope(env); err != nil {
			return fmt.Errorf("enqueue storage waybill: %w", err)
		}
	}

	return m.TransitionOrder(orderID, StatusSubmitted, "submitted to dispatch")
}

// ConfirmDelivery sends a delivery receipt and transitions to confirmed.
func (m *Manager) ConfirmDelivery(orderID int64, finalCount int64) error {
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return err
	}

	if order.Status != StatusDelivered {
		return fmt.Errorf("order must be in delivered status to confirm, got %s", order.Status)
	}

	m.DebugLog.Log("confirm: id=%d uuid=%s count=%d", orderID, order.UUID, finalCount)

	if err := m.db.UpdateOrderFinalCount(orderID, finalCount, true); err != nil {
		return err
	}

	// Enqueue delivery receipt — failure is logged but does not block
	// the confirmation. The receipt is informational; Core tracks delivery
	// via its own fleet polling. The outbox will retry if Kafka is down.
	if err := m.sender.Queue(protocol.TypeOrderReceipt, &protocol.OrderReceipt{
		OrderUUID:   order.UUID,
		ReceiptType: "confirmed",
		FinalCount:  finalCount,
	}); err != nil {
		return fmt.Errorf("enqueue delivery receipt %s: %w", order.UUID, err)
	}

	return m.TransitionOrder(orderID, StatusConfirmed, fmt.Sprintf("confirmed with count %d", finalCount))
}

// HandleDispatchReply processes an inbound reply from central dispatch.
func (m *Manager) HandleDispatchReply(orderUUID, replyType, waybillID, eta, statusDetail string) error {
	m.DebugLog.Log("dispatch reply: uuid=%s type=%s", orderUUID, replyType)
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return fmt.Errorf("order %s not found: %w", orderUUID, err)
	}

	switch replyType {
	case ReplyAck:
		return m.TransitionOrder(order.ID, StatusAcknowledged, statusDetail)
	case ReplyWaybill:
		if err := m.db.UpdateOrderWaybill(order.ID, waybillID, eta); err != nil {
			return err
		}
		return m.TransitionOrder(order.ID, StatusInTransit, fmt.Sprintf("waybill %s, ETA %s", waybillID, eta))
	case ReplyQueued:
		// Order queued by Core — awaiting inventory
		return m.TransitionOrder(order.ID, StatusQueued, statusDetail)
	case ReplyUpdate:
		// Status update with ETA only — don't touch waybill_id.
		if eta != "" {
			if err := m.db.UpdateOrderETA(order.ID, eta); err != nil {
				return err
			}
		}
		return nil
	case ReplyDelivered:
		return m.handleDelivered(order, statusDetail, nil, nil)
	case ReplyError:
		return m.TransitionOrder(order.ID, StatusFailed, statusDetail)
	case ReplySkipped:
		// "Work was never needed" terminal — distinct from ReplyError.
		// The post-skip cleanup (advancing a linked changeover node task
		// and writing the operator-facing note) happens in the edge_handler
		// HandleOrderSkipped path before this; here we only persist the
		// order's local status.
		return m.TransitionOrder(order.ID, StatusSkipped, statusDetail)
	case ReplyStaged:
		return m.TransitionOrder(order.ID, StatusStaged, statusDetail)
	case ReplyCancelled:
		return m.TransitionOrder(order.ID, StatusCancelled, statusDetail)
	default:
		return fmt.Errorf("unknown reply type: %s", replyType)
	}
}

// ApplyCoreStatusSnapshot reconciles a local order with Core's authoritative status.
func (m *Manager) ApplyCoreStatusSnapshot(snapshot protocol.OrderStatusSnapshot) error {
	m.lifecycle.debug = m.DebugLog
	return m.lifecycle.ApplyCoreStatusSnapshot(snapshot)
}

// HandleSkipped processes Core's terminal "the work was never needed"
// notification for an order. Today the sole producer is the complex-order
// dispatcher's no_source_bin path (every pickup node was genuinely empty,
// e.g. evac for a bin that was pulled to quality hold before dispatch).
//
// Three-step write, in order:
//
//  1. Transition the local order row to StatusSkipped via the standard
//     dispatch-reply path — keeps lifecycle audit consistent with every
//     other terminal reply.
//  2. Look up the changeover_node_tasks row linked to this order (either
//     leg). If found, advance its state to the post-completion state a
//     successful run would have produced — line_cleared for an evac leg,
//     released for a supply leg. This unsticks the changeover state
//     machine without requiring operator intervention.
//  3. Write skip_note on the same node task so the HMI surfaces a chip
//     ("evac skipped: bin missing — recover manually if needed") instead
//     of a sticky red error toast.
//
// Idempotent: a duplicate skip on an already-skipped order lands on a
// terminal row (TransitionOrder no-op) and the node-task updates are
// last-writer-wins on the same row.
func (m *Manager) HandleSkipped(orderUUID, errorCode, detail string) error {
	m.DebugLog.Log("dispatch reply: uuid=%s type=skipped code=%s", orderUUID, errorCode)
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return fmt.Errorf("order %s not found: %w", orderUUID, err)
	}
	if err := m.TransitionOrder(order.ID, StatusSkipped, detail); err != nil {
		return err
	}
	task, _, terr := m.db.FindChangeoverNodeTaskByOrderID(order.ID)
	if terr != nil || task == nil {
		// Not a changeover-linked order — nothing more to advance.
		return nil
	}
	postState := skippedTerminalState(task, order.ID)
	if err := m.db.UpdateChangeoverNodeTaskState(task.ID, postState); err != nil {
		log.Printf("orders: advance node task %d to %s on skip: %v", task.ID, postState, err)
	}
	note := formatSkipNote(task, errorCode, detail)
	if err := m.db.SetChangeoverNodeTaskSkipNote(task.ID, note); err != nil {
		log.Printf("orders: set skip_note on node task %d: %v", task.ID, err)
	}
	return nil
}

// skippedTerminalState picks the post-completion node-task state that a
// successful run of the skipped order would have produced. Mirrors the
// completion-handler shape in wiring_completion.go:
//
//   - evac leg (OldMaterialReleaseOrderID == orderID): line_cleared
//   - supply leg (NextMaterialOrderID == orderID): released
//   - neither matches (shouldn't happen — FindChangeoverNodeTaskByOrderID
//     OR-matches): default to released to keep the state machine moving.
func skippedTerminalState(task *processes.NodeTask, orderID int64) string {
	if task.OldMaterialReleaseOrderID != nil && *task.OldMaterialReleaseOrderID == orderID {
		return "line_cleared"
	}
	return "released"
}

// formatSkipNote builds the operator-facing chip text. Keep it short —
// the HMI renders it on a small chip; the full Detail string is logged
// elsewhere for forensics.
func formatSkipNote(task *processes.NodeTask, errorCode, detail string) string {
	leg := "order"
	if task.OldMaterialReleaseOrderID != nil {
		leg = "evac"
	} else if task.NextMaterialOrderID != nil {
		leg = "supply"
	}
	if errorCode == "no_source_bin" {
		return leg + " skipped: bin missing at " + task.NodeName
	}
	return leg + " skipped: " + detail
}

// RollbackForRetry force-transitions an order back to StatusStaged with a
// friendly detail message. Used for recoverable Core errors (e.g.
// manifest_sync_failed) where the operator can simply click release again
// instead of having to recreate the whole order.
//
// Why force-transition: the order may currently be in StatusInTransit (the
// release click already ran on Edge) or any non-terminal state, so the
// regular Transition rules don't apply. The caller has already validated
// that the rollback is appropriate (typically by inspecting an OrderError
// code from Core).
//
// The friendly detail string is what the operator UI surfaces as the
// "release error" chip on the node — see StationNodeView.LastReleaseError
// and the rendering in operator-station/operator.js.
func (m *Manager) RollbackForRetry(orderUUID, detail string) error {
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return fmt.Errorf("get order %s: %w", orderUUID, err)
	}
	m.lifecycle.debug = m.DebugLog
	return m.lifecycle.ForceTransition(order.ID, StatusStaged, detail)
}
