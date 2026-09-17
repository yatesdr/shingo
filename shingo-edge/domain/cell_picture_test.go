package domain

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/shared/scenefixtures"
)

// cell_picture_test.go — the read-only picture's data, built on the synthetic
// plants. The join rule and the choreography roles are what a hand-built
// fixture would get right by accident; a whole plant's shape is what they have
// to be right against.

// geometryOf builds the Edge cache shape from a plant, the way a full
// node-list response would have.
func geometryOf(fx scenefixtures.Plant) *SceneGeometry {
	g := &SceneGeometry{Revision: "fixture", Points: map[string]ScenePointGeom{}}
	for _, p := range fx.ScenePoints {
		g.Points[p.InstanceName] = ScenePointGeom{InstanceName: p.InstanceName, ClassName: p.ClassName, X: p.PosX, Y: p.PosY, Dir: p.Dir}
	}
	for _, e := range fx.SceneEdges {
		g.Edges = append(g.Edges, SceneEdgeGeom{From: e.FromName, To: e.ToName, FromX: e.FromX, FromY: e.FromY, ToX: e.ToX, ToY: e.ToY})
	}
	return g
}

func nodesOf(fx scenefixtures.Plant, processID int64) []Node {
	var out []Node
	for _, n := range fx.ProcessNodes {
		if n.ProcessID != processID {
			continue
		}
		node := Node{ID: n.ID, ProcessID: n.ProcessID, OperatorStationID: n.OperatorStationID, CoreNodeName: n.CoreNodeName,
			Code: n.Code, Name: n.Name, Sequence: n.Sequence, Enabled: n.Enabled == 1}
		if n.DeletedAt != nil {
			gone := time.Time{}
			node.DeletedAt = &gone
		}
		out = append(out, node)
	}
	return out
}

func claimsOf(fx scenefixtures.Plant, styleID int64) map[string]NodeClaim {
	out := map[string]NodeClaim{}
	for _, c := range fx.StyleNodeClaims {
		if c.StyleID != styleID {
			continue
		}
		out[c.CoreNodeName] = NodeClaim{StyleID: c.StyleID, CoreNodeName: c.CoreNodeName, Role: protocol.ClaimRole(c.Role),
			SwapMode: protocol.SwapMode(c.SwapMode), PayloadCode: c.PayloadCode, InboundStaging: c.InboundStaging,
			OutboundStaging: c.OutboundStaging, InboundSource: c.InboundSource, OutboundDestination: c.OutboundDestination,
			PairedCoreNode: c.PairedCoreNode, SecondPairedCoreNode: c.SecondPairedCoreNode}
	}
	return out
}

func groupsOf(fx scenefixtures.Plant) map[string][]string {
	out := map[string][]string{}
	for _, n := range fx.Nodes {
		if n.ParentName != nil && *n.ParentName != "" {
			out[*n.ParentName] = append(out[*n.ParentName], n.Name)
		}
	}
	return out
}

func positionByName(p *CellPicture, name string) *CellPosition {
	for i := range p.Positions {
		if p.Positions[i].CoreNodeName == name {
			return &p.Positions[i]
		}
	}
	return nil
}

