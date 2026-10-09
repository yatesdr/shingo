package domain

// DigFitFacts is what the plan-time dig verdict reads (plan.DigFits), in one
// statement (store.DB.DigFitFacts, which documents each field).
//
//   - GrantedSlot / GrantedTo: the dug lane's shallowest slot in front of the
//     deepest blocker, claimed by a live placer released into the lane (not
//     staged at a mark) that has not landed; "" and 0
//     when there is none.
//   - GroupName: the dug group's name, which the demand's drops name.
//   - DemandSlotsHeld: the slot rows the demand already holds in the group.
type DigFitFacts struct {
	GrantedSlot     string
	GrantedTo       int64
	GroupName       string
	DemandSlotsHeld int
}

// OwnDrop is the demand a dig is dug for, as the verdict counts it: its order
// and the groups its drops name. A drop into the dug group needs a slot there
// after the dig's shuffles, so the walk must find one beside the blockers'. The
// zero value is a dig with no such demand (a person's move, a retrieve whose
// delivery is a slot the parking already leaves out, or a line).
type OwnDrop struct {
	DemandID int64
	Groups   []string
}

// Owed is how many slots the demand's own drops still need in the group the
// facts describe: its drops naming that group, less the slots it already
// holds there (those are not in the parking the walk counts).
func (o OwnDrop) Owed(f DigFitFacts) int {
	n := 0
	for _, g := range o.Groups {
		if g != "" && g == f.GroupName {
			n++
		}
	}
	if n -= f.DemandSlotsHeld; n < 0 {
		return 0
	}
	return n
}
