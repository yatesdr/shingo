package store

import "time"

// PayloadManifestItem represents a template manifest entry for a payload.
type PayloadManifestItem struct {
	ID          int64     `json:"id"`
	PayloadID   int64     `json:"payload_id"`
	PartNumber  string    `json:"part_number"`
	Quantity    int64     `json:"quantity"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

func (db *DB) CreatePayloadManifestItem(item *PayloadManifestItem) error {
	id, err := db.insertID(`INSERT INTO payload_manifest (payload_id, part_number, quantity, description) VALUES (?, ?, ?, ?) RETURNING id`,
		item.PayloadID, item.PartNumber, item.Quantity, item.Description)
	if err != nil {
		return err
	}
	item.ID = id
	return nil
}

func (db *DB) UpdatePayloadManifestItem(id int64, partNumber string, quantity int64) error {
	_, err := db.Exec(db.Q(`UPDATE payload_manifest SET part_number=?, quantity=? WHERE id=?`),
		partNumber, quantity, id)
	return err
}

func (db *DB) DeletePayloadManifestItem(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM payload_manifest WHERE id=?`), id)
	return err
}

func (db *DB) ListPayloadManifest(payloadID int64) ([]*PayloadManifestItem, error) {
	rows, err := db.Query(db.Q(`SELECT id, payload_id, part_number, quantity, description, created_at FROM payload_manifest WHERE payload_id=? ORDER BY id`), payloadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*PayloadManifestItem
	for rows.Next() {
		item := &PayloadManifestItem{}
		var createdAt any
		if err := rows.Scan(&item.ID, &item.PayloadID, &item.PartNumber, &item.Quantity, &item.Description, &createdAt); err != nil {
			return nil, err
		}
		item.CreatedAt = parseTime(createdAt)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (db *DB) ReplacePayloadManifest(payloadID int64, items []*PayloadManifestItem) error {
	if _, err := db.Exec(db.Q(`DELETE FROM payload_manifest WHERE payload_id=?`), payloadID); err != nil {
		return err
	}
	for _, item := range items {
		item.PayloadID = payloadID
		if err := db.CreatePayloadManifestItem(item); err != nil {
			return err
		}
	}
	return nil
}

