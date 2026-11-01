// Package kanban is the arithmetic of a kanban: a node group that declares a
// level per carrier type, which Core counts, fetches to, and caps stores at.
//
// It imports domain and protocol only: no SQL lives here. The store reads the
// rows (store.ListKanbanPicture) and the answers are composed from them here, so
// a number has one spelling whichever reader asks for it.
package kanban

import "sort"

// Level is the three terms of one (kanban, carrier type)'s want.
type Level struct {
	// Base is the level declared for the type (kanban_levels.want).
	Base int `json:"base"`
	// Cover is the kanban's running cover: the extra it wants of a type per
	// supported process running that type (kanbans.running_cover).
	Cover int `json:"cover"`
	// Running is how many distinct supported processes run the type now, the
	// target style's types while a changeover is open (store.KanbanPictureSQL).
	Running int `json:"running"`
}

// Want is the effective want: Base + Cover*Running. No clamp: a want past the
// kanban's slots is shown as one that cannot be covered, never lowered here.
func (l Level) Want() int { return l.Base + l.Cover*l.Running }

// TypeLevel is one line of a kanban's declared level: how many unclaimed empty
// carriers of one type the group is to hold.
//
// A CAP, and this is where it differs from loaders.Quota, which is a PREFERENCE
// bounded by the never-2N window count, deciding only which type to fetch next.
// Want here is the number itself: the keeper tops up TO it and the resolver refuses
// a push that would exceed it (AtLevel). The two tables stay separate for exactly
// that reason; merging them would put cap semantics in a table documented as
// not-a-cap.
//
// want=0 is a legitimate declared value meaning "none of this type": a way to zero
// a line without forgetting it existed.
//
// Base is the declared number and Want the number a reader holds the group to.
// Read for configuration the two are the same and Want is what the modal edits;
// read for the plant (the store's level read) Want is the effective want,
// Level.Want(). A save only ever writes a declared number.
//
// OverflowNodeID and OverflowNode are this type's own overflow, nil and blank when
// the type takes the kanban's default: the settings modal's, and the doors' through
// the store's per-type overflow read and the door's one read.
type TypeLevel struct {
	GroupNodeID    int64  `json:"group_node_id"`
	BinTypeID      int64  `json:"bin_type_id"`
	BinTypeCode    string `json:"bin_type_code"`
	Want           int    `json:"want"`
	Base           int    `json:"-"`
	OverflowNodeID *int64 `json:"overflow_node_id,omitempty"`
	OverflowNode   string `json:"overflow_node,omitempty"`
}

// Have is a level type's three populations: what the keeper subtracts from the want.
//
// Kept distinct because they answer different questions and one of them is not the
// keeper's doing. Collapsing any two is how a level over- or under-fills with nothing
// reporting an error.
type Have struct {
	// Resident is the unclaimed empties of the type standing in the group, tops &
	// bottoms left out: it is no stock of a level.
	Resident int
	// Asked is the open episode's live root asks; 0 when no episode is open.
	Asked int
	// Coming is the empties of the type on their way in that the keeper did not ask for.
	Coming int
}

// Short is want less the three populations: the keeper's gap. At or below zero the
// group is not short of the type.
func (h Have) Short(want int) int { return want - h.Resident - h.Asked - h.Coming }

// Rank orders the groups a carrier could go to: the largest gap first, ties by name.
// It sorts in place, stably; the caller says what a group's name and gap are.
func Rank[G any](groups []G, name func(G) string, gap func(G) int) {
	sort.SliceStable(groups, func(i, j int) bool {
		if gi, gj := gap(groups[i]), gap(groups[j]); gi != gj {
			return gi > gj
		}
		return name(groups[i]) < name(groups[j])
	})
}
