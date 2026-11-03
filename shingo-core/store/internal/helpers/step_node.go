package helpers

import "fmt"

// StepNodeSQL is where plan step i of order o happens, in SQL: the step's node,
// else its group, from o.steps_json; for an order with no stored steps the plan
// its endpoints imply, step 0 at source_node and step 1 at delivery_node; the
// empty string for anything else (an index outside the plan, a step naming
// nothing).
//
// o is the orders alias; i is an SQL expression for the step (a row's
// pick_step or drop_step). It is the SQL spelling of domain.StepNode, the one
// spelling of "where" for an order's carts, and the two are held together by
// TestStepNodeSQL_AgreesWithStepNode (store), the way
// TestOrderRouteRuleMatchesThePopup holds the route.
//
// COST: the plan is parsed per evaluation, so a statement reads it once per
// row it asks about — bounded by the order's rows.
//
// The text holds no '%', so a caller may splice it into a fmt format string.
func StepNodeSQL(o, i string) string {
	return fmt.Sprintf(`(CASE
		WHEN (%[2]s) IS NULL OR (%[2]s) < 0 THEN ''
		WHEN left(COALESCE(%[1]s.steps_json, ''), 1) = '[' AND jsonb_array_length(%[1]s.steps_json::jsonb) > 0
		THEN COALESCE(NULLIF(%[1]s.steps_json::jsonb->(%[2]s)->>'node', ''), %[1]s.steps_json::jsonb->(%[2]s)->>'group', '')
		WHEN (%[2]s) = 0 THEN COALESCE(%[1]s.source_node, '')
		WHEN (%[2]s) = 1 THEN COALESCE(%[1]s.delivery_node, '')
		ELSE '' END)`, o, i)
}
