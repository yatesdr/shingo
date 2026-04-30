// Package bins holds bin-aggregate persistence for shingo-core.
//
// Stage 2D of the architecture plan moved bin CRUD, bin types, bin
// manifest operations, and node↔bin-type bindings out of the flat
// store/ package and into this sub-package. The outer store/ keeps
// type aliases (`store.Bin = bins.Bin`, etc.) and one-line delegate
// methods on *store.DB so callers see no public API change.
// Cross-aggregate methods (those whose return type or mutations span
// multiple aggregates, e.g. SetBinManifestFromTemplate) stay at the
// outer store/ level as composition methods.
package bins

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"shingocore/domain"
	"shingocore/store/internal/helpers"
)

// Bin is the bin domain entity. The struct lives in shingocore/domain
// (Stage 2A); this alias keeps the bins.Bin name that every read
// helper, scan function, and Create/Update call in this package uses.
// store.Bin aliases onto this in turn, so call sites across the
// codebase compile unchanged.
type Bin = domain.Bin

// binJoinQuery is the SELECT prefix used by every bin-reading query.
// Export as BinJoinQuery so cross-aggregate readers at the outer store/
// level (which need to add their own WHERE clauses) can reuse it.
const BinJoinQuery = `SELECT b.id, b.bin_type_id, b.label, b.description, b.node_id, b.status, b.claimed_by, b.staged_at, b.staged_expires_at,
	COALESCE(b.payload_code, ''), b.manifest, b.uop_remaining, b.manifest_confirmed,
	b.locked, b.locked_by, b.locked_at, b.last_counted_at, b.last_counted_by,
	b.loaded_at, b.created_at, b.updated_at,
	bt.code, COALESCE(n.name, '')
	FROM bins b
	JOIN bin_types bt ON bt.id = b.bin_type_id
	LEFT JOIN nodes n ON n.id = b.node_id`

// PayloadBinTypeAdvisoryClause enforces payload_bin_types as an advisory
// allow-list: when the table has rules for the payload, only matching bin
// types are eligible; when no rules exist for the payload, any bin type
// is eligible. Used by FindEmptyCompatible (empty-bin retrieve) and
// FindSourceFIFO (full-bin retrieve) so the two readers stay coherent.
//
// Both branches reference $1 (payloadCode) — callers must place
// payloadCode at parameter position $1.
//
// Rationale: the allow-list table is sparsely populated in practice. A
// pre-2026-04-27 hard INNER JOIN on this table starved orders for
// payloads with no rules even when compatible empty bins existed. Every
// other reader (FindStorageDestination, SetManifest writes) ignores the
// table entirely. Advisory enforcement matches that prior practice while
// preserving the constraint for plants that DO populate the table.
const PayloadBinTypeAdvisoryClause = `
	  AND (
	    b.bin_type_id IN (
	      SELECT pbt.bin_type_id FROM payload_bin_types pbt
	      JOIN payloads p ON p.id = pbt.payload_id WHERE p.code = $1
	    )
	    OR NOT EXISTS (
	      SELECT 1 FROM payload_bin_types pbt
	      JOIN payloads p ON p.id = pbt.payload_id WHERE p.code = $1
	    )
	  )`

// ScanBin reads a single bin row (including joined bin_type code + node name).
// Exported for cross-aggregate readers at the outer store/ level.
func ScanBin(row interface{ Scan(...any) error }) (*Bin, error) {
	var b Bin
	var nodeID, claimedBy sql.NullInt64
	var manifest sql.NullString
	err := row.Scan(&b.ID, &b.BinTypeID, &b.Label, &b.Description, &nodeID, &b.Status, &claimedBy,
		&b.StagedAt, &b.StagedExpiresAt,
		&b.PayloadCode, &manifest, &b.UOPRemaining, &b.ManifestConfirmed,
		&b.Locked, &b.LockedBy, &b.LockedAt, &b.LastCountedAt, &b.LastCountedBy,
		&b.LoadedAt, &b.CreatedAt, &b.UpdatedAt, &b.BinTypeCode, &b.NodeName)
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
	return &b, nil
}

func scanBins(rows *sql.Rows) ([]*Bin, error) {
	var bins []*Bin
	for rows.Next() {
		b, err := ScanBin(rows)
		if err != nil {
			return nil, err
		}
		bins = append(bins, b)
	}
	return bins, rows.Err()
}

