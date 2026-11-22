package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// The decision a start makes on the file before anything opens it for writing.
// A refusal changes nothing: no byte of the database, no journal-mode
// conversion (the store's DSN converts on connect, so it is not used here) and
// no startup step.

// A Refusal is a start's refusal of a database it will not bring up: the file
// is fine to read, and this build would not run on it. Any other error from
// the same check is a failure to read the file.
type Refusal struct{ msg string }

func (r *Refusal) Error() string { return r.msg }

func refuse(format string, args ...any) error {
	return &Refusal{msg: fmt.Sprintf(format, args...) + "; nothing was changed"}
}

// connector opens a DSN. Production's is connectSQLite; OpenCounting's counts.
type connector func(dsn string) (*sql.DB, error)

func connectSQLite(dsn string) (*sql.DB, error) { return sql.Open("sqlite", dsn) }

// checkFile reads what a start decides on and returns the newest version
// recorded, 0 for a file with no tables (it is born on the baseline), or the
// *Refusal. It only reads.
//
//   - Tables and no schema_migrations: refused. Nothing can tell what shape
//     that is, and born on the baseline it would fail on "already exists".
//   - A head above this build: refused. An older binary cannot know what the
//     newer one's migrations did.
//   - Any version of 1..RetiredChain missing: refused, naming them and
//     RetiredChainRelease, whose chain applies them. Holes, not the head
//     alone. v1 is the exception in the remedy: that release records it only
//     in an empty schema_migrations, so a file missing v1 beside other rows
//     goes back to a backup.
func checkFile(path string, connect connector) (cur int, err error) {
	q, err := openReadOnly(path, connect)
	if err != nil || q == nil {
		return 0, err
	}
	defer q.Close()
	var objects, versioned int
	if err := q.QueryRow(`SELECT COUNT(*), COALESCE(SUM(type = 'table' AND name = 'schema_migrations'), 0)
		FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&objects, &versioned); err != nil {
		return 0, fmt.Errorf("read the schema's state: %w", err)
	}
	if objects == 0 {
		return 0, nil
	}
	if versioned == 0 {
		return 0, refuse("the database has tables and no schema_migrations (v1–v%d): install %s, start it once, then this",
			RetiredChain, RetiredChainRelease)
	}
	rows, err := q.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return 0, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	recorded := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return 0, fmt.Errorf("read schema_migrations: %w", err)
		}
		recorded[v] = true
		cur = max(cur, v)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read schema_migrations: %w", err)
	}
	if latest := latestVersion(); cur > latest {
		return cur, refuse("the database is at v%d and this build knows v%d: install the release that wrote it, or a newer one",
			cur, latest)
	}
	var missing []int
	for v := 1; v <= RetiredChain; v++ {
		if !recorded[v] {
			missing = append(missing, v)
		}
	}
	switch {
	case len(missing) == 0:
		return cur, nil
	case missing[0] == 1 && len(recorded) > 0:
		return cur, refuse("the database is missing %s of v1–v%d and %s cannot refill v1: restore a backup",
			formatVersions(missing), RetiredChain, RetiredChainRelease)
	default:
		return cur, refuse("the database is missing %s of v1–v%d: install %s, start it once, then this",
			formatVersions(missing), RetiredChain, RetiredChainRelease)
	}
}

// openReadOnly opens path for reading alone, or returns nil for no database
// (no file, or an empty one).
//
// A plain read-only open of a WAL database still writes beside it: it creates
// -wal and -shm where there are none, and marks the -shm where there is one.
// So a WAL file with no -wal is read as immutable (a running Edge always has
// one, so nothing is writing to this file), and one with both sidecars is read
// through a read-only -shm.
func openReadOnly(path string, connect connector) (*sql.DB, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && info.Size() == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the database: %w", err)
	}
	opts := "mode=ro&_pragma=busy_timeout(5000)"
	_, walErr := os.Stat(path + "-wal")
	_, shmErr := os.Stat(path + "-shm")
	switch {
	case walErr == nil && shmErr == nil:
		opts += "&readonly_shm=1"
	case errors.Is(walErr, fs.ErrNotExist):
		wal, err := inWALMode(path)
		if err != nil {
			return nil, fmt.Errorf("read the database: %w", err)
		}
		if wal {
			opts += "&immutable=1"
		}
	}
	q, err := connect("file:" + path + "?" + opts)
	if err != nil {
		return nil, fmt.Errorf("read the database: %w", err)
	}
	q.SetMaxOpenConns(1)
	return q, nil
}

// inWALMode reads the header's write-format byte (offset 18): 2 is WAL.
func inWALMode(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	head := make([]byte, 19)
	if _, err := io.ReadFull(f, head); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil // too short to be a database: the read says so
		}
		return false, err
	}
	return head[18] == 2, nil
}

// settleJournal lets SQLite roll back the journal a killed writer left beside
// a rollback-journal database (a file just restored is one until the store
// converts it). A read-only connection cannot, and would fail every start.
func settleJournal(path string, connect connector) error {
	if info, err := os.Stat(path + "-journal"); err != nil || info.Size() == 0 {
		return nil
	}
	db, err := connect("file:" + path + "?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	var n int
	return db.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&n)
}

// CheckDatabase is `shingoedge migrate check`: what a start of this build
// would do to the file at path, found by reading alone. cur is the newest
// version recorded, 0 for an empty file that would be born on the baseline;
// target is this build's. A *Refusal is the refusal a start would give; any
// other error is a failure to read the file.
//
// A file at this build's version is also held to the baseline's tables and
// columns. One below it cannot be: what the migrations above it will leave is
// known only by running them, so a table dropped by hand there is the start's
// failure and not this check's (TestAHandDroppedTableBelowHead).
func CheckDatabase(path string) (cur, target int, err error) {
	target = latestVersion()
	if cur, err = checkFile(path, connectSQLite); err != nil || cur != target {
		return cur, target, err
	}
	q, err := openReadOnly(path, connectSQLite)
	if err != nil || q == nil {
		return cur, target, err
	}
	defer q.Close()
	missing, _, err := compareSchema(q)
	if err != nil {
		return cur, target, err
	}
	if len(missing) > 0 {
		return cur, target, missingObjects(missing)
	}
	return cur, target, nil
}
