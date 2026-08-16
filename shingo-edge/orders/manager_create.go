package orders

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingoedge/store/orders"
)

// CreateRetrieveOrder creates a new retrieve order and enqueues it to the outbox.
// If payloadCode is empty and payloadID is set, it is derived from the payload.
//
// sourceNode names the supermarket node group (or specific node) that Core
// should pull the bin from. Empty string falls back to Core's global FIFO
// search (legacy behaviour). For bin_loader manual_swap claims this MUST be
// claim.InboundSource — otherwise the planner happily pulls a payload-matching
// empty/full bin from anywhere in the system, including the empty-tote return
// area instead of the configured supermarket. See planRetrieveEmpty in
// shingo-core/dispatch/planning_service.go for the resolver branch.
// origin is REQUIRED. There is no unattributed twin of this any more — see the
// Origin type, and see specimen (c) for what the twin cost.
func (m *Manager) CreateRetrieveOrder(processNodeID *int64, retrieveEmpty bool, quantity int64, deliveryNode, sourceNode, stagingNode, loadType, payloadCode string, autoConfirm, skipAutoConfirm bool, origin Origin) (*orders.Order, error) {
	return m.createRetrieveOrder(processNodeID, retrieveEmpty, quantity,
		deliveryNode, sourceNode, stagingNode, loadType, payloadCode, autoConfirm, skipAutoConfirm, origin)
}

// createRetrieveOrder is the one body.
func (m *Manager) createRetrieveOrder(processNodeID *int64, retrieveEmpty bool, quantity int64,
	deliveryNode, sourceNode, stagingNode, loadType, payloadCode string,
	autoConfirm, skipAutoConfirm bool, origin Origin) (*orders.Order, error) {
	orderUUID := uuid.New().String()

	payloadDesc, payloadCode := m.lookupPayloadMeta(processNodeID, payloadCode)

	orderID, err := m.db.CreateOrder(orderUUID, TypeRetrieve,
		processNodeID, retrieveEmpty,
		quantity, deliveryNode, stagingNode, sourceNode, loadType, autoConfirm, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeOrderRequest, &protocol.OrderRequest{
		OrderUUID:       orderUUID,
		OrderType:       TypeRetrieve,
		PayloadDesc:     payloadDesc,
		PayloadCode:     payloadCode,
		RetrieveEmpty:   retrieveEmpty,
		Quantity:        quantity,
		DeliveryNode:    deliveryNode,
		SourceNode:      sourceNode,
		StagingNode:     stagingNode,
		LoadType:        loadType,
		SkipAutoConfirm: skipAutoConfirm,
		OriginID:        origin.ID,
		OriginClass:     origin.Class,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s delivery=%s", TypeRetrieve, orderID, orderUUID, deliveryNode)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeRetrieve, nil, processNodeID)

	return m.db.GetOrder(orderID)
}

// CreateMoveOrder creates a new move order (e.g., quality hold).
// autoConfirm threads through to the order row so Manager.handleDelivered
// can self-confirm instead of stranding the order at "delivered" when no
// operator station is wired up to confirm manually.
// origin is REQUIRED; see the Origin type.
func (m *Manager) CreateMoveOrder(processNodeID *int64, quantity int64, sourceNode, deliveryNode string, autoConfirm bool, origin Origin) (*orders.Order, error) {
	return m.createMoveOrder(processNodeID, quantity, sourceNode, deliveryNode, "", nil, autoConfirm, origin)
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
// origin is REQUIRED; see the Origin type.
func (m *Manager) CreateMoveOrderWithPayloadCode(processNodeID *int64, quantity int64, sourceNode, deliveryNode, payloadCode string, autoConfirm bool, origin Origin) (*orders.Order, error) {
	return m.createMoveOrder(processNodeID, quantity, sourceNode, deliveryNode, payloadCode, nil, autoConfirm, origin)
}

// CreateMoveOrderWithUOP creates a move order and threads remainingUOP into the
// protocol envelope so Core can atomically clear/sync the bin manifest on claim.
// autoConfirm mirrors CreateMoveOrder so operator-initiated moves at a
// manual_swap node can self-confirm on delivery.
// origin is REQUIRED; see the Origin type.
func (m *Manager) CreateMoveOrderWithUOP(processNodeID *int64, quantity int64, sourceNode, deliveryNode string, remainingUOP *int, autoConfirm bool, origin Origin) (*orders.Order, error) {
	return m.createMoveOrder(processNodeID, quantity, sourceNode, deliveryNode, "", remainingUOP, autoConfirm, origin)
}

// createMoveOrder is the one body behind all four move variants.
//
// They were four copies of the same twenty lines differing in one field each,
// which is how a change lands in three of them: the origin plumbing would have
// been exactly that change. Collapsing them first makes "every move order can
// carry an origin" true by construction rather than by inspection.
func (m *Manager) createMoveOrder(processNodeID *int64, quantity int64,
	sourceNode, deliveryNode, payloadCode string, remainingUOP *int,
	autoConfirm bool, origin Origin) (*orders.Order, error) {
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
		RemainingUOP: remainingUOP,
		OriginID:     origin.ID,
		OriginClass:  origin.Class,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s source=%s delivery=%s payload=%s remainingUOP=%v origin=%s",
		TypeMove, orderID, orderUUID, sourceNode, deliveryNode, payloadCode, remainingUOP, origin.ID)
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
// origin is REQUIRED; see the Origin type.
func (m *Manager) CreateComplexOrder(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, origin Origin) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, false, "", "", origin)
}

// CreateComplexOrderWithAutoConfirm creates an auto-confirm complex order.
// Used for orders whose destination is the supermarket / outbound staging,
// where there is no operator to press CONFIRM. The order auto-transitions
// delivered → confirmed in handleDelivered the moment the fleet reports
// FINISHED, eliminating the FINISHED → CONFIRMED race window where the
// scanner can re-claim a delivered bin and the late confirm clobbers state
// (the SMN_001 / SMN_002 teleport bug, plant-test 2026-04-27).
// origin is REQUIRED; see the Origin type.
func (m *Manager) CreateComplexOrderWithAutoConfirm(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, origin Origin) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, true, "", "", origin)
}