// Create inserts a new bin row and sets b.ID on success.
func Create(db *sql.DB, b *Bin) error {
	id, err := helpers.InsertID(db, `INSERT INTO bins (bin_type_id, label, description, node_id, status) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		b.BinTypeID, b.Label, b.Description, helpers.NullableInt64(b.NodeID), b.Status)
	if err != nil {
		return fmt.Errorf("create bin: %w", err)
	}
	b.ID = id
	return nil
}

// Update writes the mutable columns on a bin (bin_type_id, label, description,
// node_id, status).
func Update(db *sql.DB, b *Bin) error {
	_, err := db.Exec(`UPDATE bins SET bin_type_id=$1, label=$2, description=$3, node_id=$4, status=$5, updated_at=NOW() WHERE id=$6`,
		b.BinTypeID, b.Label, b.Description, helpers.NullableInt64(b.NodeID), b.Status, b.ID)
	return err
}

// Delete removes a bin.
func Delete(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM bins WHERE id=$1`, id)
	return err
}

// Get fetches a bin by ID with its joined bin_type code and node name.
func Get(db *sql.DB, id int64) (*Bin, error) {
	row := db.QueryRow(fmt.Sprintf(`%s WHERE b.id=$1`, BinJoinQuery), id)
	return ScanBin(row)
}

// GetByLabel fetches a bin by its unique label.
func GetByLabel(db *sql.DB, label string) (*Bin, error) {
	row := db.QueryRow(fmt.Sprintf(`%s WHERE b.label=$1`, BinJoinQuery), label)
	return ScanBin(row)
}

// List returns every bin ordered by ID descending.
func List(db *sql.DB) ([]*Bin, error) {
	rows, err := db.Query(fmt.Sprintf(`%s ORDER BY b.id DESC`, BinJoinQuery))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}

