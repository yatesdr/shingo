package processes

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// reorderHoldRank is the held kind's rank, in SQL, built from
// domain.ReorderHolds so the write's guard and domain's Rank cannot disagree:
// 0 for none and for a word this Edge does not know.
var reorderHoldRank = func() string {
	var b strings.Builder
	b.WriteString("CASE reorder_hold")
	for _, k := range domain.ReorderHolds {
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", k, k.Rank())
	}
	b.WriteString(" ELSE 0 END")
	return b.String()
}()

// HoldReorder sets a position's auto-reorder hold: kind, set at at (the
// engine's clock), with note, who or what set it in the board's words. It
// writes nothing when the position holds a kind ranked above kind, and reports
// whether it wrote. A kind equal to the one held is written over: a second
// pause restamps the first.
//
// ONE STATEMENT; THE RANK IS ITS WHERE. A read-then-write would let two cancels
// of one swap, arriving back to back, each read the row before the other wrote.
//
// kind must hold (domain.ReorderHolds). Clearing a hold is not this write's:
// only a go clears, and it clears by its own rule.
func HoldReorder(db *sql.DB, processNodeID int64, kind domain.ReorderHold, at time.Time, note string) (bool, error) {
	if kind.Rank() == 0 {
		return false, fmt.Errorf("hold reorder on node %d: %q is not a hold", processNodeID, string(kind))
	}
	res, err := db.Exec(`UPDATE process_node_runtime_states SET reorder_hold=?, reorder_hold_at=?, reorder_hold_note=?
		WHERE process_node_id=? AND `+reorderHoldRank+` <= ?`,
		string(kind), at.UTC().Format(helpers.TimeLayout), note, processNodeID, kind.Rank())
	if err != nil {
		return false, fmt.Errorf("hold reorder on node %d: %w", processNodeID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("hold reorder on node %d: %w", processNodeID, err)
	}
	return n > 0, nil
}
