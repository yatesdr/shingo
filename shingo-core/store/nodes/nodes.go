// Package nodes holds node-aggregate persistence for shingo-core.
//
// Stage 2D of the architecture plan moved node CRUD, node types,
// node group/lane layout, node properties, station/payload bindings,
// and node-state queries out of the flat store/ package and into this
// sub-package. The outer store/ keeps type aliases (`store.Node = nodes.Node`,
// etc.) and one-line delegate methods on *store.DB so callers see no
// public API change. Cross-aggregate helpers (those that join bins or
// payloads — GetGroupLayout, ListNodeStates, ListPayloadsForNode,
// FindSourceBinInLane, etc.) stay at the outer store/ level as
// composition methods.
package nodes

import (
	"database/sql"
	"fmt"
	"strings"

	"shingocore/domain"
	"shingocore/store/internal/helpers"
)

// Node is the node domain entity. The struct lives in shingocore/domain
// (Stage 2A); this alias keeps the nodes.Node name used by every read
// helper, ScanNode, Create/Update, and cross-aggregate callers at the
// outer store/ level.
type Node = domain.Node

// SelectCols and FromClause are exported so cross-aggregate readers
// at the outer store/ level can compose their own WHERE clauses.
const SelectCols = `n.id, n.name, n.is_synthetic, n.zone, n.enabled, n.depth, n.created_at, n.updated_at, n.node_type_id, n.parent_id, COALESCE(nt.code, ''), COALESCE(nt.name, ''), COALESCE(pn.name, ''), n.claimed_by`
const FromClause = `FROM nodes n LEFT JOIN node_types nt ON nt.id = n.node_type_id LEFT JOIN nodes pn ON pn.id = n.parent_id`

// ScanNode reads a single nodes row (with joined node_type and parent name).
// Exported for cross-aggregate readers at the outer store/ level.
func ScanNode(row interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var depth sql.NullInt32
	var nodeTypeID, parentID, claimedBy sql.NullInt64
	err := row.Scan(&n.ID, &n.Name, &n.IsSynthetic, &n.Zone, &n.Enabled, &depth, &n.CreatedAt, &n.UpdatedAt,
		&nodeTypeID, &parentID, &n.NodeTypeCode, &n.NodeTypeName, &n.ParentName, &claimedBy)
	if err != nil {
		return nil, err
	}
	if depth.Valid {
		d := int(depth.Int32)
		n.Depth = &d
	}
	if nodeTypeID.Valid {
		n.NodeTypeID = &nodeTypeID.Int64
	}
	if parentID.Valid {
		n.ParentID = &parentID.Int64
	}
	if claimedBy.Valid {
		n.ClaimedBy = &claimedBy.Int64
	}
	return &n, nil
}

// ScanNodes reads all nodes rows from a *sql.Rows.
func ScanNodes(rows *sql.Rows) ([]*Node, error) {
	var nodes []*Node
	for rows.Next() {
		n, err := ScanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// ClaimSlot — the un-seatbelted slot CAS — was deleted when the hard slot loop
// (its only production caller) was removed. The live slot-claim path is the
// reservation-guarded ClaimSlotTx, reached via db.ConfirmSlotClaim (reserve → claim
// → confirm in one tx). A forbidigo rule guards against reintroducing a raw slot
// claim; tests needing a claimed-slot fixture use testdb.ClaimSlotForTest.

// ClaimSlotTx is the reservation-guarded slot claim used by ConfirmSlotClaim — the
// slot mirror of bins.ClaimTx. It ADDS the demoted-CAS seatbelt to the
// owner-idempotent CAS + NOT EXISTS(bins) guard: EXISTS a pending slot reservation
// for (order, node), so a slot can only be hard-claimed under a live plan-time
// reservation (the split-brain fix). Runs inside the caller's tx so the claim
// and the reservation confirm commit atomically. Owner-idempotent (claimed_by=$1),
// so a claimed-but-pending slot heals on retry instead of wedging.
//
// The legacy ClaimSlot above (the still-live hard-claim loop path) deliberately
// does NOT carry the reservation clause — the loop never reserves — and is retired
// WITH that loop, at which point this is the only slot-claim path. Seatbelts only
// ever gain clauses; ClaimSlot is not weakened.
func ClaimSlotTx(tx *sql.Tx, nodeID, orderID int64) error {
	res, err := tx.Exec(`UPDATE nodes SET claimed_by=$1, updated_at=NOW()
		WHERE id=$2 AND (claimed_by IS NULL OR claimed_by=$1)
		  AND NOT EXISTS (SELECT 1 FROM bins b WHERE b.node_id = $2)
		  AND EXISTS (SELECT 1 FROM reservations
		              WHERE order_id=$1 AND node_id=$2
		                AND resource_kind='slot' AND state='pending')`, orderID, nodeID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("slot %d claim refused: already claimed, occupied, or no pending reservation", nodeID)
	}
	return nil
}

// UnclaimSlot releases a single slot's claim. Mirrors bins.Unclaim.
func UnclaimSlot(db *sql.DB, nodeID int64) error {
	_, err := db.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE id=$1`, nodeID)
	return err
}

// UnclaimOrderSlots releases all slots claimed by a specific order. Mirrors
// bins.UnclaimByOrder — it must be called from the same terminal/cleanup hooks
// so a terminated order never strands a slot claim (the bin-claim path had a
// leaked-claim failure mode under partial failure, fixed by reconciliation;
// the slot claim inherits that by riding the same hooks).
func UnclaimOrderSlots(db *sql.DB, orderID int64) error {
	_, err := db.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID)
	return err
}

// Create inserts a new node and sets n.ID on success.
func Create(db *sql.DB, n *Node) error {
	// Defense-in-depth: callers should pass an already-trimmed name
	// (admin handler trims at form ingress); this guards non-handler
	// callers and the RDS/fleet-sync path. Silent — no warning log.
	n.Name = strings.TrimSpace(n.Name)
	id, err := helpers.InsertID(db, `INSERT INTO nodes (name, is_synthetic, zone, enabled, depth, node_type_id, parent_id) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		n.Name, n.IsSynthetic, n.Zone, n.Enabled, helpers.NullableInt(n.Depth), helpers.NullableInt64(n.NodeTypeID), helpers.NullableInt64(n.ParentID))
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	n.ID = id
	return nil
}

// Update writes the mutable columns on a node.
func Update(db *sql.DB, n *Node) error {
	n.Name = strings.TrimSpace(n.Name)
	_, err := db.Exec(`UPDATE nodes SET name=$1, is_synthetic=$2, zone=$3, enabled=$4, depth=$5, node_type_id=$6, parent_id=$7, updated_at=NOW() WHERE id=$8`,
		n.Name, n.IsSynthetic, n.Zone, n.Enabled, helpers.NullableInt(n.Depth), helpers.NullableInt64(n.NodeTypeID), helpers.NullableInt64(n.ParentID), n.ID)
	if err != nil {
		return fmt.Errorf("update node: %w", err)
	}
	return nil
}

// Delete removes a node by ID.
func Delete(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM nodes WHERE id=$1`, id)
	return err
}

// Get fetches a node by ID.
func Get(db *sql.DB, id int64) (*Node, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s %s WHERE n.id=$1`, SelectCols, FromClause), id)
	return ScanNode(row)
}

