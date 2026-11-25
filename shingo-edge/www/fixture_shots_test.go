//go:build shots

package www

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/shared/scenefixtures"
	"shingoedge/domain"
)

// fixture_shots_test.go — the shots beside the mock (PROMPT-lead-recipe-
// groups, "Shots, beside the mock"), over SYNTHETIC cells written here.
//
// THE CELLS ARE INVENTED, IN THE REAL ONES' SHAPES. Round coordinates off an
// invented origin (the picture lane's geometry pins use the same numbers):
// Press 6's three columns of press spots (two 0.35 m apart, one across the
// press) in three rows 0.4 m apart with an on-deck spot beyond each end; the
// Hopkinsville press's two mirrored halves with their decks 0.15 m off the
// back line; a weld line of three that drifts 0.3 m. Node names are Press 6's
// where the cell is Press 6's shape, as v0's committed fixture names them; no
// coordinate of a plant is here.
//
// The same machinery as the real cells (seedRealCell, the drivers, shotProbe,
// INDEX.md), and two more things:
//
//   - the HMI's Which recipe and Go screens, and the desktop draft's states
//     (no focus, a group hovered, a row clicked, the Advanced sheet in group
//     scope, "On the floor") through the desktop lane's one-shot hash entries.
//     A state whose entry or marker this tree does not have yet is shot as
//     <state>-TODO.png and named in INDEX.md, not failed;
//   - plant A's board flow panel before and after: the u4- PNGs
//     TestComposerShots writes, from a run at the base and a run at this tree
//     (SPOTS_U4_BASE, SPOTS_U4_TIP), compared pixel for pixel. A cell the
//     scope gate leaves on today's picture must be identical, or this fails.
//
// Run through scripts/real-cell-shots.sh --fixtures, which sets:
//
//	SPOTS_FIXTURES_OUT   the folder for the PNGs and INDEX.md (created)
//	SPOTS_U4_BASE        a composer-shots folder from the base tree (optional)
//	SPOTS_U4_TIP         a composer-shots folder from this tree (optional)
//	SPOTS_U4_BASE_TREE   the base's name, for INDEX.md
//	SPOTS_SHOTS_TREE     this tree's, for INDEX.md
func TestFixtureShots(t *testing.T) {
	out := os.Getenv("SPOTS_FIXTURES_OUT")
	if out == "" {
		t.Skip("SPOTS_FIXTURES_OUT not set; run scripts/real-cell-shots.sh --fixtures")
	}
	chrome := chromeBinary()
	if chrome == "" {
		t.Fatal("no Chrome binary: set COMPOSER_SHOTS_CHROME")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", out, err)
	}
	checkShotProbe(t, chrome, out)
	var rows []cellRow
	for _, fx := range fixtureCells() {
		row := cellRow{title: fx.title, cell: fx.cell, modes: fx.cell.modes(), recipe: true,
			gateRelation: fx.cell.changeover(), recipeLines: fx.recipeLines, notes: fx.notes}
		t.Run(fx.key, func(t *testing.T) {
			sc := seedRealCell(t, fx.cell, fx.opts, mountRealCellDriver)
			row.grouped = sc.grouped
			cr := chromeRun{t: t, chrome: chrome, out: out}
			for _, s := range fx.shots(t, sc, fx.cell) {
				row.shots = append(row.shots, cr.measured(sc.srv.URL, fx.key+"-", s))
			}
			checkStill(t, row.shots)
		})
		rows = append(rows, row)
	}
	u4 := plantAU4(t, out)
	var also []string
	for _, r := range u4 {
		also = append(also, r.base, r.tip)
	}
	md := fixtureIndex(rows, u4, staleShots(t, out, rows, also...))
	if err := os.WriteFile(filepath.Join(out, "INDEX.md"), []byte(md), 0o644); err != nil {
		t.Fatalf("write INDEX.md: %v", err)
	}
	t.Logf("wrote %s", filepath.Join(out, "INDEX.md"))
}

// ── the cells ──────────────────────────────────────────────────────────────

// fixtureCell is one synthetic cell: how it is seeded and what is shot.
type fixtureCell struct {
	key         string // the PNGs' prefix
	title       string // its INDEX.md heading
	cell        realCell
	opts        seedOpts
	recipeLines []string
	notes       []string
	shots       func(t *testing.T, sc seededCell, c realCell) []shotSpec
}

// The invented origin, in metres; every coordinate below is off it.
const fxX0, fxY0 = 100.0, 200.0

// fxAt is a position at (x, y) off the origin, bound to station (0: none).
func fxAt(name string, seq int, x, y float64, station int64, claim *domain.CellClaim) domain.CellPosition {
	px, py := fxX0+x, fxY0+y
	return domain.CellPosition{CoreNodeName: name, Sequence: seq, X: &px, Y: &py, Kind: "front",
		StationID: station, Claim: claim}
}

func fxBack(p domain.CellPosition) domain.CellPosition { p.Kind = "back"; return p }

// fxOffMap is the position with no point on the plant map.
func fxOffMap(p domain.CellPosition) domain.CellPosition { p.X, p.Y = nil, nil; return p }

func fxLane(name string, x, y float64) domain.CellStaging {
	px, py := fxX0+x, fxY0+y
	return domain.CellStaging{CoreNodeName: name, X: &px, Y: &py}
}

