package domain

import (
	"testing"

	"shingo/protocol"
)

// cell_staging_test.go — a staging slot is drawn, and it is not a position.
//
// WHY THEIR OWN LIST AND NOT A Kind ON Positions. The plan's shape was
// `Kind: "staging"` inside Positions, and the tree closes it: nine sites treat
// a member of that list as a position an operator may claim — the model's init
// mints a tappable cell per position, "+ Add a position" offers the free ones,
// the desktop's table gives each one a row, partner pickers list them,
// placeEvenly puts them in the front row, rowKinds captions them. Every one of
// those needs a guard, and ONE miss saves a supermarket lane as a cell
// position — a write the store refuses (ErrRoutingNodeIsPosition) only if it
// is a routing row, and accepts silently if it is not.
//
// A separate list makes "a staging node is not a position" true by
// construction instead of by nine guards.

func stagingNodes(names ...string) []Node {
	out := make([]Node, 0, len(names))
	for i, n := range names {
		id := int64(5)
		out = append(out, Node{
			CoreNodeName: n, Sequence: i + 1, Enabled: true, OperatorStationID: &id,
		})
	}
	return out
}

// THE RUNNING FLOW'S STAGING, on the board's own picture: the names the active
// claims park at, and nothing else.
func TestBuildCellPicture_StagingTheRunningFlowNames(t *testing.T) {
	pic := BuildCellPicture(CellPictureInput{
		StationID: 5,
		Nodes:     stagingNodes("PLN_01", "PLN_04"),
		Claims: map[string]NodeClaim{
			"PLN_01": {CoreNodeName: "PLN_01", SwapMode: protocol.SwapModeTwoRobot,
				InboundStaging: "SLN_07"},
			"PLN_04": {CoreNodeName: "PLN_04", SwapMode: protocol.SwapModeSingleRobot,
				InboundStaging: "SLN_08", OutboundStaging: "SLN_09"},
		},
	})
	if len(pic.Staging) != 3 {
		t.Fatalf("%d staging cards, want three: %+v", len(pic.Staging), pic.Staging)
	}
	by := map[string]CellStaging{}
	for _, s := range pic.Staging {
		by[s.CoreNodeName] = s
	}
	if s := by["SLN_07"]; s.PartnerOf != "PLN_01" || s.Field != "inbound_staging" {
		t.Errorf("SLN_07 = %+v, want inbound_staging for PLN_01", s)
	}
	if s := by["SLN_09"]; s.PartnerOf != "PLN_04" || s.Field != "outbound_staging" {
		t.Errorf("SLN_09 = %+v, want outbound_staging for PLN_04", s)
	}
	// AND NONE OF THEM IS A POSITION. This is the whole rule.
	for _, p := range pic.Positions {
		if p.CoreNodeName == "SLN_07" || p.CoreNodeName == "SLN_08" || p.CoreNodeName == "SLN_09" {
			t.Errorf("%s is drawn as a position; a staging lane is not a position an operator may claim",
				p.CoreNodeName)
		}
	}
}

// A STAGING NODE THAT IS A POSITION STAYS A POSITION. Hopkinsville's P400
// parks on its own back slots — PLN_02 and PLN_05 are process_nodes rows of the
// cell AND the staging fields of its claims — and those cards must not move,
// duplicate, or change what they are.
func TestBuildCellPicture_AStagingNodeThatIsAPositionStaysOne(t *testing.T) {
	pic := BuildCellPicture(CellPictureInput{
		StationID: 5,
		Nodes:     stagingNodes("PLN_01", "PLN_02"),
		Claims: map[string]NodeClaim{
			"PLN_01": {CoreNodeName: "PLN_01", SwapMode: protocol.SwapModeTwoRobot,
				InboundStaging: "PLN_02"},
		},
	})
	if len(pic.Staging) != 0 {
		t.Errorf("PLN_02 is a position of this cell and was also drawn as a staging card: %+v", pic.Staging)
	}
	p := cellPositionNamed(pic, "PLN_02")
	if p == nil || p.PartnerKind != "staging" || p.PartnerOf != "PLN_01" {
		t.Errorf("PLN_02 = %+v, want the position it always was, captioned as PLN_01's staging slot", p)
	}
}

// THE COMPOSER'S PICTURE OFFERS WHAT THE CELL COULD PARK AT (R4): the
// process's enabled staging-role routing nodes, whether or not the running
// flow names them. An offered card carries no partner, because nothing is
// using it yet.
func TestBuildCellPicture_OfferedStagingHasNoPartner(t *testing.T) {
	pic := BuildCellPicture(CellPictureInput{
		StationID: 5,
		Nodes:     stagingNodes("PLN_01"),
		Claims: map[string]NodeClaim{
			"PLN_01": {CoreNodeName: "PLN_01", SwapMode: protocol.SwapModeTwoRobot,
				InboundStaging: "SLN_07"},
		},
		StagingOffers: []string{"SLN_07", "SLN_08"},
	})
	if len(pic.Staging) != 2 {
		t.Fatalf("%d staging cards, want the used one and the offered one: %+v", len(pic.Staging), pic.Staging)
	}
	by := map[string]CellStaging{}
	for _, s := range pic.Staging {
		by[s.CoreNodeName] = s
	}
	if s := by["SLN_07"]; s.PartnerOf != "PLN_01" {
		t.Errorf("SLN_07 is in the running flow and lost its partner: %+v", s)
	}
	if s, ok := by["SLN_08"]; !ok || s.PartnerOf != "" || s.Field != "" {
		t.Errorf("SLN_08 = %+v, want an offered card with no partner", s)
	}
	// AN OFFER THAT IS A POSITION IS NOT AN OFFER either — same rule, same
	// reason: the engineer switched a name on in Settings › Routing, and the
	// store would have refused it as a routing row if it were a position.
	// Belt and braces, because the two lists are written at different times.
	for _, s := range pic.Staging {
		if cellPositionNamed(pic, s.CoreNodeName) != nil {
			t.Errorf("%s is both a position and a staging card", s.CoreNodeName)
		}
	}
}

