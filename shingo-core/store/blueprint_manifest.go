package store

import "time"

type BlueprintManifestItem struct {
	ID          int64     `json:"id"`
	BlueprintID int64     `json:"blueprint_id"`
	PartNumber  string    `json:"part_number"`
	Quantity    int64     `json:"quantity"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

func (db *DB) CreateBlueprintManifestItem(item *BlueprintManifestItem) error {
	result, err := db.Exec(db.Q(`INSERT INTO blueprint_manifest (blueprint_id, part_number, quantity, description) VALUES (?, ?, ?, ?)`),
		item.BlueprintID, item.PartNumber, item.Quantity, item.Description)
	if err != nil {
		return err
	}
	id, err := result.LastInsertId()
	if err == nil {
		item.ID = id
	}
	return nil
}

func (db *DB) DeleteBlueprintManifestItem(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM blueprint_manifest WHERE id=?`), id)
	return err
}

func (db *DB) ListBlueprintManifest(blueprintID int64) ([]*BlueprintManifestItem, error) {
	rows, err := db.Query(db.Q(`SELECT id, blueprint_id, part_number, quantity, description, created_at FROM blueprint_manifest WHERE blueprint_id=? ORDER BY id`), blueprintID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*BlueprintManifestItem
	for rows.Next() {
		item := &BlueprintManifestItem{}
		var createdAt any
		if err := rows.Scan(&item.ID, &item.BlueprintID, &item.PartNumber, &item.Quantity, &item.Description, &createdAt); err != nil {
			return nil, err
		}
		item.CreatedAt = parseTime(createdAt)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (db *DB) ReplaceBlueprintManifest(blueprintID int64, items []*BlueprintManifestItem) error {
	if _, err := db.Exec(db.Q(`DELETE FROM blueprint_manifest WHERE blueprint_id=?`), blueprintID); err != nil {
		return err
	}
	for _, item := range items {
		item.BlueprintID = blueprintID
		if err := db.CreateBlueprintManifestItem(item); err != nil {
			return err
		}
	}
	return nil
}
