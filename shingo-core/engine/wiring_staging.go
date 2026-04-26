// wiring_staging.go — Staging helpers for bin arrival.
//
// resolveNodeStaging decides whether a destination node receives bins
// as "staged" (lineside) or "available" (storage under a LANE).
// resolveStagingExpiry computes the expiry time for staged bins using
// per-node `staging_ttl` property with parent fallback and a global
// config default.

package engine

import (
	"strings"
	"time"

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

// resolveStagingExpiry computes the staging expiry time for a node.
// Returns nil if staging is permanent (ttl=0 or ttl=none).
func (e *Engine) resolveStagingExpiry(node *nodes.Node) *time.Time {
	ttlStr := ""

	// Check node's own property first
	ttlStr = e.db.GetNodeProperty(node.ID, "staging_ttl")

	// If not set, check parent (via effective properties)
	if ttlStr == "" && node.ParentID != nil {
		ttlStr = e.db.GetNodeProperty(*node.ParentID, "staging_ttl")
	}

	// Parse the TTL value
	if ttlStr == "0" || strings.EqualFold(ttlStr, "none") {
		return nil // permanent staging
	}

	var ttl time.Duration
	if ttlStr != "" {
		parsed, err := time.ParseDuration(ttlStr)
		if err != nil {
			e.logFn("engine: staging ttl parse error for node %d: %q: %v", node.ID, ttlStr, err)
		} else {
			ttl = parsed
		}
	}

	// Fall back to global config default
	if ttl == 0 {
		ttl = e.cfg.Staging.TTL
	}
	if ttl <= 0 {
		return nil
	}

	t := time.Now().Add(ttl)
	return &t
}
