// Package kanban is the arithmetic of a kanban: a node group that declares a
// level per carrier type, which Core counts, fetches to, and caps stores at.
//
// It imports domain and protocol only: no SQL lives here. The store reads the
// rows (store.ListKanbanPicture) and the answers are composed from them here, so
// a number has one spelling whichever reader asks for it.
package kanban

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
