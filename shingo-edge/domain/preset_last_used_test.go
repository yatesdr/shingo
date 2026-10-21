package domain

import (
	"reflect"
	"testing"
	"time"

	"shingo/protocol"
)

// The two-spot press-index recipe as Springfield Press 6 stored it, and the
// cell's styles: style 1 built from it on PLN_006/007, style 2 built from it
// and then moved to PLN_010/012 (its heads' claims rewritten, so they carry no
// provenance), style 3 not from it.
func pressRecipe() []FlowCell {
	cell := func(head, paired string) FlowCell {
		return FlowCell{CoreNodeName: head, Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobotPressIndex,
			PairedCoreNode: paired}
	}
	return []FlowCell{cell("PLN_006", "PLN_005"), cell("PLN_007", "PLN_008")}
}

func pressClaim(style int64, head, paired string, preset *int64, at time.Time) NodeClaim {
	return NodeClaim{StyleID: style, CoreNodeName: head, Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeTwoRobotPressIndex, PairedCoreNode: paired, SourcePresetID: preset, UpdatedAt: &at}
}

func TestRecipeLastUsedMoves(t *testing.T) {
	t.Parallel()
	recipeV1, recipeV2, other := int64(7), int64(9), int64(4)
	ids := map[int64]bool{recipeV1: true, recipeV2: true}
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)

	t.Run("the last style's spots, moved positions included", func(t *testing.T) {
		claims := map[int64][]NodeClaim{
			1: {pressClaim(1, "PLN_006", "PLN_005", &recipeV1, day), pressClaim(1, "PLN_007", "PLN_008", &recipeV1, day)},
			// One claim still carries the recipe; the moved one does not.
			2: {pressClaim(2, "PLN_010", "PLN_005", &recipeV2, day.Add(time.Hour)), pressClaim(2, "PLN_012", "PLN_008", nil, day.Add(2*time.Hour))},
			3: {pressClaim(3, "PLN_014", "PLN_013", &other, day.Add(3*time.Hour))},
		}
		got := RecipeLastUsedMoves(pressRecipe(), ids, claims)
		want := map[string]string{"PLN_006": "PLN_010", "PLN_007": "PLN_012"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("moves = %v, want %v", got, want)
		}
	})
	t.Run("no style used it: no pre-fill, the recipe's own nodes", func(t *testing.T) {
		claims := map[int64][]NodeClaim{3: {pressClaim(3, "PLN_014", "PLN_013", &other, day)}}
		if got := RecipeLastUsedMoves(pressRecipe(), ids, claims); got != nil {
			t.Errorf("moves = %v, want none", got)
		}
	})
	t.Run("the last style sits where the recipe does: no moves", func(t *testing.T) {
		claims := map[int64][]NodeClaim{1: {pressClaim(1, "PLN_006", "PLN_005", &recipeV1, day), pressClaim(1, "PLN_007", "PLN_008", &recipeV1, day)}}
		if got := RecipeLastUsedMoves(pressRecipe(), ids, claims); got != nil {
			t.Errorf("moves = %v, want none", got)
		}
	})
	t.Run("a pre-fill that would put two spots on one node is dropped", func(t *testing.T) {
		claims := map[int64][]NodeClaim{
			// PLN_008 is the last style's head and the recipe's other pair.
			2: {pressClaim(2, "PLN_008", "PLN_005", &recipeV1, day)},
		}
		if got := RecipeLastUsedMoves(pressRecipe(), ids, claims); got != nil {
			t.Errorf("moves = %v, want none: PLN_006 → PLN_008 lands on the recipe's PLN_008", got)
		}
	})
}
