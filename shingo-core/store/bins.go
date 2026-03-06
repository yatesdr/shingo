package store

import (
	"database/sql"
	"fmt"
	"time"
)

type Bin struct {
	ID              int64      `json:"id"`
	BinTypeID       int64      `json:"bin_type_id"`
	Label           string     `json:"label"`
	Description     string     `json:"description"`
	NodeID          *int64     `json:"node_id,omitempty"`
	Status          string     `json:"status"`
	ClaimedBy       *int64     `json:"claimed_by,omitempty"`
	StagedAt        *time.Time `json:"staged_at,omitempty"`
	StagedExpiresAt *time.Time `json:"staged_expires_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	// Joined fields
	BinTypeCode string `json:"bin_type_code"`
	NodeName    string `json:"node_name"`
}

const binJoinQuery = `SELECT b.id, b.bin_type_id, b.label, b.description, b.node_id, b.status, b.claimed_by, b.staged_at, b.staged_expires_at, b.created_at, b.updated_at,
	bt.code, COALESCE(n.name, '')
	FROM bins b
	JOIN bin_types bt ON bt.id = b.bin_type_id
	LEFT JOIN nodes n ON n.id = b.node_id`

func scanBin(row interface{ Scan(...any) error }) (*Bin, error) {
	var b Bin
	var nodeID, claimedBy sql.NullInt64
	var stagedAt, stagedExpiresAt, createdAt, updatedAt any
	err := row.Scan(&b.ID, &b.BinTypeID, &b.Label, &b.Description, &nodeID, &b.Status, &claimedBy,
		&stagedAt, &stagedExpiresAt, &createdAt, &updatedAt, &b.BinTypeCode, &b.NodeName)
	if err != nil {
		return nil, err
	}
	if nodeID.Valid {
		b.NodeID = &nodeID.Int64
	}
	if claimedBy.Valid {
		b.ClaimedBy = &claimedBy.Int64
	}
	b.StagedAt = parseTimePtr(stagedAt)
	b.StagedExpiresAt = parseTimePtr(stagedExpiresAt)
	b.CreatedAt = parseTime(createdAt)
	b.UpdatedAt = parseTime(updatedAt)
	return &b, nil
}

func scanBins(rows *sql.Rows) ([]*Bin, error) {
	var bins []*Bin
	for rows.Next() {
		b, err := scanBin(rows)
		if err != nil {
			return nil, err
		}
		bins = append(bins, b)
	}
	return bins, rows.Err()
}

func (db *DB) CreateBin(b *Bin) error {
	result, err := db.Exec(db.Q(`INSERT INTO bins (bin_type_id, label, description, node_id, status) VALUES (?, ?, ?, ?, ?)`),
		b.BinTypeID, b.Label, b.Description, nullableInt64(b.NodeID), b.Status)
	if err != nil {
		return fmt.Errorf("create bin: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("create bin last id: %w", err)
	}
	b.ID = id
	return nil
}

func (db *DB) UpdateBin(b *Bin) error {
	_, err := db.Exec(db.Q(`UPDATE bins SET bin_type_id=?, label=?, description=?, node_id=?, status=?, updated_at=datetime('now') WHERE id=?`),
		b.BinTypeID, b.Label, b.Description, nullableInt64(b.NodeID), b.Status, b.ID)
	return err
}

func (db *DB) DeleteBin(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM bins WHERE id=?`), id)
	return err
}

func (db *DB) GetBin(id int64) (*Bin, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`%s WHERE b.id=?`, binJoinQuery)), id)
	return scanBin(row)
}

func (db *DB) GetBinByLabel(label string) (*Bin, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`%s WHERE b.label=?`, binJoinQuery)), label)
	return scanBin(row)
}

func (db *DB) ListBins() ([]*Bin, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s ORDER BY b.id DESC`, binJoinQuery)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}

func (db *DB) ListBinsByNode(nodeID int64) ([]*Bin, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s WHERE b.node_id=? ORDER BY b.id DESC`, binJoinQuery)), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}

func (db *DB) CountBinsByNode(nodeID int64) (int, error) {
	var count int
	err := db.QueryRow(db.Q(`SELECT COUNT(*) FROM bins WHERE node_id=?`), nodeID).Scan(&count)
	return count, err
}

// CountBinsByAllNodes returns a map of node_id -> bin count for all nodes that have bins.
func (db *DB) CountBinsByAllNodes() (map[int64]int, error) {
	rows, err := db.Query(`SELECT node_id, COUNT(*) FROM bins WHERE node_id IS NOT NULL GROUP BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[int64]int)
	for rows.Next() {
		var nodeID int64
		var count int
		if err := rows.Scan(&nodeID, &count); err != nil {
			return nil, err
		}
		counts[nodeID] = count
	}
	return counts, rows.Err()
}

// MoveBin moves a bin to a new node.
func (db *DB) MoveBin(binID, toNodeID int64) error {
	_, err := db.Exec(db.Q(`UPDATE bins SET node_id=?, updated_at=datetime('now') WHERE id=?`), toNodeID, binID)
	return err
}

// ListAvailableBins returns bins not currently assigned to a payload (available for new payloads).
func (db *DB) ListAvailableBins() ([]*Bin, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s WHERE b.id NOT IN (SELECT bin_id FROM payloads WHERE bin_id IS NOT NULL) ORDER BY b.id`, binJoinQuery)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}

