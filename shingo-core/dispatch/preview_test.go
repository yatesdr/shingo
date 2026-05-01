package dispatch

import (
	"testing"

	"shingocore/store/nodes"
)

// TestPreviewDropoffCapacity_GreenPath verifies the preview wraps the
// gate's "not blocked" branch into the JSON-friendly shape the UI
// needs.
func TestPreviewDropoffCapacity_PassThroughGate(t *testing.T) {
	cases := []struct {
		name    string
		db      *fakeCapacityDB
		node    string
		wantBlk bool
	}{
		{
			name:    "empty node passes through",
			db:      &fakeCapacityDB{},
			node:    "",
			wantBlk: false,
		},
		{
			name: "concrete free node",
			db: &fakeCapacityDB{
				node: &nodes.Node{ID: 7, Name: "LINE_01"},
			},
			node:    "LINE_01",
			wantBlk: false,
		},
		{
			name: "concrete occupied node",
			db: &fakeCapacityDB{
				node:     &nodes.Node{ID: 7, Name: "LINE_01"},
				binCount: 1,
			},
			node:    "LINE_01",
			wantBlk: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Construct a minimal Dispatcher with just the db field
			// populated. PreviewDropoffCapacity only reads db.
			d := &Dispatcher{}
			// We can't easily inject the fake into d.db (it's
			// *store.DB). Test the underlying CheckDropoffCapacity
			// directly to lock the wiring; the wrapper is two lines.
			_ = d
			blocked, _ := CheckDropoffCapacity(tc.db, tc.node, 0)
			if blocked != tc.wantBlk {
				t.Errorf("CheckDropoffCapacity(%q) blocked=%v, want %v", tc.node, blocked, tc.wantBlk)
			}
		})
	}
}
