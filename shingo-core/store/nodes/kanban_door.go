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
	// none, which leaves it short of nothing.
	Level *KanbanLevel
	kanban.Have
	// TopsAndBottoms is the type's tops & bottoms line, a cart at 0's own: the row's
	// want (0 with no row), the carts at 0 standing and coming. The keeper never
	// fetches them, so nothing is asked.
	TopsWant, TopsResident, TopsComing int
	// FreeSlots is the group's slots the store resolver's flat arm would take this cart on, its
	// admits included: at 0 the door passes the group with no statement. A level is held in
	// direct positions, so a line asked for a fit is flat. Pin:
	// TestKanbanDoor_ItsFreeSlotsAreTheResolversSlots.
	FreeSlots int
}

// Short is how short the group is of a cart in this state: want less what it has
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

// KanbanDoor is one read of a push: the kanban named, and its sources in rank order.
//
// LOOP is the verdict, decided once, in the read. A cart of the kanban's loop lands whenever a
// slot admits its type: no door below the read asks the level of it. A read that finds the
// source is no part of the loop answers no door at all, and the cap stands.
type KanbanDoor struct {
	Loop bool
	// Family is the cart's carrier family, what a park says it waits for room for.
	Family  string
	Kanban  DoorLine
	Sources []DoorLine
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
