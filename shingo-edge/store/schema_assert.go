package store

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"shingoedge/store/schema"
)

// Schema verification: after migrate, the live file is held to the baseline
// this build ships, in one catalogue read. A table or column the baseline has
// and the file lacks fails the start, every one named at once. Nothing
// creates it again at a start, so a hand-dropped table is this failure and
// not an empty table made in silence (TestBoot_TheFilesAStartTakes).

// shippedShape is the baseline's shape, parsed once per process.
var shippedShape = sync.OnceValue(func() schema.Shape { return schema.ParseShape(schema.Baseline) })

// liveShape reads the live file's shape in one catalogue read.
func liveShape(q *sql.DB) (schema.Shape, error) {
	rows, err := q.Query(`SELECT sql FROM sqlite_master WHERE sql IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("read the live schema: %w", err)
	}
	defer rows.Close()
	var all strings.Builder
	for rows.Next() {
		var stmt string
		if err := rows.Scan(&stmt); err != nil {
			return nil, fmt.Errorf("read the live schema: %w", err)
		}
		all.WriteString(stmt)
		all.WriteString("\n;\n") // on its own line: a statement may end in a comment
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the live schema: %w", err)
	}
	return schema.ParseShape(all.String()), nil
}

// compareSchema holds the live file to the shipped baseline.
func compareSchema(q *sql.DB) (missing, differs []string, err error) {
	want := shippedShape()
	if len(want) < 100 {
		return nil, nil, fmt.Errorf("schema verify: read %d objects out of the baseline: the parser or the file is broken, not the database", len(want))
	}
	got, err := liveShape(q)
	if err != nil {
		return nil, nil, fmt.Errorf("schema verify: %w", err)
	}
	missing, differs = schema.Compare(want, got)
	return missing, differs, nil
}

// missingObjects is the refusal of a file that lacks what this build needs. It
// lists every one: a partial restore and one dropped table read differently.
func missingObjects(missing []string) error {
	return &Refusal{msg: fmt.Sprintf("schema verification failed — %d required object(s) missing:\n  %s\n"+
		"A start creates none again: restore a backup, or install %s and start it once (it creates a missing table again, empty)",
		len(missing), strings.Join(missing, "\n  "), RetiredChainRelease)}
}

// verifySchema fails for a file that lacks a table or column this build ships.
func (db *DB) verifySchema() error {
	missing, _, err := compareSchema(db.DB)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return missingObjects(missing)
	}
	return nil
}