func fxIndex(deck, from, to string) *domain.CellClaim {
	return &domain.CellClaim{SwapMode: string(protocol.SwapModeTwoRobotPressIndex), PairedCoreNode: deck,
		InboundSource: from, OutboundDestination: to}
}

func fxSwap(lane, from, to string) *domain.CellClaim {
	return &domain.CellClaim{SwapMode: string(protocol.SwapModeTwoRobot), InboundStaging: lane,
		InboundSource: from, OutboundDestination: to}
}

// fxSeq is a Sequential A/B position: partner names its pair, blank a one-node
// line (final brief 2b).
func fxSeq(partner, from, to string) *domain.CellClaim {
	return &domain.CellClaim{SwapMode: string(protocol.SwapModeSequential), PairedCoreNode: partner,
		InboundSource: from, OutboundDestination: to}
}

// fxCell is a synthetic station 1 whose pull is itself.
func fxCell(code string, positions []domain.CellPosition, staging []domain.CellStaging, groups map[string][]string) realCell {
	pic := domain.CellPicture{Positions: positions, Staging: staging, Groups: groups, Geometry: true}
	for _, p := range positions {
		pic.Geometry = pic.Geometry && p.X != nil
	}
	return realCell{plant: "fx", station: 1, code: code, pic: pic,
		pull: &cellPull{Plant: "fx", Cells: map[int64]domain.CellPicture{1: pic}, StationIDs: true}}
}

func fxNames(names ...string) func(int64) string {
	return func(id int64) string {
		if int(id) >= 1 && int(id) <= len(names) {
			return names[id-1]
		}
		return fmt.Sprintf("Station %d", id)
	}
}

// press6Positions is the Press-6 shape with Press 6's node names: the middle
// row's outer spots (PLN_010, PLN_012) indexed from the decks at each end, as
// the plant runs it; the unloader's ULN_002 and ULN_005 another station's,
// far off.
func press6Positions() ([]domain.CellPosition, []domain.CellPosition) {
	const in = "Press Buffer Zone"
	return []domain.CellPosition{
			fxBack(fxAt("PLN_005", 1, 0, 0.55, 1, nil)),
			fxAt("PLN_006", 2, 0.6, 0, 1, nil),
			fxAt("PLN_007", 3, 3.5, 0, 1, nil),
			fxBack(fxAt("PLN_008", 4, 4.5, 0.6, 1, nil)),
			fxAt("PLN_009", 5, 0.95, 0, 1, nil),
			fxAt("PLN_010", 6, 0.6, 0.4, 1, fxIndex("PLN_005", in, "ULN_002")),
			fxAt("PLN_011", 7, 0.95, 0.4, 1, nil),
			fxAt("PLN_012", 8, 3.5, 0.4, 1, fxIndex("PLN_008", in, "ULN_005")),
			fxAt("PLN_013", 9, 0.6, 0.8, 1, nil),
			fxAt("PLN_015", 10, 3.5, 0.8, 1, nil),
			fxAt("PLN_014", 11, 0.95, 0.8, 1, nil),
		}, []domain.CellPosition{
			fxAt("ULN_002", 12, 60, 30, 2, nil),
			fxAt("ULN_005", 13, 62, 30, 2, nil),
		}
}

var press6DockGroups = map[string][]string{"Press Buffer Zone": {"SMN_B01", "SMN_B02", "SMN_B03", "SMN_B04"}}

const press6RecipeLine = "\"Two spot press index\" as SYNTH-round2 §2 writes it (PLN_006←PLN_005 → ULN_002, " +
	"PLN_007←PLN_008 → ULN_005, in from Press Buffer Zone), groups Left Hand and Right Hand, the running claims' " +
	"provenance on it: the card pre-fills PLN_006 → PLN_010 and PLN_007 → PLN_012"

// The draft states' markers: a state counts as reached when the page shows
// its marker, in the words the desktop's group editor draws.
const (
	draftCardsMarker = "Add a press spot"  // the group cards (SYNTH row 9's chip)
	draftFloorMarker = "Left Hand runs on" // the "On the floor" block (SYNTH row 10)
)

