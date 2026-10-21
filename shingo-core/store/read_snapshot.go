package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ReadSnapshot runs fn's reads in one read-only REPEATABLE READ transaction:
// every statement fn sends through tx sees the same snapshot, the one taken at
// its first statement, so reads of two tables cannot tear between them. It
// writes nothing; the transaction is committed after fn and rolled back on an
// error. BEGIN and COMMIT are statements of their own (QueryCounter counts
// them).
//
// A free function on *sql.DB rather than a *DB method, per the method-surface
// convention on DB. The one caller is the bins snapshot build
// (messaging.buildNodeBins), whose orders and bins reads must agree.
func ReadSnapshot(db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin read snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
