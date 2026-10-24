package www

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// handlers_flow_spots_test.go — the Spots step's moves, at the server, from
// the station's own bodies (spots v0, 2026-10-22).
//
// THE OWNER'S RULE: the floor may select pairs, but only pairings the
// engineer has set in recipes, and an on-deck spot never feeds the other
// side. Springfield's Press 6 is the fixture, by node names only
// (REPORT-agent-spr-press6-pair-nodes-2026-10-22): an on-deck card tapped and
// sent to PLN_013 was accepted, a press swapped with its own deck, and a
// spot-to-spot swap crossed the decks.
//
// Bodies come from composer-model.js mintBody under node, never a Go struct
// (handlers_flow_mint_test.go says why).

// press6 is Press 6 as Springfield has it, with the recipe's cell 2 already
// re-saved to ULN_005 (the deploy note's step) so a move is refused for the
// move and not for the stale node: station "Press 6 Operator Screen" with
// PLN_005..PLN_015 in press order, an unloader station with ULN_002 and
// ULN_005, the two routing sources, "Two spot press index" (PLN_006←PLN_005,
// PLN_007←PLN_008) and "One spot press index" (PLN_009←PLN_005, with a
// tooling lane so lanes have something to pin), the running style built from
// Two spot on PLN_010/PLN_012 (decks PLN_005/PLN_008), the one-off style on
// PLN_009, and the deleted style whose PLN_009←PLN_008 the HMI once made.
type press6 struct {
	db        *store.DB
	h         *Handlers
	router    *chi.Mux
	processID int64
	station   int64
	twoSpot   int64
	oneSpot   int64
	running   int64
	nodes     map[string]int64
}

func newPress6(t *testing.T) press6 {
	t.Helper()
	db := testdb.Open(t)
	pid, err := db.CreateProcess("Press 6", "press 6", "", "", false)
	must(t, err, "process")
	f := press6{db: db, processID: pid, nodes: map[string]int64{}}
	f.station, err = db.CreateOperatorStation(stations.Input{ProcessID: pid, Code: "press-6", Name: "Press 6 Operator Screen", Enabled: true})
	must(t, err, "station")
	unloader, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Code: "unloader", Name: "Unloader", Enabled: true})
	must(t, err, "unloader")
	node := func(name string, station int64, seq int) {
		id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, OperatorStationID: &station,
			CoreNodeName: name, Name: name, Sequence: seq, Enabled: true})
		must(t, err, "position "+name)
		f.nodes[name] = id
	}
	for i, n := range []string{"PLN_005", "PLN_006", "PLN_007", "PLN_008", "PLN_009", "PLN_010", "PLN_011",
		"PLN_012", "PLN_013", "PLN_015", "PLN_014"} {
		node(n, f.station, i+1)
	}
	node("ULN_002", unloader, 1)
	node("ULN_005", unloader, 2)
	for _, r := range []domain.RoutingNodeInput{
		{ProcessID: pid, CoreNodeName: "SMN_BUF_100", Role: domain.RoutingRoleSource, Enabled: true},
		{ProcessID: pid, CoreNodeName: "Press Buffer Zone", Role: domain.RoutingRoleSource, Enabled: true},
		{ProcessID: pid, CoreNodeName: "STG_TOOL_1", Role: domain.RoutingRoleStaging, Enabled: true, Sequence: 1},
		{ProcessID: pid, CoreNodeName: "STG_TOOL_2", Role: domain.RoutingRoleStaging, Enabled: true, Sequence: 2},
	} {
		_, err := db.UpsertRoutingNode(domain.CoreNodeKinds{}, r)
		must(t, err, "routing "+r.CoreNodeName)
	}
	f.twoSpot = f.recipe(t, "Two spot press index",
		cell("PLN_006", "PLN_005", "SMN_BUF_100", "ULN_002"),
		cell("PLN_007", "PLN_008", "SMN_BUF_100", "ULN_005"))
	one := cell("PLN_009", "PLN_005", "SMN_BUF_100", "ULN_002")
	one.InboundStaging = "STG_TOOL_1"
	f.oneSpot = f.recipe(t, "One spot press index", one)

	f.running = f.style(t, "SYN-RUN", f.twoSpot, "2026-10-21 18:10:35",
		claim("PLN_010", "PLN_005", "SYN-P6-A", "ULN_002"), claim("PLN_012", "PLN_008", "SYN-P6-B", "ULN_005"))
	f.style(t, "SYN-ONE", f.oneSpot, "2026-10-19 14:51:04", claim("PLN_009", "PLN_005", "SYN-P6-C", "ULN_002"))
	gone := f.style(t, "SYN-GONE", f.twoSpot, "2026-10-21 14:46:08",
		claim("PLN_006", "PLN_005", "SYN-P6-D", "ULN_002"), claim("PLN_009", "PLN_008", "SYN-P6-E", "ULN_005"))
	must(t, db.DeleteStyle(gone), "delete the HMI's cross-side style")
	must(t, db.SetActiveStyle(pid, &f.running), "run it")
	must(t, db.SetFlowComposerEnabled(pid, true), "HMI setup on")
	f.h, f.router = realFlowRouter(t, db)
	return f
}

func must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func cell(head, deck, from, to string) domain.FlowCell {
	return domain.FlowCell{CoreNodeName: head, Role: "produce", SwapMode: "two_robot_press_index",
		PairedCoreNode: deck, InboundSource: from, OutboundDestination: to}
}

func claim(head, deck, part, to string) processes.NodeClaimInput {
	return processes.NodeClaimInput{CoreNodeName: head, Role: "produce", SwapMode: "two_robot_press_index",
		PayloadCode: part, PairedCoreNode: deck, InboundSource: "Press Buffer Zone", OutboundDestination: to}
}

