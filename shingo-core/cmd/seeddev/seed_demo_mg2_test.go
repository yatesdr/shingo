//go:build docker

package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"shingocore/internal/testdb"
	"shingocore/plantspec"
)

// seed_demo_mg2_test.go — the maintained-group LOOP, asserted against the plant
// the campaign actually runs on.
//
// WHY THIS ONE READS plants/demo.yaml when every other seed test reads a pinned
// fixture. The fixture exists so ordinary seeding tests do not break every time
// the dev plant grows — a good rule, and it does not apply here. MG2-0's whole
// deliverable IS plants/demo.yaml: the proving loop every sim scenario runs
// against. A fixture copy of it would let the shipped file rot while this test
// stayed green, which is the exact failure the file is supposed to prevent.
//
// SO IT ASSERTS THE LOOP, NOT THE PLANT. No row counts, no totals, nothing that
// changes when somebody adds a weld — only the four facts that make the loop a
// loop: the group is flat and has spare positions, it is seeded at its declared
// level in both types, the presses draw from it, and the unloader pushes back
// into it.

func demoPlant(t *testing.T) *plantspec.Plant {
	t.Helper()
	p, err := plantspec.Load(filepath.Join("..", "..", "..", "plants", "demo.yaml"))
	if err != nil {
		t.Fatalf("load plants/demo.yaml: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("validate plants/demo.yaml: %v", err)
	}
	return p
}

// The Core half: the group, its positions, its carriers and its levels.
func TestSeedDemo_MaintainedGroupIsSeededAtItsLevel(t *testing.T) {
	db := testdb.Open(t)
	plant := demoPlant(t)
	if err := seedCore(db, plant, map[string]int64{}); err != nil {
		t.Fatalf("seedCore(demo): %v", err)
	}

	grp, err := db.GetNodeByName("SYN_PRESS_EMPTIES")
	if err != nil || grp == nil {
		t.Fatalf("SYN_PRESS_EMPTIES: %v", err)
	}

	// FLAT, and this is a save-time rule the seed must not be able to violate: a
	// lane means a carrier can be buried, and a level counted over buried
	// carriers is a number whose meaning changes with what is parked in front.
	children, err := db.ListChildNodes(grp.ID)
	if err != nil {
		t.Fatalf("list positions: %v", err)
	}
	if len(children) == 0 {
		t.Fatal("the maintained group has no positions")
	}
	for _, c := range children {
		if c.Depth != nil && *c.Depth > 1 {
			t.Errorf("position %s has depth %d — a maintained group must be flat", c.Name, *c.Depth)
		}
		if kids, kerr := db.ListChildNodes(c.ID); kerr == nil && len(kids) > 0 {
			t.Errorf("position %s has %d children of its own — that is a lane", c.Name, len(kids))
		}
	}

	// The declared level, and the carriers standing against it.
	levels, err := db.ListMaintainLevels(grp.ID)
	if err != nil {
		t.Fatalf("list levels: %v", err)
	}
	want := map[string]int{}
	total := 0
	for _, l := range levels {
		want[l.BinTypeCode] = l.Want
		total += l.Want
	}
	if len(want) < 2 {
		t.Errorf("levels = %v — the mixed-type shape needs two carrier types, and a "+
			"single-type group makes every mixed assertion in the campaign vacuous", want)
	}
	if total >= len(children) {
		t.Errorf("levels sum to %d across %d positions — a maintained group with no spare "+
			"position has nowhere to put a carrier coming back in, and the pre-resolve loop "+
			"is bounded by free positions", total, len(children))
	}

	resident := map[string]int{}
	for _, c := range children {
		bs, berr := db.ListBinsByNode(c.ID)
		if berr != nil {
			t.Fatalf("bins at %s: %v", c.Name, berr)
		}
		for _, b := range bs {
			bt, terr := db.GetBinType(b.BinTypeID)
			if terr != nil || bt == nil {
				t.Fatalf("bin type %d: %v", b.BinTypeID, terr)
			}
			resident[bt.Code]++
		}
	}
	for code, n := range want {
		if resident[code] != n {
			t.Errorf("seeded %d %s carrier(s) against a level of %d. The group must start "+
				"SETTLED, or every scenario opens on a cold-start refill it did not ask for",
				resident[code], code, n)
		}
	}
}

// The Edge half: the claims that make the group drain and refill.
//
// Without these the group is a shape with nothing routed to it — which is
// exactly what MG1 left behind, and what MG2-0 exists to finish.
func TestSeedDemo_PressesDrawFromTheGroupAndTheUnloaderPushesBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edge.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open edge db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(edgeDDL); err != nil {
		t.Fatalf("apply edge DDL: %v", err)
	}
	plant := demoPlant(t)
	if err := seedEdgeDB(db, plant, fakeBinIDs(plant)); err != nil {
		t.Fatalf("seedEdgeDB(demo): %v", err)
	}

	const group = "SYN_PRESS_EMPTIES"

	// Every produce claim on a press the group SUPPORTS must source from it.
	// Reading the supports list rather than naming PRESS-1/PRESS-2 means adding a
	// third supported press to the yaml without wiring it fails here.
	supported := map[string]bool{}
	for _, mg := range plant.MaintainedGroups {
		if mg.Group != group {
			continue
		}
		for _, proc := range mg.Supports {
			supported[proc] = true
		}
	}
	if len(supported) == 0 {
		t.Fatalf("%s supports no process — nothing draws it down, so the keeper keeps a "+
			"level nobody consumes", group)
	}

	styleProcess := map[string]string{}
	for _, st := range plant.Styles {
		styleProcess[st.Name] = st.Process
	}
	sawProduce := 0
	for _, c := range plant.Claims {
		if c.Role != "produce" || !supported[styleProcess[c.Style]] {
			continue
		}
		sawProduce++
		if c.InboundSource != group {
			t.Errorf("claim %s/%s is a produce claim on supported process %q but sources "+
				"empties from %q, not %s — the group is never drawn down through it",
				c.CoreNode, c.Style, styleProcess[c.Style], c.InboundSource, group)
		}
	}
	if sawProduce == 0 {
		t.Fatalf("no produce claim on any process %s supports", group)
	}

	// And something must push carriers back in, or the keeper is the only supply
	// and the loop never exercises the `coming` term.
	pushesBack := 0
	for _, c := range plant.Claims {
		if c.OutboundDestination == group {
			pushesBack++
		}
	}
	if pushesBack == 0 {
		t.Errorf("no claim has outbound_destination %s. Without a return path the keeper is "+
			"the only supply, and CountTypedInboundToGroup — the `coming` term the whole "+
			"subtraction turns on — is never exercised by the loop", group)
	}

	// The changeover the campaign runs needs somewhere to change over TO.
	byProcess := map[string]int{}
	for _, st := range plant.Styles {
		byProcess[st.Process]++
	}
	multi := 0
	for proc := range supported {
		if byProcess[proc] > 1 {
			multi++
		}
	}
	if multi == 0 {
		t.Errorf("no process %s supports has a second style. 'Changeover under load' is not "+
			"a scenario on a plant where every process has exactly one style", group)
	}
}
