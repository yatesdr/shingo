package store

// Delegate file: the process part set lives in store/processes/. This
// preserves the *store.DB method surface the services and tests use.

import "shingoedge/store/processes"

// ListProcessPayloads returns the stored half of a process's part set — the
// rows an engineer typed — in payload-code order.
func (db *DB) ListProcessPayloads(processID int64) ([]string, error) {
	return processes.ListProcessPayloads(db.DB, processID)
}

// ReplaceProcessPayloads sets the stored half to exactly this list.
func (db *DB) ReplaceProcessPayloads(processID int64, codes []string) error {
	return processes.ReplaceProcessPayloads(db.DB, processID, codes)
}

// ProcessPalette is the offer list: the stored rows unioned with every payload
// the process's live claims already name.
func (db *DB) ProcessPalette(processID int64) ([]string, error) {
	return processes.ProcessPalette(db.DB, processID)
}
