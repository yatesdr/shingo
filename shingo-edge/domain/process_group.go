package domain

import "time"

// ProcessGroup is an organizational grouping for the Processes admin page.
// A process belongs to at most one group (or none — "Ungrouped"). It is
// pure UI taxonomy: nothing in the runtime or order/claim engine reads
// group_id. Deleting a group reverts its members to Ungrouped via an
// explicit transactional UPDATE (foreign_keys is OFF, so the ON DELETE
// SET NULL foreign key never fires).
type ProcessGroup struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	SortOrder   int       `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
}
