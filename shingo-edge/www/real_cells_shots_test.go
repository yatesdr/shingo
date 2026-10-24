//go:build shots

package www

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
	"shingoedge/service"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// real_cells_shots_test.go — the cell picture photographed from the REAL
// cells of the plants, with the overlap check measured on every shot. A
// handoff gate, like composer_shots_test.go, not CI.
//
// THE DATA NEVER ENTERS THE REPO. The cells are read from the pulls named in
// SPOTS_CELLS ("spr=<path>;hk=<path>", the PLANTCHECK pattern), each pull's
// `cells` keyed by station id with every entry a CellPicture as the Edge's
// GET /api/operator-stations/{id}/cell answered it at the plant. No
// coordinate, node list or claim of a plant is written here, and the PNGs and
// INDEX.md go to SPOTS_SHOTS_OUT, which scripts/real-cell-shots.sh refuses to
// put inside the repo.
//
// ONE FRESH EDGE PER STATION, seeded through the store's own write paths from
// that station's picture (seedRealCell), served by the real engine, router and
// templates, and photographed by headless Chrome:
//
//	<plant>-<station>-relation-1280.png   the relation picture, nothing selected, the HMI's frame
//	<plant>-<station>-relation-1084.png   the same at the desktop's frame
//	<plant>-<station>-today-1280.png      the board's picture, for a cell the scope gate leaves on it
//
// and for the review set (the owner's order: Springfield 18, Hopkinsville 9,
// Springfield 6), through the real station page at 1280x800 and the desktop
// at 1440x900:
//
//	<plant>-<station>-spots-1280.png          the Spots step, nothing selected
//	<plant>-<station>-spots-<node>-1280.png   each place of the card selected in turn
//	<plant>-<station>-flow-1280.png           the board's flow panel
//	<plant>-<station>-draft-1440.png          the desktop recipe draft (the grouped recipe only)
//
// EVERY PNG IS MEASURED. A driver page (realCellDriver, or shotFrameDriver
// around the real page) waits for the picture and runs shotProbe over its SVG
// in the browser — cards that overlap, a card's text or mark outside its card,
// text over text, the picture's width against its frame — and writes the
// answer into its own DOM for --dump-dom. INDEX.md gives every PNG its line.
//
// Run it through scripts/real-cell-shots.sh, which sets:
//
//	SPOTS_CELLS            plant=path;plant=path — the pulls
//	SPOTS_SHOTS_OUT        the folder for the PNGs and INDEX.md (created)
//	SPOTS_SHOTS_TREE       the commit photographed, for INDEX.md (optional)
//	COMPOSER_SHOTS_CHROME  the Chrome binary, when chromeBinary finds none
func TestRealCellShots(t *testing.T) {
	spec, out := os.Getenv("SPOTS_CELLS"), os.Getenv("SPOTS_SHOTS_OUT")
	if spec == "" || out == "" {
		t.Skip("SPOTS_CELLS and SPOTS_SHOTS_OUT not set; run scripts/real-cell-shots.sh")
	}
	chrome := chromeBinary()
	if chrome == "" {
		t.Fatal("no Chrome binary: set COMPOSER_SHOTS_CHROME")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", out, err)
	}
	checkShotProbe(t, chrome, out)
	pulls := readCellPulls(t, spec)
	idx := shotIndex{Tree: os.Getenv("SPOTS_SHOTS_TREE"), Pulls: pulls}
	for _, c := range reviewFirst(pulls) {
		// A cell gets a recipe when it changes over or is Press 6 (seedRealCell),
		// and the scope gate keeps today's picture for a cell with neither.
		row := cellRow{cell: c, modes: c.modes(), grouped: c.hasAll(press6Nodes)}
		row.recipe = row.grouped || c.changeover()
		row.gateRelation = row.recipe
		t.Run(c.plant+"-"+strconv.FormatInt(c.station, 10), func(t *testing.T) {
			shootRealCell(t, chrome, out, c, &row)
		})
		idx.Rows = append(idx.Rows, row)
	}
	idx.Stale = staleShots(t, out, idx.Rows)
	if err := os.WriteFile(filepath.Join(out, "INDEX.md"), []byte(idx.markdown()), 0o644); err != nil {
		t.Fatalf("write INDEX.md: %v", err)
	}
	t.Logf("wrote %s", filepath.Join(out, "INDEX.md"))
}

// staleShots is the PNGs in out that no row of this run wrote (nor the
// other files named).
func staleShots(t *testing.T, out string, rows []cellRow, also ...string) []string {
	t.Helper()
	wrote := map[string]bool{}
	for _, f := range also {
		wrote[f] = true
	}
	for _, r := range rows {
		for _, s := range r.shots {
			wrote[s.File] = true
		}
	}
	all, err := filepath.Glob(filepath.Join(out, "*.png"))
	must(t, err, "list "+out)
	var stale []string
	for _, f := range all {
		if !wrote[filepath.Base(f)] {
			stale = append(stale, filepath.Base(f))
		}
	}
	if len(stale) > 0 {
		t.Logf("%d PNG(s) in %s are not from this run: %v", len(stale), out, stale)
	}
	return stale
}

// ── the pulls ──────────────────────────────────────────────────────────────

// cellPull is one plant's file: its key (spr, hk) and every station's cell.
type cellPull struct {
	Plant string
	Path  string
	Cells map[int64]domain.CellPicture
	// StationIDs says whether the build that answered carried
	// CellPosition.station_id at all (it landed on main 2026-10-21; an older
	// Edge sends none). Without it a missing id is "unknown", not "stationless".
	StationIDs bool
}

// realCell is one station of one pull.
type realCell struct {
	plant   string
	station int64
	pic     domain.CellPicture
	pull    *cellPull
	// code is the SYN- part prefix; empty reads as the plant's letter and the
	// station ("S18").
	code string
}

// plantNames are the names the generic processes take: "Springfield 18".
var plantNames = map[string]string{"spr": "Springfield", "hk": "Hopkinsville"}

func plantName(key string) string {
	if n, ok := plantNames[key]; ok {
		return n
	}
	return key
}

// reviewSet is the owner's order (2026-10-24): Press 6, the 4x2 weld cell, a
// nutfarm. Every other station follows by plant, in SPOTS_CELLS order, and id.
var reviewSet = []struct {
	plant   string
	station int64
}{{"spr", 18}, {"hk", 9}, {"spr", 6}}

func inReview(c realCell) bool {
	for _, r := range reviewSet {
		if r.plant == c.plant && r.station == c.station {
			return true
		}
	}
	return false
}

func readCellPulls(t *testing.T, spec string) []*cellPull {
	t.Helper()
	var pulls []*cellPull
	for _, part := range strings.Split(spec, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, path, ok := strings.Cut(part, "=")
		if !ok || key == "" || path == "" {
			t.Fatalf("SPOTS_CELLS: %q is not plant=path", part)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("SPOTS_CELLS %s: %v", key, err)
		}
		var file struct {
			Cells map[string]domain.CellPicture `json:"cells"`
		}
		if err := json.Unmarshal(raw, &file); err != nil {
			t.Fatalf("SPOTS_CELLS %s: %v", key, err)
		}
		p := &cellPull{Plant: key, Path: path, Cells: map[int64]domain.CellPicture{}}
		for id, pic := range file.Cells {
			n, err := strconv.ParseInt(id, 10, 64)
			if err != nil {
				t.Fatalf("SPOTS_CELLS %s: cell key %q is not a station id", key, id)
			}
			p.Cells[n] = pic
			for _, pos := range pic.Positions {
				if pos.StationID != 0 {
					p.StationIDs = true
				}
			}
		}
		pulls = append(pulls, p)
	}
	if len(pulls) == 0 {
		t.Fatal("SPOTS_CELLS names no pull")
	}
	return pulls
}

// reviewFirst is every station of every pull, the review set first.
func reviewFirst(pulls []*cellPull) []realCell {
	byKey := map[string]*cellPull{}
	for _, p := range pulls {
		byKey[p.Plant] = p
	}
	var out []realCell
	for _, r := range reviewSet {
		if p, ok := byKey[r.plant]; ok {
			if pic, ok := p.Cells[r.station]; ok {
				out = append(out, realCell{plant: r.plant, station: r.station, pic: pic, pull: p})
			}
		}
	}
	for _, p := range pulls {
		ids := make([]int64, 0, len(p.Cells))
		for id := range p.Cells {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			c := realCell{plant: p.Plant, station: id, pic: p.Cells[id], pull: p}
			if !inReview(c) {
				out = append(out, c)
			}
		}
	}
	return out
}

// ── the seed ───────────────────────────────────────────────────────────────

// press6Recipe is the recipe SYNTH-round2 §2 writes out for Springfield's
// Press 6 after the engineer's re-save: its two cells and the two groups,
// node names only. The `groups` key is stored as the rollback pin stores it
// (TestFlowMint_ARecipeCarryingGroupsMintsItsCells): the flow_json as written.
// A tree before recipe groups ignores the key and reads the cells.
const press6Recipe = `{"cells":[
 {"core_node_name":"PLN_006","role":"produce","swap_mode":"two_robot_press_index","paired_core_node":"PLN_005","inbound_source":"Press Buffer Zone","outbound_destination":"ULN_002"},
 {"core_node_name":"PLN_007","role":"produce","swap_mode":"two_robot_press_index","paired_core_node":"PLN_008","inbound_source":"Press Buffer Zone","outbound_destination":"ULN_005"}],
"groups":[
 {"of":"PLN_006","name":"Left Hand","spots":[
  {"core_node_name":"PLN_006","paired_core_node":["PLN_005"]},
  {"core_node_name":"PLN_010","paired_core_node":["PLN_005"]}]},
 {"of":"PLN_007","name":"Right Hand","spots":[
  {"core_node_name":"PLN_007","paired_core_node":["PLN_008"]},
  {"core_node_name":"PLN_012","paired_core_node":["PLN_008"]}],
  "advanced":{"reorder_point":1,"auto_reorder":true}}]}`

