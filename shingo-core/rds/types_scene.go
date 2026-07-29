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

// CurveEnd is one endpoint of an advanced curve: the advanced point it
// attaches to (by instance name) plus its raw position.
type CurveEnd struct {
	InstanceName string `json:"instanceName"`
	Pos          Pos3D  `json:"pos"`
}

// AdvancedCurve is a drivable path segment between two advanced points
// (className e.g. "StraightPath", "BezierPath", "DegenerateBezier"; the
// instance name is conventionally "<from>-<to>").
//
// ControlPos1/ControlPos2 are the segment's two cubic-Bezier control handles,
// in the start→end direction. SEER writes them as TOP-LEVEL siblings of
// startPos/endPos, NOT inside Property — Property carries exactly one key
// across all 377 of Springfield's curves (bindRobotMap) and no geometry
// whatsoever. Some segments omit the two keys entirely, so they are pointers:
// absent stays absent instead of collapsing into a coordinate at the origin.
//
// Vendor properties beyond the endpoints and these handles are still not
// modeled. shingo needs the connectivity, the endpoint coordinates, and the
// shape the robot actually drives between them.
type AdvancedCurve struct {
	ClassName    string          `json:"className"`
	InstanceName string          `json:"instanceName"`
	StartPos     CurveEnd        `json:"startPos"`
	EndPos       CurveEnd        `json:"endPos"`
	ControlPos1  *Pos3D          `json:"controlPos1,omitempty"`
	ControlPos2  *Pos3D          `json:"controlPos2,omitempty"`
	Property     []SceneProperty `json:"property,omitempty"`
}

// ControlPoints returns the curve's two cubic-Bezier handles and whether they
// describe real geometry.
//
// ABSENT IS NOT THE ONLY WAY THE SCENE SAYS "NO HANDLES". Of Springfield's 111
// StraightPath segments, 57 omit the keys and the other 54 carry {0,0,0}
// twice — one meaning, two encodings. The origin is a real coordinate on a
// SEER map (Springfield has scene points within a metre of it), so an all-zero
// pair cannot be read as geometry: honouring it drags a straight aisle through
// (0,0), which on LM197-LM137 is 52 m of error on a 1.4 m segment. The
// all-zero pair is the vendor's sentinel and reads here as absent.
//
// The sentinel test is deliberately asymmetric — EITHER handle being all-zero
// disqualifies the pair, not both. A one-zero-one-real pair has never appeared
// in a plant scene, and the two ways of being wrong about it are not the same
// size: mistaking a real handle for a sentinel costs sub-metre accuracy on a
// single segment, while mistaking a sentinel for a real handle is the 52 m bug
// this function exists to prevent.
func (c AdvancedCurve) ControlPoints() (Pos3D, Pos3D, bool) {
	if c.ControlPos1 == nil || c.ControlPos2 == nil {
		return Pos3D{}, Pos3D{}, false
	}
	if *c.ControlPos1 == (Pos3D{}) || *c.ControlPos2 == (Pos3D{}) {
		return Pos3D{}, Pos3D{}, false
	}
	return *c.ControlPos1, *c.ControlPos2, true
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
	AdvancedCurves   []AdvancedCurve    `json:"advancedCurves,omitempty"`
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
