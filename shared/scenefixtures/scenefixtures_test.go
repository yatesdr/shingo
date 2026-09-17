package scenefixtures

import "testing"

// The fixtures are trusted for facts a hand-built scene cannot reproduce, so
// the facts are pinned here: the full-table counts, the absence of duplicate
// instance names (the Edge cache keys on them), and the label trap that makes
// plant B the fixture worth having. These are the source plants' numbers,
// carried through the anonymiser unchanged — a re-derivation that moves one of
// them has stopped preserving the property the fixture exists for.
func TestFixturesCarryTheRecordedPlantFacts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		plant         Plant
		points, edges int
		labelled      int
	}{
		{"a", A(), 383, 635, 60},
		{"b", B(), 350, 588, 12},
	}
	for _, c := range cases {
		if got := len(c.plant.ScenePoints); got != c.points {
			t.Errorf("%s: %d scene points, the fixture carries %d", c.name, got, c.points)
		}
		if got := len(c.plant.SceneEdges); got != c.edges {
			t.Errorf("%s: %d scene edges, the fixture carries %d", c.name, got, c.edges)
		}
		seen := map[string]bool{}
		labelled := 0
		for _, p := range c.plant.ScenePoints {
			if p.InstanceName == "" {
				t.Errorf("%s: scene point id %d has a blank instance_name", c.name, p.ID)
			}
			if seen[p.InstanceName] {
				t.Errorf("%s: instance_name %q appears twice — the Edge cache keys on it", c.name, p.InstanceName)
			}
			seen[p.InstanceName] = true
			if p.Label != "" {
				labelled++
			}
		}
		if labelled != c.labelled {
			t.Errorf("%s: %d labelled points, want %d — the label trap this fixture exists for has moved", c.name, labelled, c.labelled)
		}
		straightWithHandles, bezierWithout := 0, 0
		for _, e := range c.plant.SceneEdges {
			complete := e.Ctrl1X != nil && e.Ctrl1Y != nil && e.Ctrl2X != nil && e.Ctrl2Y != nil
			none := e.Ctrl1X == nil && e.Ctrl1Y == nil && e.Ctrl2X == nil && e.Ctrl2Y == nil
			if !complete && !none {
				t.Errorf("%s: edge %s carries a partial handle pair", c.name, e.InstanceName)
			}
			if e.ClassName == "StraightPath" && complete {
				straightWithHandles++
			}
			if e.ClassName != "StraightPath" && none {
				bezierWithout++
			}
		}
		if straightWithHandles != 0 || bezierWithout != 0 {
			t.Errorf("%s: %d StraightPath rows carry handles and %d Bezier rows carry none — the NULL rows are exactly the StraightPath ones",
				c.name, straightWithHandles, bezierWithout)
		}
	}
	// The join is payload_bin_types -> payloads -> code, and the wire carries
	// the code. One payload, one bin type: the shape the resolver reads.
	if got := len(A().PayloadBinTypeCodes()["SYN-A-P002"]); got != 1 {
		t.Errorf("A SYN-A-P002 resolves to %d bin types, want 1 (TOTE-2415)", got)
	}
}
