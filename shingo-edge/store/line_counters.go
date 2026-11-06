package store

import (
	"time"

	"shingoedge/store/counters"
	"shingoedge/store/processes"
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

func (db *DB) LineCounterByTag(plcName, tagName string) (*counters.LineCounter, error) {
	return counters.LineCounterByTag(db.DB, plcName, tagName)
}

func (db *DB) ReportingPointOnTag(plcName, tagName string) (bool, error) {
	return counters.ReportingPointOnTag(db.DB, plcName, tagName)
}

func (db *DB) RenameLineCounter(id int64, label string) error {
	return counters.RenameLineCounter(db.DB, id, label)
}

func (db *DB) RepointLineCounter(id int64, label, tagName string, managed bool) error {
	return counters.RepointLineCounter(db.DB, id, label, tagName, managed)
}

func (db *DB) AnchorLineCounter(id int64, plcName, tagName string, count int64, at time.Time) error {
	return counters.AnchorLineCounter(db.DB, id, plcName, tagName, count, at)
}

func (db *DB) ClearLineCounterAnchors(processID int64) error {
	return counters.ClearLineCounterAnchors(db.DB, processID)
}

func (db *DB) RaiseLineCounterDeepest(id, deepest int64) error {
	return counters.RaiseLineCounterDeepest(db.DB, id, deepest)
}

func (db *DB) SetLineCounterManaged(id int64, managed bool) error {
	return counters.SetLineCounterManaged(db.DB, id, managed)
}

// DeleteLineCounter deletes a station counter, unbinding its positions; the
// node generation moves when any position was unbound.
func (db *DB) DeleteLineCounter(id int64) error {
	unbound, err := counters.DeleteLineCounter(db.DB, id)
	if err == nil && unbound > 0 {
		processes.BumpNodeGeneration()
	}
	return err
}

// SetNodeLineCounter binds a position to a station counter (nil unbinds it);
// the node generation moves when the binding changed.
func (db *DB) SetNodeLineCounter(nodeID int64, counterID *int64) error {
	changed, err := counters.SetNodeLineCounter(db.DB, nodeID, counterID)
	if err == nil && changed {
		processes.BumpNodeGeneration()
	}
	return err
}
