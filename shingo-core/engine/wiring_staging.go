// wiring_staging.go — Staging helpers for bin arrival.
//
// resolveNodeStaging decides whether a destination node receives bins
// as "staged" (lineside) or "available" (storage under a LANE).
// resolveStagingExpiry computes the expiry time for staged bins from the
// global config default (staging.ttl).

package engine

import (
	"time"

	"shingo/protocol/clock"
	"shingocore/service"
	"shingocore/store/nodes"
)

// resolveNodeStaging determines if a destination node should receive bins
// as "staged" (lineside nodes) or "available" (storage slots under LANEs).
func (e *Engine) resolveNodeStaging(destNode *nodes.Node) (staged bool, expiresAt *time.Time) {
	isStorage := e.isStorageSlot(destNode.ID)
	if !isStorage {
		expiresAt = e.resolveStagingExpiry(destNode)
	}
	return !isStorage, expiresAt
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

// resolveStagingExpiry computes the staging expiry time for a node from the
// global staging.ttl. Returns nil if staging is permanent (ttl <= 0).
//
// There was a per-node `staging_ttl` property, with a parent fallback, that
// could override the default. No node at either plant ever carried it, so
// both reads always came back empty and fell through to the default; the
// reads were dropped 2026-09-27. The node argument stays because callers
// pass the destination; the expiry no longer depends on it.
func (e *Engine) resolveStagingExpiry(_ *nodes.Node) *time.Time {
	ttl := e.cfg.Staging.TTL
	if ttl <= 0 {
		return nil
	}
	t := clock.Now().Add(ttl)
	return &t
}
