package orders

import (
	"database/sql"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingoedge/store/internal/helpers"
)

// CoreWordRow is one word of Core's about an order, as the versioned applier
// writes it (orders.Manager.ApplyCoreWord): Core's status and the facts that
// ride it, at Core's version.
type CoreWordRow struct {
	Status string
	// RobotID "" keeps the stored robot: an empty robot on the wire is absent,
	// not a clear.
	RobotID string
	// ETA "" keeps the stored ETA.
	ETA string
	// QueueReason and QueueCode are written as given; the applier passes the
	// word's only while the order waits, and "" otherwise.
	QueueReason, QueueCode string
	// KeepFault leaves the stored fault clock as it is (a word that carries
	// none, the projection). Otherwise the clock is written as given, and the
	// applier gives one only while the order is faulted.
	KeepFault         bool
	FaultSince        *time.Time
	FaultDeadline     *time.Time
	FaultNoticeAfterS int
	FaultRef          string
	Version           int64
}

// ApplyCoreWord writes a word of Core's onto the order in ONE statement, and
// only if its version is newer than the last one applied: the compare is in the
// UPDATE's WHERE (core_version < ?), so two words applied at once cannot both
// land and an older one cannot overwrite a newer. Reports whether it wrote.
//
// In the same statement: a status that ends the order takes its release intent
// with it (the act's promise has no leg left), and a move to staged stamps
// staged_at with the engine's clock (UpdateStatus's rule: the idle rule's hour
// starts there).
func ApplyCoreWord(db *sql.DB, id int64, w CoreWordRow) (bool, error) {
	terminal := protocol.IsTerminal(protocol.Status(w.Status))
	res, err := db.Exec(`UPDATE orders SET
		status = ?,
		robot_id = CASE WHEN ? = '' THEN robot_id ELSE ? END,
		eta = CASE WHEN ? = '' THEN eta ELSE ? END,
		queue_reason = ?, queue_code = ?,
		fault_since = CASE WHEN ? THEN fault_since ELSE ? END,
		fault_deadline = CASE WHEN ? THEN fault_deadline ELSE ? END,
		fault_notice_after_s = CASE WHEN ? THEN fault_notice_after_s ELSE ? END,
		fault_ref = CASE WHEN ? THEN fault_ref ELSE ? END,
		release_intent = CASE WHEN ? THEN '' ELSE release_intent END,
		staged_at = CASE WHEN ? = 'staged' AND status <> 'staged' THEN ? ELSE staged_at END,
		core_version = ?,
		updated_at = datetime('now')
		WHERE id = ? AND core_version < ?`,
		w.Status,
		w.RobotID, w.RobotID,
		w.ETA, w.ETA,
		w.QueueReason, w.QueueCode,
		w.KeepFault, faultInstant(w.FaultSince),
		w.KeepFault, faultInstant(w.FaultDeadline),
		w.KeepFault, w.FaultNoticeAfterS,
		w.KeepFault, w.FaultRef,
		terminal,
		w.Status, clock.Now().UTC().Format(helpers.TimeLayout),
		w.Version,
		id, w.Version)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// NoteCoreVersion records that a word of Core's at version was read for an
// order whose status it does not move (one that has already ended), so an
// older word arriving later is known to be older. One statement; no-op when
// the version is not newer.
func NoteCoreVersion(db *sql.DB, id, version int64) error {
	_, err := db.Exec(`UPDATE orders SET core_version = ? WHERE id = ? AND core_version < ?`, version, id, version)
	return err
}

// LiveMoveOffNode reports whether a live move order already takes a carrier
// off node: the guard that makes sending a carrier back once per carrier, not
// once per message about it (a redelivered order.cancelled names the same
// carrier again). One statement.
func LiveMoveOffNode(db *sql.DB, node string) (bool, error) {
	var live bool
	err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM orders
		WHERE source_node = ? AND order_type = ? AND status NOT IN (`+protocol.TerminalStatusSQLList()+`))`,
		node, string(protocol.OrderTypeMove)).Scan(&live)
	return live, err
}

// CancelledAtAWaitWithNothingSince lists, oldest first, the complex legs of a
// line that were cancelled while staged at a station wait (their history holds
// the staged to cancelled row) and after which no order of their line was made,
// their sibling apart: the cancels whose carrier the Edge may still owe a send
// back. The send-back is itself an order of the line, so a leg drops out of this
// list once it is made, and so does one any later order of the line follows.
// One statement.
func CancelledAtAWaitWithNothingSince(db *sql.DB) ([]int64, error) {
	rows, err := db.Query(`SELECT o.id FROM orders o
		WHERE o.status = ? AND o.order_type = ? AND o.process_node_id IS NOT NULL AND o.station_wait IS NOT NULL
		  AND EXISTS (SELECT 1 FROM order_history h WHERE h.order_id = o.id AND h.old_status = ? AND h.new_status = ?)
		  AND NOT EXISTS (SELECT 1 FROM orders n WHERE n.process_node_id = o.process_node_id AND n.id > o.id
		                  AND n.id IS NOT o.sibling_order_id)
		ORDER BY o.id`,
		string(protocol.StatusCancelled), string(protocol.OrderTypeComplex),
		string(protocol.StatusStaged), string(protocol.StatusCancelled))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
