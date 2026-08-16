package helpers

import (
	"database/sql"
	"fmt"
	"time"

	"shingocore/store/reservations"
)

// place_bin.go — THE ONE PLACEMENT.
//
// ── WHY THIS EXISTS ───────────────────────────────────────────────────────
//
// Where a bin IS has exactly one home already: bins.node_id. This was never a
// homeless-fact problem, it was a WRITER problem — three code paths put a bin
// down, each doing the same five things in its own words, and the words drifted:
//
//	service.BinService.applyArrival          the single-bin arrival + the mid-plan set-down
//	store.ApplyMultiBinArrival               the multi-bin settle
//	recovery.RepairConfirmedOrderCompletion  the operator's completion repair
//
// Every defect this batch opened with lived in that drift. 445f79eb scoped
// applyArrival's unclaim to the placing order and coupled the bin reservation to
// it — and the other two writers did not get the fix, so a dig leg setting a
// blocker down still cleared whoever's claim it found. Before that, fe252c57
// made intermediate stores actually record and every stored bin came back
// unclaimed, because the only placement primitive available unclaimed
// unconditionally: 13 stranded bins against 1 on the lane-stress rig.
//
// EvictStaleGhostBinsTx is the precedent, one file over: it was extracted for
// exactly this reason ("so the paths cannot drift and no caller can forget the
// synthetic exemption") and it worked. This is the rest of the placement.
//
// ── WHAT IT OWNS, AND WHY EACH PIECE IS INSIDE RATHER THAN BESIDE ─────────
//
//  1. GHOST EVICTION, first. A completed delivery is physical proof the slot was
//     empty, so a different bin still recorded there is stale. Outside the
//     primitive it is a step a writer can forget — and the repair path did
//     forget it, until it was added there by hand.
//  2. THE node_id WRITE. The fact itself.
//  3. THE OWNER-SCOPED UNCLAIM. A handoff gives up THIS ORDER'S claim, not
//     whoever's is on the bin. Compound siblings stay exempt, deliberately.
//  4. THE BIN RESERVATION, coupled to (3): released only when the claim actually
//     ended here. Releasing it unconditionally strips the reservation off a bin
//     whose claim was correctly left standing — the same defect one layer down.
//  5. THE DESTINATION SLOT's claim and reservation. A bin has arrived, so the
//     dropoff claim is fulfilled.
//  6. THE STAGING STATE.
//
// Splitting 3 from 4, or 5 from 2, is what produced every bug above. They are
// one transaction's worth of one decision.
//
// ── WHAT IT DOES NOT OWN ──────────────────────────────────────────────────
//
// The burial-shadow instrument, which is post-commit and result-free and belongs
// to the callers that have an opinion about what a placement MEANT. And the
// order-side bookkeeping (completion stamps, junction rows, history) — this
// primitive is about where a bin is, and nothing else.

// BinPlacement is one bin going down at one node.
//
// It is a struct rather than eight positional parameters because the last three
// are two bools and a pointer, and a caller swapping `staged` for `releaseClaim`
// would compile silently and produce the fe252c57 regression exactly.
type BinPlacement struct {
	BinID    int64
	ToNodeID int64

	// PlacedByOrder is the order whose placement this is. It SCOPES THE UNCLAIM
	// and it cannot be inferred from the bin's own claim: a compound child's
	// placement routinely reads claimed_by as a SIBLING's id, because
	// CreateCompoundChildren writes claims for every step in one transaction and
	// the last write wins for a bin appearing in several. Every production
	// caller has its own order in hand.
	PlacedByOrder int64

	// ReleaseClaim distinguishes a HANDOFF (true — the order is done with the
	// bin and gives it up) from a MID-PLAN SET-DOWN (false — the order will come
	// back for it, so the claim and the reservation that tracks it must survive).
	//
	// A store is not a handoff. Calling a handoff primitive for something that is
	// not a handoff is precisely the fe252c57 regression: every stored bin came
	// back unclaimed, the delivery path's teleport guard then refused it — rightly,
	// since unclaimed means no order vouches for where it is — and the bin stranded
	// at _TRANSIT with a carrier inside it while the cell it belonged to read empty.
	ReleaseClaim bool

	// ReleaseDestinationSlot releases the destination node's dispatch-time claim
	// and its slot reservation — the slot dual of the two bin releases above.
	//
	// True for the arrival writers, which is where the dispatch-time ClaimSlot is
	// taken. The repair path passes false, because an operator repair is
	// reconstructing a completion that already happened and the slot's claim, if
	// any, belongs to whatever is actually driving there now.
	ReleaseDestinationSlot bool

	// Staged and ExpiresAt are the arriving bin's staging state. A nil ExpiresAt
	// with Staged means staged with no countdown.
	Staged    bool
	ExpiresAt *time.Time
}

