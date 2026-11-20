package store

import (
	"database/sql"

	"shingocore/config"
)

// ApplyBaselineForTest runs applyBaseline, the one Exec store.migrate births a
// new database with, on any text: the atomicity test hands it the baseline
// with a failing statement after it. It compiles only into the test binary.
func ApplyBaselineForTest(db *DB, baseline string) error { return applyBaseline(db.DB, baseline) }

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

// StealSoftHoldsInTx is stealSoftHolds in the caller's transaction, so a test
// can hold it open at its writes (LOCKS.md §3, the dig race).
func StealSoftHoldsInTx(tx *sql.Tx, binID, childID, parentID int64) error {
	_, _, err := stealSoftHolds(tx, binID, childID, parentID, "")
	return err
}

// ResetPublicObjectsForTest runs ResetDatabase's fallback, the reset that drops
// every object in schema public instead of the schema, on cfg's database: the
// servers the tests run on let the schema drop through, so the fallback is
// reached here directly.
func ResetPublicObjectsForTest(cfg *config.DatabaseConfig) error {
	db, err := openPgx(&cfg.Postgres)
	if err != nil {
		return err
	}
	defer db.Close()
	return resetPublicObjects(db)
}
