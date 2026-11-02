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

// KanbanSources sets a kanban's sources, in rank order, its row made if absent; none
// clears them.
func KanbanSources(t testing.TB, db *store.DB, nodeID int64, sourceIDs ...int64) {
	t.Helper()
	if sourceIDs == nil {
		sourceIDs = []int64{}
	}
	saveKanban(t, db, store.KanbanChange{NodeID: nodeID, EnsureRow: true, Sources: &sourceIDs})
}