// PlaceBinTx puts one bin down, inside the caller's transaction.
//
// Returns the ids of any stale ghosts evicted at the destination, so callers can
// surface them as operator alerts. A normal arrival onto an empty slot returns
// nil and does no extra work.
//
// THE TRANSACTION IS THE CALLER'S, deliberately: every one of the three writers
// is placing a bin as part of something larger (an order completion, a multi-bin
// settle, a repair that also stamps completed_at), and a primitive that opened
// its own transaction would make those non-atomic — which is the partial-write
// class the R.26 assert exists to refuse.
func PlaceBinTx(tx *sql.Tx, p BinPlacement) ([]int64, error) {
	// 1. Reconcile occupancy BEFORE placing the newcomer. Synthetic nodes are
	//    exempt, handled inside the helper.
	evicted, err := EvictStaleGhostBinsTx(tx, p.ToNodeID, p.BinID)
	if err != nil {
		return nil, fmt.Errorf("reconcile stale ghost at node %d: %w", p.ToNodeID, err)
	}

	// 2. The fact.
	if _, err := tx.Exec(
		`UPDATE bins SET node_id=$1, updated_at=NOW() WHERE id=$2`, p.ToNodeID, p.BinID); err != nil {
		return nil, fmt.Errorf("move bin %d: %w", p.BinID, err)
	}

	if p.ReleaseClaim {
		// 3. A HANDOFF GIVES UP THIS ORDER'S CLAIM, NOT WHOEVER'S IS THERE.
		//
		// This was `WHERE id=$1` and nothing else at all three writers, so a
		// placement cleared the claim on the bin regardless of who held it. The
		// bin usually IS the placer's, which is why it stood; the case it breaks
		// is the one this plant makes constantly. Lane-stress rig 2026-08-13: dig
		// leg order 9 set blocker bin 6 down at LSD_010 and wiped order 1's claim
		// on it; order 1 then picked up its OWN bin with no claim, its
		// intermediate set-down found "0 bins in transit under this claim", and
		// final delivery refused a robot carrying a bin nobody owned. Twice in one
		// 17-minute window.
		//
		// COMPOUND SIBLINGS STAY EXEMPT, deliberately. A leg's placement routinely
		// finds a SIBLING's id on the bin it is putting down; scoping strictly to
		// the placer would leave those claimed until the compound terminalized and
		// stall re-reservation of bins the dig has finished with. Same compound is
		// the same owner for this purpose.
		res, err := tx.Exec(`
			UPDATE bins SET claimed_by=NULL, updated_at=NOW()
			 WHERE id=$1
			   AND (claimed_by IS NULL
			        OR claimed_by = $2
			        OR EXISTS (
			             SELECT 1 FROM orders placer, orders holder
			              WHERE placer.id = $2
			                AND holder.id = bins.claimed_by
			                AND placer.parent_order_id IS NOT NULL
			                AND holder.parent_order_id = placer.parent_order_id))`,
			p.BinID, p.PlacedByOrder)
		if err != nil {
			return nil, fmt.Errorf("unclaim bin %d: %w", p.BinID, err)
		}
		// 4. The reservation lives exactly as long as the claim — released only
		//    when the claim actually ended HERE.
		if n, _ := res.RowsAffected(); n > 0 {
			if err := reservations.ReleaseByBin(tx, p.BinID); err != nil {
				return nil, fmt.Errorf("release reservation on bin %d: %w", p.BinID, err)
			}
		}
	}

	if p.ReleaseDestinationSlot {
		// 5. The slot dual: the bin has arrived, so the dropoff claim is
		//    fulfilled and the slot frees for re-reservation now rather than at
		//    the owning order's terminal transition. Both are no-ops for a LINE
		//    delivery, which is never slot-claimed or slot-reserved.
		if _, err := tx.Exec(
			`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE id=$1`, p.ToNodeID); err != nil {
			return nil, fmt.Errorf("release destination slot claim node %d: %w", p.ToNodeID, err)
		}
		if err := reservations.ReleaseByNode(tx, p.ToNodeID); err != nil {
			return nil, fmt.Errorf("release slot reservation on node %d: %w", p.ToNodeID, err)
		}
	}

	// 6. Staging state.
	if p.Staged {
		if _, err := tx.Exec(`UPDATE bins
			SET status='staged', staged_at=NOW(), staged_expires_at=$1, updated_at=NOW()
			WHERE id=$2`, NullableTime(p.ExpiresAt), p.BinID); err != nil {
			return nil, fmt.Errorf("stage bin %d: %w", p.BinID, err)
		}
	} else {
		if _, err := tx.Exec(`UPDATE bins
			SET status='available', staged_at=NULL, staged_expires_at=NULL, updated_at=NOW()
			WHERE id=$1`, p.BinID); err != nil {
			return nil, fmt.Errorf("set available bin %d: %w", p.BinID, err)
		}
	}

	return evicted, nil
}
