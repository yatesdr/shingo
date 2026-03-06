package store

import "time"

type BlueprintCatalogEntry struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Code        string    `json:"code"`
	Description string    `json:"description"`
	UOPCapacity int       `json:"uop_capacity"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (db *DB) UpsertBlueprintCatalog(entry *BlueprintCatalogEntry) error {
	_, err := db.Exec(`INSERT INTO blueprint_catalog (id, name, code, description, uop_capacity, updated_at)
		VALUES (?, ?, ?, ?, ?, datetime('now','localtime'))
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, code=excluded.code,
		description=excluded.description, uop_capacity=excluded.uop_capacity, updated_at=datetime('now','localtime')`,
		entry.ID, entry.Name, entry.Code, entry.Description, entry.UOPCapacity)
	return err
}

func (db *DB) ListBlueprintCatalog() ([]*BlueprintCatalogEntry, error) {
	rows, err := db.Query(`SELECT id, name, code, description, uop_capacity, updated_at FROM blueprint_catalog ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*BlueprintCatalogEntry
	for rows.Next() {
		e := &BlueprintCatalogEntry{}
		if err := rows.Scan(&e.ID, &e.Name, &e.Code, &e.Description, &e.UOPCapacity, &e.UpdatedAt); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (db *DB) GetBlueprintByName(name string) (*BlueprintCatalogEntry, error) {
	e := &BlueprintCatalogEntry{}
	err := db.QueryRow(`SELECT id, name, code, description, uop_capacity, updated_at FROM blueprint_catalog WHERE name=?`, name).
		Scan(&e.ID, &e.Name, &e.Code, &e.Description, &e.UOPCapacity, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return e, nil
}

func (db *DB) GetBlueprintByCode(code string) (*BlueprintCatalogEntry, error) {
	e := &BlueprintCatalogEntry{}
	err := db.QueryRow(`SELECT id, name, code, description, uop_capacity, updated_at FROM blueprint_catalog WHERE code=?`, code).
		Scan(&e.ID, &e.Name, &e.Code, &e.Description, &e.UOPCapacity, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return e, nil
}