// press6RecipeName is the recipe's name at the plant (the deploy note's).
const press6RecipeName = "Two spot press index"

// press6Nodes are the nodes the recipe and its groups name on the press: the
// grouped recipe goes on the cell that has every one of them as a position,
// which is Press 6 and only Press 6. A cell with the shape gets it; nothing
// else about the cell has to match.
var press6Nodes = []string{"PLN_005", "PLN_006", "PLN_007", "PLN_008", "PLN_010", "PLN_012"}

// fromRunningRecipeName is the recipe every other changeover cell gets.
const fromRunningRecipeName = "Saved from the running style"

// seededCell is one station's Edge, up and serving.
type seededCell struct {
	srv       *httptest.Server
	processID int64
	stationID int64
	// recipeID is the first recipe seeded, recipes every one by name.
	recipeID int64
	recipes  map[string]int64
	grouped  bool
	notes    []string
}

// seedOpts is what a fixture states that a real cell takes from its pull.
// The zero value is the real cells' seed.
type seedOpts struct {
	// name is a station's name by its id in the picture's terms, and the
	// process takes the cell's station's; nil is "<Plant> <id>".
	name func(id int64) string
	// role is a claim's role; nil is claimRole (off the mode).
	role func(node, mode string) protocol.ClaimRole
	// extra are positions of the process that this station's picture does not
	// draw: another station's, bound by their StationID.
	extra []domain.CellPosition
	// recipes replace the real cells' rule (Press 6's grouped recipe by its
	// shape, else one saved from the running style); nil keeps the rule.
	recipes []recipeSeed
	// offers are staging lanes the routing set offers beyond the claims' own.
	offers []string
	// parts is the process's part set.
	parts []string
}

// recipeSeed is one recipe to store: its flow_json as written (json), or a
// shape built from cells the way the desktop's Save builds one, or the
// running style's shape (fromStyle). stamp points the running claims'
// provenance at it, which is what gives the card its pre-fill.
type recipeSeed struct {
	name      string
	json      string
	cells     []domain.FlowCell
	fromStyle bool
	stamp     bool
}

// changeoverMode is a swap that changes over (SYNTH-round2 row 17): exactly
// the modes a claim may persist. A loader's manual_swap and the retired simple
// are not among them.
func changeoverMode(mode string) bool {
	return slices.Contains(protocol.ConfigurableSwapModes(), protocol.SwapMode(mode))
}

// claimRole is the role a claim is written with. THE PICTURE CARRIES NONE
// (CellClaim has no role), so it is read off the mode: a 2-Robot Index claim
// is a press making parts, and every other claim consumes — which is what
// Hopkinsville's weld cell runs (its composer read is the one pull that
// carries roles). The board's picture never reads a role; the Spots step's
// card words and part count do.
func claimRole(mode string) protocol.ClaimRole {
	if protocol.SwapMode(mode) == protocol.SwapModeTwoRobotPressIndex {
		return protocol.ClaimRoleProduce
	}
	return protocol.ClaimRoleConsume
}

// synCode is a running claim's part: SYN-S18-01. Never a plant part number.
func synCode(c realCell, suffix string) string {
	if c.code != "" {
		return "SYN-" + c.code + "-" + suffix
	}
	return fmt.Sprintf("SYN-%s%d-%s", strings.ToUpper(c.plant[:1]), c.station, suffix)
}

// seedRealCell writes one station's picture into a fresh Edge database
// through the store's own write paths and serves it:
//
//   - a process and its station, both named "<Plant> <station>";
//   - the positions with their station binding — bound to this station, to
//     another (a second station of the same process), or to none
//     (stationless). A pull whose build sent no station_id binds a position of
//     Kind "back" to none and the rest to this station: they were drawn on
//     this station's picture, and a back slot is ordinarily stationless;
//   - the running claims as the active style, each with a SYN- part;
//   - a claim the picture names but does not draw (a deck whose press spot
//     is another station's, as on an unloader beside its press) is taken from
//     the cell of the same pull that draws it, with its position, so the deck
//     is drawn as the plant drew it;
//   - the routing set from the claims (testdb.SeedRoutingSet: sources,
//     destinations, staging lanes);
//   - the scene from the positions' and lanes' x/y as GeneralLocation points,
//     and the dock groups as Core's node list carries them;
//   - a recipe where the cell runs a changeover: Press 6's grouped recipe with
//     the running claims' provenance on it (so the card's pre-fill opens where
//     the plant runs), and elsewhere a recipe saved from the running style.
func seedRealCell(t *testing.T, c realCell, opts seedOpts, mount func(mux *http.ServeMux)) seededCell {
	t.Helper()
	db := testdb.Open(t)
	nameOf := opts.name
	if nameOf == nil {
		nameOf = func(id int64) string { return plantName(c.plant) + " " + strconv.FormatInt(id, 10) }
	}
	roleOf := opts.role
	if roleOf == nil {
		roleOf = func(_, mode string) protocol.ClaimRole { return claimRole(mode) }
	}
	name := nameOf(c.station)
	var sc seededCell
	var err error
	sc.processID, err = db.CreateProcess(name, "shots: "+c.plant+" station "+strconv.FormatInt(c.station, 10), "", "", false)
	must(t, err, "process")
	stationOf := map[int64]int64{}
	station := func(id int64) int64 {
		if s, ok := stationOf[id]; ok {
			return s
		}
		code := c.plant + "-" + strconv.FormatInt(id, 10)
		s, err := db.CreateOperatorStation(stations.Input{ProcessID: sc.processID, Code: code,
			Name: nameOf(id), Sequence: len(stationOf) + 1, Enabled: true})
		must(t, err, "station "+code)
		stationOf[id] = s
		return s
	}
	sc.stationID = station(c.station)
	// owner is the station whose picture drew the position.
	binding := func(p domain.CellPosition, owner int64) *int64 {
		if !c.pull.StationIDs {
			if p.Kind == "back" {
				return nil
			}
			s := station(owner)
			return &s
		}
		if p.StationID == 0 {
			return nil
		}
		s := station(p.StationID)
		return &s
	}
	ownerOf := map[string]int64{}

	// The positions, then the press spots of decks drawn here whose claims
	// live on another station's cell.
	positions := append([]domain.CellPosition(nil), c.pic.Positions...)
	drawn := map[string]bool{}
	seq := 0
	for _, p := range positions {
		drawn[p.CoreNodeName] = true
		seq = max(seq, p.Sequence)
	}
	for _, p := range c.pic.Positions {
		if p.PartnerOf == "" || drawn[p.PartnerOf] {
			continue
		}
		head, from, ok := claimedElsewhere(c, p.PartnerOf)
		if !ok {
			sc.notes = append(sc.notes, fmt.Sprintf("%s is drawn as %s for %s, and no cell of the pull carries %s's claim; drawn without it",
				p.CoreNodeName, p.PartnerKind, p.PartnerOf, p.PartnerOf))
			continue
		}
		seq++
		head.Sequence = seq
		positions = append(positions, head)
		drawn[head.CoreNodeName] = true
		ownerOf[head.CoreNodeName] = from
		sc.notes = append(sc.notes, fmt.Sprintf("%s's claim (%s, bound to station %d) is seeded from station %d's cell: %s is drawn here as its %s",
			head.CoreNodeName, domain.SwapModeWord(protocol.SwapMode(head.Claim.SwapMode)), head.StationID, from, p.CoreNodeName, p.PartnerKind))
	}
	for _, p := range opts.extra {
		positions = append(positions, p)
		ownerOf[p.CoreNodeName] = p.StationID
	}
	for _, p := range positions {
		owner, ok := ownerOf[p.CoreNodeName]
		if !ok {
			owner = c.station
		}
		_, err := db.CreateProcessNode(processes.NodeInput{ProcessID: sc.processID, OperatorStationID: binding(p, owner),
			CoreNodeName: p.CoreNodeName, Code: p.CoreNodeName, Name: p.CoreNodeName, Sequence: p.Sequence, Enabled: true})
		must(t, err, "position "+p.CoreNodeName)
	}

	// The running style.
	style, err := db.CreateStyle(synCode(c, "RUN"), "", sc.processID)
	must(t, err, "style")
	n := 0
	for _, p := range positions {
		cl := p.Claim
		if cl == nil {
			continue
		}
		n++
		_, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: style, CoreNodeName: p.CoreNodeName, Role: roleOf(p.CoreNodeName, cl.SwapMode),
			SwapMode: protocol.SwapMode(cl.SwapMode), PayloadCode: synCode(c, fmt.Sprintf("%02d", n)),
			PairedCoreNode: cl.PairedCoreNode, SecondPairedCoreNode: cl.SecondPairedCoreNode,
			InboundStaging: cl.InboundStaging, OutboundStaging: cl.OutboundStaging,
			InboundSource: cl.InboundSource, OutboundDestination: cl.OutboundDestination,
		})
		must(t, err, "claim "+p.CoreNodeName)
	}
	must(t, db.SetActiveStyle(sc.processID, &style), "run the style")
	testdb.SeedRoutingSet(t, db, sc.processID, "shots")
	for i, lane := range opts.offers {
		_, err := db.UpsertRoutingNode(domain.CoreNodeKinds{}, domain.RoutingNodeInput{ProcessID: sc.processID,
			CoreNodeName: lane, Role: domain.RoutingRoleStaging, Enabled: true, Sequence: 100 + i})
		must(t, err, "offered lane "+lane)
	}
	if len(opts.parts) > 0 {
		must(t, db.ReplaceProcessPayloads(sc.processID, opts.parts), "the part set")
	}

	// The recipes.
	recipes := opts.recipes
	if recipes == nil {
		switch {
		case c.hasAll(press6Nodes):
			recipes = []recipeSeed{{name: press6RecipeName, json: press6Recipe, stamp: true}}
		case c.changeover():
			recipes = []recipeSeed{{name: fromRunningRecipeName, fromStyle: true}}
		}
	}
	sc.recipes = map[string]int64{}
	for _, r := range recipes {
		var id int64
		if r.fromStyle {
			id, err = service.NewProcessService(db).CreateFromStyle(sc.processID, style, r.name, "shots")
		} else {
			flow := r.json
			if flow == "" {
				flow, err = domain.PresetShapeJSON(r.cells)
				must(t, err, "the shape of "+r.name)
			}
			id, err = db.CreateFlowPreset(processes.FlowPresetInput{ProcessID: sc.processID,
				Name: r.name, FlowJSON: flow, CreatedBy: "shots"})
		}
		must(t, err, "recipe "+r.name)
		if r.stamp {
			p, err := db.GetFlowPreset(id)
			must(t, err, "read "+r.name)
			_, err = db.StampClaimPresetProvenance(style, id, p.Version)
			must(t, err, "the running claims' provenance on "+r.name)
		}
		sc.recipes[r.name] = id
		if sc.recipeID == 0 {
			sc.recipeID = id
		}
		sc.grouped = sc.grouped || strings.Contains(r.json, `"groups"`)
	}
	if sc.recipeID != 0 {
		must(t, db.SetFlowComposerEnabled(sc.processID, true), "HMI setup on")
	}

	// The engine, as plant A's harness starts it: Core heard from, its node
	// list and its scene delivered.
	cfg := config.Defaults()
	cfg.StationUID = "shots." + c.plant + strconv.FormatInt(c.station, 10)
	cfg.Namespace = "shots"
	cfg.LineID = c.plant + strconv.FormatInt(c.station, 10)
	cfg.Messaging.StationID = cfg.StationUID
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: func(string, ...any) {}})
	eng.SetPayloadBinTypes(nil)
	eng.Start()
	t.Cleanup(eng.Stop)
	eng.SetCoreNodes(coreNodesOf(c, positions))
	if points, edges := sceneOf(c, positions); len(points) > 0 {
		eng.SetSceneGeometry("real-"+c.plant, points, edges)
	}
	sc.srv = shotsServer(t, shotsRouter(t, eng), mount)
	return sc
}