func fixtureCells() []fixtureCell {
	press, unloader := press6Positions()
	offMap := slices.Clone(press)
	for i := range offMap {
		if offMap[i].CoreNodeName == "PLN_008" {
			offMap[i] = fxOffMap(offMap[i])
		}
	}
	const (
		hkIn, hkOut = "Supermarket Empty Totes", "Supermarket Area"
	)
	hk := []domain.CellPosition{
		fxBack(fxAt("PLN_05", 1, 0, 0.15, 0, nil)),
		fxAt("PLN_01", 2, 5.0, 0, 1, nil),
		fxAt("PLN_03", 3, 4.5, 1.3, 1, fxSwap("PLN_02", hkIn, hkOut)),
		fxBack(fxAt("PLN_02", 4, 7.5, 0.15, 0, nil)),
		fxAt("PLN_04", 5, 2.5, 0, 1, nil),
		fxAt("PLN_06", 6, 3.0, 1.3, 1, fxSwap("PLN_05", hkIn, hkOut)),
	}
	hkCell := func(head, deck string, mode protocol.SwapMode) domain.FlowCell {
		c := domain.FlowCell{CoreNodeName: head, Role: protocol.ClaimRoleProduce, SwapMode: mode,
			InboundSource: hkIn, OutboundDestination: hkOut}
		if mode == protocol.SwapModeTwoRobotPressIndex {
			c.PairedCoreNode = deck
		} else {
			c.InboundStaging = deck
		}
		return c
	}
	weld := []domain.CellPosition{
		fxAt("ALN_01", 1, 0, 0, 1, fxSwap("SLN_01", "SMN_W01", "SMN_W01")),
		fxAt("ALN_02", 2, 2.5, 0.15, 1, fxSwap("SLN_02", "SMN_W02", "SMN_W02")),
		fxAt("ALN_03", 3, 5.0, 0.3, 1, fxSwap("SLN_02", "SMN_W03", "SMN_W03")),
	}
	grouped := recipeSeed{name: press6RecipeName, json: press6Recipe, stamp: true}
	// A CONSUME LINE WITH A ONE-NODE SEQUENTIAL A/B POSITION (final brief 2d):
	// SYN-CL-01 runs Sequential A/B with no partner, beside an A/B pair and a
	// 2‑Robot Swap; SYN-CL-05 is a free back position the partner pick offers.
	// A line 1.5 m a spot, invented. The fresh line has the same shape and no
	// flow yet, for Setup's First style.
	const clIn, clOut = "SYN-MKT-IN", "SYN-MKT-OUT"
	consumeLine := []domain.CellPosition{
		fxAt("SYN-CL-01", 1, 0, 0, 1, fxSeq("", clIn, clOut)),
		fxAt("SYN-CL-02", 2, 1.5, 0, 1, fxSeq("SYN-CL-03", clIn, clOut)),
		fxAt("SYN-CL-03", 3, 3.0, 0, 1, fxSeq("SYN-CL-02", clIn, clOut)),
		fxAt("SYN-CL-04", 4, 4.5, 0, 1, fxSwap("SYN-SLN-04", clIn, clOut)),
		fxBack(fxAt("SYN-CL-05", 5, 6.0, 1.2, 0, nil)),
	}
	freshLine := []domain.CellPosition{
		fxAt("SYN-FS-01", 1, 0, 0, 1, nil),
		fxAt("SYN-FS-02", 2, 1.5, 0, 1, nil),
		fxBack(fxAt("SYN-FS-03", 3, 3.0, 1.2, 0, nil)),
	}
	consume := func(string, string) protocol.ClaimRole { return protocol.ClaimRoleConsume }
	// THE TWO-ROBOT LINE THROUGH ITS HOME (brief G C): how a person sets up a
	// 2‑Robot Swap with no staging. SYN-HL-01 runs it the way the screen allows
	// at 9c282051, its home SYN-HOME-01 named as its source, its inbound
	// staging and its destination; SYN-HL-02 is a 2‑Robot Swap from one node
	// that stages at a lane; SYN-HL-03 one from a group (SYN-HL-MKT), which no
	// robot waits at. A line 1.5 m a spot, invented. The fresh lines have one
	// position and no flow yet, for Setup's First style: the home in every box,
	// and the home in the inbound and outbound boxes only.
	const hlHome, hlHome2, hlMkt = "SYN-HOME-01", "SYN-HOME-02", "SYN-HL-MKT"
	homeLine := []domain.CellPosition{
		fxAt("SYN-HL-01", 1, 0, 0, 1, fxSwap(hlHome, hlHome, hlHome)),
		fxAt("SYN-HL-02", 2, 1.5, 0, 1, fxSwap("SYN-SLN-02", hlHome2, hlHome2)),
		fxAt("SYN-HL-03", 3, 3.0, 0, 1, fxSwap("SYN-SLN-03", hlMkt, hlMkt)),
	}
	homeFresh := []domain.CellPosition{fxAt("SYN-HF-01", 1, 0, 0, 1, nil)}
	homeRouting := func(staging bool) []domain.RoutingNodeInput {
		rows := []domain.RoutingNodeInput{{CoreNodeName: hlHome, Role: domain.RoutingRoleSource}}
		if staging {
			rows = append(rows, domain.RoutingNodeInput{CoreNodeName: hlHome, Role: domain.RoutingRoleStaging})
		}
		return append(rows, domain.RoutingNodeInput{CoreNodeName: hlHome, Role: domain.RoutingRoleDestination})
	}
	// Off the plant map, the station draws the module picture.
	offMapLine := slices.Clone(consumeLine)
	for i := range offMapLine {
		offMapLine[i] = fxOffMap(offMapLine[i])
	}

	return []fixtureCell{
		{
			key: "consumeline", title: "Consume line shape, one position on one node (final brief 2d)",
			cell: fxCell("CL", consumeLine, []domain.CellStaging{fxLane("SYN-SLN-04", 4.5, -1.2)}, nil),
			// No recipe: at 37146992 a recipe saved from this style is refused
			// (SYN-CL-01 has no partner), and the before shots need the same seed.
			opts: seedOpts{name: fxNames("Consume line shape"), role: consume, recipes: []recipeSeed{},
				parts: []string{"SYN-CL-P1", "SYN-CL-P2", "SYN-CL-P3", "SYN-CL-P4"}},
			notes: []string{"SYN-CL-01 is a Sequential A/B position with no partner: a one-node line"},
			shots: consumeLineShots,
		},
		{
			key: "consumeline-nomap", title: "The consume line shape off the plant map: the station's module picture (final brief 2d)",
			cell: fxCell("CN", offMapLine, nil, nil),
			opts: seedOpts{name: fxNames("Consume line shape, off the map"), role: consume, recipes: []recipeSeed{},
				parts: []string{"SYN-CN-P1", "SYN-CN-P2", "SYN-CN-P3", "SYN-CN-P4"}},
			shots: func(t *testing.T, sc seededCell, c realCell) []shotSpec {
				return []shotSpec{flowShot(sc, "no mock: final brief 2d")}
			},
		},
		{
			key: "freshline", title: "Consume line shape with no flow yet, for Setup's First style (final brief 2d)",
			cell: fxCell("FS", freshLine, nil, nil),
			opts: seedOpts{name: fxNames("Fresh line shape"), role: consume, parts: []string{"SYN-FS-P1", "SYN-FS-P2"},
				routing: []domain.RoutingNodeInput{
					{CoreNodeName: clIn, Role: domain.RoutingRoleSource},
					{CoreNodeName: clOut, Role: domain.RoutingRoleDestination},
				}},
			shots: freshLineShots,
		},
		{
			key: "homeline", title: "A 2‑Robot Swap line through its home, one node as its source (brief G C)",
			cell: fxCell("HL", homeLine, []domain.CellStaging{fxLane("SYN-SLN-02", 1.5, -1.2), fxLane("SYN-SLN-03", 3.0, -1.2)},
				map[string][]string{hlMkt: {"SYN-HL-MKT-1", "SYN-HL-MKT-2"}}),
			opts: seedOpts{name: fxNames("Home line shape"), role: consume, recipes: []recipeSeed{},
				parts: []string{"SYN-HL-P1", "SYN-HL-P2", "SYN-HL-P3"}},
			notes: []string{"SYN-HL-01 names its home SYN-HOME-01 as source, inbound staging and destination: the 2‑Robot Swap with no staging as it is set up at 9c282051"},
			shots: homeLineShots,
		},
		{
			key: "homefresh", title: "A line with no flow yet, its home in the inbound, staging and outbound boxes, for Setup's First style (brief G C)",
			cell: fxCell("HF", homeFresh, nil, nil),
			opts: seedOpts{name: fxNames("Home line, fresh"), role: consume, parts: []string{"SYN-HF-P1"},
				routing: homeRouting(true)},
			shots: homeFreshShots,
		},
		{
			key: "homefresh-nostage", title: "The same, its home in the inbound and outbound boxes only (brief G C)",
			cell: fxCell("HN", homeFresh, nil, nil),
			opts: seedOpts{name: fxNames("Home line, fresh, no staging box"), role: consume, parts: []string{"SYN-HN-P1"},
				routing: homeRouting(false)},
			shots: homeFreshShots,
		},
		{
			key: "press6", title: "Press 6 shape (Press 6's node names, invented coordinates)",
			cell: fxCell("P6", press, nil, press6DockGroups),
			opts: seedOpts{name: fxNames("Press 6 shape", "Unloader shape"), extra: unloader,
				recipes: []recipeSeed{grouped}, parts: []string{"SYN-P6-01", "SYN-P6-02", "SYN-P6-A", "SYN-P6-B"}},
			recipeLines: []string{press6RecipeLine},
			notes:       []string{"ULN_002 and ULN_005 are the unloader station's positions, 60 m off: the station's picture leaves them out, a process-wide one would not"},
			shots:       press6Shots,
		},
		{
			key: "hkpress", title: "Hopkinsville-press shape (two mirrored halves, PLN_01–06, decks just off the back line)",
			cell: fxCell("HP", hk, nil, map[string][]string{
				hkIn: {"SMN_E1", "SMN_E2", "SMN_E3", "SMN_E4"}, hkOut: {"SMN_A1", "SMN_A2", "SMN_A3", "SMN_A4"}}),
			opts: seedOpts{name: fxNames("Hopkinsville press shape"),
				role: func(string, string) protocol.ClaimRole { return protocol.ClaimRoleProduce },
				recipes: []recipeSeed{
					{name: "Two spot press index", cells: []domain.FlowCell{
						hkCell("PLN_04", "PLN_05", protocol.SwapModeTwoRobotPressIndex),
						hkCell("PLN_01", "PLN_02", protocol.SwapModeTwoRobotPressIndex)}},
					{name: "Two spot swap", stamp: true, cells: []domain.FlowCell{
						hkCell("PLN_06", "PLN_05", protocol.SwapModeTwoRobot),
						hkCell("PLN_03", "PLN_02", protocol.SwapModeTwoRobot)}},
				},
				parts: []string{"SYN-HP-01", "SYN-HP-02", "SYN-HP-A", "SYN-HP-B"}},
			recipeLines: []string{
				"\"Two spot press index\": PLN_04←PLN_05, PLN_01←PLN_02 (Index Swap, the decks feed the back-line spots)",
				"\"Two spot swap\": PLN_06 staging at PLN_05, PLN_03 staging at PLN_02 (2‑Robot Swap); the running style is this one",
			},
			notes: []string{"PLN_02 and PLN_05 do two jobs: on deck under the index recipe, the staging lane under the swap — the same cell of the picture under both"},
			shots: hkPressShots,
		},
		{
			key: "weld", title: "Weld line (one row of three, drifting 0.3 m, lanes in the band)",
			cell: fxCell("WL", weld, []domain.CellStaging{fxLane("SLN_01", 0, -1.2), fxLane("SLN_02", 3.75, -1.2)}, nil),
			opts: seedOpts{name: fxNames("Weld line shape"), offers: []string{"SLN_03"},
				recipes: []recipeSeed{{name: "Three spot weld", fromStyle: true}},
				parts:   []string{"SYN-WL-01", "SYN-WL-02", "SYN-WL-03", "SYN-WL-A", "SYN-WL-B", "SYN-WL-C"}},
			recipeLines: []string{"\"Three spot weld\", saved from the running style (2‑Robot Swap, ALN_02 and ALN_03 share SLN_02)"},
			notes:       []string{"SLN_03 is in the routing set and no claim uses it: offered, so the composer's picture draws it and the board's does not"},
			shots:       weldShots,
		},
		{
			key: "fallback", title: "Fallback (the Press 6 shape with PLN_008 off the plant map)",
			cell: fxCell("FB", offMap, nil, press6DockGroups),
			opts: seedOpts{name: fxNames("Press 6 shape, PLN_008 off the map", "Unloader shape"), extra: unloader,
				recipes: []recipeSeed{grouped}, parts: []string{"SYN-FB-01", "SYN-FB-02", "SYN-FB-A", "SYN-FB-B"}},
			recipeLines: []string{press6RecipeLine},
			notes:       []string{"expected: today's picture in sequence order, with the line \"Shown in order: PLN_008 is not on the plant map\""},
			shots:       fallbackShots,
		},
	}
}

