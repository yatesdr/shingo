// Package dashboards is the persistence layer for the floor display
// platform. A dashboard row is a saved, named, station-scoped view of
// Core's live data (the AMR task board today; other kinds later). It is
// pure presentation config — it owns no operational state, so this package
// is plain CRUD with no cross-aggregate orchestration.
//
// Convention (see store/store.go): persistence logic lives here as
// functions on *sql.DB; service/dashboard_service.go wraps these for the
// www handlers. JSON-backed columns (stations_json, config_json) keep the
// schema flat — the station filter is an opaque list of station IDs, not a
// child table, because stations are strings here, not first-class entities.
package dashboards

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Dashboard is one saved floor-display definition.
type Dashboard struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Stations  []string        `json:"stations"` // station-id area filter; empty = plant-wide
	Config    json.RawMessage `json:"config"`   // per-kind options, opaque to the platform
	Enabled   bool            `json:"enabled"`
	SortOrder int             `json:"sort_order"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Input is the create/update request shape — persisted fields minus id and
// server-managed timestamps.
type Input struct {
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Stations  []string        `json:"stations"`
	Config    json.RawMessage `json:"config"`
	Enabled   bool            `json:"enabled"`
	SortOrder int             `json:"sort_order"`
}

const selectCols = `id, name, kind, stations_json, config_json, enabled, sort_order, created_at, updated_at`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(...any) error }

func scanRow(s rowScanner) (*Dashboard, error) {
	var (
		d            Dashboard
		stationsJSON string
		configJSON   string
	)
	if err := s.Scan(&d.ID, &d.Name, &d.Kind, &stationsJSON, &configJSON,
		&d.Enabled, &d.SortOrder, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	// Tolerate empty/garbled JSON rather than failing the whole list —
	// a single bad row shouldn't blank every board.
	d.Stations = []string{}
	if strings.TrimSpace(stationsJSON) != "" {
		_ = json.Unmarshal([]byte(stationsJSON), &d.Stations)
		if d.Stations == nil {
			d.Stations = []string{}
		}
	}
	if strings.TrimSpace(configJSON) == "" {
		configJSON = "{}"
	}
	d.Config = json.RawMessage(configJSON)
	return &d, nil
}

// List returns all dashboards, ordered for stable display.
func List(db *sql.DB) ([]Dashboard, error) {
	rows, err := db.Query(`SELECT ` + selectCols + ` FROM dashboards ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Dashboard{}
	for rows.Next() {
		d, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// Get returns one dashboard by id, or (nil, nil) if it does not exist.
func Get(db *sql.DB, id int64) (*Dashboard, error) {
	d, err := scanRow(db.QueryRow(`SELECT `+selectCols+` FROM dashboards WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// Create inserts a dashboard and returns its new id.
func Create(db *sql.DB, in Input) (int64, error) {
	stationsJSON, configJSON, err := marshalInput(in)
	if err != nil {
		return 0, err
	}
	var id int64
	err = db.QueryRow(`INSERT INTO dashboards
		(name, kind, stations_json, config_json, enabled, sort_order, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,NOW()) RETURNING id`,
		in.Name, in.Kind, stationsJSON, configJSON, in.Enabled, in.SortOrder).Scan(&id)
	return id, err
}

// Update overwrites an existing dashboard's fields.
func Update(db *sql.DB, id int64, in Input) error {
	stationsJSON, configJSON, err := marshalInput(in)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE dashboards
		SET name=$1, kind=$2, stations_json=$3, config_json=$4, enabled=$5, sort_order=$6, updated_at=NOW()
		WHERE id=$7`,
		in.Name, in.Kind, stationsJSON, configJSON, in.Enabled, in.SortOrder, id)
	return err
}

// Delete removes a dashboard by id.
func Delete(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM dashboards WHERE id=$1`, id)
	return err
}

// marshalInput serializes and validates the JSON-backed fields, applying
// defaults (nil stations -> "[]", empty config -> "{}"). It rejects a
// non-empty config that isn't valid JSON so a bad admin payload fails at
// write time rather than poisoning the display renderer later.
func marshalInput(in Input) (stationsJSON, configJSON string, err error) {
	stations := in.Stations
	if stations == nil {
		stations = []string{}
	}
	sb, err := json.Marshal(stations)
	if err != nil {
		return "", "", fmt.Errorf("marshal stations: %w", err)
	}
	cfg := strings.TrimSpace(string(in.Config))
	if cfg == "" {
		cfg = "{}"
	} else if !json.Valid([]byte(cfg)) {
		return "", "", fmt.Errorf("config is not valid JSON")
	}
	return string(sb), cfg, nil
}