// recipe stores a recipe as the desktop's Save does: the shape, validated.
func (f press6) recipe(t *testing.T, name string, cells ...domain.FlowCell) int64 {
	t.Helper()
	shape, err := domain.PresetShapeJSON(cells)
	must(t, err, "shape")
	id, err := f.db.CreateFlowPreset(processes.FlowPresetInput{ProcessID: f.processID, Name: name, FlowJSON: shape, CreatedBy: "test"})
	must(t, err, "recipe "+name)
	return id
}

// style stores a style built from a recipe: its claims, stamped with the
// recipe's id as provenance and written at `at`, which is what tells the
// pre-fill which style used the recipe last.
func (f press6) style(t *testing.T, name string, preset int64, at string, claims ...processes.NodeClaimInput) int64 {
	t.Helper()
	sid, err := f.db.CreateStyle(name, "", f.processID)
	must(t, err, "style "+name)
	for _, c := range claims {
		c.StyleID = sid
		_, err := f.db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, c)
		must(t, err, "claim "+c.CoreNodeName)
	}
	_, err = f.db.Exec(`UPDATE style_node_claims SET source_preset_id = ?, source_preset_version = 1, updated_at = ?
		WHERE style_id = ?`, preset, at, sid)
	must(t, err, "provenance")
	return sid
}

// mintArgs is one mintBody call.
type mintArgs struct {
	Preset int64             `json:"preset"`
	Codes  []string          `json:"codes"`
	Moves  map[string]string `json:"moves,omitempty"`
}

// mintBodies runs composer-model.js mintBody once per case, in one node
// process, and returns the bodies by case name.
func mintBodies(t *testing.T, station int64, cases map[string]mintArgs) map[string]map[string]any {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the station body pins")
	}
	blob, err := json.Marshal(map[string]any{"station": station, "cases": cases})
	must(t, err, "marshal cases")
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "cases.json")
	must(t, os.WriteFile(fixturePath, blob, 0o644), "write cases")
	modulePath, err := filepath.Abs(filepath.Join("static", "operator-station", "composer-model.js"))
	must(t, err, "resolve composer-model.js")
	harness := fmt.Sprintf(`
'use strict';
const M = require(%q);
const f = require(%q);
const out = {};
for (const k of Object.keys(f.cases)) {
  const c = f.cases[k];
  out[k] = M.mintBody(c.preset, c.codes, f.station, c.moves || {});
}
process.stdout.write(JSON.stringify(out));
`, filepath.ToSlash(modulePath), filepath.ToSlash(fixturePath))
	harnessPath := filepath.Join(dir, "emit.js")
	must(t, os.WriteFile(harnessPath, []byte(harness), 0o644), "write harness")
	raw, err := exec.Command(nodePath, harnessPath).Output()
	must(t, err, "run composer-model.js")
	var out map[string]map[string]any
	must(t, json.Unmarshal(raw, &out), "decode bodies")
	return out
}

// preview posts a body to flow/preview and returns the status and the error
// sentence, if any.
func (f press6) preview(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	resp := doRequest(t, f.router, "POST", "/api/processes/"+itoa(f.processID)+"/flow/preview", body, nil)
	defer resp.Body.Close()
	var out map[string]any
	must(t, json.NewDecoder(resp.Body).Decode(&out), "decode the preview")
	return resp.StatusCode, out
}

// card is the station's composer read's card for a recipe.
func (f press6) card(t *testing.T, preset int64) map[string]any {
	t.Helper()
	resp := doRequest(t, f.router, "GET", fmt.Sprintf("/api/operator-stations/%d/composer", f.station), nil, nil)
	assertStatus(t, resp, http.StatusOK)
	var data struct {
		Presets []map[string]any `json:"presets"`
	}
	must(t, json.NewDecoder(resp.Body).Decode(&data), "decode composer")
	for _, p := range data.Presets {
		if p["id"] == fmt.Sprint(preset) {
			return p
		}
	}
	t.Fatalf("the composer read carries no card for recipe %d", preset)
	return nil
}

// mintedCells is the preview's flow, head → deck.
func mintedCells(pv map[string]any) map[string]string {
	out := map[string]string{}
	mint, _ := pv["mint"].(map[string]any)
	cells, _ := mint["cells"].([]any)
	for _, c := range cells {
		m, _ := c.(map[string]any)
		head, _ := m["core_node_name"].(string)
		deck, _ := m["paired_core_node"].(string)
		out[head] = deck
	}
	return out
}

var (
	press6Pair   = []string{"SYN-P6-X", "SYN-P6-Y"}
	press6One    = []string{"SYN-P6-Z"}
	press6Prefil = map[string]string{"PLN_006": "PLN_010", "PLN_007": "PLN_012"}
)

