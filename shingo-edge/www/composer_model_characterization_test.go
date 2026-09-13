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
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/engine"
	"shingoedge/engine/changeover"
	"shingoedge/internal/testdb"
)

// composer_model_characterization_test.go — the Flow Composer model's pins.
//
// Same shape as TestProcessesJSClaimEditorCharacterization: a self-contained
// Node script, no npm, skipped when node is absent. What this one adds is the
// FIXTURE: the expected FlowCell JSON is produced HERE, by the real
// domain.Collapse over a whole plant's claims, and written next to the
// script before it runs.
//
// Why generated rather than hand-written: the round-trip pin asserts that
// composer-model.js's toCells() emits exactly what the U7 endpoints take. A
// hand-written expectation is a second copy of FlowCell's JSON shape, and the
// first key that changes on the Go side would leave the JS agreeing with a
// shape the server no longer speaks. Generating it means the pin fails the day
// the cell changes, which is the day it should.
func TestComposerModelCharacterization(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping composer model characterization")
	}

	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")

	// THE PLANNER, for the preview actions the ORDERS sentences are built
	// from. The routing set is derived and adopted first (a flow naming an
	// un-adopted destination is refused) and the composer gate is opened,
	// which is what PreviewFlow checks before it plans anything.
	//
	// CoreAPI is set to an unreachable host on purpose, exactly as the shots
	// harness does: refusePressIndexWhenCoreUnavailable keys on "CoreAPI is
	// not empty", so with it blank every press-index preview is refused by
	// name and style 7 would plan nothing. Nothing here calls Core — bin types
	// come from SetPayloadBinTypes and the preflight stays unchecked.
	testdb.SeedRoutingSet(t, db, seeded.ProcessID, "test")
	if err := db.SetFlowComposerEnabled(seeded.ProcessID, true); err != nil {
		t.Fatalf("open the gate: %v", err)
	}
	cfg := config.Defaults()
	cfg.CoreAPI = "http://core.invalid"
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf})
	eng.Start()
	t.Cleanup(eng.Stop)
	var pbt []protocol.PayloadBinTypeInfo
	for code, types := range fx.PayloadBinTypeCodes() {
		for _, bt := range types {
			pbt = append(pbt, protocol.PayloadBinTypeInfo{PayloadCode: code, BinTypeCode: bt})
		}
	}
	eng.SetPayloadBinTypes(pbt)

	type styleFixture struct {
		Name   string             `json:"name"`
		Claims []domain.NodeClaim `json:"claims"`
		Cells  []domain.FlowCell  `json:"cells"`
		Parts  []string           `json:"parts"`
		// Advanced is the view's per-position policy block, from Go's own
		// AdvancedOf — the same generation rule as Cells, and for the same
		// reason: a hand-written copy would be a second spelling of
		// FlowAdvanced's JSON and would agree with a shape the server had
		// stopped speaking.
		Advanced map[string]domain.FlowAdvanced `json:"advanced,omitempty"`
		LastRun  string                         `json:"last_run,omitempty"`
		// Actions is the REAL preview response for this style's own flow, from
		// the real planner (U10 P1). Generated for the same reason Cells is:
		// the ORDERS sentences are built from these, and a hand-written action
		// list is a second copy of PreviewAction's JSON that would go on
		// agreeing with a shape the server had stopped sending — which is
		// precisely how the sentence lost an end before P1.
		Actions []changeover.PreviewAction `json:"actions,omitempty"`
		// UnplacedDroppingFirst is THE SERVER'S answer to "which parts have no
		// position" for the draft that drops this style's first cell, computed
		// by domain.ValidateFlowPartsPlaced over the real plant rows.
		//
		// The model answers the same question locally, so the operator sees it
		// without a round trip — and the two had drifted: the model counted a
		// part sitting on a switched-OFF cell as placed, while the server reads
		// the DRAFT, which a cell with no choreography is not in. The bar said
		// the flow was ready and the preview came back with a finding under it.
		// The JS side compares its own answer to this.
		UnplacedDroppingFirst []string `json:"unplaced_dropping_first"`
	}
	out := struct {
		Styles map[string]styleFixture `json:"styles"`
	}{Styles: map[string]styleFixture{}}

	// The two styles §3.1 names: press-index (7) and two-robot (11).
	for key, styleName := range map[string]string{
		"7":  "PART 40421-RVJ56.37",
		"11": "PART 68644-WSL97.20",
	} {
		styleID, ok := seeded.Styles[styleName]
		if !ok {
			t.Fatalf("style %q not seeded by the HK fixture", styleName)
		}
		claims, err := db.ListStyleNodeClaims(styleID)
		if err != nil {
			t.Fatalf("list claims for %s: %v", styleName, err)
		}
		f := styleFixture{Name: styleName, Claims: claims, Advanced: map[string]domain.FlowAdvanced{}}
		seenPart := map[string]bool{}
		for _, c := range claims {
			f.Cells = append(f.Cells, domain.Collapse(c))
			f.Advanced[c.CoreNodeName] = *domain.AdvancedOf(&c)
			if c.PayloadCode != "" && !seenPart[c.PayloadCode] {
				seenPart[c.PayloadCode] = true
				f.Parts = append(f.Parts, c.PayloadCode)
			}
		}
		// The preview of this style's own flow: what the robots would be told
		// to do, planned by the planner rather than described by the test.
		pv, err := eng.PreviewFlow(context.Background(), seeded.ProcessID, engine.FlowPreviewRequest{
			ToStyleID: styleID, Cells: f.Cells,
		})
		if err != nil {
			t.Fatalf("preview %s: %v", styleName, err)
		}
		f.Actions = pv.Actions
		// Drop the first cell and ask the server which parts that leaves loose.
		for _, nf := range domain.ValidateFlowPartsPlaced(claims, dropFirstClaim(claims)) {
			f.UnplacedDroppingFirst = append(f.UnplacedDroppingFirst, nf.Message)
		}
		out.Styles[key] = f
	}

	blob, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixtures: %v", err)
	}
	// NOT BESIDE THE SCRIPT. static/ is embedded (embed.go) and served, so a
	// generated file written there is a file the next build ships: a run that
	// dies before t.Cleanup — Ctrl-C, a panic, -timeout — leaves the whole HK
	// plant's claims in the asset tree, and it is not in .gitignore either.
	// t.TempDir goes away whatever happens, and the path travels as argv[1]
	// exactly as operator_flow_test.go:67-70 already passes its three views.
	fixturePath := filepath.Join(t.TempDir(), "composer-model.fixtures.json")
	if err := os.WriteFile(fixturePath, blob, 0o644); err != nil {
		t.Fatalf("write fixtures: %v", err)
	}

	scriptPath := filepath.Join("static", "operator-station", "composer-model.test.js")
	cmd := exec.Command(nodePath, scriptPath, fixturePath)
	got, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("composer model characterization failed:\n%s\nerror: %v", got, err)
	}
	t.Logf("composer model: %s", got)
}

// dropFirstClaim is the draft a composer leaves behind when the operator takes
// the first position off the flow.
func dropFirstClaim(claims []domain.NodeClaim) []domain.NodeClaim {
	if len(claims) == 0 {
		return nil
	}
	return append([]domain.NodeClaim(nil), claims[1:]...)
}