// ListBinsByType returns all bins of a given bin type.
func (db *DB) ListBinsByType(binTypeID int64) ([]*Bin, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s WHERE b.bin_type_id=? ORDER BY b.label`, binJoinQuery)), binTypeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}

// ClaimBin marks a bin as claimed by an order to prevent double-dispatch.
func (db *DB) ClaimBin(binID, orderID int64) error {
	_, err := db.Exec(db.Q(`UPDATE bins SET claimed_by=?, updated_at=datetime('now') WHERE id=?`), orderID, binID)
	return err
}

// UnclaimBin releases a bin from an order claim.
func (db *DB) UnclaimBin(binID int64) error {
	_, err := db.Exec(db.Q(`UPDATE bins SET claimed_by=NULL, updated_at=datetime('now') WHERE id=?`), binID)
	return err
}

// UnclaimOrderBins releases all bins claimed by a specific order.
func (db *DB) UnclaimOrderBins(orderID int64) {
	db.Exec(db.Q(`UPDATE bins SET claimed_by=NULL, updated_at=datetime('now') WHERE claimed_by=?`), orderID)
}

// FindEmptyCompatibleBin finds an unclaimed, available bin with no payload that is
// compatible with the given blueprint (via blueprint_bin_types) at an enabled physical node.
// Prefers bins in the given zone, then falls back to any zone.
func (db *DB) FindEmptyCompatibleBin(blueprintCode, preferZone string) (*Bin, error) {
	// Zone-preferred query
	if preferZone != "" {
		row := db.QueryRow(db.Q(fmt.Sprintf(`%s
			JOIN blueprint_bin_types bbt ON bbt.bin_type_id = b.bin_type_id
			JOIN blueprints bp ON bp.id = bbt.blueprint_id
			WHERE bp.code = ?
			  AND b.status = 'available'
			  AND b.claimed_by IS NULL
			  AND b.node_id IS NOT NULL
			  AND n.enabled = 1
			  AND n.is_synthetic = 0
			  AND n.zone = ?
			  AND b.id NOT IN (SELECT bin_id FROM payloads WHERE bin_id IS NOT NULL)
			ORDER BY b.id ASC
			LIMIT 1`, binJoinQuery)), blueprintCode, preferZone)
		bin, err := scanBin(row)
		if err == nil {
			return bin, nil
		}
	}
	// Any zone fallback
	row := db.QueryRow(db.Q(fmt.Sprintf(`%s
		JOIN blueprint_bin_types bbt ON bbt.bin_type_id = b.bin_type_id
		JOIN blueprints bp ON bp.id = bbt.blueprint_id
		WHERE bp.code = ?
		  AND b.status = 'available'
		  AND b.claimed_by IS NULL
		  AND b.node_id IS NOT NULL
		  AND n.enabled = 1
		  AND n.is_synthetic = 0
		  AND b.id NOT IN (SELECT bin_id FROM payloads WHERE bin_id IS NOT NULL)
		ORDER BY b.id ASC
		LIMIT 1`, binJoinQuery)), blueprintCode)
	return scanBin(row)
}

// UpdateBinStatus sets the status on a bin.
func (db *DB) UpdateBinStatus(binID int64, status string) error {
	_, err := db.Exec(db.Q(`UPDATE bins SET status=?, updated_at=datetime('now') WHERE id=?`), status, binID)
	return err
}

// StageBin marks a bin as staged with expiry tracking.
// If expiresAt is nil, the bin is staged permanently (no auto-release).
func (db *DB) StageBin(binID int64, expiresAt *time.Time) error {
	_, err := db.Exec(db.Q(`UPDATE bins SET status='staged', staged_at=datetime('now'), staged_expires_at=?, updated_at=datetime('now') WHERE id=?`),
		nullableTime(expiresAt), binID)
	return err
}

// ReleaseStagedBin clears the staged status on a single bin, setting it back to available.
func (db *DB) ReleaseStagedBin(binID int64) error {
	_, err := db.Exec(db.Q(`UPDATE bins SET status='available', staged_at=NULL, staged_expires_at=NULL, updated_at=datetime('now') WHERE id=?`), binID)
	return err
}

// ReleaseExpiredStagedBins releases staged bins whose expiry has passed.
// Returns the number of bins released.
func (db *DB) ReleaseExpiredStagedBins() (int, error) {
	result, err := db.Exec(db.Q(`UPDATE bins SET status='available', staged_at=NULL, staged_expires_at=NULL, updated_at=datetime('now') WHERE status='staged' AND staged_expires_at IS NOT NULL AND staged_expires_at < datetime('now')`))
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// UpdateOrderBinID sets the bin_id on an order.
func (db *DB) UpdateOrderBinID(orderID, binID int64) error {
	_, err := db.Exec(db.Q(`UPDATE orders SET bin_id=?, updated_at=datetime('now') WHERE id=?`), binID, orderID)
	return err
}