// ── the states ─────────────────────────────────────────────────────────────

func stationPath(sc seededCell, hash string) string {
	return fmt.Sprintf("/operator/station/%d%s", sc.stationID, hash)
}

// whichShot is the HMI's Which recipe screen for the parts: every recipe that
// fits them, each card with its picture.
func whichShot(sc seededCell, parts []string, mock string) shotSpec {
	return pageShot(frame{src: stationPath(sc, "#compose=which;parts="+strings.Join(parts, ",")), w: 1280, h: 800,
		svg: ".os-comp-which svg.os-flow-picture", want: "Which recipe?"},
		"which-1280", "Which recipe? for "+strings.Join(parts, " + "), mock, "")
}

const noMock = "the mock has no such screen"

func press6Shots(t *testing.T, sc seededCell, c realCell) []shotSpec {
	card := composerCardOf(t, sc, sc.recipes[press6RecipeName])
	parts := slotParts(c, card)
	hash, mv := spotsHash(card, parts)
	mock := func(state, drawn string) string { return mockFor(true, state, drawn) }
	specs := []shotSpec{
		relationShot(sc, "Press 6 shape", "relation-1280", "relation", 1280,
			"relation picture, nothing selected, the HMI's frame (1280)", mock("relation-1280", "")),
		relationShot(sc, "Press 6 shape", "relation-1084", "relation", 1084,
			"relation picture, nothing selected, the desktop's frame (1084)", mock("relation-1084", "")),
	}
	specs = append(specs, spotsShots(sc, card, hash, "", mock, "PLN_006", "PLN_005")...)
	specs = append(specs, whichShot(sc, parts, noMock+" (nearest: tab 2's picture)"))
	goHash := fmt.Sprintf("#compose=go;parts=%s;recipe=%s", strings.Join(parts, ","), card.ID)
	if len(mv) > 0 {
		goHash += ";moves=" + strings.Join(mv, ",")
	}
	specs = append(specs,
		pageShot(frame{src: stationPath(sc, goHash), w: 1280, h: 800, svg: ".os-comp-go svg.os-flow-picture",
			want: "Start changeover"}, "go-1280", "Go: the changeover to the pre-filled spots, before Start",
			noMock+" (nearest: tab 2's picture)", ""),
		flowShot(sc, noMock+" (the flow panel; nearest: tab 2's picture)"))
	const desk = "lane DESK"
	specs = append(specs,
		draftShot(sc, press6RecipeName, "draft-1440", "", draftCardsMarker,
			"desktop recipe draft, no focus: the picture and the group cards", "mock tab 1, no focus",
			desk+": `;recipe=` opens the draft, and a grouped recipe opens with its cards"),
		draftShot(sc, press6RecipeName, "draft-focus-left-hand-1440", ";focus=Left%20Hand", draftCardsMarker,
			"desktop recipe draft, Left Hand hovered (the hover is judged by eye)", "mock tab 1, Left Hand hovered",
			desk+": `;focus=<group>`"),
		draftShot(sc, press6RecipeName, "draft-row-PLN_010-1440", ";row=PLN_010", draftCardsMarker,
			"desktop recipe draft, the PLN_010 row clicked (judged by eye)", "mock tab 1, the PLN_010 row clicked",
			desk+": `;row=<node>`"))
	// The Advanced sheet in a group's scope (;gadv=) went in slice 3: a
	// group's settings are a list on its card, in draft-1440 itself.
	floor := draftFrame(sc, press6RecipeName, "", draftFloorMarker)
	floor.scroll = draftFloorMarker
	specs = append(specs, pageShot(floor, "draft-floor-1440", "desktop recipe draft, scrolled to \"On the floor\" ("+floor.src+")",
		"mock tab 1, \"What the floor can pick with this recipe\"", desk+": the \"On the floor\" block"))
	return specs
}