func withPrefill(extra map[string]string) map[string]string {
	out := maps.Clone(press6Prefil)
	for k, v := range extra {
		if v == "" {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

// TestFlowMint_ASpotMovesOnlyWhereTheRecipesPairIt is the server half of the
// owner's rule, on Press 6's shape: every refusal is a 422 in the job's
// words, naming the next act; every acceptance keeps the deck where it was.
func TestFlowMint_ASpotMovesOnlyWhereTheRecipesPairIt(t *testing.T) {
	f := newPress6(t)
	refused := map[string]struct {
		args mintArgs
		says string
	}{
		// The report's tap: the on-deck card, then an idle front position.
		"deck to PLN_013": {mintArgs{f.twoSpot, press6Pair, withPrefill(map[string]string{"PLN_005": "PLN_013"})},
			"PLN_013 is not an on-deck spot your recipes set up for PLN_010 — an engineer adds one on the desktop"},
		// The press and its own deck swapped.
		"press swaps its deck": {mintArgs{f.twoSpot, press6Pair, map[string]string{"PLN_006": "PLN_005", "PLN_005": "PLN_006"}},
			"PLN_005 is an on-deck spot; a press spot cannot move onto it"},
		// Today's spot-to-spot swap, which crossed the decks.
		"spots swap": {mintArgs{f.twoSpot, press6Pair, map[string]string{"PLN_006": "PLN_007", "PLN_007": "PLN_006"}},
			"PLN_007 is not a press spot your recipes set up with PLN_005 on deck"},
		"press to a lane chip": {mintArgs{f.twoSpot, press6Pair, withPrefill(map[string]string{"PLN_006": "STG_TOOL_2"})},
			"STG_TOOL_2 is a staging lane; a press spot cannot move onto it"},
		// The one-off's press onto the other side's deck: free on that
		// recipe, and still only ever an attached place.
		"press to an attached-only node": {mintArgs{f.oneSpot, press6One, map[string]string{"PLN_009": "PLN_008"}},
			"PLN_008 is an on-deck spot; a press spot cannot move onto it"},
		"press to the other side": {mintArgs{f.twoSpot, press6Pair, withPrefill(map[string]string{"PLN_006": "PLN_011"})},
			"PLN_011 is not a press spot your recipes set up with PLN_005 on deck — an engineer adds one on the desktop"},
		// The owner's same-side rule: the one-off's deck may not be the
		// right-hand deck, whatever a deleted style once did.
		"deck to the other side": {mintArgs{f.oneSpot, press6One, map[string]string{"PLN_005": "PLN_008"}},
			"PLN_008 is not an on-deck spot your recipes set up for PLN_009"},
		"press to another station": {mintArgs{f.twoSpot, press6Pair, withPrefill(map[string]string{"PLN_006": "ULN_002"})},
			"ULN_002 is another station's position"},
		"lane to a position": {mintArgs{f.oneSpot, press6One, map[string]string{"STG_TOOL_1": "PLN_011"}},
			"PLN_011 is not a staging lane"},
	}
	accepted := map[string]struct {
		args  mintArgs
		cells map[string]string
	}{
		"the pre-fill alone": {mintArgs{f.twoSpot, press6Pair, press6Prefil},
			map[string]string{"PLN_010": "PLN_005", "PLN_012": "PLN_008"}},
		"press back to its own": {mintArgs{f.twoSpot, press6Pair, withPrefill(map[string]string{"PLN_006": ""})},
			map[string]string{"PLN_006": "PLN_005", "PLN_012": "PLN_008"}},
		"press to the one-off's spot": {mintArgs{f.twoSpot, press6Pair, withPrefill(map[string]string{"PLN_006": "PLN_009"})},
			map[string]string{"PLN_009": "PLN_005", "PLN_012": "PLN_008"}},
		"lane to a lane": {mintArgs{f.oneSpot, press6One, map[string]string{"STG_TOOL_1": "STG_TOOL_2"}},
			map[string]string{"PLN_009": "PLN_005"}},
	}
	cases := map[string]mintArgs{}
	for k, c := range refused {
		cases[k] = c.args
	}
	for k, c := range accepted {
		cases[k] = c.args
	}
	bodies := mintBodies(t, f.station, cases)

	for name, c := range refused {
		status, out := f.preview(t, bodies[name])
		msg, _ := out["error"].(string)
		if status != http.StatusUnprocessableEntity || !strings.Contains(msg, c.says) {
			t.Errorf("%s: %d %q, want 422 saying %q", name, status, msg, c.says)
		}
	}
	for name, c := range accepted {
		status, out := f.preview(t, bodies[name])
		if status == http.StatusUnprocessableEntity {
			t.Errorf("%s: refused: %v", name, out["error"])
			continue
		}
		got := mintedCells(out)
		for head, deck := range c.cells {
			if got[head] != deck {
				t.Errorf("%s: the flow has %s on deck for %s (%v), want %s", name, got[head], head, got, deck)
			}
		}
	}
}

// The pre-fill is the screen's own suggestion, so it is always legal: the
// body it makes mints, and the style it writes keeps the decks.
func TestFlowMint_ThePrefillAloneMints(t *testing.T) {
	f := newPress6(t)
	bodies := mintBodies(t, f.station, map[string]mintArgs{"prefill": {f.twoSpot, press6Pair, press6Prefil}})
	resp := doRequest(t, f.router, "POST", "/api/processes/"+itoa(f.processID)+"/flow/mint", bodies["prefill"], nil)
	assertStatus(t, resp, http.StatusOK)
	got := flowBody(t, resp)
	if got["minted"] != true {
		t.Fatalf("mint = %v, want a minted style", got)
	}
	claims, err := f.db.ListStyleNodeClaims(int64(got["style_id"].(float64)))
	must(t, err, "claims")
	decks := map[string]string{}
	for _, c := range claims {
		decks[c.CoreNodeName] = c.PairedCoreNode
	}
	if decks["PLN_010"] != "PLN_005" || decks["PLN_012"] != "PLN_008" || len(decks) != 2 {
		t.Errorf("minted claims = %v, want PLN_010←PLN_005 and PLN_012←PLN_008", decks)
	}
}

// press6GroupedJSON is "Two spot press index" as an engineer saves it with
// groups (SYNTH-round2 §2, on this fixture's source): the cells exactly as
// the plain recipe has them, and beside them Left Hand on PLN_006 or PLN_010
// with PLN_005 on deck, Right Hand on PLN_007 or PLN_012 with PLN_008 on
// deck and its own reorder point.
const press6GroupedJSON = `{"cells":[
 {"core_node_name":"PLN_006","role":"produce","swap_mode":"two_robot_press_index","paired_core_node":"PLN_005","inbound_source":"SMN_BUF_100","outbound_destination":"ULN_002"},
 {"core_node_name":"PLN_007","role":"produce","swap_mode":"two_robot_press_index","paired_core_node":"PLN_008","inbound_source":"SMN_BUF_100","outbound_destination":"ULN_005"}],
"groups":[
 {"of":"PLN_006","name":"Left Hand","spots":[
  {"core_node_name":"PLN_006","paired_core_node":["PLN_005"]},
  {"core_node_name":"PLN_010","paired_core_node":["PLN_005"]}]},
 {"of":"PLN_007","name":"Right Hand","spots":[
  {"core_node_name":"PLN_007","paired_core_node":["PLN_008"]},
  {"core_node_name":"PLN_012","paired_core_node":["PLN_008"]}],
  "advanced":{"reorder_point":1,"auto_reorder":true}}]}`

// TestFlowMint_ARecipeCarryingGroupsMintsItsCells is the rollback pin: a
// recipe whose flow_json carries `groups` beside its cells is stored, read
// and minted by its cells. Written green at spots-v0, where the key is
// unknown and ignored, and kept green after it: a binary rolled back past
// recipe groups runs every grouped recipe as its cells and takes none off
// the floor (SYNTH-round2 row 1, which is why groups sit beside the cells).
func TestFlowMint_ARecipeCarryingGroupsMintsItsCells(t *testing.T) {
	f := newPress6(t)
	id, err := f.db.CreateFlowPreset(processes.FlowPresetInput{ProcessID: f.processID, Name: "Two spot grouped",
		FlowJSON: press6GroupedJSON, CreatedBy: "test"})
	must(t, err, "store the grouped recipe")
	bodies := mintBodies(t, f.station, map[string]mintArgs{"unmoved": {id, press6Pair, nil}})
	resp := doRequest(t, f.router, "POST", "/api/processes/"+itoa(f.processID)+"/flow/mint", bodies["unmoved"], nil)
	assertStatus(t, resp, http.StatusOK)
	got := flowBody(t, resp)
	if got["minted"] != true {
		t.Fatalf("mint = %v, want a minted style", got)
	}
	claims, err := f.db.ListStyleNodeClaims(int64(got["style_id"].(float64)))
	must(t, err, "claims")
	cells := map[string]string{}
	for _, c := range claims {
		cells[c.CoreNodeName] = c.PairedCoreNode + " → " + c.OutboundDestination
	}
	want := map[string]string{"PLN_006": "PLN_005 → ULN_002", "PLN_007": "PLN_008 → ULN_005"}
	if !maps.Equal(cells, want) {
		t.Errorf("minted claims = %v, want the recipe's cells %v", cells, want)
	}
}

// An archived version is not an enabled recipe: its pairings are no longer
// known, so a spot that only it paired is refused.
func TestFlowMint_AnArchivedRecipeGrantsNothing(t *testing.T) {
	f := newPress6(t)
	bodies := mintBodies(t, f.station, map[string]mintArgs{
		"to the one-off's spot": {f.twoSpot, press6Pair, withPrefill(map[string]string{"PLN_006": "PLN_009"})},
	})
	if status, out := f.preview(t, bodies["to the one-off's spot"]); status == http.StatusUnprocessableEntity {
		t.Fatalf("with One spot enabled the move is refused: %v", out["error"])
	}
	must(t, f.db.ArchiveFlowPreset(f.oneSpot), "archive One spot")
	status, out := f.preview(t, bodies["to the one-off's spot"])
	if msg, _ := out["error"].(string); status != http.StatusUnprocessableEntity ||
		!strings.Contains(msg, "PLN_009 is not a press spot your recipes set up with PLN_005 on deck") {
		t.Errorf("with One spot archived: %d %q, want the pairing refused", status, msg)
	}
}

// spotsOf reads a card's spots: node → (job, of, targets).
type cardSpot struct {
	Node    string   `json:"node"`
	Job     string   `json:"job"`
	Of      string   `json:"of"`
	Targets []string `json:"targets"`
}

func spotsOf(t *testing.T, card map[string]any) map[string]cardSpot {
	t.Helper()
	raw, err := json.Marshal(card["spots"])
	must(t, err, "spots")
	var list []cardSpot
	must(t, json.Unmarshal(raw, &list), "decode spots")
	out := map[string]cardSpot{}
	for _, s := range list {
		out[s.Node] = s
	}
	return out
}

// TestComposerCard_TargetsAreWhatTheServerAccepts drives the card and the
// server from one fixture: for every place of every recipe and every node the
// cell has, the card lights it exactly when the server takes the move. The
// card's job words and the pre-fill come from the same read.
func TestComposerCard_TargetsAreWhatTheServerAccepts(t *testing.T) {
	f := newPress6(t)
	every := []string{"STG_TOOL_1", "STG_TOOL_2", "ULN_002", "ULN_005", "SMN_BUF_100"}
	for n := range f.nodes {
		every = append(every, n)
	}
	sort.Strings(every)

	two, one := f.card(t, f.twoSpot), f.card(t, f.oneSpot)
	want := map[int64]map[string]cardSpot{
		f.twoSpot: {
			"PLN_006": {Node: "PLN_006", Job: "spot", Targets: []string{"PLN_006", "PLN_009", "PLN_010"}},
			"PLN_005": {Node: "PLN_005", Job: "on_deck", Of: "PLN_006", Targets: []string{"PLN_005"}},
			"PLN_007": {Node: "PLN_007", Job: "spot", Targets: []string{"PLN_007", "PLN_012"}},
			"PLN_008": {Node: "PLN_008", Job: "on_deck", Of: "PLN_007", Targets: []string{"PLN_008"}},
		},
		f.oneSpot: {
			// Not PLN_010: the running style that pairs it with PLN_005 was
			// built from Two spot, and a style lends its pairings only to
			// the recipe it was built from.
			"PLN_009":    {Node: "PLN_009", Job: "spot", Targets: []string{"PLN_006", "PLN_009"}},
			"PLN_005":    {Node: "PLN_005", Job: "on_deck", Of: "PLN_009", Targets: []string{"PLN_005"}},
			"STG_TOOL_1": {Node: "STG_TOOL_1", Job: "lane", Of: "PLN_009", Targets: []string{"STG_TOOL_1", "STG_TOOL_2"}},
		},
	}
	cards := map[int64]map[string]any{f.twoSpot: two, f.oneSpot: one}
	for id, spots := range want {
		got := spotsOf(t, cards[id])
		for node, w := range spots {
			g := got[node]
			sort.Strings(g.Targets)
			if fmt.Sprint(g) != fmt.Sprint(w) {
				t.Errorf("recipe %d %s: card says %+v, want %+v", id, node, g, w)
			}
		}
		if len(got) != len(spots) {
			t.Errorf("recipe %d: card has %d places, want %d", id, len(got), len(spots))
		}
	}
	lm, err := json.Marshal(two["last_moves"])
	must(t, err, "marshal the pre-fill")
	if string(lm) != `{"PLN_006":"PLN_010","PLN_007":"PLN_012"}` {
		t.Errorf("Two spot's pre-fill = %s, want the running style's heads", lm)
	}

	cases := map[string]mintArgs{}
	for id, card := range cards {
		codes := press6Pair
		if id == f.oneSpot {
			codes = press6One
		}
		for node := range spotsOf(t, card) {
			for _, to := range every {
				cases[fmt.Sprintf("%d|%s|%s", id, node, to)] = mintArgs{id, codes, map[string]string{node: to}}
			}
		}
	}
	bodies := mintBodies(t, f.station, cases)
	for key, body := range bodies {
		parts := strings.Split(key, "|")
		id, node, to := parts[0], parts[1], parts[2]
		var card map[string]any
		for cid, c := range cards {
			if fmt.Sprint(cid) == id {
				card = c
			}
		}
		lit := false
		for _, n := range spotsOf(t, card)[node].Targets {
			lit = lit || n == to
		}
		status, out := f.preview(t, body)
		if took := status != http.StatusUnprocessableEntity; took != lit {
			t.Errorf("recipe %s, %s → %s: the card lights it %v, the server takes it %v (%d %v)",
				id, node, to, lit, took, status, out["error"])
		}
	}
}

// The pre-fill is filtered through the same targets before it ships: a
// last-used style whose press sits on another recipe's deck suggests nothing,
// rather than a layout the server would refuse.
func TestComposerCard_APrefillOutsideTheTargetsIsDropped(t *testing.T) {
	f := newPress6(t)
	// The newest style built from One spot runs its press on PLN_008, the
	// right-hand deck: a layout only a free-for-all Spots screen could make.
	f.style(t, "SYN-ODD", f.oneSpot, "2026-10-22 09:00:00", claim("PLN_008", "PLN_005", "SYN-P6-F", "ULN_005"))
	card := f.card(t, f.oneSpot)
	if lm, ok := card["last_moves"]; ok {
		t.Errorf("One spot's pre-fill = %v, want none: PLN_008 is an on-deck spot, never a press spot", lm)
	}
}

// A sequential pair is two spots whatever the picture calls them: both sides
// of a running A/B line read "back" on the cell picture (each names the
// other), and the card still gives each a spot's job. Kind is never read.
func TestComposerCard_ASequentialPairIsTwoSpotsWhateverTheirKind(t *testing.T) {
	db := testdb.Open(t)
	pid, err := db.CreateProcess("Line 9", "", "", "", false)
	must(t, err, "process")
	sid, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Code: "line-9", Name: "Line 9", Enabled: true})
	must(t, err, "station")
	for i, n := range []string{"SEQ_A", "SEQ_B", "SEQ_C"} {
		_, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, OperatorStationID: &sid,
			CoreNodeName: n, Name: n, Sequence: i + 1, Enabled: true})
		must(t, err, "position "+n)
	}
	seq := func(a, b string) domain.FlowCell {
		return domain.FlowCell{CoreNodeName: a, Role: "consume", SwapMode: "sequential", PairedCoreNode: b}
	}
	f := press6{db: db, processID: pid, station: sid}
	ab := f.recipe(t, "A/B", seq("SEQ_A", "SEQ_B"), seq("SEQ_B", "SEQ_A"))
	f.recipe(t, "C/B", seq("SEQ_C", "SEQ_B"), seq("SEQ_B", "SEQ_C"))
	style, err := db.CreateStyle("SYN-SEQ", "", pid)
	must(t, err, "style")
	for _, c := range []processes.NodeClaimInput{
		{StyleID: style, CoreNodeName: "SEQ_A", Role: "consume", SwapMode: "sequential", PayloadCode: "SYN-S", PairedCoreNode: "SEQ_B"},
		{StyleID: style, CoreNodeName: "SEQ_B", Role: "consume", SwapMode: "sequential", PayloadCode: "SYN-S", PairedCoreNode: "SEQ_A"},
	} {
		_, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, c)
		must(t, err, "claim")
	}
	must(t, db.SetActiveStyle(pid, &style), "run it")
	_, f.router = realFlowRouter(t, db)

	resp := doRequest(t, f.router, "GET", fmt.Sprintf("/api/operator-stations/%d/cell", sid), nil, nil)
	assertStatus(t, resp, http.StatusOK)
	var pic domain.CellPicture
	must(t, json.NewDecoder(resp.Body).Decode(&pic), "decode picture")
	for _, p := range pic.Positions {
		if (p.CoreNodeName == "SEQ_A" || p.CoreNodeName == "SEQ_B") && p.Kind != "back" {
			t.Fatalf("%s is %q on the picture; this fixture is for a pair the picture calls back", p.CoreNodeName, p.Kind)
		}
	}
	got := spotsOf(t, f.card(t, ab))
	if a := got["SEQ_A"]; a.Job != "spot" || fmt.Sprint(a.Targets) != "[SEQ_A SEQ_C]" {
		t.Errorf("SEQ_A = %+v, want a spot that may take SEQ_C (C/B pairs it with SEQ_B)", a)
	}
	if b := got["SEQ_B"]; b.Job != "spot" || fmt.Sprint(b.Targets) != "[SEQ_B]" {
		t.Errorf("SEQ_B = %+v, want a spot that stays (no recipe pairs anything else with SEQ_A)", b)
	}
}

