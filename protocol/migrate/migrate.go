// Package migrate is the shared, stdlib-only versioned-migration runner.
//
// One fact, one owner: applied-version bookkeeping for schema migrations is
// computed here, once, for both Core (Postgres) and Edge (SQLite). Before
// this package existed Core carried the only versioned runner and Edge had
// none; two private implementations of apply-once semantics would be one
// drift away from silent schema divergence on the plant floor, which is the
// failure this promotion exists to make structurally impossible
// (docs/shared-layer-promotion.md).
//
// The runner's two correctness layers, both extracted verbatim from Core's
// original private runner (shingo-core/store/migrations.go):
//
//  1. Transactional invariant: each migration's DDL/DML AND its
//     schema_migrations row insert commit in the same transaction
//     (runOne). Either both land or neither does.
//
//  2. Self-heal on startup: for migrations with a non-nil Verify, the
//     post-condition is checked before the schema_migrations gate is
//     trusted. A row that says "applied" without the state present is
//     deleted and the migration re-applied.
//
// Dialect differences are CONSTANT SQL ONLY — placeholder spelling
// ($1 vs ?) and the applied_at column type. If a dialect ever needs logic
// rather than a constant, that is the signal the abstraction is wrong, not
// a reason to grow a switch.
package migrate

import (
	"database/sql"
	"fmt"
	"log"
)

// Querier is the read surface Verify predicates receive. Its method set is
// identical to shingo-core's schema.Querier on purpose: a verify function
// written against either interface accepts the other's values without an
// adapter, so promoting a caller to this runner moves no verify code.
type Querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

// Dialect selects the constant SQL the runner uses. The zero value is
// Postgres, matching the dialect of the runner this package was extracted
// from.
type Dialect int

const (
	Postgres Dialect = iota
	SQLite
)

// createTableSQL per dialect. The Postgres text is byte-identical to the
// literal Core's private runner sent (pinned by TestDialectSQLMatchesTheExtraction),
// because Core's docker snapshot gates diff DDL, not intent.
var createTableSQL = map[Dialect]string{
	Postgres: `CREATE TABLE IF NOT EXISTS schema_migrations (
	version INTEGER PRIMARY KEY,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`,
	SQLite: `CREATE TABLE IF NOT EXISTS schema_migrations (
	version INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
)`,
}

// existsSQL, deleteSQL and insertSQL per dialect — identical apart from the
// placeholder spelling.
var (
	existsSQL = map[Dialect]string{
		Postgres: `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`,
		SQLite:   `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = ?)`,
	}
	deleteSQL = map[Dialect]string{
		Postgres: `DELETE FROM schema_migrations WHERE version = $1`,
		SQLite:   `DELETE FROM schema_migrations WHERE version = ?`,
	}
	insertSQL = map[Dialect]string{
		Postgres: `INSERT INTO schema_migrations (version) VALUES ($1)`,
		SQLite:   `INSERT INTO schema_migrations (version) VALUES (?)`,
	}
)

// Migration is one numbered, tracked schema change.
//
// Fn is the apply function — it runs inside a per-version transaction
// alongside the schema_migrations row insert (see runOne) and must be
// idempotent: the self-heal layer and any crash-retry can re-run it.
//
// Verify is the optional post-condition check — given a Querier, true iff
// the schema state the migration is supposed to produce is actually
// present. Run on startup BEFORE the schema_migrations gate: if a row says
// "applied" but Verify returns false, the runner deletes the row and
// re-applies. That catches prior deploys that recorded the version row
// without committing the DDL, and operator-induced drift (a DROPped column,
// a restored-from-backup schema_migrations ahead of actual schema). Nil
// Verify means "trust schema_migrations". Verify must be cheap — it runs
// on every startup for every applied migration.
type Migration struct {
	Version int
	Name    string
	Fn      func(tx *sql.Tx) error
	Verify  func(Querier) bool
}

// LatestVersion returns the highest version in the list (0 for an empty
// list). It scans for the max rather than trusting the last element, so a
// mis-ordered append cannot silently round the answer down; on every real
// list — append-only, ascending — the two agree.
func LatestVersion(migrations []Migration) int {
	max := 0
	for _, m := range migrations {
		if m.Version > max {
			max = m.Version
		}
	}
	return max
}