// modes are the swap modes the cell's own positions run, in picture order.
func (c realCell) modes() []string {
	var out []string
	for _, p := range c.pic.Positions {
		if p.Claim != nil && c.owns(p.CoreNodeName) && !slices.Contains(out, p.Claim.SwapMode) {
			out = append(out, p.Claim.SwapMode)
		}
	}
	return out
}

// changeover is the scope gate's question (SYNTH-round2 row 17): does a
// position of this station run a swap that changes over? Such a cell gets a
// recipe here, so "or an enabled recipe" adds nothing to it.
func (c realCell) changeover() bool {
	for _, m := range c.modes() {
		if changeoverMode(m) {
			return true
		}
	}
	return false
}

// owns says whether a position is this station's own: bound to it, bound to
// none, or (from a build that sent no binding) drawn on its picture.
func (c realCell) owns(node string) bool {
	for _, p := range c.pic.Positions {
		if p.CoreNodeName == node {
			return !c.pull.StationIDs || p.StationID == 0 || p.StationID == c.station
		}
	}
	return false
}

func (c realCell) hasAll(nodes []string) bool {
	for _, n := range nodes {
		found := false
		for _, p := range c.pic.Positions {
			found = found || p.CoreNodeName == n
		}
		if !found {
			return false
		}
	}
	return true
}

// claimedElsewhere finds the position `node` with its claim on another cell
// of the same pull, and that cell's station.
func claimedElsewhere(c realCell, node string) (domain.CellPosition, int64, bool) {
	ids := make([]int64, 0, len(c.pull.Cells))
	for id := range c.pull.Cells {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if id == c.station {
			continue
		}
		for _, p := range c.pull.Cells[id].Positions {
			if p.CoreNodeName == node && p.Claim != nil {
				return p, id, true
			}
		}
	}
	return domain.CellPosition{}, 0, false
}

// coreNodesOf is Core's node list as the picture needs it: every name the
// cell draws or routes to, and each dock group's members as "Group.Child".
func coreNodesOf(c realCell, positions []domain.CellPosition) []protocol.NodeInfo {
	seen := map[string]bool{}
	var out []protocol.NodeInfo
	add := func(name, typ string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, protocol.NodeInfo{Name: name, NodeType: typ})
	}
	for _, p := range positions {
		add(p.CoreNodeName, "")
		if cl := p.Claim; cl != nil {
			for _, n := range []string{cl.PairedCoreNode, cl.SecondPairedCoreNode, cl.InboundStaging, cl.OutboundStaging} {
				add(n, "")
			}
			for _, n := range []string{cl.InboundSource, cl.OutboundDestination} {
				if _, group := c.pic.Groups[n]; !group {
					add(n, "")
				}
			}
		}
	}
	for _, s := range c.pic.Staging {
		add(s.CoreNodeName, "")
	}
	groups := make([]string, 0, len(c.pic.Groups))
	for g := range c.pic.Groups {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		add(g, "NGRP")
		for _, m := range c.pic.Groups[g] {
			add(g+"."+m, "")
		}
	}
	return out
}

// sceneOf is the scene cache from the cell's own coordinates: a
// GeneralLocation point per placed position and lane. ONE EDGE, zero length,
// at the first point: the cache refuses a scene without edges, and nothing
// the station draws reads them.
func sceneOf(c realCell, positions []domain.CellPosition) ([]protocol.ScenePointInfo, []protocol.SceneEdgeInfo) {
	var pts []protocol.ScenePointInfo
	add := func(name string, x, y *float64) {
		if x == nil || y == nil {
			return
		}
		px, py, dir := *x, *y, 0.0
		pts = append(pts, protocol.ScenePointInfo{InstanceName: name, ClassName: "GeneralLocation", PosX: &px, PosY: &py, Dir: &dir})
	}
	for _, p := range positions {
		add(p.CoreNodeName, p.X, p.Y)
	}
	for _, s := range c.pic.Staging {
		add(s.CoreNodeName, s.X, s.Y)
	}
	if len(pts) == 0 {
		return nil, nil
	}
	p0 := pts[0]
	return pts, []protocol.SceneEdgeInfo{{From: p0.InstanceName, To: p0.InstanceName,
		FromX: p0.PosX, FromY: p0.PosY, ToX: p0.PosX, ToY: p0.PosY}}
}

// fidelity compares the seeded Edge's picture of the station with the
// plant's. A different set of positions, or geometry where the plant had
// none, is a picture of some other cell, and fails. A caption difference
// (role, kind, partner) is a note in the index: an older build can caption a
// shared lane for another of its users (Hopkinsville runs one).
func fidelity(t *testing.T, c realCell, station int64, got domain.CellPicture) []string {
	t.Helper()
	names := func(ps []domain.CellPosition) []string {
		out := make([]string, 0, len(ps))
		for _, p := range ps {
			out = append(out, p.CoreNodeName)
		}
		sort.Strings(out)
		return out
	}
	if w, g := names(c.pic.Positions), names(got.Positions); !slices.Equal(w, g) {
		t.Errorf("the seeded cell draws %v; the plant's drew %v", g, w)
	}
	if got.Geometry != c.pic.Geometry {
		t.Errorf("the seeded cell's geometry is %v; the plant's was %v", got.Geometry, c.pic.Geometry)
	}
	var notes []string
	byName := map[string]domain.CellPosition{}
	for _, p := range got.Positions {
		byName[p.CoreNodeName] = p
	}
	for _, w := range c.pic.Positions {
		g, ok := byName[w.CoreNodeName]
		if !ok {
			continue
		}
		if g.Kind != w.Kind || g.Role != w.Role || g.PartnerOf != w.PartnerOf || g.PartnerKind != w.PartnerKind {
			notes = append(notes, fmt.Sprintf("%s reads %s/%s/%s/%s here and %s/%s/%s/%s at the plant (kind/role/partner of/as)",
				w.CoreNodeName, g.Kind, g.Role, g.PartnerOf, g.PartnerKind, w.Kind, w.Role, w.PartnerOf, w.PartnerKind))
		}
		where := func(p domain.CellPosition, own int64) string {
			switch p.StationID {
			case 0:
				return "no station"
			case own:
				return "this station"
			}
			return "another station"
		}
		if c.pull.StationIDs && where(g, station) != where(w, c.station) {
			t.Errorf("%s is bound to %s here and to %s at the plant", w.CoreNodeName, where(g, station), where(w, c.station))
		}
		if (g.Claim == nil) != (w.Claim == nil) || (g.Claim != nil && g.Claim.SwapMode != w.Claim.SwapMode) {
			t.Errorf("%s's claim is %+v here and %+v at the plant", w.CoreNodeName, g.Claim, w.Claim)
		}
	}
	lanes := map[string]domain.CellStaging{}
	for _, s := range got.Staging {
		lanes[s.CoreNodeName] = s
	}
	for _, w := range c.pic.Staging {
		g, ok := lanes[w.CoreNodeName]
		if !ok {
			t.Errorf("lane %s is not drawn here; the plant drew it", w.CoreNodeName)
			continue
		}
		if g.PartnerOf != w.PartnerOf || g.Field != w.Field {
			notes = append(notes, fmt.Sprintf("lane %s is captioned %s for %s here and %s for %s at the plant",
				w.CoreNodeName, g.Field, g.PartnerOf, w.Field, w.PartnerOf))
		}
	}
	if len(got.Staging) != len(c.pic.Staging) {
		t.Errorf("the seeded cell draws %d lanes; the plant's drew %d", len(got.Staging), len(c.pic.Staging))
	}
	keys := func(m map[string][]string) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if w, g := keys(c.pic.Groups), keys(got.Groups); !slices.Equal(w, g) {
		notes = append(notes, fmt.Sprintf("the dock groups are %v here and %v at the plant", g, w))
	}
	return notes
}

