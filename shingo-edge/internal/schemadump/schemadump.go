// Package schemadump builds shingo-edge's SQLite schema and dumps it, so the
// shape can be committed as a file and compared.
//
// Same problem as Core's, same reason. Edge's migrate() also runs
// CREATE TABLE IF NOT EXISTS, which does nothing to a table that already
// exists — so a column added to sqlite_ddl.go's CREATE TABLE reaches a fresh
// edge instantly and a plant edge never. Edge's escape hatch is the idempotent
// ALTER ADD COLUMN pass in migrations.go, and nothing enforces that a new
// baseline column has an entry there.
//
// Edge's version is much cheaper than Core's: SQLite is a file, so there is no
// container and no external tool. The dump is sqlite_master, which is what
// `.schema` prints.
package schemadump

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
// migrate path against it — what a brand-new edge gets.
func BuildFresh(dir string) (string, error) {
	path := filepath.Join(dir, "fresh.db")
	db, err := store.Open(path)
	if err != nil {
		return "", fmt.Errorf("migrate fresh database: %w", err)
	}
	db.Close()
	return path, nil
}

// BuildAged creates an empty database, applies an OLD baseline DDL to it, and
// then runs the full production migrate path — what an edge installed at that
// vintage gets when it takes an upgrade.
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

	db, err := store.Open(path)
	if err != nil {
		return "", fmt.Errorf("migrate aged database: %w", err)
	}
	db.Close()
	return path, nil
}

// Dump reads sqlite_master and returns the normalized schema.
func Dump(path string) (string, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT type, name, COALESCE(sql, '')
	                         FROM sqlite_master
	                        WHERE name NOT LIKE 'sqlite_%'
	                        ORDER BY type, name`)
	if err != nil {
		return "", fmt.Errorf("read sqlite_master: %w", err)
	}
	defer rows.Close()

	var stmts []string
	for rows.Next() {
		var typ, name, ddl string
		if err := rows.Scan(&typ, &name, &ddl); err != nil {
			return "", fmt.Errorf("scan sqlite_master: %w", err)
		}
		// Auto-created indexes (UNIQUE constraints) have a NULL sql and are
		// already implied by the table they belong to.
		if strings.TrimSpace(ddl) == "" {
			continue
		}
		stmts = append(stmts, strings.TrimSpace(ddl)+";")
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate sqlite_master: %w", err)
	}
	if len(stmts) == 0 {
		return "", fmt.Errorf("sqlite_master is empty — nothing was created")
	}
	return Normalize(strings.Join(stmts, "\n\n")), nil
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

// Canonical reduces a dump to an ORDER-INSENSITIVE description of the shape.
//
// Same reasoning as Core's: a fresh database gets a column where the baseline
// CREATE TABLE puts it, an upgraded one gets it appended by an ALTER, and SQL
// addresses columns by name. Order is not part of the shape.
//
// SQLite makes this coarser than Core's, because sqlite_master stores the
// CREATE statement as literal text — including the comments and indentation of
// whichever code path wrote it. So a table rebuilt by a migration and the same
// table created by the baseline can be character-for-character different while
// being the same shape. Canonical therefore compares the SET OF COLUMN
// DEFINITIONS per object, with formatting collapsed.
func Canonical(dump string) string {
	// Comments come out BEFORE the split, not after. SQLite stores the CREATE
	// statement as the literal text it was executed with, comments and all,
	// and those comments contain semicolons — so splitting first tears them in
	// half and the halves show up as phantom schema differences.
	clean := sqlBlockComment.ReplaceAllString(dump, " ")
	clean = sqlLineComment.ReplaceAllString(clean, " ")

	var out []string
	for stmt := range strings.SplitSeq(clean, ";") {
		s := strings.TrimSpace(stmt)
		if s == "" {
			continue
		}
		out = append(out, canonicalStatement(s))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

var (
	sqlLineComment  = regexp.MustCompile(`--[^\n]*`)
	sqlBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	whitespaceRun   = regexp.MustCompile(`\s+`)
	createTableHead = regexp.MustCompile(`(?is)^(CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["\x60\[]?(\w+)["\x60\]]?)\s*\((.*)\)\s*$`)
)

func canonicalStatement(stmt string) string {
	s := strings.TrimSpace(whitespaceRun.ReplaceAllString(stmt, " "))

	m := createTableHead.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	parts := splitTopLevel(m[3])
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	sort.Strings(parts)
	return "CREATE TABLE " + m[2] + " (" + strings.Join(parts, ", ") + ")"
}

// splitTopLevel splits a column list on commas that are not inside
// parentheses — so `qty INTEGER CHECK (qty >= 0, ...)` stays whole.
func splitTopLevel(body string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range body {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, body[start:])
}