// TestCellPicture_JoinRuleResolvesEveryPlantBPosition is the join rule on the
// plant where the wrong join is invisible: every PLN_* and SMN_*
// process node in B resolves to a scene point, and the point it
// resolves to is the GeneralLocation row — the bin location — not the
// ActionPoint the same row names as the robot's approach pose. A join on
// label resolves none of them (12 of 350 labels are populated, none on a
// PLN row); a join that took the ActionPoint would place every card half a
// metre off. Both would fail this test on the coordinates.
func TestCellPicture_JoinRuleResolvesEveryPlantBPosition(t *testing.T) {
	t.Parallel()
	fx := scenefixtures.B()
	geom := geometryOf(fx)
	general := map[string]scenefixtures.ScenePoint{}
	action := map[string]scenefixtures.ScenePoint{}
	labelled := map[string]bool{}
	for _, p := range fx.ScenePoints {
		switch p.ClassName {
		case "GeneralLocation":
			general[p.InstanceName] = p
		case "ActionPoint":
			action[p.InstanceName] = p
		}
		if p.Label != "" {
			labelled[p.Label] = true
		}
	}

	checked := 0
	for _, proc := range fx.Processes {
		for _, n := range nodesOf(fx, proc.ID) {
			name := n.CoreNodeName
			if len(name) < 4 || (name[:4] != "PLN_" && name[:4] != "SMN_") {
				continue
			}
			checked++
			gl, ok := general[name]
			if !ok {
				t.Errorf("%s/%s: no GeneralLocation row for it — the fixture's coverage is meant to be complete", proc.Name, name)
				continue
			}
			x, y, placed := LocateCellPosition(geom, name)
			if !placed {
				t.Errorf("%s/%s: unresolved — a label join would leave it here (label populated for it: %v)", proc.Name, name, labelled[name])
				continue
			}
			if x != gl.PosX || y != gl.PosY {
				t.Errorf("%s/%s: placed at (%v,%v), the GeneralLocation row is at (%v,%v)", proc.Name, name, x, y, gl.PosX, gl.PosY)
			}
			if ap, ok := action[gl.PointName]; ok && x == ap.PosX && y == ap.PosY && (ap.PosX != gl.PosX || ap.PosY != gl.PosY) {
				t.Errorf("%s/%s: placed at the ActionPoint %s, not at the bin location", proc.Name, name, gl.PointName)
			}
		}
	}
	// The rule is class membership, not the name: the same instance name on
	// any other class is a different point and must not place a card.
	wrongClass := &SceneGeometry{Revision: "r", Points: map[string]ScenePointGeom{
		"PLN_001": {InstanceName: "PLN_001", ClassName: "ActionPoint", X: 1, Y: 1},
	}}
	if _, _, placed := LocateCellPosition(wrongClass, "PLN_001"); placed {
		t.Error("an ActionPoint named like the node placed a card — the join is on the GeneralLocation row only")
	}
	if _, _, placed := LocateCellPosition(nil, "PLN_001"); placed {
		t.Error("no scene at all placed a card")
	}
	if checked < 30 {
		t.Fatalf("checked only %d PLN_*/SMN_* process nodes; plant B has 33 — the walk has drifted", checked)
	}
	if labelled["PLN_001"] || labelled["SMN_001"] {
		t.Error("a plant B PLN/SMN name is now a label — the fixture no longer proves a label join fails")
	}
}