// CheckOrder reports the first entry whose version is not strictly greater
// than the one before it — a duplicate or an out-of-order append. It is the
// structural rule each module's list is pinned by, in place of a hard-coded
// head number, which conflicts with every stream that adds a migration.
func CheckOrder(migrations []Migration) error {
	for i := 1; i < len(migrations); i++ {
		prev, cur := migrations[i-1], migrations[i]
		if cur.Version <= prev.Version {
			return fmt.Errorf("migration v%d (%s) follows v%d (%s): versions must be strictly increasing",
				cur.Version, cur.Name, prev.Version, prev.Name)
		}
	}
	return nil
}

// Run applies every migration whose version is not recorded in
// schema_migrations, creating the table first if it is absent.
//
// baseline seeds the adoption seam for chains adopted mid-life: when
// baseline > 0 and the freshly-created table is EMPTY, one row for
// baseline is inserted, declaring every migration up to and including
// baseline already applied. Baseline 0 (Core) means the chain has owned
// the table since v1 — nothing to seed. Apply-once and self-heal
// behaviour is pinned by shingo-edge/store versioned_migration_test.go;
// Edge's frozen pre-baseline chain being untouched by adoption is pinned
// by shingo-edge/store frozen_chain_pin_test.go.
func Run(db *sql.DB, dialect Dialect, baseline int, migrations []Migration) error {
	if _, err := db.Exec(createTableSQL[dialect]); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	if baseline > 0 {
		if err := seedBaseline(db, baseline); err != nil {
			return err
		}
	}

	for _, m := range migrations {
		var applied bool
		// The Scan error is deliberately ignored, as it was in the runner
		// this was extracted from: the EXISTS probe runs immediately after
		// the CREATE above succeeded, so a failure here means the table
		// vanished mid-boot — and treating that as "not applied" surfaces
		// one step later as a loud INSERT failure rather than an opaque
		// boot-time abort.
		db.QueryRow(existsSQL[dialect], m.Version).Scan(&applied)

		// Self-heal check: if recorded as applied but the post-condition
		// is missing, treat as not-applied so the transactional re-run
		// below restores it.
		if applied && m.Verify != nil && !m.Verify(db) {
			log.Printf("migrations: v%d (%s) recorded as applied but post-condition fails — re-running",
				m.Version, m.Name)
			if _, err := db.Exec(deleteSQL[dialect], m.Version); err != nil {
				return fmt.Errorf("clear stale schema_migrations row v%d: %w", m.Version, err)
			}
			applied = false
		}
		if applied {
			continue
		}
		if err := runOne(db, dialect, m); err != nil {
			return err
		}
	}
	return nil
}

// seedBaseline records the adoption baseline as applied when the table is
// empty. Empty is the once-only trigger: every later call finds the seeded
// row (or applied migrations) and leaves the table alone.
func seedBaseline(db *sql.DB, baseline int) error {
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&rows); err != nil {
		return fmt.Errorf("count schema_migrations: %w", err)
	}
	if rows > 0 {
		return nil
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, baseline); err != nil {
		return fmt.Errorf("seed baseline v%d: %w", baseline, err)
	}
	log.Printf("migrations: seeded baseline v%d (chain adopted mid-life; earlier steps are self-probing)", baseline)
	return nil
}

// runOne wraps a single migration's DDL/DML and its schema_migrations row
// insert in one transaction. On any error the transaction rolls back and
// the migration is re-attempted on the next startup — which is why Fn must
// be idempotent.
func runOne(db *sql.DB, dialect Dialect, m Migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migration v%d (%s): begin tx: %w", m.Version, m.Name, err)
	}
	defer tx.Rollback() // no-op after Commit

	if err := m.Fn(tx); err != nil {
		return fmt.Errorf("migration v%d (%s): %w", m.Version, m.Name, err)
	}
	if _, err := tx.Exec(insertSQL[dialect], m.Version); err != nil {
		return fmt.Errorf("migration v%d (%s): record version: %w", m.Version, m.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration v%d (%s): commit: %w", m.Version, m.Name, err)
	}
	return nil
}

// Latest returns the highest recorded version (0 when the table is empty).
// The table is not created here — callers read it after Run has run, on a
// database this build migrated.
func Latest(db *sql.DB, dialect Dialect) (int, error) {
	var v int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema_migrations latest: %w", err)
	}
	return v, nil
}
