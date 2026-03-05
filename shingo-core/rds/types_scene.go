package rds

// --- Scene types (full-fidelity, matches real /scene API) ---

type SceneResponse struct {
	Response
	Scene *Scene `json:"scene,omitempty"`
}

type Pos3D struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type SceneProperty struct {
	Key         string   `json:"key"`
	Type        string   `json:"type"`
	StringValue string   `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	Int32Value  *int     `json:"int32Value,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	Tag         string   `json:"tag,omitempty"`
	Value       string   `json:"value,omitempty"`
}

// FindProperty searches a property slice for a key.
func FindProperty(props []SceneProperty, key string) (SceneProperty, bool) {
	for _, p := range props {
		if p.Key == key {
			return p, true
		}
	}
	return SceneProperty{}, false
}

type AdvancedPoint struct {
	ClassName    string          `json:"className"`
	InstanceName string          `json:"instanceName"`
	Desc         string          `json:"desc,omitempty"`
	Dir          float64         `json:"dir"`
	IgnoreDir    bool            `json:"ignoreDir,omitempty"`
	Pos          Pos3D           `json:"pos"`
	Property     []SceneProperty `json:"property,omitempty"`
}

type BinLocation struct {
	ClassName    string          `json:"className"`
	InstanceName string          `json:"instanceName"`
	Desc         string          `json:"desc,omitempty"`
	PointName    string          `json:"pointName"`
	GroupName    string          `json:"groupName,omitempty"`
	Pos          Pos3D           `json:"pos"`
	Property     []SceneProperty `json:"property,omitempty"`
}

type BinLocationGroup struct {
	BinLocationList []BinLocation `json:"binLocationList"`
}

type SceneMap struct {
	MapName string `json:"mapName"`
	MD5     string `json:"md5"`
	RobotID string `json:"robotId"`
}

type LogicalMap struct {
	AdvancedPoints   []AdvancedPoint    `json:"advancedPoints,omitempty"`
	BinLocationsList []BinLocationGroup `json:"binLocationsList,omitempty"`
	AdvancedCurves   []any              `json:"advancedCurves,omitempty"`
	AdvancedBlocks   []any              `json:"advancedBlocks,omitempty"`
	AdvancedAreaList []any              `json:"advancedAreaList,omitempty"`
}

type Area struct {
	Name       string     `json:"name"`
	LogicalMap LogicalMap `json:"logicalMap"`
	Maps       []SceneMap `json:"maps,omitempty"`
	Pos        *Pos3D     `json:"pos,omitempty"`
}

type SceneRobotEntry struct {
	ID       string          `json:"id"`
	Property []SceneProperty `json:"property,omitempty"`
}

type RobotGroup struct {
	Name     string            `json:"name"`
	Desc     string            `json:"desc,omitempty"`
	Robot    []SceneRobotEntry `json:"robot,omitempty"`
	Property []SceneProperty   `json:"property,omitempty"`
}

type Scene struct {
	Areas       []Area       `json:"areas,omitempty"`
	RobotGroups []RobotGroup `json:"robotGroup,omitempty"`
	BlockGroups []any        `json:"blockGroup,omitempty"`
	Doors       []any        `json:"doors,omitempty"`
	Labels      []any        `json:"labels,omitempty"`
	Lifts       []any        `json:"lifts,omitempty"`
	BinAreas    []any        `json:"binAreas,omitempty"`
	BinMonitors []any        `json:"binMonitors,omitempty"`
	Terminals   []any        `json:"terminals,omitempty"`
	Desc        string       `json:"desc,omitempty"`
}

type PingResponse struct {
	Product string `json:"product"`
	Version string `json:"version"`
}
