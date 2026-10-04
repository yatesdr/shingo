// wiring_staging.go — Staging helpers for bin arrival.
//
// resolveNodeStaging decides whether a destination node receives bins
// as "staged" (lineside) or "available" (storage under a LANE).

package engine

import (
	"shingocore/service"
	"shingocore/store/nodes"
)

// resolveNodeStaging determines if a destination node should receive bins
// as "staged" (lineside nodes) or "available" (storage slots under LANEs).
//
// STAGING DOES NOT EXPIRE. A staged bin is not sourceable, and that is the
// point of staging it: it was delivered for the node it stands on. There was a
// timer (staging.ttl) after which a staged bin became available, and any
// line's plant-wide pull could then take it off the node it was delivered to;
// for a line that keeps a staged spare, that was the spare leaving. The timer
// is retired: no arrival stamps an expiry, whatever staging.ttl says, and
// nothing releases a staged bin on a clock. A staged bin is released by the
// next claim on it or by a person.
func (e *Engine) resolveNodeStaging(destNode *nodes.Node) (staged bool) {
	return !e.isStorageSlot(destNode.ID)
}

// isStorageSlot is service.IsStorageSlot by node id — the one storage rule,
// shared with the hand Move so the two cannot drift. Its callers are
// resolveNodeStaging (arrival staging, this file) and recovery_service.
func (e *Engine) isStorageSlot(nodeID int64) bool {
	node, err := e.db.GetNode(nodeID)
	if err != nil {
		return false
	}
	return service.IsStorageSlot(e.db, node)
}
