package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type Bin struct {
	ID                int64      `json:"id"`
	BinTypeID         int64      `json:"bin_type_id"`
	Label             string     `json:"label"`
	Description       string     `json:"description"`
	NodeID            *int64     `json:"node_id,omitempty"`
	Status            string     `json:"status"`
	ClaimedBy         *int64     `json:"claimed_by,omitempty"`
	StagedAt          *time.Time `json:"staged_at,omitempty"`
	StagedExpiresAt   *time.Time `json:"staged_expires_at,omitempty"`
	PayloadCode       string     `json:"payload_code"`
	Manifest          *string    `json:"manifest,omitempty"`
	UOPRemaining      int        `json:"uop_remaining"`
	ManifestConfirmed bool       `json:"manifest_confirmed"`
	Locked            bool       `json:"locked"`
	LockedBy          string     `json:"locked_by"`
	LockedAt          *time.Time `json:"locked_at,omitempty"`
	LastCountedAt     *time.Time `json:"last_counted_at,omitempty"`
	LastCountedBy     string     `json:"last_counted_by"`
	LoadedAt          *time.Time `json:"loaded_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	// Joined fields
	BinTypeCode string `json:"bin_type_code"`
	NodeName    string `json:"node_name"`
}

const binJoinQuery = `SELECT b.id, b.bin_type_id, b.label, b.description, b.node_id, b.status, b.claimed_by, b.staged_at, b.staged_expires_at,
	b.payload_code, b.manifest, b.uop_remaining, b.manifest_confirmed,
	b.locked, b.locked_by, b.locked_at, b.last_counted_at, b.last_counted_by,
	b.loaded_at, b.created_at, b.updated_at,
	bt.code, COALESCE(n.name, '')
	FROM bins b
	JOIN bin_types bt ON bt.id = b.bin_type_id
	LEFT JOIN nodes n ON n.id = b.node_id`

func scanBin(row interface{ Scan(...any) error }) (*Bin, error) {
	var b Bin
	var nodeID, claimedBy sql.NullInt64
	var manifest sql.NullString
	var stagedAt, stagedExpiresAt, lockedAt, lastCountedAt, loadedAt, createdAt, updatedAt any
	err := row.Scan(&b.ID, &b.BinTypeID, &b.Label, &b.Description, &nodeID, &b.Status, &claimedBy,
		&stagedAt, &stagedExpiresAt,
		&b.PayloadCode, &manifest, &b.UOPRemaining, &b.ManifestConfirmed,
		&b.Locked, &b.LockedBy, &lockedAt, &lastCountedAt, &b.LastCountedBy,
		&loadedAt, &createdAt, &updatedAt, &b.BinTypeCode, &b.NodeName)
	if err != nil {
		return nil, err
	}
	if nodeID.Valid {
		b.NodeID = &nodeID.Int64
	}
	if claimedBy.Valid {
		b.ClaimedBy = &claimedBy.Int64
	}
	if manifest.Valid {
		b.Manifest = &manifest.String
	}
	b.StagedAt = parseTimePtr(stagedAt)
	b.StagedExpiresAt = parseTimePtr(stagedExpiresAt)
	b.LockedAt = parseTimePtr(lockedAt)
	b.LastCountedAt = parseTimePtr(lastCountedAt)
	b.LoadedAt = parseTimePtr(loadedAt)
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
	id, err := db.insertID(`INSERT INTO bins (bin_type_id, label, description, node_id, status) VALUES (?, ?, ?, ?, ?) RETURNING id`,
		b.BinTypeID, b.Label, b.Description, nullableInt64(b.NodeID), b.Status)
	if err != nil {
		return fmt.Errorf("create bin: %w", err)
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

// NodeTileState holds summary flags for rendering a node tile.
type NodeTileState struct {
	HasPayload  bool // bin with a confirmed payload
	HasEmptyBin bool // bin with no payload or unconfirmed manifest
	Claimed     bool
	Staged      bool
	Maintenance bool // bin in maintenance or flagged
}

// NodeTileStates returns per-node tile rendering state for all nodes that have bins.
func (db *DB) NodeTileStates() (map[int64]NodeTileState, error) {
	rows, err := db.Query(`SELECT b.node_id,
		MAX(CASE WHEN b.manifest IS NOT NULL AND b.manifest_confirmed = 1 THEN 1 ELSE 0 END),
		MAX(CASE WHEN b.manifest IS NULL OR b.manifest_confirmed = 0 THEN 1 ELSE 0 END),
		MAX(CASE WHEN b.claimed_by IS NOT NULL THEN 1 ELSE 0 END),
		MAX(CASE WHEN b.status = 'staged' THEN 1 ELSE 0 END),
		MAX(CASE WHEN b.status IN ('maintenance', 'flagged', 'quality_hold') THEN 1 ELSE 0 END)
		FROM bins b
		WHERE b.node_id IS NOT NULL
		GROUP BY b.node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := make(map[int64]NodeTileState)
	for rows.Next() {
		var nodeID int64
		var hasPayload, hasEmptyBin, claimed, staged, maintenance int
		if err := rows.Scan(&nodeID, &hasPayload, &hasEmptyBin, &claimed, &staged, &maintenance); err != nil {
			return nil, err
		}
		states[nodeID] = NodeTileState{
			HasPayload:  hasPayload == 1,
			HasEmptyBin: hasEmptyBin == 1,
			Claimed:     claimed == 1,
			Staged:      staged == 1,
			Maintenance: maintenance == 1,
		}
	}
	return states, rows.Err()
}

