package store

import (
	"shingocore/kanban"
	"shingocore/store/orders"
)

// kanban_line.go — what a kanban is short of, one level type at a
// time: the arithmetic (LevelState) and the line an unloader's card reads
// (kanban.Line). Moved out of engine so a reader in dispatch, which engine
// imports, asks the same spelling.

// LevelState is one level type's four terms: what the group wants of the type
// and the three populations the keeper subtracts from it.
//
// ── THE THREE POPULATIONS ───────────────────────────────────────────────────
// Kept distinct because they answer different questions and one of them is not
// the keeper's doing. Collapsing any two is how a level over- or under-fills
// with nothing reporting an error.
//   - Resident: unclaimed empties of the type standing in the group
//     (CountEmptyBinsOfTypeInGroup: the finder's own WHERE and the height arm
//     it appends for no height said, so tops & bottoms, which is no stock of a
//     level, is not counted).
//   - Asked: the open episode's live root asks (CountLiveRootsByOrigin); 0,
//     and not read, when no episode is open.
//   - Coming: carts of the type a live order is carrying into the kanban or onto a supported
//     position that the keeper did not ask for. The rows into the kanban are kept because an
//     unloader's group line reads them too (the totes on each, and which window carts are
//     already on their way).
//   - Held: carts of the type standing on the supported positions with no order on them.
//
// ONE SPELLING. The keeper's tick and an unloader's group line read every kanban's at once
// (KanbanCounts), so they cannot disagree about how short a group is of a type. Pin:
// TestKanbanCounts_TheBatchedReadsAgreeWithTheSingleReads.
//
// Level.Want is the effective want when the level came from DB.ListKanbanLevels
// or the picture, which is every reader's.
type LevelState struct {
	Level KanbanLevel
	kanban.Have
	Inbound []orders.InboundCarrier
}

// Short is the want less resident, asked, coming and held. At or
// below zero the group is not short of the type.
func (c LevelState) Short() int { return c.Have.Short(c.Level.Want) }
