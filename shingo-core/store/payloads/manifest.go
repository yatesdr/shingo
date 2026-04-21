package payloads

import (
	"database/sql"

	"shingocore/domain"
	"shingocore/store/internal/helpers"
)

// ManifestItem is the payload-template manifest line-item domain
// type. The struct lives in shingocore/domain as PayloadManifestItem
// (Stage 2A); this alias keeps the payloads.ManifestItem name used by
// CreateItem/ListManifest and the outer store/ payload_manifest.go
// re-export (store.PayloadManifestItem).
type ManifestItem = domain.PayloadManifestItem

// CreateItem inserts a manifest line and sets item.ID on success.
func CreateItem(db *sql.DB, item *ManifestItem) error {
	id, err := helpers.InsertID(db, `INSERT INTO payload_manifest (payload_id, part_number, quantity, description) VALUES ($1, $2, $3, $4) RETURNING id`,
		item.PayloadID, item.PartNumber, item.Quantity, item.Description)
	if err != nil {
		return err
	}
	item.ID = id
	return nil
}

// UpdateItem changes a manifest line's part number and quantity.
func UpdateItem(db *sql.DB, id int64, partNumber string, quantity int64) error {
	_, err := db.Exec(`UPDATE payload_manifest SET part_number=$1, quantity=$2 WHERE id=$3`,
		partNumber, quantity, id)
	return err
}

// DeleteItem removes a manifest line.
func DeleteItem(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM payload_manifest WHERE id=$1`, id)
	return err
}

// ListManifest returns all manifest items for a payload, ordered by insertion.
func ListManifest(db *sql.DB, payloadID int64) ([]*ManifestItem, error) {
	rows, err := db.Query(`SELECT id, payload_id, part_number, quantity, description, created_at FROM payload_manifest WHERE payload_id=$1 ORDER BY id`, payloadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*ManifestItem
	for rows.Next() {
		item := &ManifestItem{}
		if err := rows.Scan(&item.ID, &item.PayloadID, &item.PartNumber, &item.Quantity, &item.Description, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ReplaceManifest wipes the payload's manifest and re-inserts the given items.
// Runs as a single transaction; sets each item's ID on success.
func ReplaceManifest(db *sql.DB, payloadID int64, items []*ManifestItem) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM payload_manifest WHERE payload_id=$1`, payloadID); err != nil {
		return err
	}
	for _, item := range items {
		item.PayloadID = payloadID
		var id int64
		err := tx.QueryRow(`INSERT INTO payload_manifest (payload_id, part_number, quantity, description) VALUES ($1, $2, $3, $4) RETURNING id`,
			item.PayloadID, item.PartNumber, item.Quantity, item.Description).Scan(&id)
		if err != nil {
			return err
		}
		item.ID = id
	}
	return tx.Commit()
}
