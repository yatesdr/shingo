// Package helpers holds shared low-level utilities used by every store
// sub-package. It lives under store/internal/ so its visibility is bounded
// to packages under shingocore/store/... — engine, service, www, and other
// out-of-store callers cannot import it (Go's internal/ rule).
//
// The duplication this package eliminates (each sub-package previously
// carried its own helpers.go) was a deliberate Phase-pre-5 trade-off:
// keep aggregates zero-dependency until enough sub-packages exist to
// justify a shared internal package. Phase 5 crosses that threshold by
// adding 13 more core sub-packages.
package helpers

import (
	"database/sql"
	"fmt"
	"time"

	"shingocore/domain"
)

// NullableInt converts *int to a value safe for SQL params (nil-safe).
func NullableInt(p *int) any {
	if p != nil {
		return *p
	}
	return nil
}

// NullableInt64 converts *int64 to a value safe for SQL params (nil-safe).
func NullableInt64(p *int64) any {
	if p != nil {
		return *p
	}
	return nil
}

// NullableTime converts *time.Time to a UTC value safe for SQL params (nil-safe).
func NullableTime(p *time.Time) any {
	if p != nil {
		return p.UTC()
	}
	return nil
}

// InsertID executes an INSERT ... RETURNING id query and returns the new row ID.
func InsertID(db *sql.DB, query string, args ...any) (int64, error) {
	var id int64
	err := db.QueryRow(query, args...).Scan(&id)
	return id, err
}

// EvictStaleGhostBinsTx reconciles the one-bin-per-physical-node invariant at
// arrival time. Any non-retired bin OTHER than keepBinID that shingo still
// records at destNodeID is moved to _TRANSIT — unclaimed, with anomaly_at
// stamped — inside tx, and its id is returned so the caller can surface it via
// ListAnomalies / RecoverTransitAnomaly.
//
// Why the conflicting record is a stale ghost (plant-verified 2026-07-08): a
// delivery physically CANNOT complete onto an occupied slot, so a completed
// delivery is itself proof the slot was empty. RDS emits no fault code and does
// not track occupancy — the proof is the physical completion, not a vendor
// error. A different bin still recorded here is therefore a stale ghost an
// untracked manual move left behind; evict it and keep the newcomer, never the
// reverse.
//
// Synthetic nodes (LANE/NGRP/_TRANSIT) hold many bins by design and are exempt.
// The _TRANSIT lookup is lazy — only on the rare collision, not every arrival.
// This is the ONE reconciliation the arrival-writers share — single-bin
// (service.BinService.ApplyArrival), multi-bin (store.ApplyMultiBinArrival), and
// completion-repair (recovery.RepairConfirmedOrderCompletion) — so the paths
// cannot drift and no caller can forget the synthetic exemption. It lives in
// store/internal so store-layer and recovery-sub-package callers reach it
// without an import cycle; *store.DB.EvictStaleGhostsTx is a thin delegate for
// the service layer, which cannot import internal/.
//
// See docs/storage-protections.md for how this arrival-time tier composes with
// the dispatch-time protections and the two plant-verified vendor facts.
func EvictStaleGhostBinsTx(tx *sql.Tx, destNodeID, keepBinID int64) ([]int64, error) {
	var occupied bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM bins WHERE node_id=$1 AND id<>$2 AND status<>'retired')`,
		destNodeID, keepBinID).Scan(&occupied); err != nil {
		return nil, fmt.Errorf("check destination occupancy node %d: %w", destNodeID, err)
	}
	if !occupied {
		return nil, nil
	}
	var isSynthetic bool
	if err := tx.QueryRow(`SELECT is_synthetic FROM nodes WHERE id=$1`, destNodeID).Scan(&isSynthetic); err != nil {
		return nil, fmt.Errorf("lookup destination node %d: %w", destNodeID, err)
	}
	if isSynthetic {
		return nil, nil
	}
	var transitID int64
	if err := tx.QueryRow(`SELECT id FROM nodes WHERE name=$1`, domain.TransitNodeName).Scan(&transitID); err != nil {
		return nil, fmt.Errorf("lookup transit node %q: %w", domain.TransitNodeName, err)
	}
	rows, err := tx.Query(`UPDATE bins SET node_id=$1, claimed_by=NULL, anomaly_at=NOW(), updated_at=NOW()
		WHERE node_id=$2 AND id<>$3 AND status<>'retired' RETURNING id`, transitID, destNodeID, keepBinID)
	if err != nil {
		return nil, fmt.Errorf("evict stale bin(s) from node %d: %w", destNodeID, err)
	}
	defer rows.Close()
	var evicted []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan evicted bin id at node %d: %w", destNodeID, err)
		}
		evicted = append(evicted, id)
	}
	return evicted, rows.Err()
}
