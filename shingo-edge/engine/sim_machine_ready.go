//go:build sim

package engine

import (
	"database/sql"

	"shingo/protocol"
)

// SimMachineReady answers ONE question for the whole rig: is this process's
// machine able to cycle right now?
//
// ── WHY IT IS ONE FUNCTION AND NOT TWO ────────────────────────────────────
//
// Two things need this answer and they must never disagree.
//
// The fake PLC asks it to decide whether to advance a counter — a real machine
// only increments when it cycles, and one that is starved, blocked or holding
// no carrier does not. That is where this logic started
// (cmd/shingoedge/sim_enabled.go, makeReadinessGate).
//
// The sim OPERATOR asks it to decide whether to press RELEASE. A carrier is
// swapped when it is done, and "done" turns out to be the same set of facts:
//
//	consume node at remaining <= 0      the carrier is EMPTY      -> swap it
//	produce node at remaining >= cap    the carrier is FULL       -> swap it
//	any active node with no bin bound   there is nothing there    -> deliver one
//
// Every one of those stops the machine, and every one of them is fixed by the
// swap that is waiting to be released. So "the carrier is done" and "the machine
// has stopped" are the same condition read from two sides, and writing them
// twice is how they drift apart.
//
// ── AND IT IS WHY THE OPERATOR GATE CANNOT DEADLOCK ───────────────────────
//
// This cost three rig runs to learn. Gating the release on a carrier's own count
// deadlocks, because the count only moves while the machine runs and the machine
// is stopped by the very condition the swap would clear:
//
//	2026-09-06, gating on Core's bin telemetry: the readiness gate stopped WELD-2
//	at remaining_uop_cached = 0 while Core's ledger still showed 2, so the machine
//	was down and the gate saw stock. 14 orders staged from minute one to hour
//	three; the hold fired 10,276 times.
//
//	2026-09-06, gating on the runtime cache per node: ALN_005 (produce, ASSY) held
//	NO carrier, so it could never reach full pack — and the swap being held was
//	the one delivering it. WELD-2 stopped, and its two consume nodes froze at 5
//	and 20 with their own swaps held waiting for a zero that could not arrive.
//
// Asking about the PROCESS rather than the node closes both: whatever stops the
// machine releases every swap staged at it. A stopped cell swaps out whatever is
// staged, partial or not, which is what a person standing at a dead cell does —
// they do not wait for a count that has stopped moving.
//
// Fail-OPEN on any error. A rig that stops swapping is worse than one that swaps
// a carrier early, and a DB blip must not stop the line.
func SimMachineReady(db *sql.DB, processID, styleID int64) bool {
	rows, err := db.Query(`
		SELECT c.role, c.swap_mode, c.uop_capacity, r.active_bin_id, r.remaining_uop_cached, r.active_pull
		FROM process_nodes pn
		JOIN style_node_claims c ON c.style_id = ? AND c.core_node_name = pn.core_node_name
		JOIN process_node_runtime_states r ON r.process_node_id = pn.id
		WHERE pn.process_id = ?`, styleID, processID)
	if err != nil {
		return true
	}
	defer rows.Close()

	for rows.Next() {
		var role, swapMode string
		var uopCap, remainingUOP int
		var activeBinID sql.NullInt64
		var activePull bool
		if err := rows.Scan(&role, &swapMode, &uopCap, &activeBinID, &remainingUOP, &activePull); err != nil {
			return true
		}
		// manual_swap nodes are operator-managed, not PLC-ticked.
		if swapMode == string(protocol.SwapModeManualSwap) {
			continue
		}
		// Parked A/B side (active_pull=false): the line isn't filling or draining
		// it right now, so its fill level does not gate the cell — the active
		// partner does. It may legitimately sit full while parked, awaiting its
		// swap-out. The bound-bin checks below only apply to nodes the line is
		// actually working.
		if !activePull {
			continue
		}
		// All active non-manual_swap nodes need a bound carrier.
		if !activeBinID.Valid || activeBinID.Int64 == 0 {
			return false // nothing on the position
		}
		// Consume nodes need UOP > 0 — a real cell cannot cycle an empty input,
		// so the counter must stop rather than drive the count negative.
		if role == "consume" && remainingUOP <= 0 {
			return false // starved
		}
		// Produce nodes must stop when the output carrier is full — a real machine
		// cannot cycle into a full bin. The relief swap (or an A/B flip) carries it
		// out and binds an empty, then the gate reopens. Without this the count
		// drives past capacity. A/B headroom comes from the parked partner, which
		// is skipped above and becomes active on the flip.
		if role == "produce" && uopCap > 0 && remainingUOP >= uopCap {
			return false // output full
		}
	}
	return true
}
