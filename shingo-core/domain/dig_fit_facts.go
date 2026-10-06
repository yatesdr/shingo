package domain

// DigFitFacts is what the plan-time dig verdict reads (plan.DigFits), in one
// statement (store.DB.DigFitFacts, which documents each field).
//
//   - Owed: the group's running digs' legs still owed a parking slot.
//   - GrantedSlot / GrantedTo: the dug lane's shallowest slot in front of the
//     deepest blocker, claimed by a live placer released into the lane (not
//     staged at a mark) that has not landed; "" and 0
//     when there is none.
type DigFitFacts struct {
	Owed        int
	GrantedSlot string
	GrantedTo   int64
}
