package domain

import "time"

// RecipeLastUsedMoves is the pick step's pre-fill for a recipe on a cell: the
// moves (recipe node → node) that put the recipe's spots where the LAST STYLE
// THAT USED THIS RECIPE ON THIS CELL has them. Nil when no style of the cell
// used it, or when the two cannot be laid onto each other cleanly; the step
// then suggests the recipe's own stored nodes, as it always did.
//
// WHY. A recipe stores concrete nodes, and a press that has since moved (SPR
// Press 6, 2026-10-21: PLN_006/007 to PLN_010/012) keeps being offered the old
// ones. The step is always shown and the operator always picks; this only
// changes what it suggests.
//
// "USED THIS RECIPE" IS PROVENANCE: a live claim whose source_preset_id is one
// of the recipe's versions (ids). "LAST" is the style whose newest such claim
// was written most recently (updated_at, the style id breaking a tie), and
// then that style's WHOLE claim set is read: a claim a hand edit rewrote
// without provenance is still where the recipe now runs. (A moved head may
// keep its provenance too; Press 6's PLN_010/012 did, 2026-10-21, so neither
// is assumed.)
//
// FROM THE CLAIMS ALREADY READ: claims is every live claim of the process by
// style, which the composer read has in hand. No statement.
//
// LAID ON BY SHAPE. Each recipe cell takes the unused claim of the same
// choreography (and role, when both say one) that shares the most nodes with
// it, at least one, the claims' order breaking a tie; its head, pairs and
// staging map onto that claim's. A move that would put two of the recipe's
// nodes on one node drops the whole pre-fill: a suggestion that collides is
// worse than the recipe's own. The composer then keeps only what the Spots
// step would accept (LegalSpotMoves) before the card ships it.
func RecipeLastUsedMoves(recipe []FlowCell, ids map[int64]bool, claims map[int64][]NodeClaim) map[string]string {
	style, ok := lastStyleUsing(ids, claims)
	if !ok {
		return nil
	}
	used := claims[style]
	matched := make([]bool, len(used))
	moves := map[string]string{}
	for _, cell := range recipe {
		// A claim must share a node with the cell to lay on it: one that
		// shares none is a different spot's, and laying on it would move
		// every node of the cell at once.
		best, bestScore := -1, 0
		for j, c := range used {
			if matched[j] || c.SwapMode != cell.SwapMode {
				continue
			}
			if cell.Role != "" && c.Role != "" && cell.Role != c.Role {
				continue
			}
			if s := sharedNodes(cellNodes(cell), claimNodes(c)); s > bestScore {
				best, bestScore = j, s
			}
		}
		if best < 0 {
			continue
		}
		matched[best] = true
		from, to := cellNodes(cell), claimNodes(used[best])
		for i := range from {
			if from[i] != "" && to[i] != "" && from[i] != to[i] {
				moves[from[i]] = to[i]
			}
		}
	}
	if len(moves) == 0 || Collides(recipe, moves) {
		return nil
	}
	return moves
}

// lastStyleUsing is the style whose newest claim from one of ids is newest.
func lastStyleUsing(ids map[int64]bool, claims map[int64][]NodeClaim) (int64, bool) {
	var style int64
	var at time.Time
	found := false
	for id, rows := range claims {
		for _, c := range rows {
			if c.SourcePresetID == nil || !ids[*c.SourcePresetID] {
				continue
			}
			var t time.Time
			if c.UpdatedAt != nil {
				t = *c.UpdatedAt
			}
			if !found || t.After(at) || (t.Equal(at) && id > style) {
				style, at, found = id, t, true
			}
		}
	}
	return style, found
}

func cellNodes(c FlowCell) [5]string {
	return [5]string{c.CoreNodeName, c.PairedCoreNode, c.SecondPairedCoreNode, c.InboundStaging, c.OutboundStaging}
}

func claimNodes(c NodeClaim) [5]string {
	return [5]string{c.CoreNodeName, c.PairedCoreNode, c.SecondPairedCoreNode, c.InboundStaging, c.OutboundStaging}
}

func sharedNodes(a, b [5]string) int {
	n := 0
	for _, x := range a {
		if x == "" {
			continue
		}
		for _, y := range b {
			if x == y {
				n++
				break
			}
		}
	}
	return n
}
