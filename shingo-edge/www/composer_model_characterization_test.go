package www

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// Same shape as the claim editor's pin, which was
// TestProcessesJSClaimEditorCharacterization until `feat(edge): the composer's
// routes and handlers` deleted it: a self-contained Node script, no npm,
// skipped when node is absent. What this one adds is the
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
	t.Parallel()
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
	// Core's catalog is marked arrived, exactly as the shots harness does:
	// refusePressIndexWhenCoreUnavailable keys on it, so without it every
	// press-index preview is refused by name and style 7 would plan nothing. Nothing here calls Core — bin types
	// come from SetPayloadBinTypes and the preflight stays unchecked.
	testdb.SeedRoutingSet(t, db, seeded.ProcessID, "test")
	if err := db.SetFlowComposerEnabled(seeded.ProcessID, true); err != nil {
		t.Fatalf("open the gate: %v", err)
	}
	cfg := config.Defaults()
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf})
	// Core's catalog has arrived (empty): the press-index changeover's gate
	// opens on it, as it opened on a configured Core address.
	eng.SetPayloadBinTypes(nil)
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
		"7":  "PART SYN-A-S003",
		"11": "PART SYN-A-S007",
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
		for _, nf := range domain.ValidateFlowPartsPlaced(claims, dropFirstClaim(claims), nil) {
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

	scriptPath := filepath.Join("testdata", "static", "operator-station", "composer-model.test.js")
	cmd := exec.Command(nodePath, scriptPath, fixturePath)
	got, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("composer model characterization failed:\n%s\nerror: %v", got, err)
	}
	t.Logf("composer model: %s", got)
}

var updateProcessSetupGolden = flag.Bool("update-process-setup-golden", false,
	"rewrite testdata/process-setup/*.golden.txt from the current model")

// TestComposerModelGrid pins the whole mode x role grid the desktop's
// positions table and Advanced sheet read from composer-model.js: rowColumns'
// chips (key, field, label, required, possible) blank, with no back position
// and with every field set; fieldRequired per flowspec field; advancedShows
// per Advanced key; advancedSet for the defaults and for every key set.
//
// U0 of the process-setup UX run (2026-11-11): written at the base tree before
// any source moved, so every cell the rework changes is a line in this
// golden's diff and every line it does not change stayed put. The grid is
// built by the same harness and loader as TestComposerModelCharacterization
// (composer-model.test.js, in its --grid mode), over synthetic positions; it
// needs no plant and no fixture from Go.
//
// Regenerate with
//
//	go test ./www -run TestComposerModelGrid -update-process-setup-golden
//
// and read the diff: a regenerated golden pins nothing unless someone read it.
func TestComposerModelGrid(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the composer model grid")
	}
	// Written to a temp file, not beside the script: static/ is embedded and
	// served (see TestComposerModelCharacterization's fixture).
	outPath := filepath.Join(t.TempDir(), "composer-model-grid.txt")
	scriptPath := filepath.Join("testdata", "static", "operator-station", "composer-model.test.js")
	if msg, err := exec.Command(nodePath, scriptPath, "--grid", outPath).CombinedOutput(); err != nil {
		t.Fatalf("build the grid:\n%s\nerror: %v", msg, err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read the grid: %v", err)
	}
	goldPath := filepath.Join("testdata", "process-setup", "composer-model-grid.golden.txt")
	if *updateProcessSetupGolden {
		if err := os.MkdirAll(filepath.Dir(goldPath), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(goldPath), err)
		}
		if err := os.WriteFile(goldPath, got, 0o644); err != nil {
			t.Fatalf("write %s: %v", goldPath, err)
		}
		t.Logf("rewrote %s (%d lines): read the diff", goldPath, strings.Count(string(got), "\n"))
		return
	}
	want, err := os.ReadFile(goldPath)
	if err != nil {
		t.Fatalf("no golden (%v); regenerate with -update-process-setup-golden and read it", err)
	}
	if string(want) == string(got) {
		t.Logf("composer model grid: %d lines pinned", strings.Count(string(got), "\n"))
		return
	}
	// The lines are keyed (function, mode, role, column or key), so the
	// lines on one side and not the other are the cells that moved.
	t.Errorf("the composer model grid moved from %s; regenerate only for a named change:\n%s",
		goldPath, gridLineDiff(string(want), string(got)))
}

// gridLineDiff lists the golden's lines the run lost (-) and the run's lines
// the golden lacks (+), in order, capped so a wholesale change stays readable.
func gridLineDiff(want, got string) string {
	count := func(s string) map[string]int {
		m := map[string]int{}
		for _, l := range strings.Split(s, "\n") {
			m[l]++
		}
		return m
	}
	inWant, inGot := count(want), count(got)
	var b strings.Builder
	n := 0
	emit := func(sign string, lines string, other map[string]int) {
		for _, l := range strings.Split(lines, "\n") {
			if other[l] > 0 {
				other[l]--
				continue
			}
			if n++; n <= 60 {
				b.WriteString(sign + " " + l + "\n")
			}
		}
	}
	emit("-", want, inGot)
	emit("+", got, inWant)
	if n > 60 {
		fmt.Fprintf(&b, "… and %d more\n", n-60)
	}
	return b.String()
}

// dropFirstClaim is the draft a composer leaves behind when the operator takes
// the first position off the flow.
func dropFirstClaim(claims []domain.NodeClaim) []domain.NodeClaim {
	if len(claims) == 0 {
		return nil
	}
	return append([]domain.NodeClaim(nil), claims[1:]...)
}