// ── the shots ──────────────────────────────────────────────────────────────

// composerCard is the slice of the station's composer read the shots drive.
type composerCard struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	LastMoves map[string]string `json:"last_moves"`
	Slots     []json.RawMessage `json:"slots"`
	Groups    []json.RawMessage `json:"groups"`
	Spots     []struct {
		Node string `json:"node"`
		Job  string `json:"job"`
		Of   string `json:"of"`
	} `json:"spots"`
}

// mountRealCellDriver mounts the relation driver; shotsServer mounts the
// frame driver for every shots test.
func mountRealCellDriver(mux *http.ServeMux) {
	mux.HandleFunc("/__shots/real-cell", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(realCellDriver(q.Get("station"), q.Get("w"), q.Get("mode"), q.Get("caption"))))
	})
}

func shootRealCell(t *testing.T, chrome, out string, c realCell, row *cellRow) {
	sc := seedRealCell(t, c, seedOpts{}, mountRealCellDriver)
	row.notes = sc.notes
	if sc.grouped != row.grouped || (sc.recipeID != 0) != row.recipe {
		t.Fatalf("seeded grouped=%v recipe=%d for a cell read as grouped=%v recipe=%v", sc.grouped, sc.recipeID, row.grouped, row.recipe)
	}

	var pic domain.CellPicture
	getJSON(t, sc.srv.URL+fmt.Sprintf("/api/operator-stations/%d/cell", sc.stationID), &pic)
	row.notes = append(row.notes, fidelity(t, c, sc.stationID, pic)...)
	name := plantName(c.plant) + " " + strconv.FormatInt(c.station, 10)

	// Every station: the relation picture at both frames, and today's where the
	// gate keeps it.
	specs := []shotSpec{
		relationShot(sc, name, "relation-1280", "relation", 1280, "relation picture, nothing selected, the HMI's frame (1280)",
			mockFor(sc.grouped, "relation-1280", "")),
		relationShot(sc, name, "relation-1084", "relation", 1084, "relation picture, nothing selected, the desktop's frame (1084)",
			mockFor(sc.grouped, "relation-1084", "")),
	}
	if !row.gateRelation {
		specs = append(specs, relationShot(sc, name, "today-1280", "today", 1280,
			"today's picture, the board's options (the scope gate keeps it here)", ""))
	}

	// The review set: the real station page and the desktop.
	if inReview(c) {
		if sc.recipeID == 0 {
			row.notes = append(row.notes, "no changeover claim, so no recipe and no Spots step")
		} else {
			card := composerCardOf(t, sc, sc.recipeID)
			hash, mv := spotsHash(card, slotParts(c, card))
			row.recipeName, row.lastMoves = card.Name, mv
			specs = append(specs, spotsShots(sc, card, hash, "", func(state, drawn string) string {
				return mockFor(sc.grouped, state, drawn)
			})...)
		}
		specs = append(specs, flowShot(sc, mockFor(sc.grouped, "flow-1280", "")))
		if sc.grouped {
			specs = append(specs, draftShot(sc, press6RecipeName, "draft-1440", "", "",
				"desktop recipe draft, no focus", mockFor(true, "draft-1440", ""), ""))
		}
	}
	cr := chromeRun{t: t, chrome: chrome, out: out}
	base := fmt.Sprintf("%s-%d-", c.plant, c.station)
	for _, s := range specs {
		row.shots = append(row.shots, cr.measured(sc.srv.URL, base, s))
	}
	checkStill(t, row.shots)
}

// shotSpec is one PNG: its state (the file name after the cell's prefix),
// what it shows, the mock's matching state, and the driver URL that draws
// and measures it.
type shotSpec struct {
	state, what, mock string
	url               string
	w, h              int // h 0: the window is sized to the measured picture
	// todo names the lane a state waits on. Unreached, the PNG is
	// <state>-TODO.png, INDEX.md says what it shows instead, and the run does
	// not fail on it.
	todo string
	// still names the Spots step a state is one of: its first card stands at
	// one y in every state of that step (checkStill).
	still string
}

// relationShot draws the station's picture through realCellDriver.
func relationShot(sc seededCell, name, state, mode string, w int, what, mock string) shotSpec {
	u := fmt.Sprintf("/__shots/real-cell?station=%d&w=%d&mode=%s&caption=%s", sc.stationID, w, mode,
		url.QueryEscape(name+" · "+what))
	return shotSpec{state: state, what: what, mock: mock, url: u, w: w}
}

// pageShot is a real page through shotFrameDriver. A state another lane
// builds (todo) is optional to the driver: unreached, it measures what the
// page draws instead.
func pageShot(f frame, state, what, mock, todo string) shotSpec {
	f.optional = todo != ""
	return shotSpec{state: state, what: what, mock: mock, url: f.url(), w: f.w, h: f.h, todo: todo}
}

// composerCardOf is the station's composer read's card for a recipe.
func composerCardOf(t *testing.T, sc seededCell, recipe int64) composerCard {
	t.Helper()
	var comp struct {
		Presets []composerCard `json:"presets"`
	}
	getJSON(t, sc.srv.URL+fmt.Sprintf("/api/operator-stations/%d/composer", sc.stationID), &comp)
	for _, p := range comp.Presets {
		if p.ID == strconv.FormatInt(recipe, 10) {
			return p
		}
	}
	t.Fatalf("the composer read carries no card for recipe %d", recipe)
	return composerCard{}
}

// slotParts is a SYN- part per slot of the card: SYN-S18-A, -B, …
func slotParts(c realCell, card composerCard) []string {
	var parts []string
	for i := range card.Slots {
		parts = append(parts, synCode(c, string(rune('A'+i))))
	}
	return parts
}

// spotsHash is the Spots step's hash entry for a card and its parts.
//
// THE PRE-FILL AS A TAP LANDS IT. The hash entry sets moves only from
// ;moves=, and chooseRecipe opens the step on the card's last_moves; so the
// shot passes them, and the step opens where the floor's tap does.
func spotsHash(card composerCard, parts []string) (string, []string) {
	var mv []string
	for from, to := range card.LastMoves {
		mv = append(mv, from+":"+to)
	}
	sort.Strings(mv)
	hash := fmt.Sprintf("#compose=positions;recipe=%s;parts=%s", card.ID, strings.Join(parts, ","))
	if len(mv) > 0 {
		hash += ";moves=" + strings.Join(mv, ",")
	}
	return hash, mv
}

// spotsShots is the Spots step with nothing selected, then each place of the
// card selected in turn, at the HMI's 1280x800. only, when given, keeps the
// places named. prefix tells two recipes' steps on one cell apart.
//
// THE PAGE SHOWS THE HINT IT SHOULD: nothing selected, a grouped recipe's
// words (the mock's) or an ungrouped one's (v0's); a place selected, the hint
// opens on where that place stands. Every state is one step (still), so the
// first card's y is compared across them.
func spotsShots(sc seededCell, card composerCard, hash, prefix string, mock func(state, drawn string) string, only ...string) []shotSpec {
	station := fmt.Sprintf("/operator/station/%d", sc.stationID)
	state := "spots" + prefix
	at := func(h, want string) frame {
		return frame{src: station + h, w: 1280, h: 800, svg: "#os-comp-svg", want: want}
	}
	shot := func(f frame, st, what, mk string) shotSpec {
		s := pageShot(f, st, what, mk, "")
		s.still = state
		return s
	}
	opening := spotsHintUngrouped
	if len(card.Groups) > 0 {
		opening = spotsHintGrouped
	}
	specs := []shotSpec{shot(at(hash, opening), state+"-1280", "Spots step, nothing selected ("+card.Name+")", mock(state+"-1280", ""))}
	for _, s := range card.Spots {
		if len(only) > 0 && !slices.Contains(only, s.Node) {
			continue
		}
		drawn := s.Node
		if to, ok := card.LastMoves[s.Node]; ok {
			drawn = to
		}
		what := fmt.Sprintf("Spots step, %s selected (%s", s.Node, strings.ReplaceAll(s.Job, "_", " "))
		if s.Of != "" {
			what += " of " + s.Of
		}
		if drawn != s.Node {
			what += ", standing on " + drawn
		}
		st := state + "-" + s.Node + "-1280"
		specs = append(specs, shot(at(hash+";sel="+s.Node, drawn+" · "), st, what+")", mock(st, drawn)))
	}
	return specs
}

// The Spots step's hint with nothing selected, as the page's innerText reads
// it: a grouped recipe's (the mock's words) and an ungrouped one's.
const (
	spotsHintGrouped   = "Where this cell last ran the recipe. Tap a press spot or an on-deck spot to change it · as it is? Next"
	spotsHintUngrouped = "This is where the parts go. Tap one to change where it is · as it is? Next"
)

// stillLines says, for each Spots step among shots, whether its first card
// stood at one y in every state: the hint above the picture keeps two lines'
// room, so a tap whose hint wraps never moves the grid. moved is true when
// one did not.
func stillLines(shots []shotRow) (lines []string, moved bool) {
	var steps []string
	by := map[string][]shotRow{}
	for _, s := range shots {
		if s.Still == "" || s.TODO != "" || s.Result.Cards == 0 {
			continue
		}
		if _, ok := by[s.Still]; !ok {
			steps = append(steps, s.Still)
		}
		by[s.Still] = append(by[s.Still], s)
	}
	for _, k := range steps {
		states := by[k]
		y := states[0].Result.FirstY
		var off []string
		for _, s := range states[1:] {
			if math.Abs(s.Result.FirstY-y) > 0.5 {
				off = append(off, fmt.Sprintf("%.1f px in `%s`", s.Result.FirstY, s.File))
			}
		}
		if len(off) == 0 {
			lines = append(lines, fmt.Sprintf("%s: the first card stands at y = %.1f px in all %d states", k, y, len(states)))
			continue
		}
		moved = true
		lines = append(lines, fmt.Sprintf("%s: THE PICTURE MOVES BETWEEN TAPS — the first card is at %.1f px in `%s`, %s",
			k, y, states[0].File, strings.Join(off, ", ")))
	}
	return lines, moved
}

