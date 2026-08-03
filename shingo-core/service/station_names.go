package service

import (
	"maps"
	"sync"
)

// Station display names — the read side of the identity/label split v66 made.
//
// THE ROW STORES THE KEY; THE SCREEN RESOLVES THE LABEL. Every station-keyed
// row in this database holds `station_uid`, and none of them holds a name.
// Renaming a station is one UPDATE against one column of one row
// (registry.SetDisplayName), and every screen — including a view of an order
// that finished months ago — picks the new name up on its next render because
// nothing ever copied the old one anywhere.
//
// That property is the whole point, and it is easy to destroy by accident. The
// tempting shortcut is to denormalise display_name onto the data tables so a
// list query needs no second lookup. Before v66 the label and the identity WERE
// one column, and renaming a station rewrote the key under orders,
// mission_telemetry, outbox, node_stations, cell_targets and the Edge's backup
// manifest — "a plant stop caused by typing in a text box"
// (store/registry/registry.go:345-350). A denormalised copy is the same defect
// wearing a cache's clothes: the day it is stale is the day two screens disagree
// about which station an order belonged to.
//
// # WHY A CACHE AT ALL, AND WHY THIS SMALL ONE
//
// edge_registry holds ONE ROW PER PLANT — one at Springfield, one at
// Hopkinsville, measured. This is a dictionary with a single entry, not a join
// against a large table, so the cache exists to keep a per-row render from
// hitting the database once per line of a mission list, and for nothing else.
// It is a map behind an RWMutex, dropped whole on write. There is no TTL, no
// background refresh and no eviction policy, because a one-entry map does not
// need a framework around it and every one of those would be a mechanism nobody
// could later delete safely.
//
// Invalidation is by the two calls that can change the answer — RenameEdge and
// EnrollEdge — so a rename is visible on the next page load with no Core
// restart. A miss is not an error: an unknown station resolves to itself.
//
// # NOTHING HERE PARSES THE UID
//
// Resolution is a map lookup keyed on the whole string. It behaves identically
// for the legacy 'plant-a.line-1' and for a minted 'stn-a1b2c3d4e5f6a7b8', and
// it would behave identically for any other format, because no code path splits
// the value, matches a prefix, or infers anything from its shape. Whether the
// two live plants keep their backfilled uid or take fresh minted ones is an open
// decision elsewhere; this file is indifferent to the outcome by construction
// rather than by having been updated for it.
//
// # WHAT MUST NOT BE RESOLVED THROUGH HERE
//
// Not every column called "station" is an edge station. mission_events
// .robot_station holds SEER fleet-station names — 148 distinct values at
// Springfield — and Core's own synthetic order sources (core-operator, core-direct,
// core-test) and the '*' broadcast address have no registry row and never will.
// The fallback covers the second group correctly, by design: they render as
// themselves. The first group must not be passed to this resolver at all, since
// a robot station that happened to collide with a station uid would resolve to
// a plant's name.

// stationNameCache is the uid→display_name map, or nil when it needs loading.
//
// A nil map means "not loaded", which is distinct from an empty non-nil map
// meaning "loaded, no enrolled stations". Only the first triggers a read.
type stationNameCache struct {
	mu     sync.RWMutex
	byUID  map[string]string
	loaded bool
}

// invalidateStationNames drops the cache so the next resolve re-reads.
//
// Called from the writes that can change a name. It does not reload eagerly:
// the next render pays for it, which keeps a rename off the write path's
// latency and means a failed reload surfaces as raw ids on a screen rather than
// as a failed rename.
func (s *NodeService) invalidateStationNames() {
	s.names.mu.Lock()
	s.names.byUID = nil
	s.names.loaded = false
	s.names.mu.Unlock()
}

// stationNameMap returns the cached uid→label map, loading it if needed.
//
// A DATABASE ERROR IS NOT CACHED, and that asymmetry is deliberate. Caching a
// failed load would pin every screen to raw ids until the next rename or a Core
// restart — a transient database blip becoming permanent-looking cosmetic
// damage. Returning an empty map uncached means the failure degrades to today's
// behaviour and repairs itself on the next render.
func (s *NodeService) stationNameMap() map[string]string {
	s.names.mu.RLock()
	if s.names.loaded {
		m := s.names.byUID
		s.names.mu.RUnlock()
		return m
	}
	s.names.mu.RUnlock()

	edges, err := s.db.ListEdges()
	if err != nil {
		return nil
	}

	m := make(map[string]string, len(edges))
	for _, e := range edges {
		// An empty display_name is not a label. v66 backfills it from
		// station_id so this should not happen on a migrated database, but a
		// row inserted around the migration would otherwise resolve to "".
		if e.StationUID == "" || e.DisplayName == "" {
			continue
		}
		m[e.StationUID] = e.DisplayName
	}

	s.names.mu.Lock()
	s.names.byUID = m
	s.names.loaded = true
	s.names.mu.Unlock()
	return m
}

// StationName resolves one station identity to the operator's label.
//
// Falls back to the identity itself for anything with no enrolled row —
// core-operator, '*', an edge that has not been enrolled yet. Callers render the
// result directly; there is no "unknown station" sentinel to special-case,
// because degrading to the value the screen shows today is the correct
// behaviour and a sentinel would be a second thing to handle everywhere.
func (s *NodeService) StationName(station string) string {
	if station == "" {
		return ""
	}
	if name := s.stationNameMap()[station]; name != "" {
		return name
	}
	return station
}

// StationNames returns a copy of the uid→label map for callers that resolve
// many stations at once — a JSON list response whose rows each carry a station,
// where shipping the dictionary once beats resolving per row.
//
// A copy, because the cached map is shared across every in-flight request and a
// caller that ranged over it while a rename invalidated would be reading a map
// another goroutine may be replacing. Copying one entry is free; reasoning about
// a shared map is not.
func (s *NodeService) StationNames() map[string]string {
	src := s.stationNameMap()
	out := make(map[string]string, len(src))
	maps.Copy(out, src)
	return out
}
