package payloads

import (
	"database/sql"

	"shingocore/domain"
)

// ManifestItem is the payload-template manifest line-item domain
// type. The struct lives in shingocore/domain as PayloadManifestItem
// (Stage 2A); this alias keeps the payloads.ManifestItem name used by
// CreateItem/ListManifest and the outer store/ payload_manifest.go
// re-export (store.PayloadManifestItem).
type ManifestItem = domain.PayloadManifestItem

// Line is what a caller STATES about one manifest line: which part, what that
// part's controls identity is, and how many of it one production cycle uses.
//
// CATID IS NOT A COLUMN ON THIS TABLE and never becomes one. It travels here
// because the payloads page is where a part is originated — the person typing
// the part number is the person who knows its cat id — and it is written to the
// PART. That is the whole correction: the manifest names parts, and a part
// carries its own controls name.
type Line struct {
	PartNumber    string
	CATID         string
	PartsPerCycle int64
	Description   string
}

// CreateItem inserts a manifest line and sets item.ID on success. It resolves
// the line's part first, so a line always points at a parts row.
func CreateItem(db *sql.DB, item *ManifestItem, catid string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	part, err := UpsertPart(tx, item.PartNumber, catid, "")
	if err != nil {
		return err
	}
	id, err := insertManifestLine(tx, item.PayloadID, Line{
		PartNumber:    item.PartNumber,
		PartsPerCycle: item.PartsPerCycle,
		Description:   item.Description,
	}, part.ID)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	item.ID = id
	item.PartID = part.ID
	return nil
}

// UpdateItem changes a manifest line's part, its cat id, and its per-cycle
// ratio. Re-pointing the line at a different part is an ordinary edit; minting
// the part it now names is part of the same write.
func UpdateItem(db *sql.DB, id int64, partNumber, catid string, partsPerCycle int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	part, err := UpsertPart(tx, partNumber, catid, "")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE payload_manifest SET part_number=$1, part_id=$2, parts_per_cycle=$3 WHERE id=$4`,
		partNumber, part.ID, partsPerCycle, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteItem removes a manifest line.
func DeleteItem(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM payload_manifest WHERE id=$1`, id)
	return err
}

// ListManifest returns all manifest items for a payload, ordered by insertion.
func ListManifest(db *sql.DB, payloadID int64) ([]*ManifestItem, error) {
	rows, err := db.Query(`SELECT pm.id, pm.payload_id, pm.part_number, COALESCE(pm.part_id, 0),
			COALESCE(pt.catid, ''), pm.parts_per_cycle, pm.description, pm.created_at
		FROM payload_manifest pm
		LEFT JOIN parts pt ON pt.id = pm.part_id
		WHERE pm.payload_id=$1 ORDER BY pm.id`, payloadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*ManifestItem
	for rows.Next() {
		item := &ManifestItem{}
		if err := rows.Scan(&item.ID, &item.PayloadID, &item.PartNumber, &item.PartID,
			&item.CATID, &item.PartsPerCycle, &item.Description, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ReplaceManifest wipes the payload's manifest and re-inserts the given lines,
// minting or matching each line's part on the way through.
//
// ONE TRANSACTION FOR BOTH HALVES. A part that exists with no line pointing at
// it is harmless; a LINE with no part is the state this whole correction
// removes, so the two writes cannot be allowed to come apart. A cat-id conflict
// (ErrCATIDConflict) aborts the whole replace rather than saving some lines —
// the operator is being asked a question, and a half-saved manifest is a bad
// way to ask it.
func ReplaceManifest(db *sql.DB, payloadID int64, lines []Line) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM payload_manifest WHERE payload_id=$1`, payloadID); err != nil {
		return err
	}
	for _, l := range lines {
		part, err := UpsertPart(tx, l.PartNumber, l.CATID, "")
		if err != nil {
			return err
		}
		if _, err := insertManifestLine(tx, payloadID, l, part.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func insertManifestLine(tx *sql.Tx, payloadID int64, l Line, partID int64) (int64, error) {
	var id int64
	err := tx.QueryRow(`INSERT INTO payload_manifest (payload_id, part_number, part_id, parts_per_cycle, description)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		payloadID, l.PartNumber, partID, l.PartsPerCycle, l.Description).Scan(&id)
	return id, err
}
