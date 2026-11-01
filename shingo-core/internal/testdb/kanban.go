package testdb

import (
	"testing"

	"shingocore/store"
	"shingocore/store/nodes"
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

// KanbanKeeper gives a kanban what makes it run a keeper: a station, and a supported
// process position of its own (<group>-SUPPORT, created here).
func KanbanKeeper(t testing.TB, db *store.DB, nodeID int64, station string) {
	t.Helper()
	g, err := db.GetNode(nodeID)
	if err != nil || g == nil {
		t.Fatalf("kanban keeper %d: %v", nodeID, err)
	}
	pos := &nodes.Node{Name: g.Name + "-SUPPORT", Enabled: true}
	if err := db.CreateNode(pos); err != nil {
		t.Fatalf("kanban keeper %s: supported position: %v", g.Name, err)
	}
	supports := []int64{pos.ID}
	saveKanban(t, db, store.KanbanChange{NodeID: nodeID, EnsureRow: true, Station: &station, Supports: &supports})
}

// KanbanCover seeds a kanban's running cover through the one write.
func KanbanCover(t testing.TB, db *store.DB, nodeID int64, cover int) {
	t.Helper()
	saveKanban(t, db, store.KanbanChange{NodeID: nodeID, EnsureRow: true, Cover: &cover})
}