// A recipe naming a node the process no longer has is refused at the preview
// and at the mint, by name, before anything is placed, and its card carries
// the same sentence (owner, 2026-10-22: refuse and flag, never carry the last
// style's node). Re-saved under its own name with the current routing it is
// v2, its card is clean and it mints: the deploy note's step at Springfield,
// where "Two spot press index" still sends cell 2 to ULN_003.
func TestFlowMint_AStaleRecipeIsRefusedUntilReSaved(t *testing.T) {
	f := newPress6(t)
	unloader := *mustNode(t, f, "ULN_002").OperatorStationID
	uln3, err := f.db.CreateProcessNode(processes.NodeInput{ProcessID: f.processID, OperatorStationID: &unloader,
		CoreNodeName: "ULN_003", Name: "ULN_003", Sequence: 3, Enabled: true})
	must(t, err, "ULN_003")
	old := f.recipe(t, "Two spot copy",
		cell("PLN_006", "PLN_005", "SMN_BUF_100", "ULN_002"),
		cell("PLN_007", "PLN_008", "SMN_BUF_100", "ULN_003"))
	must(t, f.db.DeleteProcessNode(uln3), "the unloader loses ULN_003")

	const said = "Two spot copy sends cell 2 to ULN_003, which is no longer a position here — " +
		"an engineer re-saves the recipe on the desktop"
	if got := f.card(t, old)["stale"]; fmt.Sprint(got) != fmt.Sprint([]any{said}) {
		t.Errorf("the card's stale = %v, want [%s]", got, said)
	}
	bodies := mintBodies(t, f.station, map[string]mintArgs{
		"old": {old, press6Pair, nil}, "old moved": {old, press6Pair, press6Prefil},
	})
	for _, name := range []string{"old", "old moved"} {
		status, out := f.preview(t, bodies[name])
		if msg, _ := out["error"].(string); status != http.StatusUnprocessableEntity || msg != said {
			t.Errorf("preview %s: %d %q, want 422 %q", name, status, msg, said)
		}
	}
	resp := doRequest(t, f.router, "POST", "/api/processes/"+itoa(f.processID)+"/flow/mint", bodies["old"], nil)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	if msg, _ := decodeBody(t, resp)["error"].(string); msg != said {
		t.Errorf("mint: %q, want %q", msg, said)
	}

	fixed := f.recipe(t, "Two spot copy",
		cell("PLN_006", "PLN_005", "Press Buffer Zone", "ULN_002"),
		cell("PLN_007", "PLN_008", "Press Buffer Zone", "ULN_005"))
	p, err := f.db.GetFlowPreset(fixed)
	must(t, err, "the re-saved recipe")
	if p.Version != 2 {
		t.Fatalf("the re-save is v%d, want v2 of the same name", p.Version)
	}
	if got, has := f.card(t, fixed)["stale"]; has {
		t.Errorf("the re-saved card still says %v", got)
	}
	bodies = mintBodies(t, f.station, map[string]mintArgs{"fixed": {fixed, press6Pair, nil}})
	resp = doRequest(t, f.router, "POST", "/api/processes/"+itoa(f.processID)+"/flow/mint", bodies["fixed"], nil)
	assertStatus(t, resp, http.StatusOK)
}