// checkStill fails the run when a Spots step's picture moved between states.
func checkStill(t *testing.T, shots []shotRow) {
	t.Helper()
	if lines, moved := stillLines(shots); moved {
		t.Errorf("%s", strings.Join(lines, "; "))
	}
}

// flowShot is the board's flow panel at the HMI's 1280x800.
func flowShot(sc seededCell, mock string) shotSpec {
	return pageShot(frame{src: fmt.Sprintf("/operator/station/%d#flow", sc.stationID), w: 1280, h: 800, svg: "#os-flow-svg"},
		"flow-1280", "the board's flow panel (#flow)", mock, "")
}

// draftShot is a recipe's desktop draft at 1440x900, through the desktop's
// one-shot hash entries (#tab=presets;recipe=<id> and the rest after it).
//
// THE DESKTOP'S ONE-SHOTS ARE THE DESKTOP LANE'S. A tree without ;recipe=
// opens the Recipes tab, which lists the recipe and opens no draft; the draft
// opens from the Styles tab's rail. So the driver makes the two clicks an
// engineer makes — the Styles tab, the recipe's row — when the URL alone
// does not get there, and INDEX.md says which way it went. want is what the
// state shows once it is built (empty: the draft's own title); scroll brings
// a block into the frame.
func draftShot(sc seededCell, recipe, state, extra, want, what, mock, todo string) shotSpec {
	f := draftFrame(sc, recipe, extra, want)
	return pageShot(f, state, what+" ("+f.src+")", mock, todo)
}

// draftFrame is the frame draftShot opens.
func draftFrame(sc seededCell, recipe, extra, want string) frame {
	id := sc.recipes[recipe]
	if want == "" {
		want = "Recipe · " + recipe
	}
	return frame{src: fmt.Sprintf("/processes?process=%d#tab=presets;recipe=%d%s", sc.processID, id, extra),
		w: 1440, h: 900, svg: "#pd-svg", want: want,
		clicks: []string{`[data-tab="flows"]`, fmt.Sprintf(`[data-act="recipe-open"][data-preset="%d"]`, id)}}
}

// mockFor is the mock's matching tab and state, beside every Press 6 shot
// (MOCK-recipe-groups-2026-10-22.html: tab 1 the desktop, tab 2 the HMI).
func mockFor(grouped bool, state, drawn string) string {
	if !grouped {
		return ""
	}
	switch {
	case state == "relation-1280":
		return "mock tab 2, nothing selected (its picture)"
	case state == "relation-1084":
		return "mock tab 1, no focus (its picture)"
	case state == "spots-1280":
		return "mock tab 2, nothing selected"
	case strings.HasPrefix(state, "spots-"):
		return "mock tab 2, " + drawn + " tapped"
	case state == "flow-1280":
		return "no mock of the flow panel; nearest is mock tab 2, nothing selected"
	case state == "draft-1440":
		return "mock tab 1, no focus"
	}
	return ""
}

// frame is a real page as shotFrameDriver opens it.
type frame struct {
	src    string   // the page's path and hash
	w, h   int      // the window, and the iframe that fills it
	svg    string   // the picture(s) to measure: every match that holds a card
	want   string   // text the page shows once the state is drawn
	clicks []string // controls clicked in turn when the URL alone does not get there
	scroll string   // text brought into view before the shot
	// optional: a state another lane builds. Unreached, the driver measures
	// what the page draws and says so, rather than failing.
	optional bool
}

func (f frame) url() string {
	q := url.Values{"src": {f.src}, "w": {strconv.Itoa(f.w)}, "h": {strconv.Itoa(f.h)}, "svg": {f.svg}}
	if f.want != "" {
		q.Set("want", f.want)
	}
	if raw, err := json.Marshal(f.clicks); err == nil && len(f.clicks) > 0 {
		q.Set("clicks", string(raw))
	}
	if f.scroll != "" {
		q.Set("scroll", f.scroll)
	}
	if f.optional {
		q.Set("optional", "1")
	}
	return "/__shots/frame?" + q.Encode()
}

func getJSON(t *testing.T, u string, into any) {
	t.Helper()
	resp, err := http.Get(u)
	must(t, err, "GET "+u)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", u, resp.StatusCode)
	}
	must(t, json.NewDecoder(resp.Body).Decode(into), "decode "+u)
}

// ── Chrome ─────────────────────────────────────────────────────────────────

type chromeRun struct {
	t      *testing.T
	chrome string
	out    string
}

var shotAttr = regexp.MustCompile(`data-shot="([^"]*)"`)

// measured renders a shot twice: --dump-dom for the driver's measurement,
// then --screenshot for the PNG. Both runs load the same URL, so they draw the
// same state.
func (c chromeRun) measured(srv, base string, s shotSpec) shotRow {
	t := c.t
	t.Helper()
	res := c.measure(srv+s.url, s.w, max(s.h, 900))
	row := shotRow{File: base + s.state + ".png", What: s.what, Mock: s.mock, Result: res, Still: s.still}
	switch {
	case s.todo != "" && strings.HasPrefix(res.Reached, "no"):
		row.TODO, row.File = s.todo, base+s.state+"-TODO.png"
	case res.Error != "":
		t.Errorf("%s: %s", row.File, res.Error)
	}
	if res.PageError != "" {
		t.Errorf("%s: the page threw: %s", row.File, res.PageError)
	}
	h := s.h
	if h == 0 {
		h = max(320, int(res.Height)+capH+8)
	}
	c.screenshot(srv+s.url, row.File, s.w, h)
	t.Logf("%s: %s", row.File, res.line())
	return row
}

// measure is the driver's measurement of u.
func (c chromeRun) measure(u string, w, h int) shotResult {
	var res shotResult
	m := shotAttr.FindStringSubmatch(c.run(u, w, h, "--dump-dom"))
	if m == nil {
		res.Error = "the driver wrote no measurement — it never finished"
	} else if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &res); err != nil {
		res.Error = "the measurement is not JSON: " + err.Error()
	}
	return res
}

// screenshot writes u's PNG as file in the output folder.
func (c chromeRun) screenshot(u, file string, w, h int) {
	t := c.t
	t.Helper()
	target := filepath.Join(c.out, file)
	_ = os.Remove(target)
	raw := c.run(u, w, h, "--screenshot="+target, "--enable-logging=stderr", "--log-level=0")
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("%s was not written: %v\n%s", target, err, raw)
	}
	if info.Size() < 5_000 {
		t.Errorf("%s is %d bytes — a blank page, not a picture", target, info.Size())
	}
	for _, line := range strings.Split(raw, "\n") {
		if rendererFailure(line) {
			t.Errorf("%s: the renderer failed: %s", file, strings.TrimSpace(line))
		}
	}
}

func (c chromeRun) run(u string, w, h int, flags ...string) string {
	c.t.Helper()
	args := append([]string{
		"--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run", "--no-default-browser-check",
		"--user-data-dir=" + c.t.TempDir(), fmt.Sprintf("--window-size=%d,%d", w, h),
		// The dark scheme, as every shot of these screens has been taken: the
		// kiosk pins it, and the desktop takes Blink's setting (composer shots).
		"--blink-settings=preferredColorScheme=0",
		"--virtual-time-budget=25000",
	}, flags...)
	cmd := exec.Command(c.chrome, append(args, u)...)
	cmd.Dir = c.out
	raw, err := cmd.CombinedOutput()
	if err != nil {
		c.t.Fatalf("chrome %v: %v\n%s", flags, err, raw)
	}
	return string(raw)
}

// ── the measurement ────────────────────────────────────────────────────────

// shotResult is what shotProbe writes, plus how the driver got there.
type shotResult struct {
	Error     string   `json:"error"`
	PageError string   `json:"pageError"`
	Reached   string   `json:"reached"`
	Cards     int      `json:"cards"`
	Texts     int      `json:"texts"`
	Overlaps  []string `json:"overlaps"`
	Outside   []string `json:"outside"`
	TextHits  []string `json:"textHits"`
	Clipped   []string `json:"clipped"`
	SvgW      float64  `json:"svgW"`
	ContentR  float64  `json:"contentR"`
	FrameW    float64  `json:"frameW"`
	Height    float64  `json:"height"`
	Scrolls   bool     `json:"scrolls"`
	// Gate is the product's relationScope for the cell ("relation" or
	// "today"), empty in a tree that exports none.
	Gate string `json:"gate"`
	// Pictures is how many pictures were measured, when more than one (the
	// Which recipe screen draws one per card).
	Pictures int `json:"pictures"`
	// FirstY is the first card's top on the page, in px: where the picture
	// stands under whatever is above it.
	FirstY float64 `json:"firstY"`
}

