package store

import "time"

type StyleCatalogEntry struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Code        string    `json:"code"`
	FormFactor  string    `json:"form_factor"`
	Description string    `json:"description"`
	UOPCapacity int       `json:"uop_capacity"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (db *DB) UpsertStyleCatalog(entry *StyleCatalogEntry) error {
	_, err := db.Exec(`INSERT INTO style_catalog (id, name, code, form_factor, description, uop_capacity, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now','localtime'))
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, code=excluded.code, form_factor=excluded.form_factor,
		description=excluded.description, uop_capacity=excluded.uop_capacity, updated_at=datetime('now','localtime')`,
		entry.ID, entry.Name, entry.Code, entry.FormFactor, entry.Description, entry.UOPCapacity)
	return err
}

func (db *DB) ListStyleCatalog() ([]*StyleCatalogEntry, error) {
	rows, err := db.Query(`SELECT id, name, code, form_factor, description, uop_capacity, updated_at FROM style_catalog ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*StyleCatalogEntry
	for rows.Next() {
		e := &StyleCatalogEntry{}
		if err := rows.Scan(&e.ID, &e.Name, &e.Code, &e.FormFactor, &e.Description, &e.UOPCapacity, &e.UpdatedAt); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (db *DB) GetStyleByName(name string) (*StyleCatalogEntry, error) {
	e := &StyleCatalogEntry{}
	err := db.QueryRow(`SELECT id, name, code, form_factor, description, uop_capacity, updated_at FROM style_catalog WHERE name=?`, name).
		Scan(&e.ID, &e.Name, &e.Code, &e.FormFactor, &e.Description, &e.UOPCapacity, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return e, nil
}

func (db *DB) GetStyleByCode(code string) (*StyleCatalogEntry, error) {
	e := &StyleCatalogEntry{}
	err := db.QueryRow(`SELECT id, name, code, form_factor, description, uop_capacity, updated_at FROM style_catalog WHERE code=?`, code).
		Scan(&e.ID, &e.Name, &e.Code, &e.FormFactor, &e.Description, &e.UOPCapacity, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return e, nil
}
