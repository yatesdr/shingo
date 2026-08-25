package orders

import (
	"log"

	"shingo/protocol"
	"shingo/protocol/types"
	"shingoedge/store"
	"shingoedge/store/catalog"
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
// resolveClaimForNode returns the claim that governs orders for a process node
// right now: the active style's, or the target style's during a changeover
// where the target has a claim for this node (orders created mid-changeover
// are for the new style's material).
//
// Extracted from lookupPayloadMeta so the routing lookup below reads from the
// SAME claim the payload does — two "what does this node's claim say" answers
// that disagree would be a very quiet bug.
//
// nil for every reason a lookup can fail: no node id, no node, no active
// style, no claim. Callers treat nil as "no opinion", never as a default.
func (m *Manager) resolveClaimForNode(processNodeID *int64) *processes.NodeClaim {
	if processNodeID == nil {
		return nil
	}
	node, err := m.db.GetProcessNode(*processNodeID)
	if err != nil {
		m.DebugLog.Log("process-node lookup failed: id=%d err=%v", *processNodeID, err)
		return nil
	}
	process, err := m.db.GetProcess(node.ProcessID)
	if err != nil || process.ActiveStyleID == nil {
		return nil
	}
	styleID := *process.ActiveStyleID
	if process.TargetStyleID != nil {
		if _, err := m.db.GetStyleNodeClaimByNode(*process.TargetStyleID, node.CoreNodeName); err == nil {
			styleID = *process.TargetStyleID
		}
	}
	claim, err := m.db.GetStyleNodeClaimByNode(styleID, node.CoreNodeName)
	if err != nil {
		m.DebugLog.Log("style-node-claim lookup failed: node=%s err=%v", node.CoreNodeName, err)
		return nil
	}
	return claim
}

// lookupRouting returns the claim's SEER routing hints for an order at this
// node: keyRoute (ordered via-points) and keyTask ("load"/"unload").
//
// THE CLAIM IS THE SEAM, NOT A PARAMETER. Routing is a per-claim geometry
// fact, and the create constructors already take ten positional arguments;
// threading two more through every one of them (and through SwapDispatch, and
// through the changeover OrderSpec) would spread one fact across a dozen
// signatures. The manager already derives per-claim facts from processNodeID
// — that is exactly what lookupPayloadMeta does — so routing is read the same
// way, from the same claim, at the same moment.
//
// Consequence worth knowing: an order with no processNodeID (a manual API
// order) carries no routing, which is correct — there is no claim to speak
// for it, and empty means SEER auto-picks, today's behaviour everywhere.
func (m *Manager) lookupRouting(processNodeID *int64) (keyRoute []string, keyTask string) {
	claim := m.resolveClaimForNode(processNodeID)
	if claim == nil {
		return nil, ""
	}
	return claim.KeyRoute, claim.KeyTask
}

func (m *Manager) lookupPayloadMeta(processNodeID *int64, payloadCode string) (desc, code string) {
	claim := m.resolveClaimForNode(processNodeID)
	if claim == nil {
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