// origin is REQUIRED; see the Origin type.
func (m *Manager) CreateComplexOrderWithPayload(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, autoConfirm bool, payloadCode string, origin Origin) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, autoConfirm, payloadCode, "", origin)
}

// CreateComplexOrderSibling creates a complex order and records the
// two-robot swap sibling UUID on the outbound ComplexOrderRequest, so Core
// can pair the legs at intake — before the removal leg's synchronous
// dispatch claims the line bin. siblingUUID is the *other* leg's edge UUID,
// or "" for non-swap / first-created legs.
// origin is REQUIRED; see the Origin type.
func (m *Manager) CreateComplexOrderSibling(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, autoConfirm bool, payloadCode, siblingUUID string, origin Origin) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, autoConfirm, payloadCode, siblingUUID, origin)
}

func (m *Manager) createComplexOrder(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, autoConfirm bool, payloadOverride, siblingUUID string, origin Origin) (*orders.Order, error) {
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
		OrderUUID:        orderUUID,
		PayloadCode:      payloadCode,
		PayloadDesc:      payloadDesc,
		Quantity:         quantity,
		ProcessNode:      processNodeName,
		Steps:            steps,
		SiblingOrderUUID: siblingUUID,
		OriginID:         origin.ID,
		OriginClass:      origin.Class,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s steps=%d", TypeComplex, orderID, orderUUID, len(steps))
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeComplex, nil, processNodeID)

	return m.db.GetOrder(orderID)
}

