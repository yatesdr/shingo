package plan

import "shingocore/dispatch/binresolver"

// Reader is the planner's only store type: the reads its parts make, and no
// write. *store.DB satisfies it (dispatch asserts so), and nothing in this
// package can reach a writer through it; the planner-purity depguard rule
// (.golangci.yml) stops the package importing one by another road.
//
// It is the union of the parts' own read surfaces, each declared where it is
// used: the picker's (FinderDB), the group resolver's (binresolver.Store) and
// the slot book's (SlotBookDB).
type Reader interface {
	FinderDB
	binresolver.Store
	SlotBookDB
}
