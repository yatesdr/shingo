package www

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/service"
	"shingoedge/store/processes"
)

// TestOperatorFlowPerModeCharacterisation characterises the cell picture per
// swap mode at the PRE-CHANGE tree, so the picture rewrite starts from a
// record of what every mode draws today: each position's moves (which robot,
// which direction, what the line says), each card's lines, the dock notes,
// the staging captions and whether a route strip is drawn.
//
// GREEN BEFORE THE REWRITE, and it must stay green through it: the words on
// these cards change only where the design's owner rulings say they change,
// and every other diff this test turns up afterwards is a ruling to cite or
// a bug to name.
//
// Fixtures. Plant A's Press A1 provides the two index styles the whole-plant
// test already seeds: style "PART SYN-A-S003" is a two-row press-index cell
// (two produce positions, each with its paired back position), "PART
// SYN-A-S007" is the two-robot staging pair. single_robot and sequential are
// not in either fixture, so they are SYNTHETIC STYLES seeded through the
// store's own UpsertClaim — the same write path the fixtures' claims arrive
// through, so the view shape is the server's — on the same seeded cell's
// spare positions. The synthetic single_robot style mirrors the shape the
// lane-stress sim plant gives WELD-2: a consume and a produce position, each
// parking the new bin and clearing the old, sharing one inbound and one
// outbound lane. WELD-2 itself is covered at the sim gate, not here: the Edge
// module does not depend on the core plantspec that loads that yaml.
func TestOperatorFlowPerModeCharacterisation(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping per-mode characterisation")
	}
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	stationID := seeded.Stations["screen-a4"]
	svc := service.NewStationService(db)
	svc.SetBinTypeResolver(testdb.BinTypeOf(fx))
	svc.SetCoreNodeGroupResolver(func() map[string][]string { return testdb.GroupsOf(fx) })
	geom := testdb.SceneGeometryOf(fx)

	// ── two synthetic styles on the cell's own positions ────────────────────
	//
	// PLN_01/PLN_04 are the station-bound front positions (they carry the
	// press-index styles' claims when those styles run); PLN_02/PLN_05 are
	// their stationless back positions; PLN_03/PLN_06 are the spare
	// station-bound front positions the whole-plant test captions "front
	// position". Each synthetic style claims only the positions its
	// choreography needs, so every one of these views draws the same six cards
	// and the styles differ only in what is running.
	singleStyleID, err := db.CreateStyle("PART SYN-A-SINGLE", "", seeded.ProcessID)
	if err != nil {
		t.Fatalf("create single_robot style: %v", err)
	}
	seqStyleID, err := db.CreateStyle("PART SYN-A-SEQ", "", seeded.ProcessID)
	if err != nil {
		t.Fatalf("create sequential style: %v", err)
	}
	twoLanesStyleID, err := db.CreateStyle("PART SYN-A-TWO-LANES", "", seeded.ProcessID)
	if err != nil {
		t.Fatalf("create two_robot lanes style: %v", err)
	}
	mixedStyleID, err := db.CreateStyle("PART SYN-A-MIXED", "", seeded.ProcessID)
	if err != nil {
		t.Fatalf("create mixed style: %v", err)
	}
	claims := []processes.NodeClaimInput{
		// single_robot, WELD-2's shape: consume in, produce out, both parking
		// the new bin and clearing the old to the same two lanes.
		{StyleID: singleStyleID, CoreNodeName: "PLN_01", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapMode("single_robot"),
			PayloadCode:    "SYN-A-P002",
			InboundStaging: "PLN_02", OutboundStaging: "PLN_05",
			InboundSource: "Supermarket Empty Totes", OutboundDestination: "Supermarket Area"},
		{StyleID: singleStyleID, CoreNodeName: "PLN_04", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapMode("single_robot"),
			PayloadCode:    "SYN-A-P003",
			InboundStaging: "PLN_02", OutboundStaging: "PLN_05",
			InboundSource: "Supermarket Empty Totes", OutboundDestination: "Supermarket Area"},
		// sequential A/B: two produce claims, each pairing the other.
		{StyleID: seqStyleID, CoreNodeName: "PLN_03", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapMode("sequential"),
			PayloadCode: "SYN-A-P002", PairedCoreNode: "PLN_06",
			InboundSource: "Supermarket Empty Totes", OutboundDestination: "Supermarket Area"},
		{StyleID: seqStyleID, CoreNodeName: "PLN_06", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapMode("sequential"),
			PayloadCode: "SYN-A-P003", PairedCoreNode: "PLN_03",
			InboundSource: "Supermarket Empty Totes", OutboundDestination: "Supermarket Area"},
		// two_robot staging at lanes that are NOT positions of the cell, with
		// an outbound staging named as well: the module draws the inbound
		// lane as a slot, and Robot 2 takes the old bin straight to the dock,
		// so the named outbound lane is not part of its picture.
		{StyleID: twoLanesStyleID, CoreNodeName: "PLN_03", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapMode("two_robot"),
			PayloadCode:    "SYN-A-P002",
			InboundStaging: "SLN_010", OutboundStaging: "SLN_04",
			InboundSource: "Supermarket Empty Totes", OutboundDestination: "Supermarket Area"},
		// A MIXED FLOW: one single_robot cell and one two_robot cell, both
		// staging at lanes. Each cell's old bin leaves on its own
		// choreography's robot — Robot 1 for the single cell, Robot 2 for the
		// two-robot one — not on one robot chosen for the whole flow.
		{StyleID: mixedStyleID, CoreNodeName: "PLN_01", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapMode("single_robot"),
			PayloadCode:    "SYN-A-P002",
			InboundStaging: "SLN_011", OutboundStaging: "SLN_012",
			InboundSource: "Supermarket Empty Totes", OutboundDestination: "Supermarket Area"},
		{StyleID: mixedStyleID, CoreNodeName: "PLN_03", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapMode("two_robot"),
			PayloadCode:    "SYN-A-P003",
			InboundStaging: "SLN_010",
			InboundSource:  "Supermarket Empty Totes", OutboundDestination: "Supermarket Area"},
	}
	for _, in := range claims {
		if _, err := processes.UpsertClaim(db.DB, domain.CoreNodeKinds{}, in); err != nil {
			t.Fatalf("upsert claim %s on style %d: %v", in.CoreNodeName, in.StyleID, err)
		}
	}

	// The synthetic styles were created above; register them where write()
	// looks them up.
	seeded.Styles["PART SYN-A-SINGLE"] = singleStyleID
	seeded.Styles["PART SYN-A-SEQ"] = seqStyleID
	seeded.Styles["PART SYN-A-TWO-LANES"] = twoLanesStyleID
	seeded.Styles["PART SYN-A-MIXED"] = mixedStyleID

	dir := t.TempDir()
	write := func(name, styleName string) string {
		t.Helper()
		styleID, ok := seeded.Styles[styleName]
		if !ok {
			t.Fatalf("style %q not seeded", styleName)
		}
		if err := db.SetActiveStyle(seeded.ProcessID, &styleID); err != nil {
			t.Fatalf("set active style %s: %v", styleName, err)
		}
		svc.SetSceneGeometryResolver(func() *domain.SceneGeometry { return geom })
		view, err := svc.BuildView(context.Background(), stationID)
		if err != nil {
			t.Fatalf("BuildView %s: %v", name, err)
		}
		pic, err := svc.CellPictureForStation(stationID)
		if err != nil {
			t.Fatalf("CellPictureForStation %s: %v", name, err)
		}
		raw, err := json.Marshal(struct {
			*domain.OperatorStationView
			Cell *domain.CellPicture `json:"cell,omitempty"`
		}{OperatorStationView: view, Cell: pic})
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		p := filepath.Join(dir, name+".json")
		if err := os.WriteFile(p, raw, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	files := []string{
		write("mode-index-pair", "PART SYN-A-S003"), // two_robot_press_index, two rows
		write("mode-two-robot", "PART SYN-A-S007"),  // two_robot, two staging moves
		write("mode-single-robot", "PART SYN-A-SINGLE"),
		write("mode-sequential", "PART SYN-A-SEQ"),
		write("mode-two-robot-lanes", "PART SYN-A-TWO-LANES"), // two_robot, staging at lanes
		write("mode-mixed", "PART SYN-A-MIXED"),               // single_robot beside two_robot
	}

	script := filepath.Join("static", "operator-station", "operator-flow.modes.test.js")
	args := append([]string{script}, files...)
	out, err := exec.Command(nodePath, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("per-mode characterisation failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("per-mode: %s", out)
}
