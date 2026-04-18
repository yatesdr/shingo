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

	"shingocore/fleet"
	"shingocore/store"
)

// Store is the narrow persistence surface scene sync requires.
// *store.DB satisfies it structurally.
type Store interface {
	DeleteScenePointsByArea(areaName string) error
	UpsertScenePoint(sp *store.ScenePoint) error
	GetNodeTypeByCode(code string) (*store.NodeType, error)
	GetNodeByName(name string) (*store.Node, error)
	CreateNode(n *store.Node) error
	UpdateNode(n *store.Node) error
	ListNodes() ([]*store.Node, error)
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
func SyncScenePoints(db Store, log LogFn, areas []fleet.SceneArea) (int, map[string]string) {
	locationSet := make(map[string]string)
	total := 0
	for _, area := range areas {
		if err := db.DeleteScenePointsByArea(area.Name); err != nil {
			log("scenesync: delete points for area %s: %v", area.Name, err)
		}
		for _, ap := range area.AdvancedPoints {
			sp := &store.ScenePoint{
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
			sp := &store.ScenePoint{
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
		node := &store.Node{
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
func Sync(db Store, log LogFn, onChange NodeChangeFn, syncer fleet.SceneSyncer, syncing *atomic.Bool) (int, int, int, error) {
	if !syncing.CompareAndSwap(false, true) {
		return 0, 0, 0, fmt.Errorf("scene sync already in progress")
	}
	defer syncing.Store(false)

	areas, err := syncer.GetSceneAreas()
	if err != nil {
		return 0, 0, 0, err
	}
	total, locSet := SyncScenePoints(db, log, areas)
	created, deleted := SyncFleetNodes(db, log, onChange, locSet)
	return total, created, deleted, nil
}
