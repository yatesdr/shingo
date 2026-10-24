package www

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
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
	indexStagedStyleID, err := db.CreateStyle("PART SYN-A-INDEX-STAGED", "", seeded.ProcessID)
	if err != nil {
		t.Fatalf("create staged press index style: %v", err)
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
		// A press index that also names an inbound staging lane: the tooling
		// a changeover keeps staged. No running leg reaches it, but the claim
		// names it, so the module has to draw it.
		{StyleID: indexStagedStyleID, CoreNodeName: "PLN_01", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapMode("two_robot_press_index"),
			PayloadCode: "SYN-A-P002", PairedCoreNode: "PLN_02", InboundStaging: "SLN_013",
			InboundSource: "Supermarket Empty Totes", OutboundDestination: "Supermarket Area"},
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
	seeded.Styles["PART SYN-A-INDEX-STAGED"] = indexStagedStyleID

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
		write("mode-index-staged", "PART SYN-A-INDEX-STAGED"), // two_robot_press_index naming an inbound staging
	}

	script := filepath.Join("static", "operator-station", "operator-flow.modes.test.js")
	args := append([]string{script}, files...)
	out, err := exec.Command(nodePath, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("per-mode characterisation failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("per-mode: %s", out)
}

var updateFlowPictureGolden = flag.Bool("update-flow-picture-golden", false,
	"rewrite testdata/flow-picture/*.svg from the current renderer")

// TestFlowPicture_PlantAGolden pins the markup renderFlowPicture draws for
// every station of plant A, two ways: the board's flow panel (read-only, the
// running style, 1280 wide — syncFlowPanel's options) and the desktop's
// picture of the running style (editable, the desktop's 1084 frame, the
// model the desktop's initModel builds over the composer read).
//
// A WHOLE-MARKUP PIN, and its job is to say what moved. The recipe-groups
// tranche draws a changeover cell by relation and leaves every other cell on
// the module picture, so this golden changes where that tranche says it does
// and nowhere else; the order fix on the module picture changes it again,
// named in its own commit.
//
// Regenerate with
//
//	go test ./www -run TestFlowPicture_PlantAGolden -update-flow-picture-golden
//
// and read the diff: a regenerated golden pins nothing unless someone read it.
func TestFlowPicture_PlantAGolden(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the picture golden")
	}
	fx := scenefixtures.A()
	db := testdb.Open(t)
	svc := service.NewStationService(db)
	svc.SetBinTypeResolver(testdb.BinTypeOf(fx))
	svc.SetCoreNodeGroupResolver(func() map[string][]string { return testdb.GroupsOf(fx) })
	geom := testdb.SceneGeometryOf(fx)
	svc.SetSceneGeometryResolver(func() *domain.SceneGeometry { return geom })

	dir := t.TempDir()
	type input struct {
		Name    string `json:"name"`
		Board   string `json:"board"`
		Desktop string `json:"desktop"`
		Active  int64  `json:"active"`
	}
	var inputs []input
	for _, proc := range fx.Processes {
		seeded := testdb.SeedPlant(t, db, fx, proc.Name)
		testdb.SeedRoutingSet(t, db, seeded.ProcessID, "golden")
		process, err := db.GetProcess(seeded.ProcessID)
		if err != nil || process == nil {
			t.Fatalf("process %s: %v", proc.Name, err)
		}
		var active int64
		if process.ActiveStyleID != nil {
			active = *process.ActiveStyleID
		}
		desktop, err := svc.ComposerForProcess(seeded.ProcessID)
		if err != nil {
			t.Fatalf("ComposerForProcess %s: %v", proc.Name, err)
		}
		codes := make([]string, 0, len(seeded.Stations))
		for code := range seeded.Stations {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		for _, code := range codes {
			pic, err := svc.CellPictureForStation(seeded.Stations[code])
			if err != nil {
				t.Fatalf("CellPictureForStation %s: %v", code, err)
			}
			name := "a-" + code
			write := func(suffix string, v any) string {
				raw, err := json.Marshal(v)
				if err != nil {
					t.Fatalf("marshal %s: %v", name, err)
				}
				p := filepath.Join(dir, name+suffix+".json")
				if err := os.WriteFile(p, raw, 0o644); err != nil {
					t.Fatalf("write %s: %v", p, err)
				}
				return p
			}
			inputs = append(inputs, input{Name: name, Board: write("-board", map[string]any{"cell": pic}),
				Desktop: write("-desktop", desktop), Active: active})
		}
	}
	list, err := json.Marshal(inputs)
	if err != nil {
		t.Fatalf("marshal inputs: %v", err)
	}
	listPath := filepath.Join(dir, "inputs.json")
	if err := os.WriteFile(listPath, list, 0o644); err != nil {
		t.Fatalf("write inputs: %v", err)
	}
	abs := func(parts ...string) string {
		p, err := filepath.Abs(filepath.Join(parts...))
		if err != nil {
			t.Fatalf("resolve %v: %v", parts, err)
		}
		return filepath.ToSlash(p)
	}
	st := abs("static", "operator-station")
	harness := fmt.Sprintf(`
'use strict';
const fs = require('fs');
const vm = require('vm');
const ctx = { console, Math, Set, Map, Number, isFinite, JSON, Object, Array, String,
  document: { getElementById() { return null; } }, window: { location: { hash: '' } },
  el() { return {}; }, getView() { return null; },
  esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); } };
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(%q, 'utf8'), ctx);
vm.runInContext(fs.readFileSync(%q, 'utf8'), ctx);
vm.runInContext(fs.readFileSync(%q, 'utf8').replace(/^export /mg, ''), ctx);
const flow = fs.readFileSync(%q, 'utf8').replace(/^import[^;]+;\s*/mg, '').replace(/^export /mg, '');
vm.runInContext(flow + '\n__out = { renderFlowPicture, sentencesFromModel, relationPlacement };', ctx);
const F = ctx.__out, M = ctx.window.ComposerModel;
const out = {};
for (const inp of JSON.parse(fs.readFileSync(%q, 'utf8'))) {
  const board = JSON.parse(fs.readFileSync(inp.board, 'utf8'));
  const bo = { frame: { w: 1280 }, usedOnly: true };
  out[inp.name + '-board'] = F.renderFlowPicture(board, bo) + '\n<!-- height ' + bo.height + ' -->\n';
  const comp = JSON.parse(fs.readFileSync(inp.desktop, 'utf8'));
  const st = (comp.styles || []).find(s => s.id === inp.active);
  if (!st) continue;
  const cell = comp.cell || { positions: [] };
  const model = M.init({
    styleId: st.id, styleName: st.name,
    positions: (cell.positions || []).map(p => ({ core_node_name: p.core_node_name, kind: p.kind, sequence: p.sequence })),
    routing: comp.routing || [], routingOff: comp.routing_off || {}, scene: comp.map || null,
    claims: st.claims || [], parts: [], palette: comp.palette || [], catid: st.catid || '',
    lastRun: st.last_run || null, flowspec: ctx.window.FLOWSPEC, groups: cell.groups || {},
    processClaims: (comp.styles || []).reduce((all, x) => all.concat(x.claims || []), []),
    advanced: st.advanced || {},
  });
  const pic = M.pictureCells(model, cell);
  const dob = { selected: null, findings: {}, editable: true, frame: { w: 1084 } };
  dob.sentences = F.sentencesFromModel(model, !!F.relationPlacement(pic, dob));
  out[inp.name + '-desktop'] = F.renderFlowPicture({ cell: pic }, dob) + '\n<!-- height ' + dob.height + ' -->\n';
}
process.stdout.write(JSON.stringify(out));
`, st+"/flowspec-data.js", st+"/composer-model.js", abs("..", "..", "shared", "scene-geom.js"),
		st+"/operator-flow.js", filepath.ToSlash(listPath))
	harnessPath := filepath.Join(dir, "golden.js")
	if err := os.WriteFile(harnessPath, []byte(harness), 0o644); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	raw, err := exec.Command(nodePath, harnessPath).Output()
	if err != nil {
		var stderr []byte
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}
		t.Fatalf("render the goldens: %v\n%s", err, stderr)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode the renders: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("rendered no picture; the harness has drifted")
	}
	goldDir := filepath.Join("testdata", "flow-picture")
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		path := filepath.Join(goldDir, n+".svg")
		if *updateFlowPictureGolden {
			if err := os.MkdirAll(goldDir, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", goldDir, err)
			}
			if err := os.WriteFile(path, []byte(got[n]), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: no golden (%v); regenerate with -update-flow-picture-golden and read it", n, err)
			continue
		}
		if string(want) != got[n] {
			t.Errorf("%s: the picture's markup moved from %s; regenerate only for a named change", n, path)
		}
	}
	t.Logf("pictures pinned: %s", strings.Join(names, ", "))
}
