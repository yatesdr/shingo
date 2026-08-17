package store

// MigrateForTest re-exports the unexported migrate() so tests in package
// store_test can force a second migration pass (the self-heal) — e.g. to prove a
// retired table is not resurrected. It compiles only into the test binary and is
// not part of the production API.
func (db *DB) MigrateForTest() error { return db.migrate() }
