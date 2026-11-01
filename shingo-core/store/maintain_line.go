package store

import (
	"fmt"

	"shingocore/domain"
	"shingocore/kanban"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// maintain_line.go — what a maintained group is short of, one level type at a
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
//   - Coming: empties of the type on their way in that the keeper did not ask
//     for: ListTypedInboundToGroup's carriers (Inbound), counted by
//     orders.LevelComing (every one but a known tops & bottoms). The rows are
//     kept because an unloader's group line reads them too (the totes on each,
//     and which window carts are already on their way); the keeper only
//     counts them.
//
// ONE SPELLING. The keeper's tick (engine Maintainer) and an unloader's group line
// (engine UnloaderGroupLines) read every kanban's at once (KanbanCounts); the lift
// divert reads one through CountLevel and TypeLineOf. All three answer in this
// type over the same fragments, and an agreement test holds the batched reads to
// the single ones (TestKanbanCounts_TheBatchedReadsAgreeWithTheSingleReads), so
// they cannot disagree about how short a group is of a type.
//
// Level.Want is the effective want when the level came from DB.ListMaintainLevels
// or the picture, which is every reader's.
type LevelState struct {
	Level MaintainLevel
	kanban.Have
	Inbound []orders.InboundCarrier
}

// Short is the want less resident, asked and coming: the keeper's gap. At or
// below zero the group is not short of the type.
func (c LevelState) Short() int { return c.Have.Short(c.Level.Want) }

// CountLevel reads one level type's three populations for group g, against the
// type's open maintain episode (zero when none is open). An error names the
// count that failed and leaves the answer unusable.
func (db *DB) CountLevel(g *nodes.Node, lv MaintainLevel, episode DemandOrigin) (LevelState, error) {
	c := LevelState{Level: lv}
	var err error
	if c.Resident, err = db.CountEmptyBinsOfTypeInGroup(lv.BinTypeCode, g.ID, domain.Height{}); err != nil {
		return c, fmt.Errorf("count residents %s/%s: %w", g.Name, lv.BinTypeCode, err)
	}
	if episode.OriginID != "" {
		if c.Asked, err = db.CountLiveRootsByOrigin(episode.OriginID); err != nil {
			return c, fmt.Errorf("count asks %s/%s: %w", g.Name, lv.BinTypeCode, err)
		}
	}
	if c.Inbound, err = db.ListTypedInboundToGroup(g.ID, g.Name, lv.BinTypeCode); err != nil {
		return c, fmt.Errorf("count inbound %s/%s: %w", g.Name, lv.BinTypeCode, err)
	}
	c.Coming = orders.LevelComing(c.Inbound)
	return c, nil
}

// TypeLineOf reads one level type's line for group g, for the lift divert: the count
// (CountLevel, against the type's open maintain episode), then, for a type
// that states its totes per layer and has wanted heights among layerWants (the
// group's, ListMaintainLayerWants), each height's residents and comings: the
// carts whose totes_on_cart is exactly that many layers. The comings are the
// count's coming rows (LevelState.Inbound), so no second read spells
// "coming". The count comes back too, for the rows it read.
func (db *DB) TypeLineOf(g *nodes.Node, lv MaintainLevel, episode DemandOrigin,
	layerWants []MaintainLayerWant) (kanban.Line, LevelState, error) {
	c, err := db.CountLevel(g, lv, episode)
	if err != nil {
		return kanban.Line{}, c, err
	}
	tl := typeLineOf(c, layerWants, nil)
	if len(tl.Heights) == 0 {
		return tl, c, nil
	}
	resident, err := db.CountEmptyBinsOfTypeInGroupByTotes(tl.Code, g.ID)
	if err != nil {
		return kanban.Line{}, c, fmt.Errorf("group line: %w", err)
	}
	return typeLineOf(c, layerWants, resident), c, nil
}