func hkPressShots(t *testing.T, sc seededCell, c realCell) []shotSpec {
	index := composerCardOf(t, sc, sc.recipes["Two spot press index"])
	swap := composerCardOf(t, sc, sc.recipes["Two spot swap"])
	parts := slotParts(c, index)
	none := func(string, string) string { return "" }
	specs := []shotSpec{
		relationShot(sc, "Hopkinsville press shape", "relation-1280", "relation", 1280,
			"relation picture (the running 2‑Robot Swap), the HMI's frame (1280)", ""),
		whichShot(sc, parts, ""),
	}
	hash, _ := spotsHash(index, parts)
	specs = append(specs, spotsShots(sc, index, hash, "-index", none, "PLN_02")...)
	hash, _ = spotsHash(swap, parts)
	specs = append(specs, spotsShots(sc, swap, hash, "-swap", none, "PLN_02")...)
	return append(specs, flowShot(sc, ""))
}

func weldShots(t *testing.T, sc seededCell, c realCell) []shotSpec {
	card := composerCardOf(t, sc, sc.recipes["Three spot weld"])
	hash, _ := spotsHash(card, slotParts(c, card))
	return append([]shotSpec{
		relationShot(sc, "Weld line shape", "relation-1280", "relation", 1280,
			"relation picture: one row, the lanes in the band under the spots they serve (1280)", ""),
		relationShot(sc, "Weld line shape", "relation-1084", "relation", 1084,
			"relation picture at the desktop's frame (1084)", ""),
		flowShot(sc, ""),
	}, spotsShots(sc, card, hash, "", func(string, string) string { return "" }, "SLN_02")...)
}

