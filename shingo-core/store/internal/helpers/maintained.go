package helpers

// DeclaresLevelSQL is true when the node at groupExpr declares a level: a
// maintained group, an empties bank its keeper tops up. One spelling for
// every reader that asks it, so the loader save's refusal of a maintained
// inbound source and the drain's re-check of the same source are one test
// (nodes.DeclaresLevel), and the drain's walk stops at the same groups inside
// the source (nodetree.UnmaintainedDescendantsOf). The join is
// nodes.ListMaintainLevels' own, so none of them can disagree with the level
// list about a row. Binds nothing.
func DeclaresLevelSQL(groupExpr string) string {
	return `EXISTS (SELECT 1 FROM node_maintain_levels ml
		JOIN bin_types mlt ON mlt.id = ml.bin_type_id
		WHERE ml.group_node_id = ` + groupExpr + `)`
}
