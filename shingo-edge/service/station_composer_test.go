package service

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
)

// station_composer_test.go — the composer block, against the real Hopkinsville
// pull rather than the golden's bare station.
//
// The golden fixture carries no styles at all, so it pins the block's PRESENCE
// and nothing else. What U8 actually depends on is the block's CONTENTS: a
// picker row cannot be drawn without a claim count, a flow summary needs the
// modes and nodes, the set-up card needs the parts, and the position panel's
// option lists are the routing set. Those are asserted here, on a plant that
// has them.
// TestFlowProvenance_SaysWhereAndWhen, and says nothing when the claims
// disagree (owner ruling R1).
//
// The set-up card's dim line is the only place a flow's authorship is read
// back, and the whole point of the handler choosing the source is that the
// line can be TRUE. A style whose claims were written from both surfaces has
// no single answer, and the rule is that it then says when without saying
// where — the alternative is a caption picking whichever claim sorted first.
func TestFlowProvenance_SaysWhereAndWhen(t *testing.T) {
	t.Parallel()
	at := func(day int) *time.Time {
		when := time.Date(2026, 9, day, 8, 0, 0, 0, time.UTC)
		return &when
	}
	desktop := processes.NodeClaim{Source: domain.ClaimSourceAdmin, CalledBy: "admin", UpdatedAt: at(2)}
	station := processes.NodeClaim{Source: domain.ClaimSourceHMI, CalledBy: "Press 400", UpdatedAt: at(3)}

	for _, tc := range []struct {
		name             string
		claims           []processes.NodeClaim
		wantFrom, wantOn string
	}{
		{"the desktop", []processes.NodeClaim{desktop, desktop}, "the desktop", "09-02"},
		{"a station, by the name on the floor", []processes.NodeClaim{station, station}, "Press 400", "09-03"},
		{"newest wins the date", []processes.NodeClaim{{Source: domain.ClaimSourceAdmin, UpdatedAt: at(1)}, desktop}, "the desktop", "09-02"},
		{"they disagree: when, but not where", []processes.NodeClaim{desktop, station}, "", "09-03"},
		{"no flow at all", nil, "", ""},
		{"never written", []processes.NodeClaim{{Source: domain.ClaimSourceAdmin}}, "the desktop", ""},
	} {
		from, on := flowProvenance(tc.claims)
		if from != tc.wantFrom || on != tc.wantOn {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, from, on, tc.wantFrom, tc.wantOn)
		}
	}
}