func fallbackShots(t *testing.T, sc seededCell, c realCell) []shotSpec {
	card := composerCardOf(t, sc, sc.recipes[press6RecipeName])
	hash, _ := spotsHash(card, slotParts(c, card))
	return append([]shotSpec{
		relationShot(sc, "Press 6 shape, PLN_008 off the map", "relation-1280", "relation", 1280,
			"relation picture asked for, one position off the map (1280)", ""),
		flowShot(sc, ""),
	}, spotsShots(sc, card, hash, "", func(string, string) string { return "" }, "PLN_008")...)
}

// consumeLineShots are the screens a one-node Sequential A/B position changes
// (final brief 2d): the desktop Styles tab, its partner pick, its swap menu
// and its row's robot work list, then the station's flow panel and relation
// picture. Each click state's `want` is text only the clicked control shows,
// so the frame driver makes the click (it clicks only when the URL alone does
// not reach the state): the free back SYN-CL-05 is folded off the tab and
// listed in the partner pick only; no row runs a 1‑Robot Swap.
func consumeLineShots(t *testing.T, sc seededCell, c realCell) []shotSpec {
	const mock = "no mock: final brief 2d"
	desk := func(state, what, want string, clicks ...string) shotSpec {
		return pageShot(frame{src: fmt.Sprintf("/processes?process=%d#tab=flows", sc.processID), w: 1440, h: 900,
			svg: "#pd-svg", want: want, clicks: clicks}, state, what, mock, "")
	}
	chip := func(node, kind string) string {
		return fmt.Sprintf(`.pd-postbl [data-act="pick"][data-node="%s"][data-kind="%s"]`, node, kind)
	}
	return []shotSpec{
		desk("styles-1440", "the Styles tab: SYN-CL-01 Sequential A/B with no partner, an A/B pair, a 2‑Robot Swap",
			"SYN-CL-04"),
		desk("partner-pick-1440", "SYN-CL-01's partner pick open", "SYN-CL-05", chip("SYN-CL-01", "col:partner:paired")),
		desk("mode-menu-1440", "SYN-CL-01's swap menu open", "1‑Robot Swap", chip("SYN-CL-01", "mode")),
		pageShot(frame{src: fmt.Sprintf("/processes?process=%d#tab=flows", sc.processID), w: 1440, h: 900, svg: "#pd-svg",
			want: "Robot work lists", clicks: []string{`.pd-postbl [data-row="SYN-CL-01"] td:not(.pos)`},
			scroll: "Robot work lists"}, "work-list-1440", "SYN-CL-01's row clicked: its robot work lists", mock, ""),
		flowShot(sc, mock),
		relationShot(sc, "Consume line shape", "relation-1280", "relation", 1280,
			"the station's picture by relation (1280)", mock),
	}
}

// freshLineShots is Setup's First style on a line with no flow yet, its first
// position set to Sequential A/B: the partner pick as it starts (final brief
// 2d).
func freshLineShots(t *testing.T, sc seededCell, c realCell) []shotSpec {
	return []shotSpec{pageShot(frame{src: fmt.Sprintf("/processes?process=%d#tab=setup", sc.processID), w: 1440, h: 900,
		svg: "#pd-svg", want: "Paired core node",
		clicks: []string{`[data-act="setup-style"]`, `[data-act="fs-mode"][data-mode="sequential"]`}},
		"first-style-1440", "Setup › First style: SYN-FS-01 set to Sequential A/B", "no mock: final brief 2d",
		"First style draws no cell picture, so the frame driver cannot measure it: shot unmeasured")}
}

