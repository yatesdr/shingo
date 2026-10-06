package www

import (
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"strings"

	"shingocore/domain"
	"shingocore/fleet"
	"shingocore/service"
)

// nodesPageDataAdapter composes NodeService + BinService so getNodesPageData
// can stay independent of *engine.Engine while we phase out the engine's
// store passthroughs (PR 3a.5.1). robots is the engine's in-memory fleet
// snapshot (GetAllCachedRobots): no query.
type nodesPageDataAdapter struct {
	ns     *service.NodeService
	bs     *service.BinService
	robots func() []fleet.RobotStatus
}

func (a *nodesPageDataAdapter) ListNodes() ([]*domain.Node, error) { return a.ns.ListNodes() }
func (a *nodesPageDataAdapter) CountBinsByAllNodes() (map[int64]int, error) {
	return a.bs.CountBinsByAllNodes()
}
func (a *nodesPageDataAdapter) NodeTileStates() (map[int64]domain.NodeTileState, error) {
	return a.ns.NodeTileStates()
}
func (a *nodesPageDataAdapter) ListScenePoints() ([]*domain.ScenePoint, error) {
	return a.ns.ListScenePoints()
}
func (a *nodesPageDataAdapter) ListBinTypes() ([]*domain.BinType, error)  { return a.bs.ListBinTypes() }
func (a *nodesPageDataAdapter) ListEdges() ([]domain.RegistryEdge, error) { return a.ns.ListEdges() }
func (a *nodesPageDataAdapter) CachedRobots() []fleet.RobotStatus {
	if a.robots == nil {
		return nil
	}
	return a.robots()
}

// nodesPageDataStore is the narrow read surface getNodesPageData needs.
// nodesPageDataAdapter satisfies this; *engine.Engine no longer does
// (PR 3a.5.1 absorbed the 5 underlying queries into NodeService /
// BinService and dropped the corresponding passthroughs).
type nodesPageDataStore interface {
	ListNodes() ([]*domain.Node, error)
	CountBinsByAllNodes() (map[int64]int, error)
	NodeTileStates() (map[int64]domain.NodeTileState, error)
	ListScenePoints() ([]*domain.ScenePoint, error)
	ListBinTypes() ([]*domain.BinType, error)
	ListEdges() ([]domain.RegistryEdge, error)
	CachedRobots() []fleet.RobotStatus
}

// nodeSceneInfo holds parsed scene data for a node location.
type nodeSceneInfo struct {
	PointName string
	Tasks     string
	BoundMap  string
}

// nodeRobots is what a node tile says about the robots standing on it.
type nodeRobots struct {
	// Label is the tile's marker: the robot, or the first one and a count.
	Label string
	// Title names every robot and whether its deck holds a bin.
	Title string
}

// nodesPageData aggregates all data needed to render the nodes page.
type nodesPageData struct {
	Nodes       []*domain.Node
	Counts      map[int64]int
	TileStates  map[int64]domain.NodeTileState
	Zones       []string
	NodeLabels  map[string]string
	NodeInfo    map[string]*nodeSceneInfo
	MapGroups   map[string][]*domain.ScenePoint
	BinTypes    []*domain.BinType
	Edges       []domain.RegistryEdge
	ChildCounts map[int64]int
	Depths      map[int64]int
	RobotsAt    map[int64]nodeRobots
}

// getNodesPageData assembles all data for the nodes page.
func getNodesPageData(db nodesPageDataStore) (*nodesPageData, error) {
	all, err := db.ListNodes()
	if err != nil {
		log.Printf("nodes page: list nodes: %v", err)
		return &nodesPageData{}, err
	}
	// _TRANSIT is where a bin is booked while it rides a robot: bookkeeping,
	// not a place on the floor, and nothing on this page can be done with it.
	// The Bins page shows what is in transit.
	nodes := make([]*domain.Node, 0, len(all))
	for _, n := range all {
		if n.Name != domain.TransitNodeName {
			nodes = append(nodes, n)
		}
	}

	counts, err := db.CountBinsByAllNodes()
	if err != nil {
		log.Printf("nodes page: count bins: %v", err)
	}
	if counts == nil {
		counts = make(map[int64]int, len(nodes))
	}
	tileStates, err := db.NodeTileStates()
	if err != nil {
		log.Printf("nodes page: tile states: %v", err)
	}
	if tileStates == nil {
		tileStates = make(map[int64]domain.NodeTileState, len(nodes))
	}
	zoneSet := map[string]bool{}
	for _, n := range nodes {
		if n.Zone != "" {
			zoneSet[n.Zone] = true
		}
		if _, ok := tileStates[n.ID]; !ok {
			tileStates[n.ID] = domain.NodeTileState{}
		}
	}
	zones := make([]string, 0, len(zoneSet))
	for z := range zoneSet {
		zones = append(zones, z)
	}

	scenePoints, err := db.ListScenePoints()
	if err != nil {
		log.Printf("nodes page: list scene points: %v", err)
	}
	nodeLabels := make(map[string]string)
	nodeInfo := make(map[string]*nodeSceneInfo)
	mapGroups := make(map[string][]*domain.ScenePoint)
	for _, sp := range scenePoints {
		if sp.ClassName == "GeneralLocation" {
			nodeLabels[sp.InstanceName] = sp.Label
			info := &nodeSceneInfo{PointName: sp.PointName}
			var props []sceneProperty
			if err := json.Unmarshal([]byte(sp.PropertiesJSON), &props); err == nil {
				if v, ok := findSceneProperty(props, "bindRobotMap"); ok {
					info.BoundMap = v
				}
				if v, ok := findSceneProperty(props, "binTask"); ok {
					info.Tasks = parseNodeTasks(v)
				}
			}
			nodeInfo[sp.InstanceName] = info
		} else {
			mapGroups[sp.ClassName] = append(mapGroups[sp.ClassName], sp)
		}
	}

	binTypes, err := db.ListBinTypes()
	if err != nil {
		log.Printf("nodes page: list bin types: %v", err)
	}
	edges, err := db.ListEdges()
	if err != nil {
		log.Printf("nodes page: list edges: %v", err)
	}

	// Depth comes off the node row ListNodes already read. It was one
	// GetSlotDepth query per child node — SELECT depth FROM nodes WHERE id=$1,
	// the same column, and most of the page's queries at a plant.
	// NULL reads as 0, as GetSlotDepth reported it.
	childCounts := make(map[int64]int)
	depths := make(map[int64]int)
	for _, n := range nodes {
		if n.ParentID != nil {
			childCounts[*n.ParentID]++
			d := 0
			if n.Depth != nil {
				d = *n.Depth
			}
			depths[n.ID] = d
		}
	}

	return &nodesPageData{
		Nodes:       nodes,
		Counts:      counts,
		TileStates:  tileStates,
		Zones:       zones,
		NodeLabels:  nodeLabels,
		NodeInfo:    nodeInfo,
		MapGroups:   mapGroups,
		BinTypes:    binTypes,
		Edges:       edges,
		ChildCounts: childCounts,
		Depths:      depths,
		RobotsAt:    robotsAtNodes(db.CachedRobots(), nodes, scenePoints),
	}, nil
}

