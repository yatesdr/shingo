package www

import (
	"encoding/json"
	"log"
	"strings"

	"shingocore/store"
)

// nodeSceneInfo holds parsed scene data for a node location.
type nodeSceneInfo struct {
	PointName string
	Tasks     string
	BoundMap  string
}

// nodesPageData aggregates all data needed to render the nodes page.
type nodesPageData struct {
	Nodes       []*store.Node
	Counts      map[int64]int
	TileStates  map[int64]store.NodeTileState
	Zones       []string
	NodeLabels  map[string]string
	NodeInfo    map[string]*nodeSceneInfo
	MapGroups   map[string][]*store.ScenePoint
	BinTypes    []*store.BinType
	Edges       []store.EdgeRegistration
	ChildCounts map[int64]int
	Depths      map[int64]int
}

// getNodesPageData assembles all data for the nodes page.
func getNodesPageData(db *store.DB) (*nodesPageData, error) {
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
		tileStates = make(map[int64]store.NodeTileState, len(nodes))
	}
	zoneSet := map[string]bool{}
	for _, n := range nodes {
		if n.Zone != "" {
			zoneSet[n.Zone] = true
		}
		if _, ok := tileStates[n.ID]; !ok {
			tileStates[n.ID] = store.NodeTileState{}
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
	mapGroups := make(map[string][]*store.ScenePoint)
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
