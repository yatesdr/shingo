package helpers

// DeclaresLevelSQL is true when the node at groupExpr declares a level: a
// maintained group, an empties bank its keeper tops up. One spelling for
// every reader that asks it, so the loader save's refusal of a maintained
// inbound source and the drain's re-check of the same source are one test
// (nodes.DeclaresLevel), and the drain's walk stops at the same groups inside
// the source (nodetree.UnmaintainedDescendantsOf). kanban_levels.bin_type_id is a
// foreign key, so every row is a row of the level list. Binds nothing.
func DeclaresLevelSQL(groupExpr string) string {
	return `EXISTS (SELECT 1 FROM kanban_levels ml WHERE ml.kanban_node_id = ` + groupExpr + `)`
}

// StrictFencesSQL is true when the kanban row aliased k fences the process node
// at processExpr: strict on, and the process absent from its supports. A kanban
// that supports nobody fences everybody, and a blank process is absent from every
// list. One spelling for the plant-wide scan (bins.FencedNodesCTE) and the need
// that names a group (nodes.GroupFencesAsker), which must not disagree about who
// is fenced. Pin: TestKanbanPin_AStrictGroupFencesABlankProcess. Binds nothing.
func StrictFencesSQL(processExpr string) string {
	return `k.strict AND NOT EXISTS (
			SELECT 1 FROM kanban_supports s
			JOIN nodes pn ON pn.id = s.process_node_id
			WHERE s.kanban_node_id = k.node_id AND pn.name = ` + processExpr + `
		)`
}