// homeLineShots is the Styles tab of the line through its home, and each
// 2‑Robot Swap row's inbound staging pick open (brief G C).
func homeLineShots(t *testing.T, sc seededCell, c realCell) []shotSpec {
	const mock = "no mock: brief G C"
	desk := func(state, what, want string, clicks ...string) shotSpec {
		return pageShot(frame{src: fmt.Sprintf("/processes?process=%d#tab=flows", sc.processID), w: 1440, h: 900,
			svg: "#pd-svg", want: want, clicks: clicks}, state, what, mock, "")
	}
	chip := func(node string) string {
		return fmt.Sprintf(`.pd-postbl [data-act="pick"][data-node="%s"][data-kind="col:staging:staging"]`, node)
	}
	return []shotSpec{
		desk("styles-1440", "the Styles tab: SYN-HL-01 from its home with no staging hop, SYN-HL-02 from one node "+
			"through a lane, SYN-HL-03 from a group", "SYN-HL-03"),
		desk("staging-pick-1440", "SYN-HL-01's inbound staging pick open", "used by", chip("SYN-HL-01")),
		desk("staging-pick-one-1440", "SYN-HL-02's inbound staging pick open: its source is one node",
			"used by", chip("SYN-HL-02")),
		desk("staging-pick-group-1440", "SYN-HL-03's inbound staging pick open: its source is a group",
			"used by", chip("SYN-HL-03")),
	}
}

// homeFreshShots is Setup's First style on a fresh line through its home,
// its one position set to 2‑Robot Swap, and its inbound staging pick open
// (brief G C).
func homeFreshShots(t *testing.T, sc seededCell, c realCell) []shotSpec {
	const (
		mock = "no mock: brief G C"
		todo = "First style draws no cell picture, so the frame driver cannot measure it: shot unmeasured"
	)
	first := func(state, what string, clicks ...string) shotSpec {
		return pageShot(frame{src: fmt.Sprintf("/processes?process=%d#tab=setup", sc.processID), w: 1440, h: 900,
			svg: "#pd-svg", want: "Inbound staging",
			clicks: append([]string{`[data-act="setup-style"]`, `[data-act="fs-mode"][data-mode="two_robot"]`}, clicks...)},
			state, what, mock, todo)
	}
	return []shotSpec{
		first("first-style-1440", "Setup › First style: SYN-HF-01 set to 2‑Robot Swap"),
		first("staging-pick-1440", "the same, its Inbound staging pick open", `[data-act="fs-pick"][data-pick="staging"]`),
	}
}

// ── plant A, before and after ──────────────────────────────────────────────

// u4Row is one of TestComposerShots' board pictures, at the base and here.
type u4Row struct {
	shot, style string
	base, tip   string // the copies' names in the output folder
	modes       []string
	gateOff     bool
	same        string // "identical", how they differ, or why not compared
	tipResult   shotResult
	measured    bool
}

// The board flow panels TestComposerShots photographs, and the plant A style
// each runs (u4-schematic is S003 with no scene cached).
var u4Shots = []struct{ shot, style string }{
	{"u4-press-index", "PART SYN-A-S003"},
	{"u4-two-robot-swap", "PART SYN-A-S007"},
	{"u4-schematic", "PART SYN-A-S003"},
}

