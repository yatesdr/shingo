// Package schemadump builds shingo-edge's SQLite schema and dumps it, so the
// shape can be committed as files and compared: the baseline a new file is
// born on (baseline_gen.go) and the snapshot of a born database.
//
// The baseline is generated from the retired chain's release and this tree's
// migrations, so a schema change has one home, a numbered migration. The
// shapes a plant can hold are held to it by TestSchemaConvergesAcrossVintages.
//
// It is Core's arrangement and much cheaper: SQLite is a file, so there is no
// container and no external tool. The dump is sqlite_master, which is what
// `.schema` prints.
package schemadump

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	_ "modernc.org/sqlite"

	"shingoedge/store"
)

// SnapshotPath is where the committed dump lives, relative to the shingo-edge
// module root.
const SnapshotPath = "store/schema/schema.snapshot.sql"

// RegenCommand is the exact command the staleness test tells you to run.
const RegenCommand = "make schema-snapshot"

// BuildFresh creates an empty database in dir and runs the full production
// open on it: what a brand-new edge gets.
func BuildFresh(dir string) (string, error) {
	path := filepath.Join(dir, "fresh.db")
	db, err := store.Open(path)
	if err != nil {
		return "", fmt.Errorf("migrate fresh database: %w", err)
	}
	db.Close()
	return path, nil
}

// BuildAged creates an empty database and applies an old baseline DDL to it:
// what an edge installed at that vintage holds before it takes an upgrade.
func BuildAged(dir, baselineDDL string) (string, error) {
	if strings.TrimSpace(baselineDDL) == "" {
		return "", fmt.Errorf("empty baseline DDL")
	}
	path := filepath.Join(dir, "aged.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	_, execErr := raw.Exec(baselineDDL)
	raw.Close()
	if execErr != nil {
		return "", fmt.Errorf("apply old baseline: %w", execErr)
	}
	return path, nil
}

// Dump reads sqlite_master and returns the normalized schema.
func Dump(path string) (string, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()
	stmts, err := catalogue(db, `type, name`)
	if err != nil {
		return "", err
	}
	return Normalize(strings.Join(stmts, "\n\n")), nil
}

// catalogue reads the stored CREATE statements in order, each ended with its
// semicolon. An index SQLite makes for a UNIQUE has no text and is the table's.
func catalogue(db *sql.DB, order string) ([]string, error) {
	rows, err := db.Query(`SELECT sql FROM sqlite_master
	                        WHERE name NOT LIKE 'sqlite_%' AND sql IS NOT NULL
	                        ORDER BY ` + order)
	if err != nil {
		return nil, fmt.Errorf("read sqlite_master: %w", err)
	}
	defer rows.Close()
	var stmts []string
	for rows.Next() {
		var ddl string
		if err := rows.Scan(&ddl); err != nil {
			return nil, fmt.Errorf("scan sqlite_master: %w", err)
		}
		if strings.TrimSpace(ddl) == "" {
			continue
		}
		stmts = append(stmts, strings.TrimSpace(ddl)+";")
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite_master: %w", err)
	}
	if len(stmts) == 0 {
		return nil, fmt.Errorf("sqlite_master is empty — nothing was created")
	}
	return stmts, nil
}

// RemoveDB deletes a database file and its WAL/shm siblings.
func RemoveDB(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
}

var blankRun = regexp.MustCompile(`\n{3,}`)

// Normalize gives the dump stable whitespace and LF endings. SQLite stores the
// CREATE statement verbatim as it was executed, so the text already reflects
// the source; there are no dump-time artefacts to strip.
func Normalize(dump string) string {
	s := strings.ReplaceAll(dump, "\r\n", "\n")
	var kept []string
	for line := range strings.SplitSeq(s, "\n") {
		kept = append(kept, strings.TrimRight(line, " \t"))
	}
	s = blankRun.ReplaceAllString(strings.Join(kept, "\n"), "\n\n")
	return strings.TrimSpace(s) + "\n"
}
