// Package scenesync reconciles the fleet backend's authoritative scene
// layout with shingo's node and scene-point tables.
//
// The fleet owns the list of physical locations; shingo mirrors it.
// Sync pulls the current areas, persists each point (SyncScenePoints),
// then reconciles the node table against the new layout (SyncFleetNodes).
// UpdateNodeZones is the zone-reassignment pass — invoked by
// SyncFleetNodes at the end and directly from admin operations.
//
// Constructed as free functions over the narrow Store interface; the
// engine holds orchestration state (the sceneSyncing atomic) and wires
// emitters and logging via callbacks.
package scenesync

import (
	"fmt"
	"sync/atomic"
	"time"

	"shingocore/fleet"
	"shingocore/store/nodes"
	"shingocore/store/scene"
	"shingocore/store/sceneversion"
)

// Store is the narrow persistence surface scene sync requires.
// *store.DB satisfies it structurally.
type Store interface {
	DeleteScenePointsByArea(areaName string) error
	UpsertScenePoint(sp *scene.Point) error
	DeleteSceneEdgesByArea(areaName string) error
	// ListSceneEdges reads what is stored BEFORE the replace destroys it.
	// The lane diff is only computable while both states exist.
	ListSceneEdges() ([]*scene.Edge, error)
	// ApplyLaneVersions records the edit: one diff row, one version row per
	// lane that actually moved.
	ApplyLaneVersions(source, gateHash string, observedAt time.Time,
		previousSync *time.Time, areas []string, lanes []sceneversion.Lane) (sceneversion.DiffResult, error)
	UpsertSceneEdge(se *scene.Edge) error
	ListSceneAreas() ([]string, error)
	GetNodeTypeByCode(code string) (*nodes.NodeType, error)
	GetNodeByName(name string) (*nodes.Node, error)
	CreateNode(n *nodes.Node) error
	UpdateNode(n *nodes.Node) error
	ListNodes() ([]*nodes.Node, error)
	DeleteNode(id int64) error
}

// LogFn is the logging callback.
type LogFn func(format string, args ...any)

// NodeChangeFn is invoked when scene sync creates, updates, or deletes
// a node. The engine wires this to its event bus.
type NodeChangeFn func(nodeID int64, nodeName, action string)

