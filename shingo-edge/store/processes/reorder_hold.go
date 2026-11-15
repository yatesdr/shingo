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

// goClears is the SET a go writes: it clears a hold ranked at or below an off
// (a pause, an off) and leaves a person's off, which only a person clears, as
// it is. Every expression reads the row as it was before the statement.
var goClears = func() string {
	cleared := reorderHoldRank + fmt.Sprintf(" <= %d", domain.ReorderHoldOff.Rank())
	return `reorder_hold=CASE WHEN ` + cleared + ` THEN '' ELSE reorder_hold END,
		reorder_hold_at=CASE WHEN ` + cleared + ` THEN '' ELSE reorder_hold_at END,
		reorder_hold_note=CASE WHEN ` + cleared + ` THEN '' ELSE reorder_hold_note END`
}()

// GoReorder is a person saying go at a position: a REQUEST that passed its
// refusals. It clears a pause or an off, leaves a person's off, and stamps the
// position's last go (at, the engine's clock), in one statement.
func GoReorder(db DBTX, processNodeID int64, at time.Time) error {
	if _, err := db.Exec(`UPDATE process_node_runtime_states SET `+goClears+`, reorder_go_at=?
		WHERE process_node_id=?`, at.UTC().Format(helpers.TimeLayout), processNodeID); err != nil {
		return fmt.Errorf("go reorder on node %d: %w", processNodeID, err)
	}
	return nil
}

// GoReorderAtCoreNode is GoReorder at every live position of processID on
// coreNode, in one statement: Processes turning a claim's auto-reorder on,
// which names its position by core node.
func GoReorderAtCoreNode(db DBTX, processID int64, coreNode string, at time.Time) error {
	if _, err := db.Exec(`UPDATE process_node_runtime_states SET `+goClears+`, reorder_go_at=?
		WHERE process_node_id IN (SELECT id FROM process_nodes
		                          WHERE process_id=? AND core_node_name=? AND deleted_at IS NULL)`,
		at.UTC().Format(helpers.TimeLayout), processID, coreNode); err != nil {
		return fmt.Errorf("go reorder at %s on process %d: %w", coreNode, processID, err)
	}
	return nil
}

// ClearIdleOffAtProcess clears the idle rule's offs (domain.IsIdleOff, by the
// note it writes) set before lastPart at any position of the process, in one
// statement, and reports how many cleared. The rule wrote each at a line that
// had counted nothing for its hour, so a part counted after it is a restart,
// whether or not this Edge was up to see the stop: lastPart is the line's
// last counted part (reporting_points.last_part_at), never a reading with no
// part. An older Core's off and a pause are left as they are.
func ClearIdleOffAtProcess(db DBTX, processID int64, lastPart time.Time) (int64, error) {
	res, err := db.Exec(`UPDATE process_node_runtime_states SET reorder_hold='', reorder_hold_at='', reorder_hold_note=''
		WHERE reorder_hold=? AND substr(reorder_hold_note, 1, ?)=? AND reorder_hold_at < ?
		  AND process_node_id IN (SELECT id FROM process_nodes WHERE process_id=?)`,
		string(domain.ReorderHoldOff), len(domain.IdleOffNotePrefix), domain.IdleOffNotePrefix,
		lastPart.UTC().Format(helpers.TimeLayout), processID)
	if err != nil {
		return 0, fmt.Errorf("clear the idle rule's offs: %w", err)
	}
	return res.RowsAffected()
}

// ClearReorderOffAtProcess clears an off set before before at any position of
// the process, in one statement: the line has counted a part after a stop
// (engine/idle_cancel.go), which is one of the two things that end an off. A
// pause and a person's off are left as they are. It reports how many cleared.
func ClearReorderOffAtProcess(db DBTX, processID int64, before time.Time) (int64, error) {
	res, err := db.Exec(`UPDATE process_node_runtime_states SET reorder_hold='', reorder_hold_at='', reorder_hold_note=''
		WHERE reorder_hold=? AND reorder_hold_at < ?
		  AND process_node_id IN (SELECT id FROM process_nodes WHERE process_id=?)`,
		string(domain.ReorderHoldOff), before.UTC().Format(helpers.TimeLayout), processID)
	if err != nil {
		return 0, fmt.Errorf("clear reorder off: %w", err)
	}
	return res.RowsAffected()
}