// line is the PNG's result line: ok, or what overlapped.
func (r shotResult) line() string {
	if r.Error != "" {
		return "NOT MEASURED: " + r.Error
	}
	var bad []string
	add := func(label string, items []string) {
		if len(items) == 0 {
			return
		}
		shown := items
		if len(shown) > 8 {
			shown = append(append([]string(nil), items[:8]...), fmt.Sprintf("… %d more", len(items)-8))
		}
		bad = append(bad, label+" "+strings.Join(shown, "; "))
	}
	add("cards overlap:", r.Overlaps)
	add("outside its card:", r.Outside)
	add("text over text:", r.TextHits)
	add("clipped:", r.Clipped)
	s := "ok"
	if len(bad) > 0 {
		s = strings.Join(bad, " · ")
	}
	pics := ""
	if r.Pictures > 1 {
		pics = fmt.Sprintf(" over %d pictures", r.Pictures)
	}
	s += fmt.Sprintf(" (%d cards, %d texts%s; picture %.0f px", r.Cards, r.Texts, pics, max(r.SvgW, r.ContentR))
	if r.Scrolls {
		s += fmt.Sprintf(" in a %.0f px frame: scrolls", r.FrameW)
	} else {
		s += fmt.Sprintf(" in a %.0f px frame", r.FrameW)
	}
	return s + ")"
}

func (r shotResult) ok() bool {
	return r.Error == "" && len(r.Overlaps)+len(r.Outside)+len(r.TextHits)+len(r.Clipped) == 0
}

// shotProbe measures one picture with getBoundingClientRect, in the browser:
//
//   - the cards are the SVG's g[data-pos] and g[data-staging], each by its own
//     rect.box; no two may overlap;
//   - every drawn descendant of a card (text, chevrons, tags, marks) lies
//     inside its card's box, a group tag's top excepted: it straddles the
//     border as the mock draws it, within the bar room above the box, and is
//     itself checked against every other card and every press bar;
//   - no text overlaps another text, anywhere in the picture;
//   - nothing is drawn past the SVG's own box (it would be clipped);
//   - the picture's width against its frame: wider "scrolls".
//
// A TEXT IS MEASURED BY ITS INK, not its box. An SVG text's client rect is
// its font's line box — ascent to descent, whatever the glyphs — so two lines
// of one card, 14 px apart in a 12 px face, "overlap" by two pixels whose ink
// is a clear gap apart. The ink is the font's own measurement of the same
// string (canvas measureText in the text's document, same font): its actual
// ascent and descent around the baseline the rect puts it on, its advance
// across. Half a pixel of slack on every comparison.
const shotProbe = `
function shotProbe(svg, frameW) {
  var TAG_ROOM = 12.5;
  var sr = svg.getBoundingClientRect();
  var real = function (r) { return r.width > 0 || r.height > 0; };
  var ctx = svg.ownerDocument.createElement('canvas').getContext('2d');
  var R = function (el) {
    var r = el.getBoundingClientRect();
    if (el.tagName.toLowerCase() !== 'text' || !real(r)) return r;
    var cs = svg.ownerDocument.defaultView.getComputedStyle(el);
    ctx.font = cs.fontStyle + ' ' + cs.fontWeight + ' ' + cs.fontSize + ' ' + cs.fontFamily;
    var m = ctx.measureText(el.textContent || '');
    var fa = m.fontBoundingBoxAscent, fd = m.fontBoundingBoxDescent;
    if (!(fa + fd > 0)) return r;
    var k = r.height / (fa + fd), base = r.top + fa * k;
    var top = base - m.actualBoundingBoxAscent * k, bottom = base + m.actualBoundingBoxDescent * k;
    return { left: r.left, right: r.right, top: top, bottom: bottom, width: r.width, height: bottom - top };
  };
  var hits = function (a, b) {
    return Math.min(a.right, b.right) - Math.max(a.left, b.left) > 0.5 &&
      Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top) > 0.5;
  };
  var LEAVES = { text: 1, path: 1, rect: 1, circle: 1, ellipse: 1, line: 1, polyline: 1, polygon: 1, use: 1, image: 1 };
  var label = function (el) {
    var s = (el.textContent || '').trim().replace(/\s+/g, ' ');
    var cls = el.getAttribute('class');
    return el.tagName.toLowerCase() + (cls ? '.' + cls.split(' ')[0] : '') +
      (s ? ' "' + (s.length > 32 ? s.slice(0, 31) + '…' : s) + '"' : '');
  };
  var CARD = 'g[data-pos], g[data-staging]';
  var cards = [];
  svg.querySelectorAll(CARD).forEach(function (g) {
    var box = null;
    for (var i = 0; i < g.children.length; i++) if (g.children[i].matches('rect.box')) { box = g.children[i]; break; }
    if (!box) return;
    var r = R(box);
    if (!real(r)) return;
    cards.push({ g: g, box: box, r: r, name: g.getAttribute('data-staging') || g.getAttribute('data-pos') });
  });
  var overlaps = [], outside = [], textHits = [], clipped = [];
  for (var i = 0; i < cards.length; i++) for (var j = i + 1; j < cards.length; j++) {
    if (hits(cards[i].r, cards[j].r)) overlaps.push(cards[i].name + ' × ' + cards[j].name);
  }
  cards.forEach(function (c) {
    c.g.querySelectorAll('*').forEach(function (d) {
      if (d === c.box || !LEAVES[d.tagName.toLowerCase()] || d.closest(CARD) !== c.g) return;
      var r = R(d);
      if (!real(r)) return;
      // A GROUP TAG STRADDLES ITS CARD'S TOP BORDER, as the mock draws it: up
      // to the bar room above the box is the tag's own. Below, beside and
      // against another card or the bar, it is measured like anything else.
      var over = c.r.top - r.top;
      if (d.closest('.gtag') && over <= TAG_ROOM) over = 0;
      var by = Math.max(c.r.left - r.left, r.right - c.r.right, over, r.bottom - c.r.bottom);
      if (by > 0.5) outside.push(c.name + ': ' + label(d) + ' ' + Math.ceil(by) + ' px out');
    });
  });
  svg.querySelectorAll('.gtag').forEach(function (tag) {
    var own = tag.closest(CARD), r = R(tag);
    if (!real(r)) return;
    cards.forEach(function (c) {
      if (c.g !== own && hits(r, c.r)) overlaps.push('tag of ' + (own ? own.getAttribute('data-pos') : '?') + ' × ' + c.name);
    });
    svg.querySelectorAll('path.press').forEach(function (bar) {
      if (hits(r, R(bar))) outside.push((own ? own.getAttribute('data-pos') : '?') + ': its tag on a press bar');
    });
  });
  var texts = [];
  svg.querySelectorAll('text').forEach(function (el) {
    var r = R(el);
    if (real(r) && (el.textContent || '').trim()) texts.push({ el: el, r: r });
  });
  for (var a = 0; a < texts.length; a++) for (var b = a + 1; b < texts.length; b++) {
    if (hits(texts[a].r, texts[b].r)) textHits.push(label(texts[a].el) + ' × ' + label(texts[b].el));
  }
  var l = Infinity, rt = 0, bt = 0, tp = Infinity;
  svg.querySelectorAll('*').forEach(function (el) {
    if (!LEAVES[el.tagName.toLowerCase()]) return;
    var r = R(el);
    if (!real(r)) return;
    l = Math.min(l, r.left - sr.left); rt = Math.max(rt, r.right - sr.left);
    tp = Math.min(tp, r.top - sr.top); bt = Math.max(bt, r.bottom - sr.top);
  });
  if (rt > sr.width + 0.5) clipped.push('drawn to x=' + Math.ceil(rt) + ' in a ' + Math.round(sr.width) + ' px wide picture');
  if (l < -0.5) clipped.push('drawn from x=' + Math.floor(l));
  if (bt > sr.height + 0.5) clipped.push('drawn to y=' + Math.ceil(bt) + ' in a ' + Math.round(sr.height) + ' px tall picture');
  if (tp < -0.5) clipped.push('drawn from y=' + Math.floor(tp));
  return { cards: cards.length, texts: texts.length, overlaps: overlaps, outside: outside, textHits: textHits,
    clipped: clipped, svgW: Math.round(sr.width), contentR: Math.ceil(rt), frameW: Math.round(frameW),
    height: Math.round(sr.height), scrolls: Math.max(sr.width, rt) > frameW + 0.5,
    firstY: cards.length ? Math.round(cards[0].r.top * 10) / 10 : 0 };
}
function shotProbeAll(svgs) {
  var rs = svgs.map(function (s) {
    return shotProbe(s, s.parentElement ? s.parentElement.clientWidth : s.getBoundingClientRect().width);
  });
  if (rs.length === 1) return rs[0];
  var all = { cards: 0, texts: 0, overlaps: [], outside: [], textHits: [], clipped: [], svgW: 0, contentR: 0,
    frameW: 0, height: 0, scrolls: false, pictures: rs.length, firstY: rs.length ? rs[0].firstY : 0 };
  rs.forEach(function (r, i) {
    var p = 'picture ' + (i + 1) + ': ';
    all.cards += r.cards; all.texts += r.texts;
    ['overlaps', 'outside', 'textHits', 'clipped'].forEach(function (k) {
      r[k].forEach(function (x) { all[k].push(p + x); });
    });
    all.svgW = Math.max(all.svgW, r.svgW); all.contentR = Math.max(all.contentR, r.contentR);
    all.frameW = Math.max(all.frameW, r.frameW); all.height = Math.max(all.height, r.height);
    all.scrolls = all.scrolls || r.scrolls;
  });
  return all;
}
`