// TestCellPicture_PlantAStyle7IsTwoIndexPairs: plant A's running style 7 —
// PLN_01 and PLN_04 index over PLN_02 and PLN_05, PLN_03 and PLN_06 unused,
// totes from Supermarket Empty Totes to Supermarket Area.
func TestCellPicture_PlantAStyle7IsTwoIndexPairs(t *testing.T) {
	t.Parallel()
	fx := scenefixtures.A()
	pic := BuildCellPicture(CellPictureInput{
		StationID: 5, Nodes: nodesOf(fx, 1), Claims: claimsOf(fx, 7),
		Geometry: geometryOf(fx), Groups: groupsOf(fx),
	})
	if !pic.Geometry {
		t.Fatal("plant A's press has geometry for every position; the picture fell back to the schematic")
	}
	var names []string
	for _, p := range pic.Positions {
		names = append(names, p.CoreNodeName)
	}
	if len(pic.Positions) != 6 {
		t.Fatalf("positions = %v, want the six press positions and nothing from the supermarket", names)
	}
	for _, smn := range []string{"SMN_01", "SMN_03", "SMN_04"} {
		if positionByName(pic, smn) != nil {
			t.Errorf("%s is a supermarket node, bound to no station and no claim — it must not be a press position", smn)
		}
	}
	for _, c := range []struct{ front, back string }{{"PLN_01", "PLN_02"}, {"PLN_04", "PLN_05"}} {
		f := positionByName(pic, c.front)
		if f == nil || f.Role != "front" || f.Claim == nil || f.Claim.SwapMode != "two_robot_press_index" || f.Claim.PairedCoreNode != c.back {
			t.Errorf("%s = %+v, want a front position indexing over %s", c.front, f, c.back)
		}
		b := positionByName(pic, c.back)
		if b == nil || b.Role != "back" || b.PartnerOf != c.front || b.PartnerKind != "deck" {
			t.Errorf("%s = %+v, want a back position on deck for %s", c.back, b, c.front)
		}
		if b != nil && b.Claim != nil {
			t.Errorf("%s carries a claim; a back position has no row of its own", c.back)
		}
	}
	for _, unused := range []string{"PLN_03", "PLN_06"} {
		u := positionByName(pic, unused)
		if u == nil || u.Role != "" || u.Claim != nil || u.Kind != "front" {
			t.Errorf("%s = %+v, want an unused front position", unused, u)
		}
	}
	if f := positionByName(pic, "PLN_01"); f != nil && f.Claim != nil {
		if f.Claim.PayloadCode != "SYN-A-P002" {
			t.Errorf("PLN_01 claim = %+v, want the style's front payload", f.Claim)
		}
		if f.Claim.InboundSource != "Supermarket Empty Totes" || f.Claim.OutboundDestination != "Supermarket Area" {
			t.Errorf("PLN_01 dock = %q -> %q", f.Claim.InboundSource, f.Claim.OutboundDestination)
		}
	}
	if got := pic.Groups["Supermarket Empty Totes"]; len(got) != 4 {
		t.Errorf("Supermarket Empty Totes members = %v, want SMN_05..08", got)
	}
	if got := pic.Groups["Supermarket Area"]; len(got) != 4 {
		t.Errorf("Supermarket Area members = %v, want SMN_01..04", got)
	}
	// TRUE RELATIVE SPACING. The fixture's map is the plant's under one rigid
	// motion, so the distances are the plant's and the position carries the
	// scene point's own coordinates through unrounded.
	if p := positionByName(pic, "PLN_05"); p == nil || p.X == nil || *p.X != 141.945 || *p.Y != 14.542 {
		t.Errorf("PLN_05 = %+v, want (141.945, 14.542)", p)
	}
}

// TestCellPicture_PlantAStyle11IsTwoStagingMoves: style 11 runs the other
// choreography on the same press — PLN_03 and PLN_06 with Robot 1 staging at
// PLN_02 and PLN_05.
func TestCellPicture_PlantAStyle11IsTwoStagingMoves(t *testing.T) {
	t.Parallel()
	fx := scenefixtures.A()
	pic := BuildCellPicture(CellPictureInput{
		StationID: 5, Nodes: nodesOf(fx, 1), Claims: claimsOf(fx, 11),
		Geometry: geometryOf(fx), Groups: groupsOf(fx),
	})
	for _, c := range []struct{ front, staging string }{{"PLN_03", "PLN_02"}, {"PLN_06", "PLN_05"}} {
		f := positionByName(pic, c.front)
		if f == nil || f.Role != "front" || f.Claim == nil || f.Claim.SwapMode != "two_robot" || f.Claim.InboundStaging != c.staging {
			t.Errorf("%s = %+v, want a front position staging at %s", c.front, f, c.staging)
		}
		s := positionByName(pic, c.staging)
		if s == nil || s.Role != "back" || s.PartnerOf != c.front || s.PartnerKind != "staging" {
			t.Errorf("%s = %+v, want a back position staging for %s", c.staging, s, c.front)
		}
	}
	for _, unused := range []string{"PLN_01", "PLN_04"} {
		if u := positionByName(pic, unused); u == nil || u.Role != "" || u.Kind != "front" {
			t.Errorf("%s = %+v, want an unused FRONT position under style 11 — it is a front slot of the press", unused, u)
		}
	}
	// PLN_02 and PLN_05 are the press's back slots under every style: with
	// the process-wide partner set handed in, they read as back positions
	// even in a style that does not use them.
	// The store's query: partners named by the LIVE styles of THIS process.
	// An orphaned claim (its style row gone) or another process's claim does
	// not shape this press.
	live := map[int64]bool{}
	for _, s := range fx.Styles {
		if s.ProcessID == 1 && s.DeletedAt == nil {
			live[s.ID] = true
		}
	}
	allBack := map[string]bool{}
	for _, c := range fx.StyleNodeClaims {
		if !live[c.StyleID] {
			continue
		}
		for _, n := range []string{c.PairedCoreNode, c.SecondPairedCoreNode, c.InboundStaging, c.OutboundStaging} {
			if n != "" {
				allBack[n] = true
			}
		}
	}
	none := BuildCellPicture(CellPictureInput{StationID: 5, Nodes: nodesOf(fx, 1), BackPositions: allBack, Geometry: geometryOf(fx)})
	for _, back := range []string{"PLN_02", "PLN_05"} {
		if p := positionByName(none, back); p == nil || p.Kind != "back" || p.Role != "" {
			t.Errorf("%s with no style running = %+v, want an unused back position", back, p)
		}
	}
	if p := positionByName(none, "PLN_03"); p == nil || p.Kind != "front" {
		t.Errorf("PLN_03 with no style running = %+v, want kind front", p)
	}
}

