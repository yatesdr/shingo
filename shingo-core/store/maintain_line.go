package store

import (
	"fmt"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// maintain_line.go — what a maintained group is short of, one level type at a
// time: the keeper's arithmetic (CountLevel) and the line an unloader's card
// reads (TypeLine). Moved out of engine so a reader in dispatch, which engine
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
// ONE SPELLING. The keeper's tick (engine tickOne) and an unloader's group line
// (engine UnloaderGroupLines, through TypeLineOf) both count through CountLevel,
// so the two cannot disagree about how short a group is of a type.
type LevelState struct {
	Level    MaintainLevel
	Resident int
	Asked    int
	Coming   int
	Inbound  []orders.InboundCarrier
}

// Short is the want less resident, asked and coming: the keeper's gap. At or
// below zero the group is not short of the type.
func (c LevelState) Short() int {
	return c.Level.Want - c.Resident - c.Asked - c.Coming
}

// CountLevel reads one level type's three populations for group g, against the
// type's open maintain episode (zero when none is open). An error names the
// count that failed and leaves the answer unusable.
func (db *DB) CountLevel(g *nodes.Node, lv MaintainLevel, episode DemandOrigin) (LevelState, error) {
	c := LevelState{Level: lv}
	var err error
	if c.Resident, err = db.CountEmptyBinsOfTypeInGroup(lv.BinTypeCode, g.ID, bins.Height{}); err != nil {
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

// TypeLine is one level type of a group as its line reads it: what the group
// wants of the type, what it has (resident, asked and coming: want less the
// keeper's Short), and its wanted heights when the type states its totes per
// layer. Its readers are an unloader's group line (engine UnloaderGroupLines
// and its walk) and the drop choice.
type TypeLine struct {
	Code          string
	Want, Have    int
	TotesPerLayer int // 0: the type states none, and has no heights
	Heights       []HeightLine
}

// HeightLine is one wanted height of a type: how many carts standing layers
// high the group wants, and how many it has (resident and coming at exactly
// that many totes).
type HeightLine struct {
	Layers     int
	Want, Have int
}

// Short is how many more of the type the group wants: want less have. At or
// below zero the group is not short of it.
func (tl TypeLine) Short() int { return tl.Want - tl.Have }

// HeightShort is how many more carts standing layers high the group wants: the
// wanted height's want less have, held under the type's Short (a cart at a
// height is a cart of the type), but for the tops & bottoms row (0 layers),
// which is a line of its own. 0 for a height the group does not want. Its
// reader is the by-contents choice (dispatch dropByContents), which takes a
// tote cart into a group only as its own height.
func (tl TypeLine) HeightShort(layers int) int {
	for _, h := range tl.Heights {
		if h.Layers != layers {
			continue
		}
		if layers == 0 {
			return h.Want - h.Have
		}
		return min(h.Want-h.Have, tl.Short())
	}
	return 0
}

// TypeLineOf reads one level type's line for group g: the keeper's count
// (CountLevel, against the type's open maintain episode), then, for a type
// that states its totes per layer and has wanted heights among layerWants (the
// group's, ListMaintainLayerWants), each height's residents and comings: the
// carts whose totes_on_cart is exactly that many layers. The comings are the
// count's coming rows (LevelState.Inbound), so no second read spells
// "coming". The count comes back too, for the rows it read.
func (db *DB) TypeLineOf(g *nodes.Node, lv MaintainLevel, episode DemandOrigin,
	layerWants []MaintainLayerWant) (TypeLine, LevelState, error) {
	c, err := db.CountLevel(g, lv, episode)
	if err != nil {
		return TypeLine{}, c, err
	}
	tl := TypeLine{Code: lv.BinTypeCode, Want: lv.Want, Have: lv.Want - c.Short()}
	for _, w := range layerWants {
		if w.BinTypeID != lv.BinTypeID || !protocol.IsToteType(w.TotesPerLayer) {
			continue
		}
		tl.TotesPerLayer = *w.TotesPerLayer
		tl.Heights = append(tl.Heights, HeightLine{Layers: w.Layers, Want: w.Want})
	}
	if len(tl.Heights) == 0 {
		return tl, c, nil
	}
	resident, err := db.CountEmptyBinsOfTypeInGroupByTotes(tl.Code, g.ID)
	if err != nil {
		return TypeLine{}, c, fmt.Errorf("group line: %w", err)
	}
	coming := map[int]int{}
	for _, ib := range c.Inbound {
		if ib.TotesOnCart != nil {
			coming[*ib.TotesOnCart]++
		}
	}
	for i := range tl.Heights {
		totes := tl.Heights[i].Layers * tl.TotesPerLayer
		tl.Heights[i].Have = resident[totes] + coming[totes]
	}
	return tl, c, nil
}
