package www

import (
	"encoding/json"
	"log"
	"strings"

	"shingocore/domain"
	"shingocore/service"
)

// nodesPageDataAdapter composes NodeService + BinService so getNodesPageData
// can stay independent of *engine.Engine while we phase out the engine's
// store passthroughs (PR 3a.5.1).
type nodesPageDataAdapter struct {
	ns *service.NodeService
	bs *service.BinService
}

func (a *nodesPageDataAdapter) ListNodes() ([]*domain.Node, error)                      { return a.ns.ListNodes() }
func (a *nodesPageDataAdapter) CountBinsByAllNodes() (map[int64]int, error)            { return a.bs.CountBinsByAllNodes() }
func (a *nodesPageDataAdapter) NodeTileStates() (map[int64]domain.NodeTileState, error) { return a.ns.NodeTileStates() }
func (a *nodesPageDataAdapter) ListScenePoints() ([]*domain.ScenePoint, error)          { return a.ns.ListScenePoints() }
func (a *nodesPageDataAdapter) ListBinTypes() ([]*domain.BinType, error)                { return a.bs.ListBinTypes() }
func (a *nodesPageDataAdapter) ListEdges() ([]domain.RegistryEdge, error)           { return a.ns.ListEdges() }
func (a *nodesPageDataAdapter) GetSlotDepth(nodeID int64) (int, error)                 { return a.ns.GetSlotDepth(nodeID) }

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
	GetSlotDepth(nodeID int64) (int, error)
}

// nodeSceneInfo holds parsed scene data for a node location.
type nodeSceneInfo struct {
	PointName string
	Tasks     string
	BoundMap  string
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
}

// getNodesPageData assembles all data for the nodes page.
func getNodesPageData(db nodesPageDataStore) (*nodesPageData, error) {
	nodes, err := db.ListNodes()
	if err != nil {
		log.Printf("nodes page: list nodes: %v", err)
		return &nodesPageData{}, err
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

	childCounts := make(map[int64]int)
	depths := make(map[int64]int)
	for _, n := range nodes {
		if n.ParentID != nil {
			childCounts[*n.ParentID]++
			if d, err := db.GetSlotDepth(n.ID); err == nil {
				depths[n.ID] = d
			}
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
	}, nil
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
