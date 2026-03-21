package store

import (
	"database/sql"
	"time"
)

// Demand represents a material demand tracked by cat_id.
type Demand struct {
	ID          int64   `json:"id"`
	CatID       string  `json:"cat_id"`
	Description string  `json:"description"`
	DemandQty   int64 `json:"demand_qty"`
	ProducedQty int64 `json:"produced_qty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Remaining returns demand_qty - produced_qty.
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

func (db *DB) CreateDemand(catID, description string, demandQty int64) (int64, error) {
	return db.insertID(`INSERT INTO demands (cat_id, description, demand_qty) VALUES ($1, $2, $3) RETURNING id`,
		catID, description, demandQty)
}

func (db *DB) UpdateDemand(id int64, catID, description string, demandQty, producedQty int64) error {
	_, err := db.Exec(`UPDATE demands SET cat_id=$1, description=$2, demand_qty=$3, produced_qty=$4, updated_at=NOW() WHERE id=$5`,
		catID, description, demandQty, producedQty, id)
	return err
}

func (db *DB) UpdateDemandAndResetProduced(id int64, description string, demandQty int64) error {
	_, err := db.Exec(`UPDATE demands SET description=$1, demand_qty=$2, produced_qty=0, updated_at=NOW() WHERE id=$3`,
		description, demandQty, id)
	return err
}

func (db *DB) DeleteDemand(id int64) error {
	_, err := db.Exec(`DELETE FROM demands WHERE id=$1`, id)
	return err
}

func (db *DB) ListDemands() ([]*Demand, error) {
	rows, err := db.Query(`SELECT ` + demandSelectCols + ` FROM demands ORDER BY cat_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDemands(rows)
}

func (db *DB) GetDemand(id int64) (*Demand, error) {
	row := db.QueryRow(`SELECT `+demandSelectCols+` FROM demands WHERE id=$1`, id)
	return scanDemand(row)
}

func (db *DB) GetDemandByCatID(catID string) (*Demand, error) {
	row := db.QueryRow(`SELECT `+demandSelectCols+` FROM demands WHERE cat_id=$1`, catID)
	return scanDemand(row)
}

func (db *DB) IncrementProduced(catID string, qty int64) error {
	_, err := db.Exec(`UPDATE demands SET produced_qty = produced_qty + $1, updated_at=NOW() WHERE cat_id=$2`,
		qty, catID)
	return err
}

func (db *DB) ClearAllProduced() error {
	_, err := db.Exec(`UPDATE demands SET produced_qty = 0, updated_at=NOW()`)
	return err
}

func (db *DB) ClearProduced(id int64) error {
	_, err := db.Exec(`UPDATE demands SET produced_qty = 0, updated_at=NOW() WHERE id=$1`, id)
	return err
}

func (db *DB) SetProduced(id int64, qty int64) error {
	_, err := db.Exec(`UPDATE demands SET produced_qty = $1, updated_at=NOW() WHERE id=$2`, qty, id)
	return err
}

func (db *DB) LogProduction(catID, stationID string, qty int64) error {
	_, err := db.Exec(`INSERT INTO production_log (cat_id, station_id, quantity) VALUES ($1, $2, $3)`,
		catID, stationID, qty)
	return err
}

func (db *DB) ListProductionLog(catID string, limit int) ([]*ProductionLogEntry, error) {
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
