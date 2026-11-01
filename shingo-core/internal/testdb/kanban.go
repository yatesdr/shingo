package testdb

import (
	"testing"

	"shingocore/store"
)

// KanbanStation seeds a kanban's station through the one write, its row made if
// absent. Blank clears it.
func KanbanStation(t testing.TB, db *store.DB, nodeID int64, station string) {
	t.Helper()
	saveKanban(t, db, store.KanbanChange{NodeID: nodeID, EnsureRow: true, Station: &station})
}

// KanbanStrict seeds a kanban's strict switch through the one write.
func KanbanStrict(t testing.TB, db *store.DB, nodeID int64, strict bool) {
	t.Helper()
	saveKanban(t, db, store.KanbanChange{NodeID: nodeID, EnsureRow: true, Strict: &strict})
}

// KanbanOverflow seeds a kanban's default overflow by name through the one write.
// A blank name clears it; a name that does not resolve fails the test, since a row
// cannot hold one.
func KanbanOverflow(t testing.TB, db *store.DB, nodeID int64, overflow string) {
	t.Helper()
	id := int64(0)
	if overflow != "" {
		n, err := db.GetNodeByDotName(overflow)
		if err != nil || n == nil {
			t.Fatalf("kanban overflow %q: %v", overflow, err)
		}
		id = n.ID
	}
	saveKanban(t, db, store.KanbanChange{NodeID: nodeID, EnsureRow: true, Overflow: &id})
}

func saveKanban(t testing.TB, db *store.DB, c store.KanbanChange) {
	t.Helper()
	if err := db.SaveKanban(c, "test"); err != nil {
		t.Fatalf("save kanban %d: %v", c.NodeID, err)
	}
}
