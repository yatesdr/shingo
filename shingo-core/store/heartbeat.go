package store

// Delegate file: production-heartbeat persistence lives in store/heartbeat/.
// Preserves the *store.DB method surface for the tick handlers (messaging)
// and the read service (service/heartbeat_service.go).

import (
	"time"

	"shingocore/store/heartbeat"
)

// InsertCellPartEvents projects ticks in one statement and returns the rows
// actually inserted (a re-sent tick is skipped by the table's unique key), and
// the rows the database refused when the page had to be retried row by row.
func (db *DB) InsertCellPartEvents(events []heartbeat.PartEvent) ([]heartbeat.PartEvent, []heartbeat.RejectedTick) {
	return heartbeat.InsertPartEvents(db.DB, events)
}

func (db *DB) ListCellPartEvents(cellID string, since, until time.Time) ([]heartbeat.PartEvent, error) {
	return heartbeat.ListEvents(db.DB, cellID, since, until)
}

func (db *DB) GetCellTarget(cellID, payloadCode string) (time.Duration, bool) {
	return heartbeat.GetTarget(db.DB, cellID, payloadCode)
}

// ── cell_config (Phase E, Q-025) ────────────────────────────────────────────

func (db *DB) ListCellConfigs() ([]heartbeat.CellConfig, error) {
	return heartbeat.ListCellConfigs(db.DB)
}

func (db *DB) GetCellConfig(cellID string) (heartbeat.CellConfig, bool, error) {
	return heartbeat.GetCellConfig(db.DB, cellID)
}

func (db *DB) UpsertCellConfig(c heartbeat.CellConfig) error {
	return heartbeat.UpsertCellConfig(db.DB, c)
}

func (db *DB) DeleteCellConfig(cellID string) error {
	return heartbeat.DeleteCellConfig(db.DB, cellID)
}

func (db *DB) DistinctCellProcesses(station string) ([]heartbeat.ProcessOption, error) {
	return heartbeat.DistinctProcesses(db.DB, station)
}

func (db *DB) EnsureHeartbeatPartitions(ref time.Time) error {
	return heartbeat.EnsurePartitions(db.DB, ref)
}

func (db *DB) EnsureHeartbeatPartitionsRange(start, end time.Time) error {
	return heartbeat.EnsurePartitionsRange(db.DB, start, end)
}

func (db *DB) DropOldHeartbeatPartitions(keepDays int, now time.Time) (int, error) {
	return heartbeat.DropOldPartitions(db.DB, keepDays, now)
}