// TestComposerPresets_AStoredPresetRendersACard is the bug U10 found sitting
// at c40444fb: the strip card builder never rendered one.
//
// CreateFlowPreset validates `{"cells":[…]}` — an OBJECT — and the card
// builder unmarshalled the same string into a bare `[]FlowCell`. The two
// never match, so `err != nil` fired on every stored preset and the `continue`
// skipped it silently. U8 left the strip-card code unexercised (R9) and
// nothing since had stored a preset and looked for its card, so a whole
// feature was dead with no test red.
//
// One parser now (domain.ParsePresetShape) and this, which is the test that
// would have caught it: store a preset through the real store, ask the real
// composer block, expect a card.
func TestComposerPresets_AStoredPresetRendersACard(t *testing.T) {
	t.Parallel()
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	testdb.SeedRoutingSet(t, db, seeded.ProcessID, "test")
	// Named through the service, exactly as the desktop names one.
	presets := NewProcessService(db)
	view, err := presets.PresetsFor(seeded.ProcessID)
	if err != nil {
		t.Fatalf("PresetsFor: %v", err)
	}
	if _, err := presets.CreateFromCandidate(seeded.ProcessID, "Press index, two positions", view.Candidates[0].ShapeKey, "s.brown"); err != nil {
		t.Fatalf("CreateFromCandidate: %v", err)
	}

	svc := NewStationService(db)
	svc.SetBinTypeResolver(testdb.BinTypeOf(fx))
	svc.SetCoreNodeGroupResolver(func() map[string][]string { return testdb.GroupsOf(fx) })
	geom := testdb.SceneGeometryOf(fx)
	svc.SetSceneGeometryResolver(func() *domain.SceneGeometry { return geom })
	data, err := svc.ComposerForProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ComposerForProcess: %v", err)
	}
	if len(data.Presets) != 1 {
		t.Fatalf("the composer block carries %d preset cards for one stored preset — a card the strip can draw is the whole point", len(data.Presets))
	}
	card := data.Presets[0]
	if card.Name != "Press index, two positions" || card.ID == "" {
		t.Errorf("card = %+v, want the preset's name and a key the tap can resolve", card)
	}
	if len(card.Cells) == 0 {
		t.Error("the card carries no cells, so a tap would apply nothing")
	}
	if card.Mode != "two_robot_press_index" {
		t.Errorf("card mode = %q, want two_robot_press_index for the glyph", card.Mode)
	}
	for node, cell := range card.Cells {
		if cell.PayloadCode != "" {
			t.Errorf("%s carries part %q — a preset is a shape", node, cell.PayloadCode)
		}
	}
	// THE SECOND LINE COUNTS PARTS, NOT POSITIONS. It said "2 positions",
	// which the glyph and the name already say — a suggested name IS the mode
	// word and the positions — so the line repeated the line above it and told
	// the operator nothing they could act on. `used by 7 parts` is the only
	// evidence the floor has that a shape is the right one for this press.
	//
	// The count is the candidate's member count, because naming an offered
	// shape stamps exactly the styles that already run it.
	want := len(view.Candidates[0].Members)
	if want < 2 {
		t.Fatalf("the named shape has %d members; this pin needs a plural to be worth anything", want)
	}
	// THE POSITIONS COME FIRST AND THE COUNT SECOND (owner rulings R2 and R5,
	// 2026-09-12): `PLN_01 / PLN_04 · used by 7 parts`. The positions are on
	// the card because the NAME no longer carries them — a preset is called
	// whatever the engineer typed — and the count is what makes a card worth
	// tapping.
	// TWO FACTS, TWO FIELDS, TWO LINES (owner rulings R2 and R5, 2026-09-12).
	// The positions are on the card because the NAME no longer carries them —
	// a preset is called whatever the engineer typed — and the count is what
	// makes a card worth tapping. One line carrying both ellipsised inside the
	// card's 200 px and cut the count off; a line each fits.
	// The SAME press order the builder used: the positions are printed in the
	// order the press has them, so a pin that passed nil would be asserting
	// against a lexical sort the screen does not use.
	nodes, err := db.ListProcessNodesByProcess(seeded.ProcessID)
	testutil.MustNoErr(t, err, "db.ListProcessNodesByProcess")
	order := make([]string, 0, len(nodes))
	for _, n := range nodes {
		order = append(order, n.CoreNodeName)
	}
	wantWhere := domain.PresetShapeNodeWord(shapeOf(t, card), order)
	if wantWhere == "" {
		t.Fatal("the card names no positions; two presets would then be one word apart with nothing to tell them apart")
	}
	if card.Where != wantWhere {
		t.Errorf("card positions line = %q, want %q", card.Where, wantWhere)
	}
	if card.Use != "used by "+strconv.Itoa(want)+" parts" {
		t.Errorf("card count line = %q, want %q", card.Use, "used by "+strconv.Itoa(want)+" parts")
	}
	// NO BIN WORD, ANYWHERE. It used to be appended by the render layer
	// (`· totes`) from the style being built; owner ruling R2 removed it from
	// every surface, so neither line may carry one.
	for _, line := range []string{card.Where, card.Use} {
		if strings.Contains(strings.ToLower(line), "tote") {
			t.Errorf("card line = %q — the bin word is gone from every surface", line)
		}
	}
}