func mustNode(t *testing.T, f press6, name string) *processes.Node {
	t.Helper()
	n, err := f.db.GetProcessNode(f.nodes[name])
	must(t, err, name)
	return n
}

// ── recipe groups (2026-10-24): the group is the whole answer ───────────────
//
// SYNTH-round2 rows 7 and 8: a grouped recipe's spot moves only to its group's
// spots, an attached place only within the list its spot's stand has, and
// nothing is lent from another recipe or style. An on-deck spot follows its
// spot: the card says where (carry), and the Spots step's body carries the
// deck's move with the spot's.

// pressGrouped is press6 with "Two spot press index" re-saved by the engineer
// with groups (v2): Left Hand on PLN_006 or PLN_010 fed by PLN_005, and on
// PLN_013 fed only by PLN_014 — SYNTHETIC, Springfield has no such list; it is
// here so a tied list exists, the shape an on-deck spot must follow its spot
// through — and Right Hand on PLN_007 or PLN_012 fed by PLN_008, with its own
// reorder point and curtain.
type pressGrouped struct {
	press6
	grouped int64
}

func newPressGrouped(t *testing.T) pressGrouped {
	t.Helper()
	f := pressGrouped{press6: newPress6(t)}
	groups := press6Groups()
	groups[0]["spots"] = append(groups[0]["spots"].([]map[string]any),
		map[string]any{"core_node_name": "PLN_013", "paired_core_node": []string{"PLN_014"}})
	groups[1]["advanced"] = map[string]any{"reorder_point": 1, "auto_reorder": true, "curtain_required": true}
	resp := f.saveRecipe(t, f.engineer(t), "Two spot press index", press6TwoSpotCells, groups)
	assertStatus(t, resp, http.StatusOK)
	f.grouped = int64(decodeBody(t, resp)["id"].(float64))
	return f
}

