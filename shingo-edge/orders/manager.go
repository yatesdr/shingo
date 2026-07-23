package orders

import (
	"log"

	"shingo/protocol"
	"shingo/protocol/types"
	"shingoedge/store"
	"shingoedge/store/catalog"
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
	// Backfill the payload from the claim only for serial consume/produce
	// claims, where PayloadCode is the single bound payload. manual_swap (bin
	// loader/unloader) claims carry no meaningful PayloadCode — the allowed set
	// governs, and an operator empty request is intentionally payload-AGNOSTIC
	// (RequestEmptyBin / maybeStageLoaderEmpty ship a blank code so the carrier
	// is generic and LoadBin binds the real payload). Re-injecting the claim's
	// payload here would silently re-tag that agnostic empty.
	if payloadCode == "" && claim.SwapMode != protocol.SwapModeManualSwap {
		payloadCode = claim.PayloadCode
	}
	if entry, err := catalog.GetCatalogByCode(m.db.DB, payloadCode); err == nil && entry.Description != "" {
		desc = entry.Description
	}
	return desc, payloadCode
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
		log.Printf("auto-submit order %s: %v (enqueued to outbox but status stayed pending; reconciles when Core replies)", orderUUID, err)
	}
}