// shapeOf reads a card's cells back as the shape they came from, so a pin can
// ask what the card should be drawing rather than repeating the answer.
func shapeOf(t *testing.T, card domain.ComposerPreset) []domain.FlowCell {
	t.Helper()
	out := make([]domain.FlowCell, 0, len(card.Cells))
	for _, c := range card.Cells {
		out = append(out, c)
	}
	return out
}

// TestComposerPresets_AShapeNobodyRunsSaysSo: a preset named from one style's
// flow stamps nobody, so its card has no members — and it says so rather than
// saying "used by 0 parts", which reads as a broken count on a card that is
// still worth tapping.
func TestComposerPresets_AShapeNobodyRunsSaysSo(t *testing.T) {
	t.Parallel()
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	testdb.SeedRoutingSet(t, db, seeded.ProcessID, "test")
	presets := NewProcessService(db)
	view, err := presets.PresetsFor(seeded.ProcessID)
	if err != nil {
		t.Fatalf("PresetsFor: %v", err)
	}
	styleID := view.Candidates[0].Members[0]
	if _, err := presets.CreateFromStyle(seeded.ProcessID, styleID, "Named from one flow", "s.brown"); err != nil {
		t.Fatalf("CreateFromStyle: %v", err)
	}

	svc := NewStationService(db)
	svc.SetBinTypeResolver(testdb.BinTypeOf(fx))
	data, err := svc.ComposerForProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ComposerForProcess: %v", err)
	}
	if len(data.Presets) != 1 {
		t.Fatalf("%d cards for one preset", len(data.Presets))
	}
	// THE POSITIONS ARE ALWAYS THERE (R5); what follows them is the count, and
	// a shape nobody runs says so rather than saying "used by 0 parts" — a
	// zero reads as a broken count on a card that is still worth tapping.
	if got := data.Presets[0].Use; got != "not used yet" {
		t.Errorf("card count line = %q, want %q — naming a style's flow names a shape, "+
			"it does not declare which parts run it", got, "not used yet")
	}
	if got := data.Presets[0].Where; !strings.Contains(got, "PLN_") {
		t.Errorf("card positions line = %q, want the shape's positions on it (R5)", got)
	}
}

