package store

import "time"

type PayloadStyleManifestItem struct {
	ID          int64     `json:"id"`
	StyleID     int64     `json:"style_id"`
	PartNumber  string    `json:"part_number"`
	Quantity    float64   `json:"quantity"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

func (db *DB) CreateStyleManifestItem(item *PayloadStyleManifestItem) error {
	result, err := db.Exec(db.Q(`INSERT INTO payload_style_manifest (style_id, part_number, quantity, description) VALUES (?, ?, ?, ?)`),
		item.StyleID, item.PartNumber, item.Quantity, item.Description)
	if err != nil {
		return err
	}
	id, _ := result.LastInsertId()
	item.ID = id
	return nil
}

func (db *DB) DeleteStyleManifestItem(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM payload_style_manifest WHERE id=?`), id)
	return err
}

func (db *DB) ListStyleManifest(styleID int64) ([]*PayloadStyleManifestItem, error) {
	rows, err := db.Query(db.Q(`SELECT id, style_id, part_number, quantity, description, created_at FROM payload_style_manifest WHERE style_id=? ORDER BY id`), styleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*PayloadStyleManifestItem
	for rows.Next() {
		item := &PayloadStyleManifestItem{}
		var createdAt any
		if err := rows.Scan(&item.ID, &item.StyleID, &item.PartNumber, &item.Quantity, &item.Description, &createdAt); err != nil {
			continue
		}
		item.CreatedAt = parseTime(createdAt)
		items = append(items, item)
	}
	return items, nil
}

func (db *DB) ReplaceStyleManifest(styleID int64, items []*PayloadStyleManifestItem) error {
	if _, err := db.Exec(db.Q(`DELETE FROM payload_style_manifest WHERE style_id=?`), styleID); err != nil {
		return err
	}
	for _, item := range items {
		item.StyleID = styleID
		if err := db.CreateStyleManifestItem(item); err != nil {
			return err
		}
	}
	return nil
}
