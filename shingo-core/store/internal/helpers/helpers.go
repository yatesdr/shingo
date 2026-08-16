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
	"log"
	"time"

	"shingo/protocol"
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

// NullableText maps a Go empty string to a SQL NULL. For columns where "" and
// NULL are meaningfully different — queue_code/queue_cause (a pre-schema row
// reads back NULL, not the empty code) and orders.origin_id (a UUID column that
// rejects "" outright, and whose partial index keys on IS NOT NULL). Non-empty
// strings pass through.
func NullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// NullableTime converts *time.Time to a UTC value safe for SQL params (nil-safe).
func NullableTime(p *time.Time) any {
	if p != nil {
		return p.UTC()
	}
	return nil
}

// QueryRower is satisfied by both *sql.DB and *sql.Tx, so an insert primitive
// built on it can run standalone or inside a caller's transaction. Same idea as
// bins.binExecer, narrowed to the single method InsertID needs — deliberately
// NOT widened to Exec/Query, because a caller that needs those should take the
// concrete handle and say so.
type QueryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// InsertID executes an INSERT ... RETURNING id query and returns the new row ID.
func InsertID(db QueryRower, query string, args ...any) (int64, error) {
	var id int64
	err := db.QueryRow(query, args...).Scan(&id)
	return id, err
}

// EvictStaleGhostBinsTx reconciles the one-bin-per-physical-node invariant at
// arrival time. Any non-retired bin OTHER than keepBinID that shingo still
// records at destNodeID is moved to _TRANSIT with anomaly_at stamped, inside
// tx, and its id is returned so the caller can surface it via ListAnomalies /
// RecoverTransitAnomaly.
//
// Its CLAIM is kept when the holder is still live and cleared only when it is
// dead — see the ownership note at the UPDATE for why the two facts are not
// evicted together. A bin that keeps its claim is still returned as evicted:
// its position changed, which is what the callers act on.
//
// Why the conflicting record is stale (plant-verified 2026-07-08): a delivery
// physically CANNOT complete onto an occupied slot, so a completed delivery is
// itself proof the slot was empty. RDS emits no fault code and does not track
// occupancy — the proof is the physical completion, not a vendor error. So the
// RECORD is wrong and the newcomer is right; evict it, never the reverse.
//
// ── TWO CAUSES, NOT ONE, AND THE SECOND IS OURS ───────────────────────────
//
// This paragraph used to end "a stale ghost an untracked manual move left
// behind", as though an operator outside the system were the only way to get
// here. That was false when it was written and PLAN §R.5/§R.6 proved it: the
// clobbered swap re-bound BOTH of an order's dropoffs to one lane slot, so the
// order delivered two bins to one node and the occupant this function evicted
// was manufactured by CORE'S OWN CORRUPTED PLAN. The eviction was quietly
// laundering the evidence of the defect that produced it, and the false premise
// is what made that read as routine housekeeping.
//
// The two are distinguishable at the moment of eviction, and the discriminator
// is the occupant's CLAIM:
//
//   - NO LIVE HOLDER → an orphan. Nobody is coming for it, which is what an
//     untracked manual move or an abandoned record looks like. Evicted and
//     surfaced on the anomalies page.
//   - A LIVE HOLDER → Core put two orders on one node. The claim is PRESERVED
//     (see the ownership note below) and the eviction logs a WARN naming the
//     holder — that line is the tripwire, and reaching it means something
//     upstream recorded a bin in a slot a delivery landed on. It is a defect
//     report, not a rescue notice; the rescue does not excuse whatever caused it.
//
// So a reader arriving here with an eviction in hand should read the WARN before
// concluding anything about manual moves.
//
// Synthetic nodes (LANE/NGRP/_TRANSIT) hold many bins by design and are exempt.
// The _TRANSIT lookup is lazy — only on the rare collision, not every arrival.
// This is the ONE reconciliation the arrival-writers share — single-bin
// (service.BinService.ApplyArrival), multi-bin (store.ApplyMultiBinArrival), and
// completion-repair (recovery.RepairConfirmedOrderCompletion) — so the paths
// cannot drift and no caller can forget the synthetic exemption. It lives in
// store/internal so store-layer and recovery-sub-package callers reach it
// without an import cycle.
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
	// ── THE POSITION IS EVICTED; OWNERSHIP IS NOT ────────────────────────────
	// This used to null claimed_by unconditionally, and that is a different
	// claim than the one the eviction is entitled to make. "A completed delivery
	// proves this slot was empty" is a statement about POSITION. It says nothing
	// about who owns the bin whose position was wrong, and a bin claimed by a
	// LIVE order is one a robot may be carrying right now.
	//
	// Wiping that claim broke the carrier: the delivering order's own arrival
	// then read as a teleport to the arrival guard (the bin is not claimed by
	// me), the guard refused it, the bin stranded at _TRANSIT owned by nobody,
	// and the order confirmed anyway — reporting a delivery it did not make.
	// Observed end to end on the rig, one bin refused twice for two different
	// orders 76s apart (PLAN §R.5).
	//
	// So the claim survives when its holder is live, and is cleared only when it
	// is genuinely dead — no holder, or a holder that already went terminal.
	// That is the same "verify, don't assume" the position side gets.
	rows, err := tx.Query(fmt.Sprintf(`UPDATE bins SET
			node_id=$1, anomaly_at=NOW(), updated_at=NOW(),
			claimed_by = CASE WHEN EXISTS (
				SELECT 1 FROM orders o WHERE o.id = bins.claimed_by AND o.status NOT IN (%s)
			) THEN bins.claimed_by ELSE NULL END
		WHERE node_id=$2 AND id<>$3 AND status<>'retired'
		RETURNING id, claimed_by`, protocol.TerminalStatusSQLList()),
		transitID, destNodeID, keepBinID)
	if err != nil {
		return nil, fmt.Errorf("evict stale bin(s) from node %d: %w", destNodeID, err)
	}
	defer rows.Close()
	var evicted []int64
	for rows.Next() {
		var id int64
		var heldBy sql.NullInt64
		if err := rows.Scan(&id, &heldBy); err != nil {
			return nil, fmt.Errorf("scan evicted bin id at node %d: %w", destNodeID, err)
		}
		if heldBy.Valid {
			// SWEEP-AS-TRIPWIRE. Reaching here means a bin a live order still owns
			// was recorded in a slot another delivery just completed into — two
			// orders pointed at one bin. The rescue keeps the carrier working, but
			// the position was wrong before this function ran and something upstream
			// put it there. A silent save is indistinguishable from no save.
			log.Printf("WARN: ghost eviction moved bin %d off node %d but KEPT its claim — "+
				"order %d is live and may be carrying it. The position was already wrong when "+
				"the delivery landed: find what recorded this bin here.",
				id, destNodeID, heldBy.Int64)
		}
		evicted = append(evicted, id)
	}
	return evicted, rows.Err()
}
