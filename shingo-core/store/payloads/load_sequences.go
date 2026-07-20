package payloads

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// LoadSequence is one row of the load_sequences registry: a sequence name and
// the ordered list of RDS binTask names dispatch emits for a payload configured
// to use it. TaskNames is stored as a JSON array in the task_names TEXT column
// (editable data, NOT constants — a plant names its RDS-side binTask keys
// however it likes, so the list lives in a table an engineer edits, not in Go).
type LoadSequence struct {
	Name      string
	TaskNames []string
	UpdatedAt time.Time
}

// GetLoadSequence returns the registry entry for name, or (nil, nil) when no
// such sequence exists — callers treat a missing entry as "unknown sequence"
// (a config-save rejection reason, not a hard error). task_names is decoded
// from its JSON-array TEXT column.
func GetLoadSequence(db *sql.DB, name string) (*LoadSequence, error) {
	var (
		seq     LoadSequence
		rawJSON string
	)
	err := db.QueryRow(
		`SELECT name, task_names, updated_at FROM load_sequences WHERE name=$1`, name,
	).Scan(&seq.Name, &rawJSON, &seq.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get load sequence %q: %w", name, err)
	}
	if err := json.Unmarshal([]byte(rawJSON), &seq.TaskNames); err != nil {
		return nil, fmt.Errorf("load sequence %q: decode task_names: %w", name, err)
	}
	return &seq, nil
}

// ListLoadSequenceNames returns every registered sequence name, ordered, for the
// payload-editor dropdown. The empty option ("normal load") is added by the UI,
// not stored here.
func ListLoadSequenceNames(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM load_sequences ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}