// SyncScenePoints persists fleet scene areas to the database.
// Returns the total number of points synced and a map of bin
// location instanceName → areaName.
func SyncScenePoints(db Store, log LogFn, areas []fleet.SceneArea,
	gateHash string, previousSync *time.Time) (int, map[string]string) {
	// THE DIFF RUNS FIRST, AND THAT ORDERING IS THE POINT. Everything below
	// deletes an area's edges and re-inserts them, so once the loop starts
	// the previous geometry is gone and nothing can measure how far anything
	// moved. Recording the edit has to happen while both states exist.
	DiffLanesBeforeReplace(db, log, areas, gateHash, time.Now(), previousSync)

	locationSet := make(map[string]string)
	fetched := make(map[string]bool, len(areas))
	total := 0
	for _, area := range areas {
		fetched[area.Name] = true
		if err := db.DeleteScenePointsByArea(area.Name); err != nil {
			log("scenesync: delete points for area %s: %v", area.Name, err)
		}
		if err := db.DeleteSceneEdgesByArea(area.Name); err != nil {
			log("scenesync: delete edges for area %s: %v", area.Name, err)
		}
		for _, ap := range area.AdvancedPoints {
			sp := &scene.Point{
				AreaName:       area.Name,
				InstanceName:   ap.InstanceName,
				ClassName:      ap.ClassName,
				Label:          ap.Label,
				PosX:           ap.PosX,
				PosY:           ap.PosY,
				PosZ:           ap.PosZ,
				Dir:            ap.Dir,
				PropertiesJSON: ap.PropertiesJSON,
			}
			if err := db.UpsertScenePoint(sp); err != nil {
				log("scenesync: upsert point %s: %v", ap.InstanceName, err)
			}
			total++
		}
		for _, bin := range area.BinLocations {
			locationSet[bin.InstanceName] = area.Name
			sp := &scene.Point{
				AreaName:       area.Name,
				InstanceName:   bin.InstanceName,
				ClassName:      bin.ClassName,
				Label:          bin.Label,
				PointName:      bin.PointName,
				GroupName:      bin.GroupName,
				PosX:           bin.PosX,
				PosY:           bin.PosY,
				PosZ:           bin.PosZ,
				PropertiesJSON: bin.PropertiesJSON,
			}
			if err := db.UpsertScenePoint(sp); err != nil {
				log("scenesync: upsert point %s: %v", bin.InstanceName, err)
			}
			total++
		}
		// Drivable path segments (advanced curves) — the scene's real
		// connectivity, consumed by the robot-map travel network.
		for _, ed := range area.Edges {
			// An edge with no endpoint names is storable and useless: it has
			// no lane key, so it can carry no version row and every sample
			// landing on it is quarantined by the roll-up. Refusing it here
			// is the fix that makes the quarantine unnecessary; the
			// quarantine stays because the rows already in the database
			// cannot be un-written.
			if RejectUnnameableEdge(ed.FromName, ed.ToName) {
				log("scenesync: refusing edge %q in area %s — endpoint names %q/%q "+
					"give it no lane key, so nothing could aggregate or version it",
					ed.InstanceName, area.Name, ed.FromName, ed.ToName)
				continue
			}
			se := &scene.Edge{
				AreaName:     area.Name,
				InstanceName: ed.InstanceName,
				ClassName:    ed.ClassName,
				FromName:     ed.FromName,
				ToName:       ed.ToName,
				FromX:        ed.FromX,
				FromY:        ed.FromY,
				ToX:          ed.ToX,
				ToY:          ed.ToY,
			}
			// Both handles or neither. A half-written pair is three of the
			// four numbers a cubic needs, and the renderer would have to
			// invent the fourth.
			//
			// Copied out rather than pointed at: &ed.Ctrl1.X would alias the
			// caller's fleet payload into a row we are about to persist, so
			// anything that reused that slice would silently move an aisle.
			if ed.Ctrl1 != nil && ed.Ctrl2 != nil {
				c1x, c1y := ed.Ctrl1.X, ed.Ctrl1.Y
				c2x, c2y := ed.Ctrl2.X, ed.Ctrl2.Y
				se.Ctrl1X, se.Ctrl1Y = &c1x, &c1y
				se.Ctrl2X, se.Ctrl2Y = &c2x, &c2y
			}
			if err := db.UpsertSceneEdge(se); err != nil {
				log("scenesync: upsert edge %s: %v", ed.InstanceName, err)
			}
		}
	}

	// Full reconcile: sweep any stored area no longer in the fleet payload.
	// Per-area delete (above) only refreshes areas the fleet still reports, so
	// areas deleted from RDS (old commissioning/test areas) lingered forever as
	// ghost points/edges on the Robot Map. Skip when the fetch came back empty —
	// that's far likelier a transient fleet hiccup than a real "all areas
	// removed", and we don't want to wipe the whole scene on a blip.
	if len(areas) > 0 {
		stored, err := db.ListSceneAreas()
		if err != nil {
			log("scenesync: reconcile: list stored areas: %v", err)
		}
		for _, name := range stored {
			if fetched[name] {
				continue
			}
			if err := db.DeleteScenePointsByArea(name); err != nil {
				log("scenesync: reconcile: delete points for stale area %s: %v", name, err)
			}
			if err := db.DeleteSceneEdgesByArea(name); err != nil {
				log("scenesync: reconcile: delete edges for stale area %s: %v", name, err)
			}
			log("scenesync: reconcile: swept stale scene area %q (absent from fleet payload)", name)
		}
	}
	return total, locationSet
}

