package store

import (
	"time"

	"shingoedge/store/counters"
)

// The station counters (store/counters/line_counters.go).

func (db *DB) CreateLineCounter(processID int64, label, plcName, tagName string) (int64, error) {
	return counters.CreateLineCounter(db.DB, processID, label, plcName, tagName)
}

func (db *DB) GetLineCounter(id int64) (*counters.LineCounter, error) {
	return counters.GetLineCounter(db.DB, id)
}

func (db *DB) ListLineCounters(processID int64) ([]counters.LineCounter, error) {
	return counters.ListLineCounters(db.DB, processID)
}

func (db *DB) ListPolledLineCounters() ([]counters.LineCounter, error) {
	return counters.ListPolledLineCounters(db.DB)
}

func (db *DB) UpdateLineCounterRead(id int64, plcName, tagName string, count int64, at time.Time) (bool, error) {
	return counters.UpdateLineCounterRead(db.DB, id, plcName, tagName, count, at)
}

func (db *DB) DeleteLineCounter(id int64) error {
	return counters.DeleteLineCounter(db.DB, id)
}

func (db *DB) SetNodeLineCounter(nodeID int64, counterID *int64) error {
	return counters.SetNodeLineCounter(db.DB, nodeID, counterID)
}