// checkShotProbe runs shotProbe over a picture drawn wrong on purpose, once,
// before any station: two cards overlapping, a card line running out of its
// card, two lane names on top of each other, a mark past the picture's edge,
// and a picture wider than its frame — and a card whose two lines sit 14 px
// apart in a 12 px face, and a group tag over its card's top border, which
// must NOT be flagged. A probe that answers "ok" on every real cell has to be
// seen to say something else here first.
func checkShotProbe(t *testing.T, chrome, out string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(probeCheckPage))
	}))
	defer srv.Close()
	dom := chromeRun{t: t, chrome: chrome, out: out}.run(srv.URL, 800, 400, "--dump-dom")
	m := shotAttr.FindStringSubmatch(dom)
	if m == nil {
		t.Fatal("the probe check wrote no measurement")
	}
	var r shotResult
	must(t, json.Unmarshal([]byte(html.UnescapeString(m[1])), &r), "the probe check's measurement")
	joined := func(xs []string) string { return strings.Join(xs, " | ") }
	if !slices.Equal(r.Overlaps, []string{"A × B"}) {
		t.Errorf("probe check: overlaps = %v, want exactly A × B", r.Overlaps)
	}
	if !strings.Contains(joined(r.Outside), `A: text.ln "sticks out`) {
		t.Errorf("probe check: outside = %v, want A's line", r.Outside)
	}
	if !strings.Contains(joined(r.TextHits), `"LANE ONE" × text.nm "LANE TWO"`) {
		t.Errorf("probe check: text over text = %v, want LANE ONE × LANE TWO", r.TextHits)
	}
	if strings.Contains(joined(r.Outside), "C: ") {
		t.Errorf("probe check: a group tag over its card's top border read as outside it: %v", r.Outside)
	}
	if strings.Contains(joined(r.TextHits), "first line") {
		t.Errorf("probe check: two card lines 14 px apart read as overlapping: %v", r.TextHits)
	}
	if len(r.Clipped) == 0 || !r.Scrolls {
		t.Errorf("probe check: clipped = %v, scrolls = %v; want the mark past the edge and a picture wider than its frame",
			r.Clipped, r.Scrolls)
	}
	if t.Failed() {
		t.FailNow()
	}
	t.Logf("probe check: %s", r.line())
}

// probeCheckPage is checkShotProbe's picture, in the cards' own markup.
const probeCheckPage = `<!doctype html><meta charset="utf-8"><title>probe check</title>
<style>
  body { margin:0; }
  text { font: 600 15px system-ui, sans-serif; }
  text.ln { font: 500 12px system-ui, sans-serif; }
  rect.box { fill:none; stroke:#888; }
</style>
<body>
<div id="frame" style="width:400px;overflow:auto">
<svg id="pic" width="600" height="300" viewBox="0 0 600 300">
  <g data-pos="A" transform="translate(10,10)"><rect class="box" width="188" height="92"/>
    <text class="nm" x="14" y="26">A</text><text class="ln" x="150" y="43">sticks out past the edge</text></g>
  <g data-pos="B" transform="translate(150,60)"><rect class="box" width="188" height="92"/>
    <text class="nm" x="14" y="26">B</text></g>
  <g data-pos="C" transform="translate(360,10)"><rect class="box" width="188" height="92"/>
    <text class="nm" x="14" y="26">C</text><text class="ln" x="14" y="43">first line, Robot 1 supplies</text>
    <text class="ln" x="14" y="57">second line, Robot 2 indexes</text>
    <g class="gtag"><rect x="4" y="-9" width="18" height="18"/><text x="13" y="4">L</text></g></g>
  <g data-staging="L" transform="translate(10,200)"><rect class="box" width="140" height="44"/>
    <text class="nm" x="12" y="19">LANE ONE</text><text class="nm" x="22" y="21">LANE TWO</text></g>
  <rect x="590" y="260" width="40" height="10"/>
</svg></div>
<script>` + shotProbe + `
document.body.dataset.shot = JSON.stringify(shotProbe(document.getElementById('pic'), 400));
</script>
</body>`

// capH is the relation driver's caption line above the picture.
const capH = 26

// realCellDriver draws the station's picture through the product's own
// renderer, stylesheets and model, from the station's real view and cell
// reads, the way the board's flow panel draws it — and measures it.
//
// mode "relation" asks renderFlowPicture(view, {relation: true, frame: {w}});
// "today" asks the board's own options, {frame: {w: 1280}, usedOnly: true}.
// A tree without the relation picture ignores `relation` and draws today's.
// When operator-flow.js exports the scope gate's predicate (relationScope),
// its answer for the cell is recorded beside the measurement, so INDEX.md can
// say whether the product's gate agrees with the cells this harness gave a
// today-1280 shot.
//
// THE FRAME IS w WIDE AND SCROLLS SIDEWAYS: the SVG is the picture's own size
// (opts.width when the renderer reports one, else the frame), so a picture
// wider than its frame is cut where the screen would cut it, and the
// measurement says "scrolls".
func realCellDriver(station, w, mode, caption string) string {
	width, err := strconv.Atoi(w)
	if err != nil || width <= 0 {
		width = 1280
	}
	if mode != "today" {
		mode = "relation"
	}
	return `<!doctype html><html lang="en" data-theme="dark"><meta charset="utf-8"><title>real cell</title>
<link rel="stylesheet" href="/static/shared/tokens.css">
<link rel="stylesheet" href="/static/operator-station/operator.css">
<link rel="stylesheet" href="/static/operator-station/flow-picture.css">
<link rel="stylesheet" href="/static/operator-station/composer-glyphs.css">
<style>
  /* operator.css lays the body out as the kiosk's grid (header, alerts,
     board, footer rows at 100vh): this page is a caption and a picture. */
  html,body { width:auto; height:auto; margin:0; background:var(--surface-1,#0f1217); overflow:hidden; }
  body { display:block; }
  .cap { font: 500 12px/` + strconv.Itoa(capH) + `px Inter,system-ui,sans-serif; color:var(--text-muted,#8b95a5);
         padding:0 20px; height:` + strconv.Itoa(capH) + `px; white-space:nowrap; overflow:hidden; }
  .frame { width:` + strconv.Itoa(width) + `px; overflow-x:auto; overflow-y:hidden; }
</style>
<body data-step="starting">
<div class="cap">` + template.HTMLEscapeString(caption) + `</div>
<div class="frame"><svg id="pic" class="os-flow-picture"></svg></div>
<script src="/static/operator-station/flowspec-data.js"></script>
<script src="/static/operator-station/composer-model.js"></script>
<script>` + shotProbe + `</script>
<script type="module">
import * as Flow from '/static/operator-station/operator-flow.js';
const STATION = "` + template.JSEscapeString(station) + `", W = ` + strconv.Itoa(width) + `, MODE = "` + mode + `";
const out = document.body;
try {
  out.dataset.step = 'view';
  const view = await (await fetch('/api/operator-stations/' + STATION + '/view')).json();
  out.dataset.step = 'cell';
  view.cell = await (await fetch('/api/operator-stations/' + STATION + '/cell')).json();
  out.dataset.step = 'draw';
  const o = MODE === 'relation' ? { relation: true, frame: { w: W } } : { frame: { w: 1280 }, usedOnly: true };
  const svg = document.getElementById('pic');
  svg.innerHTML = Flow.renderFlowPicture(view, o);
  const pw = Math.max(o.width || 0, W), ph = o.height || 560;
  svg.setAttribute('viewBox', '0 0 ' + pw + ' ' + ph);
  svg.setAttribute('width', String(pw));
  svg.setAttribute('height', String(ph));
  out.dataset.step = 'measure';
  const r = shotProbe(svg, W);
  r.reached = 'driver';
  r.gate = typeof Flow.relationScope === 'function' ? (Flow.relationScope(view.cell) ? 'relation' : 'today') : '';
  out.dataset.shot = JSON.stringify(r);
  out.dataset.step = 'done';
} catch (e) {
  out.dataset.shot = JSON.stringify({ error: 'at ' + out.dataset.step + ': ' + String(e) });
}
</script>
</body></html>`
}

// shotFrameDriver puts a real page in a same-origin iframe the size of the
// window, waits for its picture — every svg matching the selector that holds
// a card, the page showing `want`, the markup unchanged for three polls — and
// measures it. When the URL alone does not reach the state and `clicks` (a
// JSON list of selectors) names controls, it clicks each in turn, the clicks
// a person makes, and waits again. An `optional` state (one another lane
// builds) that is still not shown is measured as the page draws it, and the
// measurement says so; `scroll` brings a block of text into view first.
//
// THE IFRAME IS THE VIEWPORT, so the PNG of this page is the product's page;
// the measurement is written into this page's own body, which shows nothing.
func shotFrameDriver(q url.Values) string {
	clicks := q.Get("clicks")
	if clicks == "" {
		clicks = "[]"
	}
	optional := "false"
	if q.Get("optional") == "1" {
		optional = "true"
	}
	j := template.JSEscapeString
	return `<!doctype html><meta charset="utf-8"><title>shot frame</title>
<style>html,body{margin:0;overflow:hidden;background:#000} iframe{position:fixed;left:0;top:0;border:0}</style>
<body data-step="starting">
<iframe id="f" style="width:` + template.HTMLEscapeString(q.Get("w")) + `px;height:` + template.HTMLEscapeString(q.Get("h")) + `px"></iframe>
<script>` + shotProbe + `</script>
<script>
const SRC = "` + j(q.Get("src")) + `", SVG = "` + j(q.Get("svg")) + `", WANT = "` + j(q.Get("want")) + `";
const SCROLL = "` + j(q.Get("scroll")) + `", OPTIONAL = ` + optional + `, CLICKS = JSON.parse("` + j(clicks) + `");
const CARD = 'g[data-pos], g[data-staging]';
const out = document.body;
const f = document.getElementById('f');
const sleep = ms => new Promise(r => setTimeout(r, ms));
const pictures = () => {
  const doc = f.contentDocument;
  return doc ? Array.from(doc.querySelectorAll(SVG)).filter(s => s.querySelector(CARD)) : [];
};
const shows = text => {
  const doc = f.contentDocument;
  return !text || !!(doc && doc.body && doc.body.innerText.indexOf(text) >= 0);
};
async function settle(ms, want) {
  let last = '', same = 0;
  for (let t = 0; t < ms; t += 100) {
    await sleep(100);
    const svgs = pictures();
    const html = svgs.map(s => s.innerHTML).join('|');
    if (svgs.length && shows(want) && html === last) { if (++same >= 3) return svgs; } else { same = 0; }
    last = html;
  }
  return null;
}
// The deepest element of the last block that holds the text.
function bringIntoView(text) {
  let found = null;
  f.contentDocument.body.querySelectorAll('*').forEach(el => {
    if ((el.textContent || '').indexOf(text) >= 0) found = el;
  });
  if (found) found.scrollIntoView({ block: 'start' });
  return !!found;
}
(async () => {
  try {
    out.dataset.step = 'load';
    await new Promise(r => { f.addEventListener('load', r, { once: true }); f.src = SRC; });
    out.dataset.step = 'settle';
    let reached = 'url';
    let svgs = await settle(4000, WANT);
    if (!svgs && CLICKS.length) {
      const done = [];
      for (const sel of CLICKS) {
        let b = null;
        for (let t = 0; t < 3000 && !b; t += 100) { await sleep(100); b = f.contentDocument.querySelector(sel); }
        if (!b) break;
        b.click();
        done.push(sel);
      }
      reached = 'click: ' + (done.join(' then ') || 'nothing to click');
      svgs = await settle(5000, WANT);
    }
    if (!svgs && OPTIONAL) {
      reached = 'no: "' + WANT + '" is not shown (reached by ' + reached + ')';
      svgs = await settle(2000, '');
    }
    if (svgs && SCROLL) {
      if (!bringIntoView(SCROLL)) reached += '; "' + SCROLL + '" is nowhere to scroll to';
      await sleep(300);
    }
    const body = (f.contentDocument && f.contentDocument.body) || { dataset: {} };
    const pageError = body.dataset.composerError || body.dataset.pdError || '';
    if (!svgs) {
      out.dataset.shot = JSON.stringify({ error: 'never drew ' + SVG + (WANT ? ' showing "' + WANT + '"' : '') +
        ' (reached by ' + reached + ')', reached: reached, pageError: pageError });
      return;
    }
    out.dataset.step = 'measure';
    const r = shotProbeAll(svgs);
    r.reached = reached;
    r.pageError = pageError;
    out.dataset.shot = JSON.stringify(r);
    out.dataset.step = 'done';
  } catch (e) {
    out.dataset.shot = JSON.stringify({ error: 'at ' + out.dataset.step + ': ' + String(e) });
  }
})();
</script>
</body>`
}

