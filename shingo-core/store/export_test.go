package store

// MigrateForTest re-exports the unexported migrate() so tests in package
// store_test can force a second migration pass (the self-heal) — e.g. to prove a
// retired table is not resurrected. It compiles only into the test binary and is
// not part of the production API.
func (db *DB) MigrateForTest() error { return db.migrate() }

// StealSoftHoldsForTest runs a dig child's steal of a bin's soft holds in its
// own transaction (LOCKS.md §3: its lock order against a terminalize).
func (db *DB) StealSoftHoldsForTest(binID, childID, parentID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, _, err := stealSoftHolds(tx, binID, childID, parentID, ""); err != nil {
		return err
	}
	return tx.Commit()
}
