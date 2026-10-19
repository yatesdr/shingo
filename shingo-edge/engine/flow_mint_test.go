package engine

import (
	"context"
	"errors"
	"sort"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// flow_mint_test.go — the mint (pick-a-part U3): resolve by parts, place by
// evidence then picture order, expand with the minted policy, create in one
// transaction. The station's own body is pinned through the router in
// www/handlers_flow_mint_test.go; these pin the engine's decisions.

type mintFixture struct {
	db      *store.DB
	eng     *Engine
	plant   testdb.SeededPlant
	preset  int64
	members []int64
}

// newMintFixture seeds plant A's press with its routing set, names the offered
// press-index shape "Standard" (stamping its members), and returns it.
func newMintFixture(t *testing.T) mintFixture {
	t.Helper()
	db := testEngineDB(t)
	plant := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")
	testdb.SeedRoutingSet(t, db, plant.ProcessID, "test")
	svc := service.NewProcessService(db)
	offer, err := svc.PresetsFor(plant.ProcessID)
	testutil.MustNoErr(t, err, "PresetsFor")
	for _, c := range offer.Candidates {
		if c.Mode != string(protocol.SwapModeTwoRobotPressIndex) || len(c.Members) < 2 {
			continue
		}
		id, err := svc.CreateFromCandidate(plant.ProcessID, "Standard", c.ShapeKey, "test")
		testutil.MustNoErr(t, err, "name the recipe")
		eng := testEngine(t, db)
		eng.coreClient = stubCoreClient(testCoreURL)
		// The station mints only where HMI setup is on.
		enableComposer(t, db, plant.ProcessID)
		return mintFixture{db: db, eng: eng, plant: plant, preset: id, members: c.Members}
	}
	t.Fatal("plant A offers no press-index shape with two members")
	return mintFixture{}
}

// partsByNode is a style's parts keyed by the node they sit on.
func (f mintFixture) partsByNode(t *testing.T, styleID int64) map[string]string {
	t.Helper()
	claims, err := f.db.ListStyleNodeClaims(styleID)
	testutil.MustNoErr(t, err, "claims")
	out := map[string]string{}
	for _, c := range claims {
		out[c.CoreNodeName] = c.PayloadCode
	}
	return out
}

func sortedNodes(m map[string]string) []string {
	var out []string
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (f mintFixture) mint(t *testing.T, picks ...PartPick) (*MintResult, error) {
	t.Helper()
	return f.eng.MintStyle(f.plant.ProcessID, MintRequest{
		PresetID: f.preset, Parts: picks, Source: domain.ClaimSourceHMI, CalledBy: "Screen A4",
	})
}

// R-M: each part to the node it last ran on here when that node is a slot of
// the recipe, whatever order it was tapped in; the rest to the free slots in
// picture order.
func TestMintStyle_PlacesByEvidenceThenPictureOrder(t *testing.T) {
	t.Parallel()
	f := newMintFixture(t)
	a, b := f.partsByNode(t, f.members[0]), f.partsByNode(t, f.members[1])
	nodes := sortedNodes(a)
	if len(nodes) != 2 {
		t.Fatalf("member parts = %v, want two positions", a)
	}
	first, second := a[nodes[0]], b[nodes[1]]

	// Tapped back to front: the evidence puts each where it last ran.
	res, err := f.mint(t, PartPick{Code: second}, PartPick{Code: first})
	testutil.MustNoErr(t, err, "mint")
	if !res.Minted {
		t.Fatalf("result = %+v, want a minted style (no live style runs this pair)", res)
	}
	if res.Placement[nodes[0]] != first || res.Placement[nodes[1]] != second {
		t.Errorf("placement = %v, want %s on %s and %s on %s", res.Placement, first, nodes[0], second, nodes[1])
	}
	if got := f.partsByNode(t, res.StyleID); got[nodes[0]] != first || got[nodes[1]] != second {
		t.Errorf("stored claims = %v, want the placement", got)
	}

	// A part this press has never run takes the first free slot in picture
	// order; the one with evidence still goes home.
	res, err = f.mint(t, PartPick{Code: "SYN-A-NEW1"}, PartPick{Code: second})
	testutil.MustNoErr(t, err, "mint with a new part")
	if res.Placement[nodes[1]] != second || res.Placement[nodes[0]] != "SYN-A-NEW1" {
		t.Errorf("placement = %v, want %s home on %s and the new part on %s", res.Placement, second, nodes[1], nodes[0])
	}
}

// R-H / R-L: a minted style has no auto-reorder, a zero reorder point and the
// 'unset' source the engineer can find; the recipe is its provenance; the
// process's containment route rides its produce claims and a curtained node
// stays curtained (cell facts, not policy — see DECISIONS).
func TestMintStyle_StampsTheMintedPolicyAndTheCellFacts(t *testing.T) {
	t.Parallel()
	f := newMintFixture(t)
	a, b := f.partsByNode(t, f.members[0]), f.partsByNode(t, f.members[1])
	nodes := sortedNodes(a)
	testutil.MustNoErr(t, service.NewProcessService(f.db).SetContainment(domain.CoreNodeKinds{}, f.plant.ProcessID,
		true, "Supermarket Area", "qa"), "turn containment on")
	if _, err := f.db.Exec(`UPDATE style_node_claims SET curtain_required = 1 WHERE style_id = ? AND core_node_name = ?`,
		f.members[0], nodes[0]); err != nil {
		t.Fatalf("curtain a node: %v", err)
	}

	res, err := f.mint(t, PartPick{Code: a[nodes[0]]}, PartPick{Code: b[nodes[1]]})
	testutil.MustNoErr(t, err, "mint")
	p, err := f.db.GetFlowPreset(f.preset)
	testutil.MustNoErr(t, err, "preset")
	claims, err := f.db.ListStyleNodeClaims(res.StyleID)
	testutil.MustNoErr(t, err, "minted claims")
	if len(claims) == 0 {
		t.Fatal("the minted style has no claims")
	}
	for _, c := range claims {
		if c.AutoReorder || c.ReorderPoint != 0 || c.ReorderPointSource != MintedReorderSource {
			t.Errorf("%s: auto_reorder=%v reorder_point=%d source=%q, want false/0/%q",
				c.CoreNodeName, c.AutoReorder, c.ReorderPoint, c.ReorderPointSource, MintedReorderSource)
		}
		if c.SourcePresetID == nil || *c.SourcePresetID != f.preset || c.SourcePresetVersion == nil || *c.SourcePresetVersion != p.Version {
			t.Errorf("%s: provenance %v/%v, want preset %d v%d", c.CoreNodeName, c.SourcePresetID, c.SourcePresetVersion, f.preset, p.Version)
		}
		if c.Source != domain.ClaimSourceHMI || c.CalledBy != "Screen A4" {
			t.Errorf("%s: source %q by %q, want hmi by the station", c.CoreNodeName, c.Source, c.CalledBy)
		}
		if c.Role == protocol.ClaimRoleProduce && c.ContainmentDestination != "Supermarket Area" {
			t.Errorf("%s: containment %q, want the process's route", c.CoreNodeName, c.ContainmentDestination)
		}
		if c.CurtainRequired != (c.CoreNodeName == nodes[0]) {
			t.Errorf("%s: curtain_required=%v, want it only where the node is curtained", c.CoreNodeName, c.CurtainRequired)
		}
	}
}

func TestMintStyle_ARecipeThatDoesNotFitThePartsIsRefusedByName(t *testing.T) {
	t.Parallel()
	f := newMintFixture(t)
	_, err := f.mint(t, PartPick{Code: "SYN-A-ONLY"})
	var refused *MintRefusal
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a MintRefusal (the recipe takes two parts)", err)
	}
	_, err = f.eng.MintStyle(f.plant.ProcessID, MintRequest{PresetID: 99999, Parts: []PartPick{{Code: "X"}, {Code: "Y"}},
		Source: domain.ClaimSourceHMI})
	if !errors.Is(err, ErrMintRecipe) {
		t.Fatalf("err = %v, want ErrMintRecipe for a recipe that is not this process's", err)
	}
}

// Resolve by parts, and the running style first: the parts on the press
// resolve to the press's style, nothing is written, and the answer says so.
func TestMintStyle_TheRunningPartsResolveToTheRunningStyle(t *testing.T) {
	t.Parallel()
	f := newMintFixture(t)
	proc, err := f.db.GetProcess(f.plant.ProcessID)
	testutil.MustNoErr(t, err, "process")
	running := f.partsByNode(t, *proc.ActiveStyleID)
	var picks []PartPick
	for _, n := range sortedNodes(running) {
		picks = append(picks, PartPick{Code: running[n]})
	}
	before, err := f.db.ListStylesByProcess(f.plant.ProcessID)
	testutil.MustNoErr(t, err, "styles")
	res, err := f.mint(t, picks...)
	if err != nil {
		// The running style's shape may not be the recipe's; resolve comes
		// first either way, so a refusal here is the regression.
		t.Fatalf("mint of the running parts: %v", err)
	}
	if res.Minted || !res.Running || res.StyleID != *proc.ActiveStyleID {
		t.Errorf("result = %+v, want the running style resolved, nothing minted", res)
	}
	after, err := f.db.ListStylesByProcess(f.plant.ProcessID)
	testutil.MustNoErr(t, err, "styles")
	if len(after) != len(before) {
		t.Errorf("styles %d → %d: a resolve wrote a style", len(before), len(after))
	}
}

// flow/preview's mint form plans a style that does not exist yet, and writes
// nothing.
func TestPreviewFlow_TheMintFormPlansAStyleThatDoesNotExistYet(t *testing.T) {
	t.Parallel()
	f := newMintFixture(t)
	a, b := f.partsByNode(t, f.members[0]), f.partsByNode(t, f.members[1])
	nodes := sortedNodes(a)
	before, err := f.db.ListStylesByProcess(f.plant.ProcessID)
	testutil.MustNoErr(t, err, "styles")
	pv, err := f.eng.PreviewFlow(context.Background(), f.plant.ProcessID, FlowPreviewRequest{Mint: &MintRequest{
		PresetID: f.preset, Parts: []PartPick{{Code: a[nodes[0]]}, {Code: b[nodes[1]]}},
	}})
	if err != nil {
		var noOrders *FlowNoOrdersError
		if !errors.As(err, &noOrders) {
			t.Fatalf("mint preview: %v", err)
		}
		pv = noOrders.Preview
	}
	if pv == nil || pv.Mint == nil || !pv.Mint.Minted || pv.Mint.StyleName == "" {
		t.Fatalf("preview = %+v, want the mint facts of a style to be minted", pv)
	}
	if pv.OrderCount == nil {
		t.Error("a preview of a style to be minted carries no order count; it is not the running style")
	}
	after, err := f.db.ListStylesByProcess(f.plant.ProcessID)
	testutil.MustNoErr(t, err, "styles")
	if len(after) != len(before) {
		t.Errorf("styles %d → %d: a preview wrote", len(before), len(after))
	}
	_ = processes.Style{}
}

// One Part, Two Spots: the one part on both produce slots of the two-slot
// recipe, named apart from the same part on one spot, and found again by its
// part AND its spot count — the one-spot question does not resolve to it.
func TestMintStyle_OnePartTwoSpotsFillsBothSlotsAndResolvesBySpots(t *testing.T) {
	t.Parallel()
	f := newMintFixture(t)
	nodes := sortedNodes(f.partsByNode(t, f.members[0]))
	split := func() (*MintResult, error) {
		return f.eng.MintStyle(f.plant.ProcessID, MintRequest{PresetID: f.preset, Parts: []PartPick{{Code: "SYN-A-SPLIT"}},
			Split: true, Source: domain.ClaimSourceHMI, CalledBy: "Screen A4"})
	}
	res, err := split()
	testutil.MustNoErr(t, err, "split mint")
	if !res.Minted || res.StyleName != "SYN-A-SPLIT (two spots)" {
		t.Fatalf("result = %+v, want a minted style named for its two spots", res)
	}
	if got := f.partsByNode(t, res.StyleID); got[nodes[0]] != "SYN-A-SPLIT" || got[nodes[1]] != "SYN-A-SPLIT" {
		t.Errorf("claims = %v, want the part on %v", got, nodes)
	}
	again, err := split()
	testutil.MustNoErr(t, err, "split mint again")
	if again.Minted || again.StyleID != res.StyleID {
		t.Errorf("second split = %+v, want style %d resolved", again, res.StyleID)
	}
	// Set up new: the same request with New mints another rather than
	// resolving, named apart by the recipe.
	fresh, err := f.eng.MintStyle(f.plant.ProcessID, MintRequest{PresetID: f.preset, Parts: []PartPick{{Code: "SYN-A-SPLIT"}},
		Split: true, New: true, Source: domain.ClaimSourceHMI, CalledBy: "Screen A4"})
	testutil.MustNoErr(t, err, "set up new")
	if !fresh.Minted || fresh.StyleID == res.StyleID || fresh.StyleName == res.StyleName {
		t.Errorf("set up new = %+v, want a second style with its own name", fresh)
	}
	// The same part on one spot is a different style: the two-slot
	// recipe cannot take it, so it is refused rather than resolved.
	if _, err := f.mint(t, PartPick{Code: "SYN-A-SPLIT"}); err == nil {
		t.Error("one part with no split resolved to the two-spot style")
	}
	if _, err := f.eng.MintStyle(f.plant.ProcessID, MintRequest{PresetID: f.preset, Split: true,
		Parts: []PartPick{{Code: "X"}, {Code: "Y"}}, Source: domain.ClaimSourceHMI}); err == nil {
		t.Error("a split of two parts was accepted")
	}
}

// A style tapped in the list is that style: nothing resolved or
// minted. Another process's style is refused by name.
func TestMintStyle_ASavedRecipeByIDIsThatStyle(t *testing.T) {
	t.Parallel()
	f := newMintFixture(t)
	res, err := f.eng.MintStyle(f.plant.ProcessID, MintRequest{StyleID: f.members[1], Source: domain.ClaimSourceHMI})
	testutil.MustNoErr(t, err, "open by id")
	if res.Minted || res.StyleID != f.members[1] || len(res.Placement) == 0 {
		t.Errorf("result = %+v, want style %d as it is", res, f.members[1])
	}
	var refused *MintRefusal
	if _, err := f.eng.MintStyle(f.plant.ProcessID, MintRequest{StyleID: 99999, Source: domain.ClaimSourceHMI}); !errors.As(err, &refused) {
		t.Errorf("err = %v, want a refusal for a style that is not this process's", err)
	}
}
