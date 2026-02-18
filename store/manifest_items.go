package store

import (
	"database/sql"
	"fmt"
	"time"
)

type ManifestItem struct {
	ID             int64
	PayloadID      int64
	PartNumber     string
	Quantity       float64
	ProductionDate string
	LotCode        string
	Notes          string
	CreatedAt      time.Time
}

const manifestItemSelectCols = `id, payload_id, part_number, quantity, production_date, lot_code, notes, created_at`

func scanManifestItem(row interface{ Scan(...any) error }) (*ManifestItem, error) {
	var m ManifestItem
	var prodDate, lotCode sql.NullString
	var createdAt string
	err := row.Scan(&m.ID, &m.PayloadID, &m.PartNumber, &m.Quantity, &prodDate, &lotCode, &m.Notes, &createdAt)
	if err != nil {
		return nil, err
	}
	if prodDate.Valid {
		m.ProductionDate = prodDate.String
	}
	if lotCode.Valid {
		m.LotCode = lotCode.String
	}
	m.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	return &m, nil
}

func scanManifestItems(rows *sql.Rows) ([]*ManifestItem, error) {
	var items []*ManifestItem
	for rows.Next() {
		m, err := scanManifestItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return items, rows.Err()
}

func (db *DB) CreateManifestItem(m *ManifestItem) error {
	var prodDate, lotCode any
	if m.ProductionDate != "" {
		prodDate = m.ProductionDate
	}
	if m.LotCode != "" {
		lotCode = m.LotCode
	}
	result, err := db.Exec(db.Q(`INSERT INTO manifest_items (payload_id, part_number, quantity, production_date, lot_code, notes) VALUES (?, ?, ?, ?, ?, ?)`),
		m.PayloadID, m.PartNumber, m.Quantity, prodDate, lotCode, m.Notes)
	if err != nil {
		return fmt.Errorf("create manifest item: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("create manifest item last id: %w", err)
	}
	m.ID = id
	return nil
}

func (db *DB) UpdateManifestItem(m *ManifestItem) error {
	var prodDate, lotCode any
	if m.ProductionDate != "" {
		prodDate = m.ProductionDate
	}
	if m.LotCode != "" {
		lotCode = m.LotCode
	}
	_, err := db.Exec(db.Q(`UPDATE manifest_items SET part_number=?, quantity=?, production_date=?, lot_code=?, notes=? WHERE id=?`),
		m.PartNumber, m.Quantity, prodDate, lotCode, m.Notes, m.ID)
	return err
}

func (db *DB) DeleteManifestItem(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM manifest_items WHERE id=?`), id)
	return err
}

func (db *DB) ListManifestItems(payloadID int64) ([]*ManifestItem, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM manifest_items WHERE payload_id=? ORDER BY id`, manifestItemSelectCols)), payloadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanManifestItems(rows)
}

func (db *DB) DeleteManifestItemsByPayload(payloadID int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM manifest_items WHERE payload_id=?`), payloadID)
	return err
}