// groupedSpot is a card place with the grouped fields.
type groupedSpot struct {
	Node    string                       `json:"node"`
	Job     string                       `json:"job"`
	Of      string                       `json:"of,omitempty"`
	Targets []string                     `json:"targets"`
	Group   string                       `json:"group,omitempty"`
	At      map[string][]string          `json:"at,omitempty"`
	Carry   map[string]map[string]string `json:"carry,omitempty"`
}

func groupedSpotsOf(t *testing.T, card map[string]any) map[string]groupedSpot {
	t.Helper()
	raw, err := json.Marshal(card["spots"])
	must(t, err, "spots")
	var list []groupedSpot
	must(t, json.Unmarshal(raw, &list), "decode spots")
	out := map[string]groupedSpot{}
	for _, s := range list {
		sort.Strings(s.Targets)
		out[s.Node] = s
	}
	return out
}

// TestComposerCard_AGroupedCardOffersOnlyItsGroups is v0's card ⇔ server table
// over the grouped recipe: the card's places are its groups' (no PLN_009, which
// v0 lent from "One spot press index"), and for every place and every node the
// card lights it exactly when the server takes the move — a spot's move with
// the carry the card gives for it, as the Spots step sends it.
func TestComposerCard_AGroupedCardOffersOnlyItsGroups(t *testing.T) {
	f := newPressGrouped(t)
	card := f.card(t, f.grouped)
	got := groupedSpotsOf(t, card)
	want := map[string]groupedSpot{
		"PLN_006": {Node: "PLN_006", Job: "spot", Group: "Left Hand", Targets: []string{"PLN_006", "PLN_010", "PLN_013"},
			Carry: map[string]map[string]string{"PLN_013": {"PLN_005": "PLN_014"}}},
		"PLN_007": {Node: "PLN_007", Job: "spot", Group: "Right Hand", Targets: []string{"PLN_007", "PLN_012"}},
		"PLN_005": {Node: "PLN_005", Job: "on_deck", Of: "PLN_006", Group: "Left Hand", Targets: []string{"PLN_005"},
			At: map[string][]string{"PLN_006": {"PLN_005"}, "PLN_010": {"PLN_005"}, "PLN_013": {"PLN_014"}}},
		"PLN_008": {Node: "PLN_008", Job: "on_deck", Of: "PLN_007", Group: "Right Hand", Targets: []string{"PLN_008"},
			At: map[string][]string{"PLN_007": {"PLN_008"}, "PLN_012": {"PLN_008"}}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the grouped card's places:\n got %+v\nwant %+v", got, want)
	}
	lm, err := json.Marshal(card["last_moves"])
	must(t, err, "marshal the pre-fill")
	if string(lm) != `{"PLN_006":"PLN_010","PLN_007":"PLN_012"}` {
		t.Errorf("the grouped pre-fill = %s, want the running style's heads", lm)
	}

	every := []string{"STG_TOOL_1", "STG_TOOL_2", "ULN_002", "ULN_005", "SMN_BUF_100"}
	for n := range f.nodes {
		every = append(every, n)
	}
	sort.Strings(every)
	cases := map[string]mintArgs{}
	for node, s := range got {
		for _, to := range every {
			moves := map[string]string{node: to}
			for from, carried := range s.Carry[to] {
				moves[from] = carried
			}
			cases[node+"|"+to] = mintArgs{f.grouped, press6Pair, moves}
		}
	}
	for key, body := range mintBodies(t, f.station, cases) {
		node, to, _ := strings.Cut(key, "|")
		lit := false
		for _, n := range got[node].Targets {
			lit = lit || n == to
		}
		status, out := f.preview(t, body)
		if took := status != http.StatusUnprocessableEntity; took != lit {
			t.Errorf("%s → %s: the card lights it %v, the server takes it %v (%d %v)", node, to, lit, took, status, out["error"])
		}
	}
}

// TestFlowMint_AGroupedRecipeMovesOnlyWithinItsGroups is the server's
// final-state check, in the group's words: v0's lending took PLN_006 to
// PLN_009 and the group refuses it; a spot moved to PLN_013 is refused
// without its deck and taken with the carry; the decks never cross. "One spot
// press index", still ungrouped on the same process, lends and moves as v0.
func TestFlowMint_AGroupedRecipeMovesOnlyWithinItsGroups(t *testing.T) {
	f := newPressGrouped(t)
	refused := map[string]struct {
		args mintArgs
		says string
	}{
		"to the one-off's spot": {mintArgs{f.grouped, press6Pair, withPrefill(map[string]string{"PLN_006": "PLN_009"})},
			"PLN_009 is not a press spot of Left Hand — an engineer adds it on the desktop"},
		"the deck to an idle position": {mintArgs{f.grouped, press6Pair, withPrefill(map[string]string{"PLN_005": "PLN_011"})},
			"PLN_011 is not an on-deck spot Left Hand has for PLN_010 — an engineer adds one on the desktop"},
		"a spot moved without its deck": {mintArgs{f.grouped, press6Pair, withPrefill(map[string]string{"PLN_006": "PLN_013"})},
			"PLN_005 is not an on-deck spot Left Hand has for PLN_013 — an engineer adds one on the desktop"},
		"spots swap": {mintArgs{f.grouped, press6Pair, map[string]string{"PLN_006": "PLN_007", "PLN_007": "PLN_006"}},
			"PLN_007 is not a press spot of Left Hand — an engineer adds it on the desktop"},
		"decks cross": {mintArgs{f.grouped, press6Pair, map[string]string{"PLN_005": "PLN_008", "PLN_008": "PLN_005"}},
			"PLN_008 is not an on-deck spot Left Hand has for PLN_006 — an engineer adds one on the desktop"},
		"press to a lane": {mintArgs{f.grouped, press6Pair, withPrefill(map[string]string{"PLN_006": "STG_TOOL_2"})},
			"STG_TOOL_2 is a staging lane; a press spot cannot move onto it"},
		"press onto the other side's deck": {mintArgs{f.grouped, press6Pair, map[string]string{"PLN_006": "PLN_008"}},
			"PLN_008 is an on-deck spot; a press spot cannot move onto it"},
		"press to another station": {mintArgs{f.grouped, press6Pair, withPrefill(map[string]string{"PLN_006": "ULN_002"})},
			"ULN_002 is another station's position"},
	}
	accepted := map[string]struct {
		args  mintArgs
		cells map[string]string
	}{
		"the pre-fill alone": {mintArgs{f.grouped, press6Pair, press6Prefil},
			map[string]string{"PLN_010": "PLN_005", "PLN_012": "PLN_008"}},
		"a tied list, the deck carried": {mintArgs{f.grouped, press6Pair, withPrefill(map[string]string{"PLN_006": "PLN_013", "PLN_005": "PLN_014"})},
			map[string]string{"PLN_013": "PLN_014", "PLN_012": "PLN_008"}},
		"press back to its own": {mintArgs{f.grouped, press6Pair, withPrefill(map[string]string{"PLN_006": ""})},
			map[string]string{"PLN_006": "PLN_005", "PLN_012": "PLN_008"}},
		"the ungrouped recipe still lends": {mintArgs{f.oneSpot, press6One, map[string]string{"PLN_009": "PLN_006"}},
			map[string]string{"PLN_006": "PLN_005"}},
	}
	cases := map[string]mintArgs{}
	for k, c := range refused {
		cases[k] = c.args
	}
	for k, c := range accepted {
		cases[k] = c.args
	}
	bodies := mintBodies(t, f.station, cases)
	for name, c := range refused {
		status, out := f.preview(t, bodies[name])
		if msg, _ := out["error"].(string); status != http.StatusUnprocessableEntity || !strings.Contains(msg, c.says) {
			t.Errorf("%s: %d %q, want 422 saying %q", name, status, msg, c.says)
		}
	}
	for name, c := range accepted {
		status, out := f.preview(t, bodies[name])
		if status == http.StatusUnprocessableEntity {
			t.Errorf("%s: refused: %v", name, out["error"])
			continue
		}
		got := mintedCells(out)
		for head, deck := range c.cells {
			if got[head] != deck {
				t.Errorf("%s: the flow has %s on deck for %s (%v), want %s", name, got[head], head, got, deck)
			}
		}
	}
}

// A grouped recipe's pre-fill is kept slot by slot: the last style that used
// it ran Left Hand on PLN_013 with PLN_005 on deck, which Left Hand does not
// set up, and Right Hand on PLN_012, which it does. Right Hand's half ships;
// v0 kept both, lent by that same style.
func TestComposerCard_AGroupedPrefillKeepsTheSlotsThatStand(t *testing.T) {
	f := newPressGrouped(t)
	f.style(t, "SYN-ODD", f.grouped, "2026-10-23 09:00:00",
		claim("PLN_013", "PLN_005", "SYN-P6-F", "ULN_002"), claim("PLN_012", "PLN_008", "SYN-P6-G", "ULN_005"))
	lm, err := json.Marshal(f.card(t, f.grouped)["last_moves"])
	must(t, err, "marshal the pre-fill")
	if string(lm) != `{"PLN_007":"PLN_012"}` {
		t.Errorf("the grouped pre-fill = %s, want Right Hand's alone", lm)
	}
}

// ── the mint, grouped (SYNTH-round2 rows 4 and 24) ──────────────────────────

// mintedClaims mints a body and returns the new style's claims by node.
func (f press6) mintedClaims(t *testing.T, body map[string]any) map[string]processes.NodeClaim {
	t.Helper()
	resp := doRequest(t, f.router, "POST", "/api/processes/"+itoa(f.processID)+"/flow/mint", body, nil)
	assertStatus(t, resp, http.StatusOK)
	got := flowBody(t, resp)
	if got["minted"] != true {
		t.Fatalf("mint = %v, want a minted style", got)
	}
	claims, err := f.db.ListStyleNodeClaims(int64(got["style_id"].(float64)))
	must(t, err, "claims")
	out := map[string]processes.NodeClaim{}
	for _, c := range claims {
		out[c.CoreNodeName] = c
	}
	return out
}

// A group's Advanced is laid over the minted defaults on every cell of its
// slot: Right Hand's reorder point (its source "manual": an engineer set it),
// auto-reorder and curtain, kept through the mint's curtain stamp (D10, which
// ORs the node's own curtain in). Left Hand says nothing and mints as today,
// with the curtain its node already has.
func TestFlowMint_AGroupsAdvancedIsLaidOnItsCells(t *testing.T) {
	f := newPressGrouped(t)
	_, err := f.db.Exec(`UPDATE style_node_claims SET curtain_required = 1 WHERE style_id = ? AND core_node_name = 'PLN_010'`, f.running)
	must(t, err, "PLN_010's curtain")
	bodies := mintBodies(t, f.station, map[string]mintArgs{"prefill": {f.grouped, press6Pair, press6Prefil}})
	claims := f.mintedClaims(t, bodies["prefill"])
	type policy struct {
		Point   int
		Source  string
		Auto    bool
		Curtain bool
	}
	got := map[string]policy{}
	for n, c := range claims {
		got[n] = policy{c.ReorderPoint, c.ReorderPointSource, c.AutoReorder, c.CurtainRequired}
	}
	want := map[string]policy{
		"PLN_010": {0, "unset", false, true},
		"PLN_012": {1, "manual", true, true},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("minted policy = %v, want %v", got, want)
	}
}

// A part goes where it last ran once the Spots step has moved the recipe
// (round 1's F7: the parts were placed before the moves, on the recipe's own
// nodes, so the history never matched a moved cell), and on a grouped recipe
// onto any of its group's spots. Tapped second, SYN-P6-A still lands on the
// slot of PLN_010, where the running style has it.
func TestFlowMint_APartGoesWhereItLastRanAfterTheMoves(t *testing.T) {
	f := newPressGrouped(t)
	bodies := mintBodies(t, f.station, map[string]mintArgs{
		// Two spot v1, ungrouped, moved onto the running style's spots.
		"moved":   {f.twoSpot, []string{"SYN-P6-B", "SYN-P6-A"}, press6Prefil},
		"grouped": {f.grouped, []string{"SYN-P6-C", "SYN-P6-A"}, nil},
	})
	parts := func(claims map[string]processes.NodeClaim) map[string]string {
		out := map[string]string{}
		for n, c := range claims {
			out[n] = c.PayloadCode
		}
		return out
	}
	if got := parts(f.mintedClaims(t, bodies["moved"])); fmt.Sprint(got) != "map[PLN_010:SYN-P6-A PLN_012:SYN-P6-B]" {
		t.Errorf("moved mint placed %v, want SYN-P6-A on PLN_010 where it last ran", got)
	}
	// Unmoved: Left Hand stands on PLN_006, and PLN_010 is one of its spots.
	if got := parts(f.mintedClaims(t, bodies["grouped"])); fmt.Sprint(got) != "map[PLN_006:SYN-P6-A PLN_007:SYN-P6-C]" {
		t.Errorf("grouped mint placed %v, want SYN-P6-A on Left Hand, which ran it on PLN_010", got)
	}
}