// GetByName fetches a node by its unique name.
func GetByName(db *sql.DB, name string) (*Node, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s %s WHERE n.name=$1`, SelectCols, FromClause), name)
	return ScanNode(row)
}

// GetByDotName resolves a node name that may use dot-notation (PARENT.CHILD).
// If the name contains a dot, it looks up the child under the given parent.
// Otherwise it falls back to GetByName.
func GetByDotName(db *sql.DB, name string) (*Node, error) {
	parts := strings.SplitN(name, ".", 2)
	if len(parts) != 2 {
		return GetByName(db, name)
	}
	row := db.QueryRow(fmt.Sprintf(`SELECT %s %s WHERE n.name=$1 AND n.parent_id IN (SELECT id FROM nodes WHERE name=$2)`, SelectCols, FromClause), parts[1], parts[0])
	return ScanNode(row)
}

// GetRoot walks the parent_id chain from the given node up to the
// top-level ancestor (where parent_id IS NULL) and returns it.
// If the node has no parent, it returns the node itself.
func GetRoot(db *sql.DB, nodeID int64) (*Node, error) {
	row := db.QueryRow(fmt.Sprintf(`
		WITH RECURSIVE ancestors AS (
			SELECT id, parent_id FROM nodes WHERE id = $1
			UNION ALL
			SELECT n.id, n.parent_id FROM nodes n JOIN ancestors a ON n.id = a.parent_id
		)
		SELECT %s %s WHERE n.id = (SELECT id FROM ancestors WHERE parent_id IS NULL)`, SelectCols, FromClause), nodeID)
	return ScanNode(row)
}

// List returns every node ordered by name.
func List(db *sql.DB) ([]*Node, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s %s ORDER BY n.name`, SelectCols, FromClause))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanNodes(rows)
}

// ListChildren returns all direct children of a parent ordered by name.
func ListChildren(db *sql.DB, parentID int64) ([]*Node, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s %s WHERE n.parent_id=$1 ORDER BY n.name`, SelectCols, FromClause), parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanNodes(rows)
}

// SetParent assigns a node to a parent.
func SetParent(db *sql.DB, nodeID, parentID int64) error {
	_, err := db.Exec(`UPDATE nodes SET parent_id=$1, updated_at=NOW() WHERE id=$2`, parentID, nodeID)
	return err
}

// ClearParent removes a node's parent assignment.
func ClearParent(db *sql.DB, nodeID int64) error {
	_, err := db.Exec(`UPDATE nodes SET parent_id=NULL, updated_at=NOW() WHERE id=$1`, nodeID)
	return err
}

// Reparent moves a node into a new parent (or removes it from a parent).
// When adopting into a lane, it sets the depth based on position.
// When orphaning, it clears depth and role properties.
func Reparent(db *sql.DB, nodeID int64, parentID *int64, position int) error {
	if parentID == nil {
		if err := ClearParent(db, nodeID); err != nil {
			return err
		}
		db.Exec(`UPDATE nodes SET depth=NULL WHERE id=$1`, nodeID)
		DeleteProperty(db, nodeID, "role")
		return nil
	}
	if err := SetParent(db, nodeID, *parentID); err != nil {
		return err
	}
	if position > 0 {
		db.Exec(`UPDATE nodes SET depth=$1 WHERE id=$2`, position, nodeID)
	}
	return nil
}

// ReorderLaneSlots updates depth for all slots in a lane based on
// the provided ordered list of node IDs.
func ReorderLaneSlots(db *sql.DB, laneID int64, orderedNodeIDs []int64) error {
	for i, nid := range orderedNodeIDs {
		depth := i + 1
		if _, err := db.Exec(`UPDATE nodes SET depth=$1 WHERE id=$2`, depth, nid); err != nil {
			return err
		}
	}
	return nil
}
