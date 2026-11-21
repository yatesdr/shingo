package service

import "sync/atomic"

// FeedDoors is a service's two change doors into the Edges' copies of the
// node list, the scene and the catalog, over one hook:
//
//   - FeedsChangedPlantWide, after a write that may change what any station is
//     sent: a node, a group, a lane, a loader, a kanban level, a bin type or
//     its family, a payload, its carriers or manifest lines, an Edge's name,
//     the scene;
//   - FeedsChangedAt, after one that changes only the node lists of the
//     stations named: a node's stations.
//
// Each is called after the write commits, and only when it succeeded. The
// hook is the messaging layer's CoreDataService.FeedsChanged, wired once at
// the composition root (Engine.SetFeedsChangedFunc): it forgets the builds the
// write made stale at once and pushes each Edge its new copy a moment later.
// Unset (a test, a tool), nothing is called, and the heartbeat compare carries
// the change once the memo ages out. Pinned by
// TestFeedsChanged_PushesWhatTheWriteChanged.
//
// Atomic because the composition root sets it after the services are built,
// while HTTP handlers may already be writing.
type FeedDoors struct {
	changed atomic.Pointer[func(stations []string)]
}

// SetFeedsChangedFunc installs the hook both doors call: nil stations for the
// plant-wide door.
func (d *FeedDoors) SetFeedsChangedFunc(fn func(stations []string)) {
	d.changed.Store(&fn)
}

// FeedsChangedPlantWide is the door after a write that may change what any
// station is sent.
func (d *FeedDoors) FeedsChangedPlantWide() { d.call(nil) }

// FeedsChangedAt is the door after a write that changes only these stations'
// node lists. No stations calls nothing.
func (d *FeedDoors) FeedsChangedAt(stations ...string) {
	if len(stations) > 0 {
		d.call(stations)
	}
}

func (d *FeedDoors) call(stations []string) {
	if fn := d.changed.Load(); fn != nil && *fn != nil {
		(*fn)(stations)
	}
}
