package store

import (
	"database/sql"

	"shingo/protocol"
	"shingocore/store/messaging"
	"shingocore/store/orders"
)

// order_tell.go — the station's tell, queued by the write it reports.
//
// ONE TELL, AT THE WRITE. Every write Core tells an order's station about — a
// status, a robot that changed, a wait that changed while the order waits —
// queues one order.update on that write's own transaction, built from the row
// the write's statement read back (orders.Told) and carrying the version the
// same statement bumped. The outbox row commits if and only if the write does:
// a write with no tell, or a tell for a write that never landed (a refused
// transition, a lost compare-and-swap), cannot happen. Before this, each call
// site told the station beside the write, after its commit, and five writers
// told nobody (a release, a coordinated dispatch, a reshuffle start, a
// force-recover, the bypass fails), so the station showed `staged` while the
// robot drove.
//
// The dedicated messages (order.staged, order.delivered, order.error,
// order.cancelled, order.skipped, the waybill and the ack) are unchanged: they
// carry the facts a status does not. This tell is the status.

// stationTells is where a tell is queued: the dispatch topic and this Core's
// station, the envelope's sender.
type stationTells struct {
	topic       string
	coreStation string
}

// SetStationTells names the topic and Core station every order's station tell
// is queued under. Set once by the engine when it starts. A store with none set
// (a tool, a store test) queues no tell.
func (db *DB) SetStationTells(topic, coreStation string) {
	db.tells.Store(&stationTells{topic: topic, coreStation: coreStation})
}

// queueTellTx queues the order's station tell on tx, from the row the write
// read back and the facts the row does not hold (tmpl: the sentence, the ETA,
// the fault clock). detail is the write's own history sentence, used when tmpl
// names none.
//
// Nothing is queued for an order no station holds: one with no station or no
// edge uuid, or one its station has no row for (station_unaware). The queue
// fields ride only while the order waits, exactly as before: an empty one on
// the wire reads as absent, and the station keeps a reason only while waiting.
func (db *DB) queueTellTx(tx *sql.Tx, told orders.Told, tmpl protocol.OrderUpdate, detail string) error {
	t := db.tells.Load()
	if t == nil || t.topic == "" || told.EdgeUUID == "" || told.StationID == "" || told.StationUnaware {
		return nil
	}
	u := tmpl
	u.OrderUUID = told.EdgeUUID
	u.Status = string(told.Status)
	u.RobotID = told.RobotID
	u.QueueReason, u.QueueCode = "", ""
	if protocol.IsWaiting(told.Status) {
		u.QueueReason, u.QueueCode = told.QueueReason, told.QueueCode
	}
	u.Version = told.Version
	if u.Detail == "" {
		u.Detail = detail
	}
	return messaging.EnqueueToEdge(tx, t.topic, protocol.TypeOrderUpdate, t.coreStation, told.StationID, &u)
}
