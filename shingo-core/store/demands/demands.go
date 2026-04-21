// Package demands holds demand + production-log + demand-registry
// persistence for shingo-core.
//
// Phase 5 of the architecture plan moved demands, production_log, and
// demand_registry CRUD out of the flat store/ package and into this
// sub-package. The outer store/ keeps type aliases
// (`store.Demand = demands.Demand`, etc.) and one-line delegate methods
// on *store.DB so external callers see no API change.
package demands

import (
	"database/sql"
	"time"

	"shingocore/store/internal/helpers"
)

// Demand represents a material demand tracked by cat_id.
type Demand struct {
	ID          int64     `json:"id"`
	CatID       string    `json:"cat_id"`
	Description string    `json:"description"`
	DemandQty   int64     `json:"demand_qty"`
	ProducedQty int64     `json:"produced_qty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Remaining returns demand_qty - produced_qty (floored at 0).
func (d *Demand) Remaining() int64 {
	r := d.DemandQty - d.ProducedQty
	if r < 0 {
		return 0
	}
	return r
}

// ProductionLogEntry records an individual station's production report.
type ProductionLogEntry struct {
	ID         int64     `json:"id"`
	CatID      string    `json:"cat_id"`
	StationID  string    `json:"station_id"`
	Quantity   int64     `json:"quantity"`
	ReportedAt time.Time `json:"reported_at"`
}

// RegistryEntry maps a payload code to a manual_swap node that accepts
// it. Synced from Edge claim config via the ClaimSync protocol message.
type RegistryEntry struct {
	ID           int64     `json:"id"`
	StationID    string    `json:"station_id"`
	CoreNodeName string    `json:"core_node_name"`
	Role         string    `json:"role"` // "produce" or "consume"
	PayloadCode  string    `json:"payload_code"`
	OutboundDest string    `json:"outbound_dest"`
	UpdatedAt    time.Time `json:"updated_at"`
}

const demandSelectCols = `id, cat_id, description, demand_qty, produced_qty, created_at, updated_at`

func scanDemand(row interface{ Scan(...any) error }) (*Demand, error) {
	var d Demand
	err := row.Scan(&d.ID, &d.CatID, &d.Description, &d.DemandQty, &d.ProducedQty, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func scanDemands(rows *sql.Rows) ([]*Demand, error) {
	var demands []*Demand
	for rows.Next() {
		d, err := scanDemand(rows)
		if err != nil {
			return nil, err
		}
		demands = append(demands, d)
	}
	return demands, rows.Err()
}

// Create inserts a new demand row and returns the new ID.
func Create(db *sql.DB, catID, description string, demandQty int64) (int64, error) {
	return helpers.InsertID(db, `INSERT INTO demands (cat_id, description, demand_qty) VALUES ($1, $2, $3) RETURNING id`,
		catID, description, demandQty)
}

// Update writes every mutable column on a demand.
func Update(db *sql.DB, id int64, catID, description string, demandQty, producedQty int64) error {
	_, err := db.Exec(`UPDATE demands SET cat_id=$1, description=$2, demand_qty=$3, produced_qty=$4, updated_at=NOW() WHERE id=$5`,
		catID, description, demandQty, producedQty, id)
	return err
}

// UpdateAndResetProduced rewrites description + demand_qty and zeroes produced_qty.
func UpdateAndResetProduced(db *sql.DB, id int64, description string, demandQty int64) error {
	_, err := db.Exec(`UPDATE demands SET description=$1, demand_qty=$2, produced_qty=0, updated_at=NOW() WHERE id=$3`,
		description, demandQty, id)
	return err
}

// Delete removes a demand row.
func Delete(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM demands WHERE id=$1`, id)
	return err
}

