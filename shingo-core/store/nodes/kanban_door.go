package nodes

import (
	"database/sql"
	"strings"

	"shingo/protocol"
	"shingocore/kanban"
)

// kanban_door.go — what one read tells the door about a window's push into a
// kanban (store.DB.ReadKanbanDoor).

// DoorLine is one group at a push's door, for the one carrier type asked: the
// group's row, and its level of the type when it declares one.
type DoorLine struct {
	Group *Node
	// Level is the group's row for the type, Want effective; nil when it declares
	// none, which leaves it short of nothing: it can be reached only as an overflow.
	Level *MaintainLevel
	kanban.Have
	// TopsAndBottoms is the type's tops & bottoms line, a cart at 0's own: the row's
	// want (0 with no row), the carts at 0 standing and coming. The keeper never
	// fetches them, so nothing is asked.
	TopsWant, TopsResident, TopsComing int
}

// Short is how short the group is of a cart in this state: the keeper's gap
// (store.LevelState.Short) on the stack line, or the tops & bottoms line's for a
// cart at 0. Never positive with no level.
func (l DoorLine) Short(state protocol.CarrierState) int {
	if l.Level == nil {
		return 0
	}
	if state == protocol.CarrierTopsAndBottoms {
		return kanban.Have{Resident: l.TopsResident, Coming: l.TopsComing}.Short(l.TopsWant)
	}
	return l.Have.Short(l.Level.Want)
}

// KanbanDoor is one read of a window's push: the kanban the unloader's outbound
// names, its sources in rank order, and the type's overflow (nil with none).
type KanbanDoor struct {
	Kanban   DoorLine
	Sources  []DoorLine
	Overflow *DoorLine
}

// WindowNameOf is the node name a door matches a window on: a name is split at its
// first dot as GetByDotName splits it, and node names are unique, so the child's
// name alone says which node a dot name means.
func WindowNameOf(dotName string) string {
	if parts := strings.SplitN(dotName, ".", 2); len(parts) == 2 {
		return parts[1]
	}
	return dotName
}

// ScanDoorLine reads one line of the door's statement: the node's columns, then
// the extra columns the caller names.
func ScanDoorLine(rows *sql.Rows, extra ...any) (*Node, error) {
	return ScanNode(withTrailingCols{rows, extra})
}

// PerTypeOverflowTargetOf reads the overflow a store of one carrier type takes
// from a group: for a kanban the type's own row, then the kanban's default; for a
// plain group its property. One statement. Pin:
// TestOverflow_ATypesOwnOverflowOutranksTheKanbansDefault.
func PerTypeOverflowTargetOf(db *sql.DB, groupNodeID, binTypeID int64) (OverflowTarget, error) {
	return scanOverflowTarget(db.QueryRow(perTypeOverflowSQL+` WHERE g.id=$1`, groupNodeID, binTypeID), groupNodeID)
}

// PerTypeOverflowTargetByName is PerTypeOverflowTargetOf for a caller holding the
// group's name. A binTypeID of 0 names no level row, and reads the group's own.
func PerTypeOverflowTargetByName(db *sql.DB, groupName string, binTypeID int64) (OverflowTarget, error) {
	return scanOverflowTarget(db.QueryRow(perTypeOverflowSQL+` WHERE g.name=$1`, groupName, binTypeID), groupName)
}

const perTypeOverflowSQL = `SELECT COALESCE(l.overflow_node_id, k.overflow_node_id, 0),
		CASE WHEN k.node_id IS NOT NULL THEN COALESCE(lo.name, o.name, '') ELSE COALESCE(p.value, '') END
	  FROM nodes g
	  LEFT JOIN kanbans k ON k.node_id = g.id
	  LEFT JOIN nodes o ON o.id = k.overflow_node_id
	  LEFT JOIN kanban_levels l ON l.kanban_node_id = g.id AND l.bin_type_id = $2
	  LEFT JOIN nodes lo ON lo.id = l.overflow_node_id
	  LEFT JOIN node_properties p ON p.node_id = g.id AND p.key = '` + PropOverflowDestination + `'`