func TestComposerDataCarriesWhatThePickerDraws(t *testing.T) {
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	svc := NewStationService(db)
	svc.SetBinTypeResolver(testdb.BinTypeOf(fx))
	svc.SetCoreNodeGroupResolver(func() map[string][]string { return testdb.GroupsOf(fx) })
	geom := testdb.SceneGeometryOf(fx)
	svc.SetSceneGeometryResolver(func() *domain.SceneGeometry { return geom })
	// The routing set is DERIVED, not seeded: U5's backfill is what turns a
	// plant's claims into the source/staging/destination options, and it runs
	// at migration on a real edge. SeedPlant stops at the claims, so deriving
	// here is what makes this the same set the position panel would offer.
	if _, err := db.DeriveRoutingNodesForProcess(seeded.ProcessID, func(string) bool { return false }); err != nil {
		t.Fatalf("derive routing set: %v", err)
	}
	// AND THEN ADOPTED. The backfill inserts every derived name DISABLED — it
	// is a suggestion, and an engineer adopts it (store/processes:
	// SetRoutingNodeEnabled, "switching on stamps origin='engineer'; switching
	// off does not flip it back"). Until that
	// happens the position panel offers nothing, which is right: a name Core
	// may not know is not an option to put in front of an operator. This test
	// walks the whole path — derive, adopt, open the gate — because that is
	// the only state in which the panel is reachable at all.
	derived, err := db.ListRoutingNodes(seeded.ProcessID)
	if err != nil {
		t.Fatalf("list routing set: %v", err)
	}
	for _, r := range derived {
		if err := db.SetRoutingNodeEnabled(seeded.ProcessID, r.ID, true, "test"); err != nil {
			t.Fatalf("adopt %s: %v", r.CoreNodeName, err)
		}
	}
	if err := db.SetFlowComposerEnabled(seeded.ProcessID, true); err != nil {
		t.Fatalf("open the gate: %v", err)
	}

	view, err := svc.BuildView(context.Background(), seeded.Stations["screen-a4"])
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	c := view.Composer
	if c == nil {
		t.Fatal("the view carries no composer block; every station gets the picker (brief R7)")
	}
	if len(c.Styles) == 0 {
		t.Fatal("composer.styles is empty — the picker would render no rows on a press with ten live styles")
	}

	// Style 7: press-index on PLN_01/PLN_04, two parts.
	var seven *domain.ComposerStyle
	for i := range c.Styles {
		if c.Styles[i].Name == "PART 40421-RVJ56.37" {
			seven = &c.Styles[i]
		}
	}
	if seven == nil {
		t.Fatal("the press-index style is not in composer.styles")
	}
	if seven.ClaimCount != 2 {
		t.Errorf("claim_count = %d, want 2 — the verdict pill reads Build at zero", seven.ClaimCount)
	}
	if len(seven.Modes) != 1 || seven.Modes[0] != "two_robot_press_index" {
		t.Errorf("claim_modes = %v, want one press-index entry — it is the row's flow summary", seven.Modes)
	}
	if len(seven.Nodes) != 2 {
		t.Errorf("claim_nodes = %v, want the two claimed positions", seven.Nodes)
	}
	if len(seven.Parts) != 2 {
		t.Errorf("parts = %v, want two — the set-up card draws a chip each", seven.Parts)
	}
	for _, p := range seven.Parts {
		if p.Node == "" {
			t.Errorf("part %q carries no position; a placed part draws as green → node, an unplaced one amber", p.PayloadCode)
		}
	}
	// WHAT THE POLL DOES NOT CARRY (S8). The cells, the routing set, the
	// preset cards and the travel graph are the COMPOSER's, fetched once when
	// it opens; the view carries the picker's rows. They rode the poll at
	// 500 ms to be discarded on every one of them.
	if len(seven.Claims) != 0 {
		t.Errorf("the view carries %d stored cells for one style; they belong on the composer's own read", len(seven.Claims))
	}
	if c.Routing != nil || c.Presets != nil || c.Scene != nil {
		t.Errorf("the view carries routing=%v presets=%v scene=%v — all three are the composer's read",
			c.Routing != nil, c.Presets != nil, c.Scene != nil)
	}

	// And the station's own composer read carries every one of them.
	stn, err := svc.ComposerForStation(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ComposerForStation: %v", err)
	}
	var sevenFlow *domain.ComposerStyle
	for i := range stn.Styles {
		if stn.Styles[i].Name == "PART 40421-RVJ56.37" {
			sevenFlow = &stn.Styles[i]
		}
	}
	if sevenFlow == nil {
		t.Fatal("the press-index style is not in the composer read's styles")
	}
	if len(sevenFlow.Claims) != sevenFlow.ClaimCount {
		t.Errorf("claims = %d rows but claim_count = %d; the composer opens a flow from these without a second read",
			len(sevenFlow.Claims), sevenFlow.ClaimCount)
	}
	// The routing set feeds every option list in the position panel, and only
	// its ENABLED members: a retired lane is not an option to offer.
	if len(stn.Routing) == 0 {
		t.Error("composer.routing is empty — the position panel would offer no source and no destination")
	}
	roles := map[string]int{}
	for _, r := range stn.Routing {
		roles[r.Role]++
	}
	if roles["source"] == 0 || roles["destination"] == 0 {
		t.Errorf("routing roles = %v, want at least one source and one destination", roles)
	}

	// The scene is what "Robot drives via" walks. Absent is legal (the row then
	// offers "shortest way" alone) but the HK pull has one, so this asserts the
	// reduction actually happened rather than that nil is tolerated.
	if stn.Scene == nil || len(stn.Scene.Edges) == 0 {
		t.Error("composer.scene has no edges; the HK pull has a travel graph and the waypoint row needs it")
	}
}
