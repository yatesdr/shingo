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

	"shingo/protocol"
	"shingo/protocol/clock"
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

// isStorageSlot returns true if the node is a storage slot — either a
// LANE/NGRP itself, a direct child of one, or a dedicated loader home/buffer
// position. NGRP children added 2026-04-29: plants modeling supermarkets as
// an NGRP → direct concrete children (no LANE in the path) need those
// children treated as storage so arriving bins land `available`, not `staged`.
// Lineside cells remain parented under processes/zones and continue to stage
// on arrival.
//
// Dedicated loader home/buffer positions (bin_loader_homes) are always
// storage-like: the loader aggregate owns their inventory, bins should arrive
// available so the threshold monitor and swap planner see them correctly.
// These nodes are parentless (no LANE/NGRP in their lineage), so the
// bin_loader_homes check must come before the parentless early-return.
//
// The string was "NODE_GROUP" until the SMKT→NGRP rename (commit 3e3fb4a)
// dropped the legacy code — anything still comparing to "NODE_GROUP" is
// a dead branch.
//
// Formerly lived in wiring_kanban.go; moved when the kanban demand-signal
// path was removed (2026-08) — its remaining callers are resolveNodeStaging
// (arrival staging, this file) and recovery_service.
func (e *Engine) isStorageSlot(nodeID int64) bool {
	node, err := e.db.GetNode(nodeID)
	if err != nil {
		return false
	}
	if node.NodeTypeCode == protocol.NodeClassLANE || node.NodeTypeCode == protocol.NodeClassNGRP {
		return true
	}
	if node.ParentID == nil {
		// Parentless nodes default to lineside (staged arrivals) unless this is a
		// dedicated loader home or buffer position — those are storage-like.
		home, herr := e.db.GetLoaderHomeByPositionNode(nodeID)
		return herr == nil && home != nil
	}
	parent, err := e.db.GetNode(*node.ParentID)
	if err != nil {
		return false
	}
	return parent.NodeTypeCode == protocol.NodeClassLANE || parent.NodeTypeCode == protocol.NodeClassNGRP
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

	t := clock.Now().Add(ttl)
	return &t
}