// MoveBin moves a bin to a new node.
func (db *DB) MoveBin(binID, toNodeID int64) error {
	_, err := db.Exec(db.Q(`UPDATE bins SET node_id=?, updated_at=datetime('now') WHERE id=?`), toNodeID, binID)
	return err
}

// ListAvailableBins returns bins with no manifest (empty, available for loading).
func (db *DB) ListAvailableBins() ([]*Bin, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s WHERE (b.manifest IS NULL OR b.payload_code = '') ORDER BY b.id`, binJoinQuery)))
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
// Fails if the bin is locked.
func (db *DB) ClaimBin(binID, orderID int64) error {
	res, err := db.Exec(db.Q(`UPDATE bins SET claimed_by=?, updated_at=datetime('now') WHERE id=? AND locked=0`), orderID, binID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bin %d is locked or does not exist", binID)
	}
	return nil
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

// FindEmptyCompatibleBin finds an unclaimed, available bin with no manifest that is
// compatible with the given payload code (via payload_bin_types) at an enabled physical node.
// Prefers bins in the given zone, then falls back to any zone.
func (db *DB) FindEmptyCompatibleBin(payloadCode, preferZone string) (*Bin, error) {
	// Zone-preferred query
	if preferZone != "" {
		row := db.QueryRow(db.Q(fmt.Sprintf(`%s
			JOIN payload_bin_types pbt ON pbt.bin_type_id = b.bin_type_id
			JOIN payloads p ON p.id = pbt.payload_id
			WHERE p.code = ?
			  AND b.status = 'available'
			  AND b.claimed_by IS NULL
			  AND b.locked = 0
			  AND b.node_id IS NOT NULL
			  AND n.enabled = 1
			  AND n.is_synthetic = 0
			  AND n.zone = ?
			  AND (b.manifest IS NULL OR b.payload_code = '')
			ORDER BY b.id ASC
			LIMIT 1`, binJoinQuery)), payloadCode, preferZone)
		bin, err := scanBin(row)
		if err == nil {
			return bin, nil
		}
	}
	// Any zone fallback
	row := db.QueryRow(db.Q(fmt.Sprintf(`%s
		JOIN payload_bin_types pbt ON pbt.bin_type_id = b.bin_type_id
		JOIN payloads p ON p.id = pbt.payload_id
		WHERE p.code = ?
		  AND b.status = 'available'
		  AND b.claimed_by IS NULL
		  AND b.locked = 0
		  AND b.node_id IS NOT NULL
		  AND n.enabled = 1
		  AND n.is_synthetic = 0
		  AND (b.manifest IS NULL OR b.payload_code = '')
		ORDER BY b.id ASC
		LIMIT 1`, binJoinQuery)), payloadCode)
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

// LockBin prevents automated claiming/movement of a bin.
func (db *DB) LockBin(binID int64, actor string) error {
	res, err := db.Exec(db.Q(`UPDATE bins SET locked=1, locked_by=?, locked_at=datetime('now'), updated_at=datetime('now') WHERE id=? AND locked=0`),
		actor, binID)
	if err != nil {
		return fmt.Errorf("lock bin: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bin %d is already locked", binID)
	}
	return nil
}

// UnlockBin clears the lock on a bin.
func (db *DB) UnlockBin(binID int64) error {
	_, err := db.Exec(db.Q(`UPDATE bins SET locked=0, locked_by='', locked_at=NULL, updated_at=datetime('now') WHERE id=?`), binID)
	return err
}

// RecordBinCount updates UOP and records the count timestamp.
func (db *DB) RecordBinCount(binID int64, actualUOP int, actor string) error {
	_, err := db.Exec(db.Q(`UPDATE bins SET uop_remaining=?, last_counted_at=datetime('now'), last_counted_by=?, updated_at=datetime('now') WHERE id=?`),
		actualUOP, actor, binID)
	return err
}

// UnconfirmBinManifest resets the manifest confirmation flag.
func (db *DB) UnconfirmBinManifest(binID int64) error {
	_, err := db.Exec(db.Q(`UPDATE bins SET manifest_confirmed=0, updated_at=datetime('now') WHERE id=?`), binID)
	return err
}

// ListOrdersByBin returns recent orders involving a specific bin.
func (db *DB) ListOrdersByBin(binID int64, limit int) ([]*Order, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM orders WHERE bin_id=? ORDER BY id DESC LIMIT ?`, orderSelectCols)), binID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

// BinHasNotes returns a map indicating which bins have audit log entries.
func (db *DB) BinHasNotes(binIDs []int64) (map[int64]bool, error) {
	result := make(map[int64]bool)
	if len(binIDs) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(binIDs))
	args := make([]any, len(binIDs))
	for i, id := range binIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`SELECT DISTINCT entity_id FROM audit_log WHERE entity_type='bin' AND entity_id IN (%s)`,
		strings.Join(placeholders, ","))
	rows, err := db.Query(db.Q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			result[id] = true
		}
	}
	return result, rows.Err()
}
