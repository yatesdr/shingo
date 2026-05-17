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

	"shingo/protocol"
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
//
// ReplenishUOPThreshold (UOP-threshold replenishment) carries the
// per-(loader, payload) trigger Core uses to decide when to send
// LoopBelowThresholdSignal. Zero means "Core does not monitor this
// pair" — Edge falls back to legacy bin-count behavior.
type RegistryEntry struct {
	ID                    int64              `json:"id"`
	StationID             string             `json:"station_id"`
	CoreNodeName          string             `json:"core_node_name"`
	Role                  protocol.ClaimRole `json:"role"` // "produce" or "consume"
	PayloadCode           string             `json:"payload_code"`
	OutboundDest          string             `json:"outbound_dest"`
	ReplenishUOPThreshold int                `json:"replenish_uop_threshold"`
	UpdatedAt             time.Time          `json:"updated_at"`
}

// registrySelectCols is the canonical column list for demand_registry
// reads. Centralized so a future column addition only requires touching
// the SELECT list in one place — and the scan order in the helpers
// below. Adding a new column without updating both will break the
// positional Scan().
const registrySelectCols = `id, station_id, core_node_name, role, payload_code, outbound_dest, replenish_uop_threshold, updated_at`

func scanRegistryEntry(row interface{ Scan(...any) error }) (RegistryEntry, error) {
	var e RegistryEntry
	err := row.Scan(&e.ID, &e.StationID, &e.CoreNodeName, &e.Role, &e.PayloadCode, &e.OutboundDest, &e.ReplenishUOPThreshold, &e.UpdatedAt)
	return e, err
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

// RegistryChange describes a single (loader, payload) row whose
// replenish_uop_threshold value moved as a result of a SyncRegistry
// call. Callers (specifically threshold_monitor) use this to reset
// per-binding debounce state so a new threshold takes immediate effect.
type RegistryChange struct {
	StationID    string
	CoreNodeName string
	PayloadCode  string
	OldThreshold int
	NewThreshold int
}

// SyncRegistry replaces all demand_registry entries for a station atomically.
//
// Returns the list of (loader, payload) rows whose threshold value
// changed (including newly-created rows where old=0, and deleted rows
// where new=0 because the entry vanished). Empty slice when no thresholds
// shifted. Threshold-monitor consumes this to reset its in-memory
// debounce timers so the new threshold engages without waiting out the
// debounce window.
func SyncRegistry(db *sql.DB, stationID string, entries []RegistryEntry) ([]RegistryChange, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Snapshot existing thresholds for change detection. Keyed by
	// (core_node_name, payload_code) — the natural composite that a
	// loader binding is identified by.
	prior := make(map[string]int)
	priorKey := func(node, payload string) string { return node + "\x00" + payload }
	rows, err := tx.Query(`SELECT core_node_name, payload_code, replenish_uop_threshold FROM demand_registry WHERE station_id = $1`, stationID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n, p string
		var thr int
		if err := rows.Scan(&n, &p, &thr); err != nil {
			rows.Close()
			return nil, err
		}
		prior[priorKey(n, p)] = thr
	}
	rows.Close()

	if _, err := tx.Exec(`DELETE FROM demand_registry WHERE station_id = $1`, stationID); err != nil {
		return nil, err
	}

	var changes []RegistryChange
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if _, err := tx.Exec(`INSERT INTO demand_registry (station_id, core_node_name, role, payload_code, outbound_dest, replenish_uop_threshold) VALUES ($1, $2, $3, $4, $5, $6)`,
			stationID, e.CoreNodeName, e.Role, e.PayloadCode, e.OutboundDest, e.ReplenishUOPThreshold); err != nil {
			return nil, err
		}
		key := priorKey(e.CoreNodeName, e.PayloadCode)
		seen[key] = true
		old := prior[key]
		if old != e.ReplenishUOPThreshold {
			changes = append(changes, RegistryChange{
				StationID:    stationID,
				CoreNodeName: e.CoreNodeName,
				PayloadCode:  e.PayloadCode,
				OldThreshold: old,
				NewThreshold: e.ReplenishUOPThreshold,
			})
		}
	}
	// Deleted rows whose old threshold was non-zero: clear the
	// debounce timer so a future re-create at a different threshold
	// (or any value) engages cleanly.
	for k, old := range prior {
		if seen[k] || old == 0 {
			continue
		}
		sep := -1
		for i := 0; i < len(k); i++ {
			if k[i] == 0 {
				sep = i
				break
			}
		}
		if sep < 0 {
			continue
		}
		changes = append(changes, RegistryChange{
			StationID:    stationID,
			CoreNodeName: k[:sep],
			PayloadCode:  k[sep+1:],
			OldThreshold: old,
			NewThreshold: 0,
		})
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return changes, nil
}

// LookupRegistry returns all demand_registry entries for a given payload code.
func LookupRegistry(db *sql.DB, payloadCode string) ([]RegistryEntry, error) {
	rows, err := db.Query(`SELECT `+registrySelectCols+`
		FROM demand_registry WHERE payload_code = $1`, payloadCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []RegistryEntry
	for rows.Next() {
		e, err := scanRegistryEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// LookupThresholdsByPayload returns every demand_registry binding for
// the given payload that has a non-zero replenish_uop_threshold —
// the set of (loader, payload) pairs Core's threshold monitor is
// responsible for. Pairs with threshold = 0 are opt-out (legacy
// bin-count owned by Edge) and excluded.
func LookupThresholdsByPayload(db *sql.DB, payloadCode string) ([]RegistryEntry, error) {
	rows, err := db.Query(`SELECT `+registrySelectCols+`
		FROM demand_registry
		WHERE payload_code = $1 AND replenish_uop_threshold > 0`, payloadCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []RegistryEntry
	for rows.Next() {
		e, err := scanRegistryEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ListThresholds returns every demand_registry binding with a non-zero
// replenish_uop_threshold across all payloads. Used by the threshold-
// monitor startup sweep, which iterates every active binding once on
// startup bypassing debounce.
func ListThresholds(db *sql.DB) ([]RegistryEntry, error) {
	rows, err := db.Query(`SELECT ` + registrySelectCols + `
		FROM demand_registry
		WHERE replenish_uop_threshold > 0
		ORDER BY station_id, core_node_name, payload_code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []RegistryEntry
	for rows.Next() {
		e, err := scanRegistryEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ListRegistry returns every demand_registry entry.
func ListRegistry(db *sql.DB) ([]RegistryEntry, error) {
	rows, err := db.Query(`SELECT ` + registrySelectCols + `
		FROM demand_registry ORDER BY station_id, core_node_name, payload_code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []RegistryEntry
	for rows.Next() {
		e, err := scanRegistryEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