// TestCellPicture_NoGeometryIsTheSchematicNeverBlank: before the first full
// sync there are no coordinates. The picture still has every position, in
// sequence order, flagged for even spacing — never an empty panel.
func TestCellPicture_NoGeometryIsTheSchematicNeverBlank(t *testing.T) {
	t.Parallel()
	fx := scenefixtures.A()
	pic := BuildCellPicture(CellPictureInput{StationID: 5, Nodes: nodesOf(fx, 1), Claims: claimsOf(fx, 7)})
	if pic == nil {
		t.Fatal("no picture at all")
	}
	if pic.Geometry {
		t.Error("geometry flagged true with no scene cached")
	}
	if len(pic.Positions) != 6 {
		t.Fatalf("%d positions without geometry, want 6 — the schematic draws every position", len(pic.Positions))
	}
	// process_nodes.sequence order: PLN_05 (0), PLN_01 (1), PLN_03 (2), PLN_02 (3), PLN_04 (3), PLN_06 (4).
	want := []string{"PLN_05", "PLN_01", "PLN_03", "PLN_02", "PLN_04", "PLN_06"}
	for i, p := range pic.Positions {
		if p.X != nil || p.Y != nil {
			t.Errorf("%s has coordinates with no scene cached", p.CoreNodeName)
		}
		if p.CoreNodeName != want[i] {
			t.Errorf("position %d = %s, want %s (sequence order, name as the tiebreak)", i, p.CoreNodeName, want[i])
		}
	}

	// A scene that has the plant but not this press: still the schematic,
	// and still every position.
	partial := &SceneGeometry{Revision: "r", Points: map[string]ScenePointGeom{"PLN_01": {InstanceName: "PLN_01", ClassName: "GeneralLocation", X: 1, Y: 2}}}
	pic = BuildCellPicture(CellPictureInput{StationID: 5, Nodes: nodesOf(fx, 1), Claims: claimsOf(fx, 7), Geometry: partial})
	if pic.Geometry || len(pic.Positions) != 6 {
		t.Errorf("one placed position out of six: geometry=%v positions=%d, want schematic with all six", pic.Geometry, len(pic.Positions))
	}
}

// THE BIN WORD IS GONE, AND SO IS THE TEST THAT PINNED IT. Owner ruling R2
// (2026-09-12) removed "totes"/"bins" from every surface, and with its one
// reader gone CellPicture.BinWord, CellClaim.BinTypeCode and
// CellPictureInput.BinTypeFor went with it. What the test asserted —
// TestCellPicture_BinWordComesFromTheBinType: a press-index claim on a KD bin
// says bins, a two-robot claim on a tote says totes, "" is unknown and not
// tote — was right about a question nothing asks any more.