// robotsAtNodes places each connected robot on the node its CurrentStation
// names, from data the page has already read: the node list and the scene
// points. No query.
//
// THE SAME ORDER AS service.resolvePoint, read from memory: the node's own
// name first (the simulator reports node names), then the scene alias — a
// GeneralLocation whose point_name is what the robot reports and whose
// instance_name is the node (a plant reports map furniture: AP102). An alias
// naming more than one node is skipped, as resolvePoint fails it closed.
// Synthetic nodes are skipped (a group or _TRANSIT is not where a robot
// stands). Unlike resolvePoint, a DISABLED node still shows its robot: this
// says where the robot is, not where a bin may be put.
//
// CurrentStation only: it is empty while a robot is between points, so a
// robot that has moved on is not left on the node it last passed
// (LastStation). A disconnected robot's position is stale and is not drawn.
func robotsAtNodes(robots []fleet.RobotStatus, nodes []*domain.Node, points []*domain.ScenePoint) map[int64]nodeRobots {
	out := map[int64]nodeRobots{}
	if len(robots) == 0 {
		return out
	}
	byName := make(map[string]*domain.Node, len(nodes))
	for _, n := range nodes {
		byName[n.Name] = n
	}
	alias := map[string][]string{}
	for _, sp := range points {
		if sp.ClassName != "GeneralLocation" || sp.PointName == "" || sp.InstanceName == "" {
			continue
		}
		dup := false
		for _, s := range alias[sp.PointName] {
			dup = dup || s == sp.InstanceName
		}
		if !dup {
			alias[sp.PointName] = append(alias[sp.PointName], sp.InstanceName)
		}
	}
	place := func(point string) *domain.Node {
		if n := byName[point]; n != nil && !n.IsSynthetic {
			return n
		}
		if s := alias[point]; len(s) == 1 {
			if n := byName[s[0]]; n != nil && !n.IsSynthetic {
				return n
			}
		}
		return nil
	}

	type here struct {
		id       string
		carrying bool
	}
	at := map[int64][]here{}
	for _, r := range robots {
		point := strings.TrimSpace(r.CurrentStation)
		if !r.Connected || point == "" {
			continue
		}
		n := place(point)
		if n == nil {
			continue
		}
		carrying, ok := service.RobotCarryingBin(r)
		at[n.ID] = append(at[n.ID], here{id: r.VehicleID, carrying: carrying && ok})
	}
	for id, hs := range at {
		sort.Slice(hs, func(i, j int) bool { return hs[i].id < hs[j].id })
		names := make([]string, len(hs))
		for i, h := range hs {
			names[i] = h.id
			if h.carrying {
				names[i] += " (carrying a bin)"
			}
		}
		label := hs[0].id
		if len(hs) > 1 {
			label += " +" + strconv.Itoa(len(hs)-1)
		}
		out[id] = nodeRobots{Label: label, Title: "At this node: " + strings.Join(names, ", ")}
	}
	return out
}

// sceneProperty is a minimal representation for parsing scene point properties.
type sceneProperty struct {
	Key         string `json:"key"`
	StringValue string `json:"stringValue,omitempty"`
}

func findSceneProperty(props []sceneProperty, key string) (string, bool) {
	for _, p := range props {
		if p.Key == key {
			return p.StringValue, true
		}
	}
	return "", false
}

// parseNodeTasks extracts task names from a binTask JSON property value.
// Input is like: [{"Load":{}},{"Unload":{}}]  →  "Load, Unload"
func parseNodeTasks(jsonStr string) string {
	var tasks []map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &tasks); err != nil {
		return ""
	}
	var names []string
	for _, t := range tasks {
		for k := range t {
			names = append(names, k)
		}
	}
	return strings.Join(names, ", ")
}
