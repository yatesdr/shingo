package bins_test

import (
	"testing"

	"shingocore/store/bins"
)

// TestAccessibleEmptyOrder_TextUnchanged pins the ORDER BY every empty finder
// appends, byte for byte, as rendered at init (ReachableSQL composed in).
//
// A preference added beside it must leave this text standing when it has
// nothing to prefer by: a finder asked with no want runs exactly today's
// statement, with no added argument. The mouth-first ranking this text
// produces is pinned on every finder by TestPin_EveryEmptyFinderPrefersTheMouth.
func TestAccessibleEmptyOrder_TextUnchanged(t *testing.T) {
	t.Parallel()
	const want = "\n\tORDER BY (n.parent_id IS NULL OR n.depth IS NULL OR NOT EXISTS (\n" +
		"\t\t\tSELECT 1 FROM nodes lane_blocker\n" +
		"\t\t\tWHERE EXISTS (SELECT 1 FROM bins lane_blocker_bin WHERE lane_blocker_bin.node_id = lane_blocker.id)\n" +
		"\t\t\t  AND lane_blocker.parent_id = n.parent_id\n" +
		"\t\t\t  AND lane_blocker.id != n.id\n" +
		"\t\t\t  AND lane_blocker.depth IS NOT NULL\n" +
		"\t\t\t  AND n.depth IS NOT NULL\n" +
		"\t\t\t  AND lane_blocker.depth < n.depth\n" +
		"\t\t  )) DESC,\n" +
		"\t         COALESCE(n.depth, 0) ASC,\n" +
		"\t         b.id ASC\n" +
		"\tLIMIT 1"
	if bins.AccessibleEmptyOrder != want {
		t.Errorf("AccessibleEmptyOrder moved:\n%q\nwant\n%q", bins.AccessibleEmptyOrder, want)
	}
}
