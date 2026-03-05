package engine

import (
	"encoding/json"
	"strings"

	"shingocore/fleet"
	"shingocore/store"
)

// OccupancyEntry represents a single node's fleet vs ShinGo occupancy comparison.
type OccupancyEntry struct {
	LocationID    string `json:"location_id"`
	NodeName      string `json:"node_name"`
	FleetOccupied *bool  `json:"fleet_occupied"`
	InShinGo      bool   `json:"in_shingo"`
	Discrepancy   string `json:"discrepancy"`
}

// GetNodeOccupancy compares fleet bin occupancy against ShinGo node records.
func (e *Engine) GetNodeOccupancy() ([]OccupancyEntry, error) {
	np, ok := e.fleet.(fleet.NodeOccupancyProvider)
	if !ok {
		return nil, errFleetUnsupported("occupancy status")
	}
	locations, err := np.GetNodeOccupancy()
	if err != nil {
		return nil, err
	}

	nodes, _ := e.db.ListNodes()

	locMap := make(map[string]bool, len(locations))
	for _, loc := range locations {
		locMap[loc.ID] = loc.Occupied
	}

	nodeVendor := make(map[string]string, len(nodes))
	for _, n := range nodes {
		if n.VendorLocation != "" {
			nodeVendor[n.VendorLocation] = n.Name
		}
	}

	var results []OccupancyEntry

	for _, loc := range locations {
		e := OccupancyEntry{
			LocationID:    loc.ID,
			FleetOccupied: &loc.Occupied,
			InShinGo:      nodeVendor[loc.ID] != "",
			NodeName:      nodeVendor[loc.ID],
		}
		if !e.InShinGo {
			e.Discrepancy = "fleet_only"
		}
		results = append(results, e)
	}

	for _, n := range nodes {
		if n.VendorLocation == "" {
			continue
		}
		if _, ok := locMap[n.VendorLocation]; !ok {
			results = append(results, OccupancyEntry{
				LocationID:  n.VendorLocation,
				NodeName:    n.Name,
				InShinGo:    true,
				Discrepancy: "shingo_only",
			})
		}
	}

	return results, nil
}

// NodeSceneInfo holds parsed scene data for a node location.
type NodeSceneInfo struct {
	PointName string
	Tasks     string
	BoundMap  string
}

// NodesPageData aggregates all data needed to render the nodes page.
type NodesPageData struct {
	Nodes          []*store.Node
	Counts         map[int64]int
	Zones          []string
	NodeLabels     map[string]string
	NodeInfo       map[string]*NodeSceneInfo
	MapGroups      map[string][]*store.ScenePoint
	NodeTypes      []*store.NodeType
	SyntheticNodes []*store.Node
	PayloadStyles  []*store.PayloadStyle
	Edges          []store.EdgeRegistration
	ChildCounts    map[int64]int
}

// GetNodesPageData assembles all data for the nodes page.
func (e *Engine) GetNodesPageData() (*NodesPageData, error) {
	nodes, _ := e.db.ListNodes()
	states, _ := e.nodeState.GetAllNodeStates()

	counts := make(map[int64]int, len(nodes))
	zoneSet := map[string]bool{}
	for _, n := range nodes {
		if st, ok := states[n.ID]; ok {
			counts[n.ID] = st.ItemCount
		}
		if n.Zone != "" {
			zoneSet[n.Zone] = true
		}
	}
	zones := make([]string, 0, len(zoneSet))
	for z := range zoneSet {
		zones = append(zones, z)
	}

	scenePoints, _ := e.db.ListScenePoints()
	nodeLabels := make(map[string]string)
	nodeInfo := make(map[string]*NodeSceneInfo)
	mapGroups := make(map[string][]*store.ScenePoint)
	for _, sp := range scenePoints {
		if sp.ClassName == "GeneralLocation" {
			nodeLabels[sp.InstanceName] = sp.Label
			info := &NodeSceneInfo{PointName: sp.PointName}
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

	nodeTypes, _ := e.db.ListNodeTypes()
	syntheticNodes, _ := e.db.ListSyntheticNodes()
	payloadStyles, _ := e.db.ListPayloadStyles()
	edges, _ := e.db.ListEdges()

	childCounts := make(map[int64]int)
	for _, n := range nodes {
		if n.ParentID != nil {
			childCounts[*n.ParentID]++
		}
	}

	return &NodesPageData{
		Nodes:          nodes,
		Counts:         counts,
		Zones:          zones,
		NodeLabels:     nodeLabels,
		NodeInfo:       nodeInfo,
		MapGroups:      mapGroups,
		NodeTypes:      nodeTypes,
		SyntheticNodes: syntheticNodes,
		PayloadStyles:  payloadStyles,
		Edges:          edges,
		ChildCounts:    childCounts,
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

type fleetUnsupportedError struct {
	feature string
}

func (e *fleetUnsupportedError) Error() string {
	return "fleet backend does not support " + e.feature
}

func errFleetUnsupported(feature string) error {
	return &fleetUnsupportedError{feature: feature}
}

// IsFleetUnsupported returns true if the error indicates the fleet backend
// does not support the requested feature.
func IsFleetUnsupported(err error) bool {
	_, ok := err.(*fleetUnsupportedError)
	return ok
}