func plantAU4(t *testing.T, out string) []u4Row {
	t.Helper()
	baseDir, tipDir := os.Getenv("SPOTS_U4_BASE"), os.Getenv("SPOTS_U4_TIP")
	if baseDir == "" || tipDir == "" {
		return nil
	}
	baseTree := os.Getenv("SPOTS_U4_BASE_TREE")
	if baseTree == "" {
		baseTree = "base"
	}
	fx := scenefixtures.A()
	modesOf := func(style string) []string {
		var id int64
		for _, s := range fx.Styles {
			if s.Name == style {
				id = s.ID
			}
		}
		var modes []string
		for _, c := range fx.StyleNodeClaims {
			if c.StyleID == id && !slices.Contains(modes, c.SwapMode) {
				modes = append(modes, c.SwapMode)
			}
		}
		return modes
	}
	var rows []u4Row
	for _, u := range u4Shots {
		r := u4Row{shot: u.shot, style: u.style, base: "plantA-" + u.shot + "-" + baseTree + ".png",
			tip: "plantA-" + u.shot + "-tip.png", modes: modesOf(u.style), gateOff: true}
		for _, m := range r.modes {
			r.gateOff = r.gateOff && !changeoverMode(m)
		}
		a, errA := os.ReadFile(filepath.Join(baseDir, u.shot+".png"))
		b, errB := os.ReadFile(filepath.Join(tipDir, u.shot+".png"))
		switch {
		case errA != nil || errB != nil:
			r.same = fmt.Sprintf("not compared: base %v, tip %v", errA, errB)
			t.Errorf("%s: %s", u.shot, r.same)
		default:
			must(t, os.WriteFile(filepath.Join(out, r.base), a, 0o644), "copy "+r.base)
			must(t, os.WriteFile(filepath.Join(out, r.tip), b, 0o644), "copy "+r.tip)
			r.same = pixelDiff(a, b)
			if r.gateOff && r.same != "identical" {
				t.Errorf("%s: plant A's cell under %s is one the scope gate leaves alone, and its picture changed: %s",
					u.shot, u.style, r.same)
			}
		}
		if raw, err := os.ReadFile(filepath.Join(tipDir, u.shot+".png.measure.json")); err == nil {
			if json.Unmarshal(raw, &r.tipResult) == nil {
				r.measured = true
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// pixelDiff says whether two PNGs draw the same pixels, and if not how many
// differ and where.
func pixelDiff(a, b []byte) string {
	ia, errA := png.Decode(bytes.NewReader(a))
	ib, errB := png.Decode(bytes.NewReader(b))
	if errA != nil || errB != nil {
		return fmt.Sprintf("not decoded: %v / %v", errA, errB)
	}
	ra, rb := ia.Bounds(), ib.Bounds()
	if ra != rb {
		return fmt.Sprintf("different sizes: %dx%d before, %dx%d after", ra.Dx(), ra.Dy(), rb.Dx(), rb.Dy())
	}
	n := 0
	box := image.Rectangle{}
	for y := ra.Min.Y; y < ra.Max.Y; y++ {
		for x := ra.Min.X; x < ra.Max.X; x++ {
			r1, g1, b1, a1 := ia.At(x, y).RGBA()
			r2, g2, b2, a2 := ib.At(x, y).RGBA()
			if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
				n++
				box = box.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	if n == 0 {
		return "identical"
	}
	return fmt.Sprintf("%d pixels differ, within x %d–%d, y %d–%d", n, box.Min.X, box.Max.X, box.Min.Y, box.Max.Y)
}

// ── INDEX.md ───────────────────────────────────────────────────────────────

func fixtureIndex(rows []cellRow, u4 []u4Row, stale []string) string {
	var b strings.Builder
	pngs, ok, todo := 0, 0, 0
	for _, r := range rows {
		for _, s := range r.shots {
			pngs++
			switch {
			case s.TODO != "":
				todo++
			case s.Result.ok():
				ok++
			}
		}
	}
	tree := os.Getenv("SPOTS_SHOTS_TREE")
	if tree == "" {
		tree = "(tree not given)"
	}
	fmt.Fprintf(&b, "# Fixture shots at %s\n\n", tree)
	fmt.Fprintf(&b, "Written by `TestFixtureShots` (`scripts/real-cell-shots.sh --fixtures`) on %s. %d PNGs of the "+
		"fixtures: the overlap check is ok on %d and has findings on %d; %d wait on another lane (`-TODO`, the shot "+
		"shows what this tree draws there). Plant A before and after: %d pairs.\n\n",
		time.Now().Format("2006-01-02 15:04"), pngs, ok, pngs-ok-todo, todo, len(u4))
	b.WriteString("The cells are synthetic and committed in `shingo-edge/www/fixture_shots_test.go`: invented round " +
		"coordinates in the real cells' shapes, node names only, SYN- parts. Each is a fresh Edge seeded through the " +
		"store's own writes and served whole; the HMI states are the real station page at 1280x800, the drafts the real " +
		"desktop at 1440x900, `relation-*` the product's renderer through the relation driver. The overlap check is " +
		"measured in the browser on every PNG (cards overlapping, a card's text or mark outside its card, text over " +
		"text, anything past the picture's edge, the width against the frame). The mock is " +
		"`MOCK-recipe-groups-2026-10-22.html` (tab 1 the engineer, tab 2 the operator), which draws Press 6 only.\n\n")
	if len(stale) > 0 {
		fmt.Fprintf(&b, "Not from this run (an earlier run's, left in the folder): %s.\n\n", strings.Join(stale, ", "))
	}
	for _, r := range rows {
		r.markdown(&b)
	}
	b.WriteString("### Plant A's board flow panel, before and after\n\n")
	if len(u4) == 0 {
		b.WriteString("Not run in this pass (SPOTS_U4_BASE and SPOTS_U4_TIP unset).\n\n")
		return b.String()
	}
	b.WriteString("TestComposerShots' u4- shots (synthetic plant A, station screen-a4, the board's flow panel), " +
		"from a run at the base and a run at this tree, compared pixel for pixel. A cell the scope gate leaves on " +
		"today's picture must be identical; a changeover cell is drawn by relation now and is expected to change. " +
		"The overlap check is this tree's (TestComposerShots writes it beside each u4 PNG).\n\n")
	b.WriteString("| Before | After | Style (modes) | Scope gate | Before → after | Overlap check, after |\n|---|---|---|---|---|---|\n")
	for _, r := range u4 {
		var words []string
		for _, m := range r.modes {
			words = append(words, domain.SwapModeWord(protocol.SwapMode(m)))
		}
		gate := "relation picture"
		if r.gateOff {
			gate = "today's picture: must be identical"
		}
		check := "not measured (no measurement beside the tip's PNG)"
		if r.measured {
			check = r.tipResult.line()
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %s (%s) | %s | %s | %s |\n", r.base, r.tip, r.style,
			strings.Join(words, ", "), gate, mdCell(r.same), mdCell(check))
	}
	b.WriteString("\n")
	return b.String()
}