// ListByNode returns all non-retired bins at a node ordered by ID descending.
// Retired bins are excluded so operational consumers (node-bins telemetry,
// occupancy checks, swap-vs-move decision) don't see a retired carrier as
// occupying its old node. Audit/admin views that need retired bins should
// query via List + status filter instead. This is a hedge until retirement
// vacates the operational node entirely (RETIRED_HOLD migration — see
// SHINGO_TODO.md).
func ListByNode(db *sql.DB, nodeID int64) ([]*Bin, error) {
	rows, err := db.Query(fmt.Sprintf(`%s WHERE b.node_id=$1 AND b.status != 'retired' ORDER BY b.id DESC`, BinJoinQuery), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}

// CountByNode returns how many non-retired bins sit at the given node.
// Same retired-bin exclusion rationale as ListByNode.
func CountByNode(db *sql.DB, nodeID int64) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM bins WHERE node_id=$1 AND status != 'retired'`, nodeID).Scan(&count)
	return count, err
}

// CountByAllNodes returns a map of node_id -> bin count for all nodes that have bins.
func CountByAllNodes(db *sql.DB) (map[int64]int, error) {
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

// NodeTileState holds summary flags for rendering a node tile. The
// struct lives in shingocore/domain (Stage 2A.2); this alias keeps
// the bins.NodeTileState name that the NodeTileStates aggregator
// below and downstream callers (page-data builder, www handlers)
// reference.
type NodeTileState = domain.NodeTileState

// NodeTileStates returns per-node tile rendering state for all nodes that have bins.
func NodeTileStates(db *sql.DB) (map[int64]NodeTileState, error) {
	rows, err := db.Query(`SELECT b.node_id,
		MAX(CASE WHEN b.manifest IS NOT NULL AND b.manifest_confirmed = true THEN 1 ELSE 0 END),
		MAX(CASE WHEN b.manifest IS NULL OR b.manifest_confirmed = false THEN 1 ELSE 0 END),
		MAX(CASE WHEN b.claimed_by IS NOT NULL THEN 1 ELSE 0 END),
		MAX(CASE WHEN b.status = 'staged' THEN 1 ELSE 0 END),
		MAX(CASE WHEN b.status IN ('maintenance', 'flagged', 'quality_hold') THEN 1 ELSE 0 END)
		FROM bins b
		WHERE b.node_id IS NOT NULL AND b.status != 'retired'
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

// Move moves a bin to a new node. Returns an error if the bin is already
// at the destination (same-node move is physically impossible).
func Move(db *sql.DB, binID, toNodeID int64) error {
	res, err := db.Exec(`UPDATE bins SET node_id=$1, updated_at=NOW() WHERE id=$2 AND (node_id IS NULL OR node_id != $1)`, toNodeID, binID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bin %d is already at node %d", binID, toNodeID)
	}
	return nil
}

// ListAvailable returns bins with no payload (empty, available for loading).
//
// Empty-bin definition: COALESCE(b.payload_code, '') = ''. Same NULL-safe
// form FindEmptyCompatible uses post-2026-04-27. The previous filter
// `(manifest IS NULL OR payload_code = '')` was the same bug class as
// the FindEmptyCompatible bug fixed in 7c274ac/4337344: a bin with
// payload_code=NULL evaluates `payload_code = ''` to NULL (falsy in
// WHERE), but the OR-clause `manifest IS NULL` could rescue it. The
// COALESCE form is unambiguous about the NULL case.
//
// SetManifest and ClearManifest always set payload_code and manifest
// together, so under normal operation the two columns are correlated
// and the simpler payload_code-only filter produces identical results.
// In partial-write/legacy states where manifest is NULL but payload_code
// is non-empty, this filter correctly treats the bin as NOT available
// (it has a payload, even without a manifest blob).
func ListAvailable(db *sql.DB) ([]*Bin, error) {
	rows, err := db.Query(fmt.Sprintf(`%s WHERE COALESCE(b.payload_code, '') = '' ORDER BY b.id`, BinJoinQuery))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}

// Claim marks a bin as claimed by an order to prevent double-dispatch.
// Fails if the bin is locked or already claimed by another order.
func Claim(db *sql.DB, binID, orderID int64) error {
	res, err := db.Exec(`UPDATE bins SET claimed_by=$1, updated_at=NOW() WHERE id=$2 AND locked=false AND claimed_by IS NULL`, orderID, binID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bin %d is locked, already claimed, or does not exist", binID)
	}
	return nil
}

// Unclaim releases a bin from an order claim.
func Unclaim(db *sql.DB, binID int64) error {
	_, err := db.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE id=$1`, binID)
	return err
}

// UnclaimByOrder releases all bins claimed by a specific order.
func UnclaimByOrder(db *sql.DB, orderID int64) {
	db.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID)
}

// FindEmptyCompatible finds an unclaimed, available bin with no manifest that is
// compatible with the given payload code (via payload_bin_types) at an enabled
// physical node. Prefers bins in the given zone, then falls back to any zone.
// FindEmptyCompatible looks for an unclaimed empty bin matching payloadCode,
// preferring preferZone. excludeNodeID > 0 omits bins parked at that node —
// pass the order's destination node so the caller never receives a same-node
// bin (would produce a fleet order with src == dst, which the fleet cancels
// and the kanban demand re-fires, producing an order spam loop). Pass 0 to
// disable exclusion. See SHINGO_TODO.md "Same-node retrieve" entry.
//
// Empty-bin definition (post-2026-04-27 fix): COALESCE(payload_code, '') = ''.
// A bin with no payload_code is empty by definition — manifest is a
// derived/stale field that's unreliable when bins go through arrival
// transitions. The previous filter `(manifest IS NULL OR payload_code = '')`
// was brittle around NULL-vs-empty-string (a bin with manifest='' and
// payload_code=NULL evaluated to `false OR NULL`, treated as falsy in
// WHERE, silently rejecting genuinely-empty bins). An earlier attempt
// added `manifest_confirmed = false` but that's too strict — it requires
// the operator to have explicitly cleared the confirmation, which doesn't
// happen on every arrival path. Plant test 2026-04-27 (order #462 stuck
// on 'awaiting inventory' with empties at SMN_002 / SMN_003 visible).
//
// Compatibility enforcement (post-2026-04-27 v2 fix): advisory.
// payload_bin_types is treated as an allow-list — rows say "this payload IS
// allowed in this bin type." Absence of rows for a payload means "no
// restrictions configured" → any bin works. This matches how every other
// reader treats the table (FindSourceFIFO, FindStorageDestination, SetManifest
// all ignore it) and how the admin UI populates it. The previous form used
// hard INNER JOINs to payload_bin_types/payloads which eliminated all
// candidates when no rules existed — the cause of the 2026-04-27 starvation.
func FindEmptyCompatible(db *sql.DB, payloadCode, preferZone string, excludeNodeID int64) (*Bin, error) {
	// Zone-preferred query
	if preferZone != "" {
		row := db.QueryRow(fmt.Sprintf(`%s
			WHERE b.status = 'available'
			  AND b.claimed_by IS NULL
			  AND b.locked = false
			  AND b.node_id IS NOT NULL
			  AND n.enabled = true
			  AND n.is_synthetic = false
			  AND n.zone = $2
			  AND COALESCE(b.payload_code, '') = ''
			  AND ($3 = 0 OR b.node_id != $3)%s
			ORDER BY b.id ASC
			LIMIT 1`, BinJoinQuery, PayloadBinTypeAdvisoryClause), payloadCode, preferZone, excludeNodeID)
		bin, err := ScanBin(row)
		if err == nil {
			return bin, nil
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
		// sql.ErrNoRows: fall through to any-zone query
	}
	// Any zone fallback
	row := db.QueryRow(fmt.Sprintf(`%s
		WHERE b.status = 'available'
		  AND b.claimed_by IS NULL
		  AND b.locked = false
		  AND b.node_id IS NOT NULL
		  AND n.enabled = true
		  AND n.is_synthetic = false
		  AND COALESCE(b.payload_code, '') = ''
		  AND ($2 = 0 OR b.node_id != $2)%s
		ORDER BY b.id ASC
		LIMIT 1`, BinJoinQuery, PayloadBinTypeAdvisoryClause), payloadCode, excludeNodeID)
	return ScanBin(row)
}

// UpdateStatus sets the status on a bin.
func UpdateStatus(db *sql.DB, binID int64, status domain.BinStatus) error {
	_, err := db.Exec(`UPDATE bins SET status=$1, updated_at=NOW() WHERE id=$2`, status, binID)
	return err
}

// Stage marks a bin as staged with expiry tracking.
// If expiresAt is nil, the bin is staged permanently (no auto-release).
func Stage(db *sql.DB, binID int64, expiresAt *time.Time) error {
	_, err := db.Exec(`UPDATE bins SET status='staged', staged_at=NOW(), staged_expires_at=$1, updated_at=NOW() WHERE id=$2`,
		helpers.NullableTime(expiresAt), binID)
	return err
}

// ReleaseStaged clears the staged status on a single bin, setting it back to available.
func ReleaseStaged(db *sql.DB, binID int64) error {
	_, err := db.Exec(`UPDATE bins SET status='available', staged_at=NULL, staged_expires_at=NULL, updated_at=NOW() WHERE id=$1`, binID)
	return err
}

// ReleaseExpiredStaged releases staged bins whose expiry has passed.
// Returns the number of bins released.
func ReleaseExpiredStaged(db *sql.DB) (int, error) {
	result, err := db.Exec(`UPDATE bins SET status='available', staged_at=NULL, staged_expires_at=NULL, updated_at=NOW() WHERE status='staged' AND claimed_by IS NULL AND staged_expires_at IS NOT NULL AND staged_expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// Lock prevents automated claiming/movement of a bin.
func Lock(db *sql.DB, binID int64, actor string) error {
	res, err := db.Exec(`UPDATE bins SET locked=true, locked_by=$1, locked_at=NOW(), updated_at=NOW() WHERE id=$2 AND locked=false`,
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

// Unlock clears the lock on a bin.
func Unlock(db *sql.DB, binID int64) error {
	_, err := db.Exec(`UPDATE bins SET locked=false, locked_by='', locked_at=NULL, updated_at=NOW() WHERE id=$1`, binID)
	return err
}

// RecordCount updates UOP and records the count timestamp.
func RecordCount(db *sql.DB, binID int64, actualUOP int, actor string) error {
	_, err := db.Exec(`UPDATE bins SET uop_remaining=$1, last_counted_at=NOW(), last_counted_by=$2, updated_at=NOW() WHERE id=$3`,
		actualUOP, actor, binID)
	return err
}

// UnconfirmManifest resets the manifest confirmation flag.
func UnconfirmManifest(db *sql.DB, binID int64) error {
	_, err := db.Exec(`UPDATE bins SET manifest_confirmed=false, updated_at=NOW() WHERE id=$1`, binID)
	return err
}

// HasNotes returns a map indicating which bins have audit log entries.
func HasNotes(db *sql.DB, binIDs []int64) (map[int64]bool, error) {
	result := make(map[int64]bool)
	if len(binIDs) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(binIDs))
	args := make([]any, len(binIDs))
	for i, id := range binIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	query := fmt.Sprintf(`SELECT DISTINCT entity_id FROM audit_log WHERE entity_type='bin' AND entity_id IN (%s)`,
		strings.Join(placeholders, ","))
	rows, err := db.Query(query, args...)
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
