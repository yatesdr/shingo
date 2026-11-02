// Package kanban is a kanban's arithmetic and its unloader's walk. A kanban is a node
// group that declares a level per bin type and a pool per carrier family: Core counts its
// loop, admits every cart of the loop, fetches only what the loop has lost, and caps stores
// from outside the loop at the level.
//
// TWO HALVES. kanban.go and at_level.go are the concept: any kanban has a want, a have and a
// family. assign.go and line.go are the instance: an unloader's windows and a tote cart's
// heights, which a kanban with no unloader never reads. They live here because nothing else
// imports them yet.
//
// It imports domain and protocol only: no SQL lives here. The store reads the
// rows (store.ListKanbanPicture) and the answers are composed from them here, so
// a number has one spelling whichever reader asks for it.
package kanban

// Level is the two terms of one (kanban, bin type)'s want.
type Level struct {
	// Base is the ready mix declared for the type (kanban_levels.want).
	Base int `json:"base"`
	// Cover is the positions the effective style's claims put on the line in this type,
	// counted from the claims and never typed (store.KanbanPictureSQL).
	Cover int `json:"cover"`
}

// Want is the effective want: Base + Cover. No clamp: a want past the kanban's slots is
// shown as one that cannot be covered, never lowered here.
func (l Level) Want() int { return l.Base + l.Cover }

// SameFamily says a cart of one bin type may be set as another: a family is one physical
// cart, and every bin type is one setting of exactly one. The only spelling of the
// question. Its truth table is testdata/same_family.json, which the walk, the arm, the save,
// the CLEAR door, simcalc and the Edge's panels all read.
func SameFamily(cart, asked int64) bool { return cart != 0 && cart == asked }

// TypeLevel is one line of a kanban's declared level: how many unclaimed empty carriers of
// one type the group holds ready.
//
// FOR A CART FROM OUTSIDE THE LOOP IT IS A CAP, and this is where it differs from
// loaders.Quota, which is a preference bounded by the never-2N window count. A cart of the
// kanban's own loop never meets it: its door has already said so (nodes.KanbanDoor.Loop).
//
// want=0 is a legitimate declared value meaning "none of this type ready": a way to zero a
// line without forgetting it existed.
//
// Base is the declared number and Want the number a reader holds the group to.
// Read for configuration the two are the same and Want is what the modal edits;
// read for the plant (the store's level read) Want is the effective want,
// Level.Want(). A save only ever writes a declared number.
type TypeLevel struct {
	GroupNodeID int64  `json:"group_node_id"`
	BinTypeID   int64  `json:"bin_type_id"`
	BinTypeCode string `json:"bin_type_code"`
	Want        int    `json:"want"`
	Base        int    `json:"-"`
}

// Have is a level type's four populations: what the keeper subtracts from the want.
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
	// Coming is the carts of the type a live order is carrying into the kanban or onto a
	// supported position, that the keeper did not ask for.
	Coming int
	// Held is the carts of the type standing on the supported positions with no order
	// carrying them: the cover the line already holds. A kanban's windows are not in it, a
	// cart there counts as what the walk lights it (store.KanbanCounts.Lit).
	Held int
}

// Short is want less the four populations. At or below zero the group is not short of the
// type.
func (h Have) Short(want int) int { return want - h.Resident - h.Asked - h.Coming - h.Held }