// ── INDEX.md ───────────────────────────────────────────────────────────────

type shotRow struct {
	File   string
	What   string
	Mock   string
	Result shotResult
	// TODO is the lane an unreached state waits on.
	TODO string
	// Still is the Spots step the state is one of (shotSpec.still).
	Still string
}

type cellRow struct {
	// title and recipeLines, when set, replace the real cell's heading and
	// its recipe line (a fixture states its own).
	title        string
	recipeLines  []string
	cell         realCell
	modes        []string
	gateRelation bool
	recipe       bool
	grouped      bool
	recipeName   string
	lastMoves    []string
	notes        []string
	shots        []shotRow
}

type shotIndex struct {
	Tree  string
	Pulls []*cellPull
	Rows  []cellRow
	// Stale is the PNGs in the folder this run did not write: an earlier
	// run's, under a name this one no longer takes.
	Stale []string
}

// markdown is INDEX.md: the review set first, then every other station by
// plant and id; each PNG with its state, its result line and, for Press 6,
// the mock's matching state. Node names only — no part number is on any line.
func (x shotIndex) markdown() string {
	var b strings.Builder
	pngs, ok := 0, 0
	for _, r := range x.Rows {
		for _, s := range r.shots {
			pngs++
			if s.Result.ok() {
				ok++
			}
		}
	}
	tree := x.Tree
	if tree == "" {
		tree = "(tree not given)"
	}
	fmt.Fprintf(&b, "# Real-cell shots at %s\n\n", tree)
	fmt.Fprintf(&b, "Written by `TestRealCellShots` (`scripts/real-cell-shots.sh`) on %s. %d PNGs; the overlap check "+
		"is ok on %d and has findings on %d.\n\n", time.Now().Format("2006-01-02 15:04"), pngs, ok, pngs-ok)
	b.WriteString("Pulls:")
	for _, p := range x.Pulls {
		fmt.Fprintf(&b, " %s = `%s` (%d stations%s);", p.Plant, filepath.Base(p.Path), len(p.Cells),
			map[bool]string{true: "", false: ", no station_id: the build predates it"}[p.StationIDs])
	}
	b.WriteString("\n\nEach station is a fresh Edge seeded from its own picture (generic names: process and station " +
		"\"<Plant> <id>\"; SYN- parts). The overlap check is measured in the browser on every PNG: cards overlapping, " +
		"a card's text or mark outside its card, text over text, anything drawn past the picture's edge, and the " +
		"picture's width against its frame (\"scrolls\" when wider). `relation-*` asks the renderer for " +
		"`{relation: true, frame: {w}}`; a tree without the relation picture ignores it and draws today's.\n\n")
	if len(x.Stale) > 0 {
		fmt.Fprintf(&b, "Not from this run (an earlier run's, left in the folder): %s.\n\n", strings.Join(x.Stale, ", "))
	}
	b.WriteString("## The review set\n\n")
	b.WriteString("Through the real pages: the HMI states at 1280x800 (the station page), the draft at 1440x900 " +
		"(the desktop).\n\n")
	review := true
	for _, r := range x.Rows {
		if review && !inReview(r.cell) {
			b.WriteString("## Every other station\n\n")
			review = false
		}
		r.markdown(&b)
	}
	return b.String()
}

func (r cellRow) markdown(b *strings.Builder) {
	c := r.cell
	var words []string
	for _, m := range r.modes {
		words = append(words, domain.SwapModeWord(protocol.SwapMode(m)))
	}
	runs := "no changeover claim"
	if len(words) > 0 {
		runs = "runs " + strings.Join(words, ", ")
	}
	var nodes []string
	for _, p := range c.pic.Positions {
		nodes = append(nodes, p.CoreNodeName)
	}
	var lanes []string
	for _, s := range c.pic.Staging {
		lanes = append(lanes, s.CoreNodeName)
	}
	title := fmt.Sprintf("%s %d", plantName(c.plant), c.station)
	if r.grouped {
		title += " (Press 6)"
	}
	title += fmt.Sprintf(" · %s %d", c.plant, c.station)
	if r.title != "" {
		title = r.title
	}
	fmt.Fprintf(b, "### %s\n\n", title)
	fmt.Fprintf(b, "- **Cell:** %d positions (%s)", len(nodes), strings.Join(nodes, ", "))
	if len(lanes) > 0 {
		fmt.Fprintf(b, "; lanes %s", strings.Join(lanes, ", "))
	}
	fmt.Fprintf(b, "; %s; geometry %v.\n", runs, c.pic.Geometry)
	gate := "today's picture (no changeover claim, no recipe)"
	if r.gateRelation {
		gate = "the relation picture (a changeover claim or recipe)"
	}
	product := "this tree exports no relationScope"
	for _, s := range r.shots {
		switch {
		case s.Result.Gate == "":
			continue
		case (s.Result.Gate == "relation") == r.gateRelation:
			product = "the product's relationScope agrees"
		default:
			product = "THE PRODUCT'S relationScope SAYS " + s.Result.Gate + "'s picture"
		}
		break
	}
	fmt.Fprintf(b, "- **Scope gate:** %s; %s.\n", gate, product)
	switch {
	case r.recipeLines != nil:
		for _, l := range r.recipeLines {
			fmt.Fprintf(b, "- **Recipe:** %s.\n", l)
		}
	case r.grouped:
		fmt.Fprintf(b, "- **Recipe:** \"%s\" as SYNTH-round2 §2 writes it (PLN_006←PLN_005 → ULN_002, PLN_007←PLN_008 → "+
			"ULN_005, in from Press Buffer Zone) with groups Left Hand and Right Hand; the running claims' provenance points "+
			"at it. Pre-fill (last_moves): %s.\n", r.recipeName, orNone(r.lastMoves))
	case r.recipe:
		fmt.Fprintf(b, "- **Recipe:** \"%s\", saved from the running style's shape. Pre-fill (last_moves): %s.\n",
			fromRunningRecipeName, orNone(r.lastMoves))
	}
	for _, n := range r.notes {
		fmt.Fprintf(b, "- Note: %s.\n", n)
	}
	if len(r.shots) == 0 {
		b.WriteString("\nNot photographed in this pass.\n\n")
		return
	}
	mocks := false
	for _, s := range r.shots {
		mocks = mocks || s.Mock != ""
	}
	b.WriteString("\n| PNG | State | Overlap check |")
	if mocks {
		b.WriteString(" Mock |")
	}
	b.WriteString("\n|---|---|---|")
	if mocks {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, s := range r.shots {
		what := s.What
		if s.TODO != "" {
			what += "; **TODO** " + s.TODO + ": " + s.Result.Reached + ", so the shot shows what this tree draws there"
		} else if strings.HasPrefix(s.Result.Reached, "click") {
			what += "; TODO desktop lane: the `;recipe=` one-shot. This tree opens the Recipes tab on it, and the " +
				"driver reached the draft by clicking the Styles tab and the recipe's row in the rail"
		}
		fmt.Fprintf(b, "| `%s` | %s | %s |", s.File, mdCell(what), mdCell(s.Result.line()))
		if mocks {
			fmt.Fprintf(b, " %s |", mdCell(s.Mock))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	if lines, _ := stillLines(r.shots); len(lines) > 0 {
		for _, l := range lines {
			fmt.Fprintf(b, "- **Still:** %s.\n", l)
		}
		b.WriteString("\n")
	}
}

func orNone(moves []string) string {
	if len(moves) == 0 {
		return "none (the step opens on the recipe's own nodes)"
	}
	return strings.ReplaceAll(strings.Join(moves, ", "), ":", " → ")
}

func mdCell(s string) string { return strings.ReplaceAll(s, "|", "\\|") }
