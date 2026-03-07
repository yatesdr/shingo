package store

import (
	"database/sql"
	"fmt"
	"time"
)

type Payload struct {
	ID                int64      `json:"id"`
	BlueprintID       int64      `json:"blueprint_id"`
	BinID             *int64     `json:"bin_id,omitempty"`
	UOPRemaining      int        `json:"uop_remaining"`
	ManifestConfirmed bool       `json:"manifest_confirmed"`
	LoadedAt          *time.Time `json:"loaded_at,omitempty"`
	Notes             string     `json:"notes"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	// Joined fields (read-only, from bin)
	ClaimedBy     *int64 `json:"claimed_by,omitempty"`
	BinStatus     string `json:"bin_status"`
	BlueprintCode string `json:"blueprint_code"`
	BinLabel      string `json:"bin_label"`
	NodeName      string `json:"node_name"`
	NodeID        *int64 `json:"node_id,omitempty"`
}

const payloadJoinQuery = `SELECT p.id, p.blueprint_id, p.bin_id, p.uop_remaining, p.manifest_confirmed, p.loaded_at, p.notes, p.created_at, p.updated_at,
	b.claimed_by, COALESCE(b.status, ''), bp.code, COALESCE(b.label, ''), COALESCE(n.name, ''), b.node_id
	FROM payloads p
	JOIN blueprints bp ON bp.id = p.blueprint_id
	LEFT JOIN bins b ON b.id = p.bin_id
	LEFT JOIN nodes n ON n.id = b.node_id`

func scanPayload(row interface{ Scan(...any) error }) (*Payload, error) {
	var p Payload
	var binID, claimedBy, nodeID sql.NullInt64
	var loadedAt, createdAt, updatedAt any

	err := row.Scan(&p.ID, &p.BlueprintID, &binID, &p.UOPRemaining, &p.ManifestConfirmed, &loadedAt, &p.Notes, &createdAt, &updatedAt,
		&claimedBy, &p.BinStatus, &p.BlueprintCode, &p.BinLabel, &p.NodeName, &nodeID)
	if err != nil {
		return nil, err
	}

	if binID.Valid {
		p.BinID = &binID.Int64
	}
	if claimedBy.Valid {
		p.ClaimedBy = &claimedBy.Int64
	}
	if nodeID.Valid {
		p.NodeID = &nodeID.Int64
	}
	p.LoadedAt = parseTimePtr(loadedAt)
	p.CreatedAt = parseTime(createdAt)
	p.UpdatedAt = parseTime(updatedAt)
	return &p, nil
}

func scanPayloads(rows *sql.Rows) ([]*Payload, error) {
	var payloads []*Payload
	for rows.Next() {
		p, err := scanPayload(rows)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, p)
	}
	return payloads, rows.Err()
}

func (db *DB) CreatePayload(p *Payload) error {
	result, err := db.Exec(db.Q(`INSERT INTO payloads (blueprint_id, bin_id, uop_remaining, manifest_confirmed, loaded_at, notes) VALUES (?, ?, ?, ?, ?, ?)`),
		p.BlueprintID, nullableInt64(p.BinID), p.UOPRemaining, p.ManifestConfirmed, nullableTime(p.LoadedAt), p.Notes)
	if err != nil {
		return fmt.Errorf("create payload: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("create payload last id: %w", err)
	}
	p.ID = id
	db.logPayloadEvent(id, PayloadEventCreated, fmt.Sprintf("blueprint_id=%d confirmed=%v", p.BlueprintID, p.ManifestConfirmed))
	return nil
}

// CreatePayloadWithManifest creates a payload and copies the blueprint's manifest
// items into the payload's manifest in a single transaction.
func (db *DB) CreatePayloadWithManifest(p *Payload) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(db.Q(`INSERT INTO payloads (blueprint_id, bin_id, uop_remaining, manifest_confirmed, loaded_at, notes) VALUES (?, ?, ?, ?, ?, ?)`),
		p.BlueprintID, nullableInt64(p.BinID), p.UOPRemaining, p.ManifestConfirmed, nullableTime(p.LoadedAt), p.Notes)
	if err != nil {
		return fmt.Errorf("create payload: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("create payload last id: %w", err)
	}
	p.ID = id

	// Copy blueprint manifest items
	rows, err := tx.Query(db.Q(`SELECT part_number, quantity, description FROM blueprint_manifest WHERE blueprint_id=? ORDER BY id`), p.BlueprintID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var partNumber, description string
			var quantity int64
			if err := rows.Scan(&partNumber, &quantity, &description); err != nil {
				continue
			}
			tx.Exec(db.Q(`INSERT INTO manifest_items (payload_id, part_number, quantity, notes) VALUES (?, ?, ?, ?)`),
				p.ID, partNumber, quantity, description)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit payload+manifest: %w", err)
	}

	db.logPayloadEvent(id, PayloadEventCreated, fmt.Sprintf("blueprint_id=%d confirmed=%v", p.BlueprintID, p.ManifestConfirmed))
	return nil
}

func (db *DB) UpdatePayload(p *Payload) error {
	_, err := db.Exec(db.Q(`UPDATE payloads SET blueprint_id=?, bin_id=?, uop_remaining=?, manifest_confirmed=?, loaded_at=?, notes=?, updated_at=datetime('now') WHERE id=?`),
		p.BlueprintID, nullableInt64(p.BinID), p.UOPRemaining, p.ManifestConfirmed, nullableTime(p.LoadedAt), p.Notes, p.ID)
	return err
}

func (db *DB) DeletePayload(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM payloads WHERE id=?`), id)
	return err
}