// List returns all demands ordered by cat_id.
func List(db *sql.DB) ([]*Demand, error) {
	rows, err := db.Query(`SELECT ` + demandSelectCols + ` FROM demands ORDER BY cat_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDemands(rows)
}

// Get fetches a demand by ID.
func Get(db *sql.DB, id int64) (*Demand, error) {
	row := db.QueryRow(`SELECT `+demandSelectCols+` FROM demands WHERE id=$1`, id)
	return scanDemand(row)
}

// GetByCatID fetches a demand by its unique cat_id.
func GetByCatID(db *sql.DB, catID string) (*Demand, error) {
	row := db.QueryRow(`SELECT `+demandSelectCols+` FROM demands WHERE cat_id=$1`, catID)
	return scanDemand(row)
}

// IncrementProduced bumps produced_qty by qty for a given cat_id.
func IncrementProduced(db *sql.DB, catID string, qty int64) error {
	_, err := db.Exec(`UPDATE demands SET produced_qty = produced_qty + $1, updated_at=NOW() WHERE cat_id=$2`,
		qty, catID)
	return err
}

// ClearAllProduced zeroes produced_qty on every demand row.
func ClearAllProduced(db *sql.DB) error {
	_, err := db.Exec(`UPDATE demands SET produced_qty = 0, updated_at=NOW()`)
	return err
}

// ClearProduced zeroes produced_qty for a single demand.
func ClearProduced(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE demands SET produced_qty = 0, updated_at=NOW() WHERE id=$1`, id)
	return err
}

// SetProduced sets produced_qty to qty for a single demand.
func SetProduced(db *sql.DB, id int64, qty int64) error {
	_, err := db.Exec(`UPDATE demands SET produced_qty = $1, updated_at=NOW() WHERE id=$2`, qty, id)
	return err
}

// LogProduction appends a production_log row.
func LogProduction(db *sql.DB, catID, stationID string, qty int64) error {
	_, err := db.Exec(`INSERT INTO production_log (cat_id, station_id, quantity) VALUES ($1, $2, $3)`,
		catID, stationID, qty)
	return err
}

// ListProductionLog returns recent production_log entries for a cat_id.
func ListProductionLog(db *sql.DB, catID string, limit int) ([]*ProductionLogEntry, error) {
	rows, err := db.Query(`SELECT id, cat_id, station_id, quantity, reported_at FROM production_log WHERE cat_id=$1 ORDER BY reported_at DESC LIMIT $2`,
		catID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []*ProductionLogEntry
	for rows.Next() {
		e := &ProductionLogEntry{}
		if err := rows.Scan(&e.ID, &e.CatID, &e.StationID, &e.Quantity, &e.ReportedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// SyncRegistry replaces all demand_registry entries for a station atomically.
func SyncRegistry(db *sql.DB, stationID string, entries []RegistryEntry) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM demand_registry WHERE station_id = $1`, stationID); err != nil {
		return err
	}

	for _, e := range entries {
		if _, err := tx.Exec(`INSERT INTO demand_registry (station_id, core_node_name, role, payload_code, outbound_dest) VALUES ($1, $2, $3, $4, $5)`,
			stationID, e.CoreNodeName, e.Role, e.PayloadCode, e.OutboundDest); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// LookupRegistry returns all demand_registry entries for a given payload code.
func LookupRegistry(db *sql.DB, payloadCode string) ([]RegistryEntry, error) {
	rows, err := db.Query(`SELECT id, station_id, core_node_name, role, payload_code, outbound_dest, updated_at
		FROM demand_registry WHERE payload_code = $1`, payloadCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []RegistryEntry
	for rows.Next() {
		var e RegistryEntry
		if err := rows.Scan(&e.ID, &e.StationID, &e.CoreNodeName, &e.Role, &e.PayloadCode, &e.OutboundDest, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ListRegistry returns every demand_registry entry.
func ListRegistry(db *sql.DB) ([]RegistryEntry, error) {
	rows, err := db.Query(`SELECT id, station_id, core_node_name, role, payload_code, outbound_dest, updated_at
		FROM demand_registry ORDER BY station_id, core_node_name, payload_code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []RegistryEntry
	for rows.Next() {
		var e RegistryEntry
		if err := rows.Scan(&e.ID, &e.StationID, &e.CoreNodeName, &e.Role, &e.PayloadCode, &e.OutboundDest, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