// SyncFleetNodes creates nodes for new scene locations and removes
// nodes no longer in the scene. Returns the counts of nodes created
// and deleted. Delegates to UpdateNodeZones at the end to reconcile
// zone assignments on surviving nodes.
func SyncFleetNodes(db Store, log LogFn, onChange NodeChangeFn, locationSet map[string]string) (created, deleted int) {
	// Look up default storage node type ID
	var storageTypeID *int64
	if nt, err := db.GetNodeTypeByCode("STAG"); err == nil {
		storageTypeID = &nt.ID
	}

	// Create nodes for locations not yet in DB (matched by name).
	for instanceName, areaName := range locationSet {
		if existing, err := db.GetNodeByName(instanceName); err == nil {
			// Node exists — update zone if needed
			if existing.Zone != areaName && areaName != "" {
				existing.Zone = areaName
				if err := db.UpdateNode(existing); err != nil {
					log("scenesync: update node %s zone: %v", instanceName, err)
				}
			}
			continue
		}
		node := &nodes.Node{
			Name:       instanceName,
			NodeTypeID: storageTypeID,
			Zone:       areaName,
			Enabled:    true,
		}
		if err := db.CreateNode(node); err != nil {
			log("scenesync: create node %q: %v", instanceName, err)
			continue
		}
		if onChange != nil {
			onChange(node.ID, node.Name, "created")
		}
		created++
	}

	// Delete physical nodes not present in current scene.
	// Skip synthetic nodes (node groups, lanes), nodes
	// without a name, and child nodes (part of a hierarchy)
	// — these are managed by shingo, not the fleet.
	nodes, err := db.ListNodes()
	if err != nil {
		log("scenesync: list nodes: %v", err)
	}
	for _, n := range nodes {
		if n.IsSynthetic || n.Name == "" || n.ParentID != nil {
			continue
		}
		if _, inScene := locationSet[n.Name]; !inScene {
			if err := db.DeleteNode(n.ID); err != nil {
				log("scenesync: delete node %s: %v", n.Name, err)
			}
			if onChange != nil {
				onChange(n.ID, n.Name, "deleted")
			}
			deleted++
		}
	}

	// Update zones on remaining nodes.
	UpdateNodeZones(db, log, onChange, locationSet, true)
	return
}

// UpdateNodeZones updates node zones from a location→area map.
// If overwrite is true, updates zone whenever it differs; if false,
// only fills empty zones.
func UpdateNodeZones(db Store, log LogFn, onChange NodeChangeFn, locationSet map[string]string, overwrite bool) {
	nodes, err := db.ListNodes()
	if err != nil {
		log("scenesync: update zones: list nodes: %v", err)
		return
	}
	for _, n := range nodes {
		if n.Name == "" {
			continue
		}
		zone, ok := locationSet[n.Name]
		if !ok {
			continue
		}
		if !overwrite && n.Zone != "" {
			continue
		}
		if n.Zone == zone {
			continue
		}
		n.Zone = zone
		if err := db.UpdateNode(n); err != nil {
			log("scenesync: update node %s zone: %v", n.Name, err)
		}
		if onChange != nil {
			onChange(n.ID, n.Name, "updated")
		}
	}
}

// Sync loads scene data from the fleet backend and reconciles shingo's
// node table. Guarded by the provided atomic bool to prevent concurrent
// runs. Returns (total points synced, nodes created, nodes deleted,
// error).
func Sync(db Store, log LogFn, onChange NodeChangeFn, syncer fleet.SceneSyncer,
	syncing *atomic.Bool, gateHash string, previousSync *time.Time) (int, int, int, error) {
	if !syncing.CompareAndSwap(false, true) {
		return 0, 0, 0, fmt.Errorf("scene sync already in progress")
	}
	defer syncing.Store(false)

	areas, err := syncer.GetSceneAreas()
	if err != nil {
		return 0, 0, 0, err
	}
	total, locSet := SyncScenePoints(db, log, areas, gateHash, previousSync)
	created, deleted := SyncFleetNodes(db, log, onChange, locSet)
	return total, created, deleted, nil
}