func (db *DB) GetPayload(id int64) (*Payload, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`%s WHERE p.id=?`, payloadJoinQuery)), id)
	return scanPayload(row)
}

func (db *DB) ListPayloads() ([]*Payload, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s ORDER BY p.id DESC`, payloadJoinQuery)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloads(rows)
}

// ListPayloadsByNode returns payloads at a node via bin join.
func (db *DB) ListPayloadsByNode(nodeID int64) ([]*Payload, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s WHERE b.node_id=? ORDER BY p.id DESC`, payloadJoinQuery)), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloads(rows)
}

// FindSourcePayloadFIFO finds the best unclaimed payload at an enabled storage node using FIFO.
func (db *DB) FindSourcePayloadFIFO(blueprintCode string) (*Payload, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`%s
		WHERE bp.code = ?
		  AND n.enabled = 1
		  AND n.is_synthetic = 0
		  AND b.claimed_by IS NULL
		  AND p.manifest_confirmed = 1
		  AND COALESCE(b.status, 'available') NOT IN ('staged', 'maintenance', 'flagged', 'retired')
		ORDER BY COALESCE(p.loaded_at, p.created_at) ASC
		LIMIT 1`, payloadJoinQuery)), blueprintCode)
	return scanPayload(row)
}

// FindStorageDestination finds the best storage node for a payload's blueprint.
// Each physical node holds at most one bin.
func (db *DB) FindStorageDestination(blueprintID int64) (*Node, error) {
	// Try consolidation: storage nodes that already have bins with payloads of this blueprint.
	row := db.QueryRow(db.Q(fmt.Sprintf(`
		SELECT %s %s WHERE n.id = (
			SELECT sn.id
			FROM nodes sn
			JOIN bins match_b ON match_b.node_id = sn.id
			JOIN payloads match_p ON match_p.bin_id = match_b.id AND match_p.blueprint_id = ?
			LEFT JOIN bins total_b ON total_b.node_id = sn.id
			WHERE sn.enabled = 1 AND sn.is_synthetic = 0
			GROUP BY sn.id
			HAVING COUNT(DISTINCT total_b.id) < 1
			ORDER BY COUNT(DISTINCT match_b.id) DESC
			LIMIT 1
		)`, nodeSelectCols, nodeFromClause)), blueprintID)
	n, err := scanNode(row)
	if err == nil {
		return n, nil
	}

	// Fall back to emptiest storage node (no bins)
	row = db.QueryRow(db.Q(fmt.Sprintf(`
		SELECT %s %s WHERE n.id = (
			SELECT sn.id
			FROM nodes sn
			LEFT JOIN bins sb ON sb.node_id = sn.id
			WHERE sn.enabled = 1 AND sn.is_synthetic = 0
			GROUP BY sn.id
			HAVING COUNT(sb.id) < 1
			ORDER BY COUNT(sb.id) ASC
			LIMIT 1
		)`, nodeSelectCols, nodeFromClause)))
	return scanNode(row)
}

// ListPayloadsByBin returns all payloads associated with a specific bin.
func (db *DB) ListPayloadsByBin(binID int64) ([]*Payload, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s WHERE p.bin_id=?`, payloadJoinQuery)), binID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloads(rows)
}