// QueueIngestManifest sends a manifest-only ingest stamp to Core WITHOUT
// minting a local order. Swap-mode produce finalize uses this: the swap's
// complex order carries the bin, and the ingest exists only to stamp Core's
// bin manifest. Creating a local order there made a phantom that the
// operator-abort fan-out later cancelled, producing the "not_found" error —
// and Core's manifest-only ingest handler creates no order and sends no reply,
// so nothing ever matched it back. This mirrors CreateIngestOrder's envelope
// but ships it through the fire-and-forget Queue path (like ConfirmDelivery's
// receipt): no order row, no transition, no EmitOrderCreated. The stamp is
// still delivered (durable outbox, idempotent SetForProduction at Core).
// binID (0 = absent) pins the exact Core bin for the release-time produce
// manifest — see protocol.OrderIngestRequest.BinID.
func (m *Manager) QueueIngestManifest(payloadCode, binLabel string, binID int64, sourceNode string, quantity int64, manifest []protocol.IngestManifestItem, producedAt string) error {
	return m.sender.Queue(protocol.TypeOrderIngest, &protocol.OrderIngestRequest{
		OrderUUID:   uuid.New().String(),
		BinID:       binID,
		PayloadCode: payloadCode,
		BinLabel:    binLabel,
		SourceNode:  sourceNode,
		Quantity:    quantity,
		Manifest:    manifest,
		ProducedAt:  producedAt,
	})
}

// Origin attributes an order to the demand episode that asked for it.
//
// A VALUE, NOT TWO MORE POSITIONAL PARAMETERS. CreateRetrieveOrder already
// takes ten, and the eleventh and twelfth would be two adjacent strings whose
// order nothing but the compiler could check — and the compiler cannot tell
// origin_id from origin_class.
//
// ── THERE IS NO "NOT STATED" ANY MORE ─────────────────────────────────────
//
// The zero value used to be a third state, defended as honest: "Core classifies
// those; saying nothing here is honest, where guessing would not be." The
// defence was sound and the outcome was not. Every door that was never wired
// took the zero — the HMI button, the sequential backfill, the changeover
// applier's legs, RequestEmptyBin's multi-step arm — and each of them WAS
// demand-serving. They reached Core carrying nothing, Core honestly stamped
// orphan, and the episode surface showed changeovers and thresholds only. Not
// mislabelled: absent. A zero value that means "nobody decided" is
// indistinguishable from "somebody decided nothing", and the whole of specimen
// (c) is the second one wearing the first one's clothes.
//
// So the type is TOTAL: exactly two constructors, and every create site names
// one. "Forgot" is now a compile error, because the unattributed wrappers that
// used to supply this silently are deleted and origin is a required argument.
// The remaining hole is a bare Origin{} literal, which Go will always let you
// write; a forbidigo fence closes it outside this package.
//
// NO_DEMAND MEANS "BELONGS TO NO DEMAND EPISODE BY CONSTRUCTION" — not "nobody
// wanted it". An API-commanded retrieve, a loader's owed outbound, a home
// consolidation: somebody or something wanted every one of those, and none of
// them will ever belong to a cell's episode. That is the distinction the class
// records, and it is what keeps the orphan bucket meaning "should have had one
// and lost it".
//
// AND A WRONG no_demand IS WORSE THAN A WRONG attached, which is the thing to
// hold on to when adding a site. An order labelled no_demand has ANSWERED the
// question and leaves the orphan bucket for good; an order labelled with the
// wrong episode is visibly wrong on a surface somebody reads. When a site is
// genuinely unclear, it is the no_demand answer that needs the argument.
type Origin struct {
	// ID is the demand episode. Empty for a no_demand order.
	ID string
	// Class is one of protocol.OriginClass*. Empty means the value was never
	// built through a constructor — see the fence above; nothing in the tree
	// should be able to produce it.
	Class string
}

// Attached is the origin of an order created to serve a demand episode.
func Attached(originID string) Origin {
	return Origin{ID: originID, Class: protocol.OriginClassAttached}
}

// NoDemand marks an order that is STRUCTURALLY originless — one nothing asked
// for, created because the system had a reason of its own.
//
// Stamped at the CREATE SITE, where it is known, and never inferred later.
// Without it, `origin_id IS NULL` selects every opportunistic stage and every
// admin action along with the actual lost origins: a haystack with the needle
// in it. This is what keeps the orphan bucket meaning something.
func NoDemand() Origin {
	return Origin{Class: protocol.OriginClassNoDemand}
}