// PLACED WHERE THE MAP PUTS THEM, by the same join rule as a position: the
// GeneralLocation point of that name. A staging lane the map does not carry is
// drawn in the band without coordinates, and — this is the part that matters —
// it must NOT drag the cell into the schematic.
func TestBuildCellPicture_UnplacedStagingDoesNotFlipTheCellToSchematic(t *testing.T) {
	geom := twoPointScene(t)
	pic := BuildCellPicture(CellPictureInput{
		StationID: 5,
		Nodes:     stagingNodes("PLN_01", "PLN_04"),
		Claims: map[string]NodeClaim{
			"PLN_01": {CoreNodeName: "PLN_01", SwapMode: protocol.SwapModeTwoRobot,
				InboundStaging: "SLN_NOWHERE"},
		},
		Geometry: geom,
	})
	if !pic.Geometry {
		t.Error("a staging lane the map does not place flipped the whole cell to the schematic; " +
			"Geometry is about the POSITIONS, which are all placed here")
	}
	if len(pic.Staging) != 1 || pic.Staging[0].X != nil {
		t.Errorf("staging = %+v, want one card with no coordinates", pic.Staging)
	}
}

func cellPositionNamed(pic *CellPicture, name string) *CellPosition {
	for i := range pic.Positions {
		if pic.Positions[i].CoreNodeName == name {
			return &pic.Positions[i]
		}
	}
	return nil
}

func twoPointScene(t *testing.T) *SceneGeometry {
	t.Helper()
	at := func(v float64) *float64 { return &v }
	g, err := NewSceneGeometry("staging-scene", []protocol.ScenePointInfo{
		{InstanceName: "PLN_01", ClassName: "GeneralLocation", PosX: at(1), PosY: at(1)},
		{InstanceName: "PLN_04", ClassName: "GeneralLocation", PosX: at(2), PosY: at(1)},
		{InstanceName: "LM100", ClassName: "LocationMark", PosX: at(1.5), PosY: at(1.4)},
	}, []protocol.SceneEdgeInfo{
		{From: "PLN_01", To: "LM100", FromX: at(1), FromY: at(1), ToX: at(1.5), ToY: at(1.4)},
		{From: "LM100", To: "PLN_04", FromX: at(1.5), FromY: at(1.4), ToX: at(2), ToY: at(1)},
	})
	if err != nil {
		t.Fatalf("scene: %v", err)
	}
	return g
}

// ── the LM points that ride the picture ──────────────────────────────────────
//
// THE STATION HAS NO PLANT MAP, and that is why these exist. The desktop's
// ComposerMap is a hundred kilobytes of coordinates for a screen the station
// never opens, so without LMs on the picture the HMI could draw a robot's path
// only as a straight line between two cards. They ride the PICTURE — once per
// fetch, when its version moves — and never the poll.
func TestBuildCellPicture_LMsAreBoundedToTheCellsOwnRegion(t *testing.T) {
	at := func(v float64) *float64 { return &v }
	g, err := NewSceneGeometry("lm-scene", []protocol.ScenePointInfo{
		{InstanceName: "PLN_01", ClassName: "GeneralLocation", PosX: at(10), PosY: at(10)},
		{InstanceName: "PLN_04", ClassName: "GeneralLocation", PosX: at(12), PosY: at(10)},
		{InstanceName: "LM_NEAR", ClassName: "LocationMark", PosX: at(11), PosY: at(11)},
		{InstanceName: "LM_FAR", ClassName: "LocationMark", PosX: at(90), PosY: at(90)},
		{InstanceName: "SMN_01", ClassName: "GeneralLocation", PosX: at(11), PosY: at(11)},
	}, []protocol.SceneEdgeInfo{
		{From: "PLN_01", To: "LM_NEAR", FromX: at(10), FromY: at(10), ToX: at(11), ToY: at(11)},
	})
	if err != nil {
		t.Fatalf("scene: %v", err)
	}
	in := CellPictureInput{
		StationID:   5,
		Nodes:       stagingNodes("PLN_01", "PLN_04"),
		Geometry:    g,
		LMRegionPad: 5,
	}
	pic := BuildCellPicture(in)
	names := make([]string, 0, len(pic.LMs))
	for _, lm := range pic.LMs {
		names = append(names, lm.Name)
	}
	if len(names) != 1 || names[0] != "LM_NEAR" {
		t.Errorf("LMs = %v, want just LM_NEAR — the far one is the plant, and a point that is not "+
			"an LM is not a waypoint however close it sits", names)
	}

	// PAD ZERO IS NO LMs AT ALL, which is what the board's read asks for: the
	// field is opt-in, so a caller that does not draw a path does not pay for
	// the points.
	in.LMRegionPad = 0
	if bare := BuildCellPicture(in); len(bare.LMs) != 0 {
		t.Errorf("a picture built with no LM region carries %d LMs", len(bare.LMs))
	}
}
