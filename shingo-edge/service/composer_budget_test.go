package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"shingo/protocol/testutil"
	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/catalog"
)

// composer_budget_test.go — the S0 budgets, pinned.
//
// THE FIXTURE IS THE POINT. The station view's golden carries no styles and no
// scene, so a byte pin taken against it pins the empty case and would stay
// green through any regression that only bites at plant size. This one is the
// press the brief names: 40 styles x 6 positions, 3 presets, 600 changeovers,
// 250 catalog rows, 12 routing rows, and a scene in the geometry cache.
//
// The numbers are QUERIES and PRE-GZIP JSON BYTES. Not wall time (noise on a
// loaded runner, and the Pi is 4-8x slower than the box this runs on) and not
// bytes on the wire: gzip takes 40 near-identical style blocks down about 30x,
// so the compressed size hides exactly the growth that costs the Pi its CPU.
// What the box pays is the marshalling, and that is len(json).

const (
	budgetStyles    = 40
	budgetPositions = 6
	budgetCatalog   = 250
	budgetChangeovs = 600
	budgetPresets   = 3
	budgetRouting   = 12
)

type budgetFixture struct {
	db        *store.DB
	counter   *store.QueryCounter
	svc       *StationService
	processID int64
	stationID int64
}

// The budget fixture is built ONCE and copied per test.
//
// It is ~1,100 INSERTs and a migration chain on a one-connection SQLite —
// 1.7s — and ten tests across four files ask for it, which was 13s of this
// package's 15s and most of what the branch added to the gate. The rows are
// identical every time, so the expensive half is a pure function of the
// constants above: seed it into a file on the first call, copy the file
// afterwards. Same pool as engine/operator_stations_test.go's
// copyEngineDBTemplate, which is where this pattern is written down.
//
// THE IDS ARE CAPTURED WITH THE TEMPLATE, not re-derived: a copy is the same
// bytes, so the process and station a test wants are the ones the seed made.
var (
	budgetTemplateOnce    sync.Once
	budgetTemplatePath    string
	budgetTemplateProcess int64
	budgetTemplateStation int64
	budgetTemplateErr     error
)

// seedBudgetFixture builds the press S0 measures against, on a counting store.
func seedBudgetFixture(t *testing.T) budgetFixture {
	t.Helper()
	fx := scenefixtures.A()
	// THE SENTINEL IS SET BEFORE THE BUILD, not after. SeedPlant reports through
	// t.Fatalf, which is runtime.Goexit — so a template that dies half-built
	// leaves the Once marked done and every other test in the package reading a
	// file that is not there. Arming the error first means they say what
	// actually happened and point at the one test that carries the real
	// message.
	budgetTemplateOnce.Do(func() {
		budgetTemplateErr = errors.New("the template build did not finish; the first failing test in this package has the reason")
		buildBudgetTemplate(t, fx)
		budgetTemplateErr = nil
	})
	if budgetTemplateErr != nil {
		t.Fatalf("budget fixture: %v", budgetTemplateErr)
	}
	path := filepath.Join(t.TempDir(), "budget.db")
	blob, err := os.ReadFile(budgetTemplatePath)
	if err != nil {
		t.Fatalf("read budget template: %v", err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("copy budget template: %v", err)
	}
	// OpenCounting, not OpenMigrated: the counter is the instrument these
	// tests exist for, and it has to open the store exactly as production
	// does. migrate() over an already-migrated copy is a no-op; verifySchema
	// still runs, so a bad template fails loudly here rather than as a wrong
	// number.
	db, counter, err := store.OpenCounting(path)
	if err != nil {
		t.Fatalf("OpenCounting: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc := NewStationService(db)
	geom := testdb.SceneGeometryOf(fx)
	svc.SetSceneGeometryResolver(func() *domain.SceneGeometry { return geom })
	groups := testdb.GroupsOf(fx)
	svc.SetCoreNodeGroupResolver(func() map[string][]string { return groups })

	counter.Reset()
	return budgetFixture{
		db: db, counter: counter, svc: svc,
		processID: budgetTemplateProcess, stationID: budgetTemplateStation,
	}
}

// buildBudgetTemplate writes the seeded press to a file the copies are taken
// from. Called once, under budgetTemplateOnce.
func buildBudgetTemplate(t *testing.T, fx scenefixtures.Plant) {
	t.Helper()
	dir, err := os.MkdirTemp("", "shingo-edge-budget-tpl-*")
	if err != nil {
		t.Fatalf("budget template dir: %v", err)
	}
	path := filepath.Join(dir, "budget.db")
	// Open, not OpenMigrated: this is the one that RUNS the migration chain.
	// The copies below are already carrying its schema.
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open budget template: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close budget template: %v", err)
		}
	}()

	seeded := testdb.SeedPlant(t, db, fx, "Press A1")

	// The catalog, up to 250 rows: a claim resolves its capacity from this on
	// every read, so its size is part of what the claim read costs. Seeded
	// from 0 so every BUDGET-PART code a claim below names has a row — a
	// missing one resolves to capacity 0 and logs, which is a different
	// fixture from the one S0 is measuring.
	budgetParts := budgetCatalog - len(fx.PayloadCatalog)
	for i := 0; i < budgetParts; i++ {
		code := fmt.Sprintf("BUDGET-PART-%04d", i)
		// An explicit ID: the catalog is keyed by Core's id, so a run of
		// entries left at zero is one row overwritten 227 times.
		if err := db.UpsertPayloadCatalog(&catalog.CatalogEntry{
			ID: int64(100000 + i), Name: code, Code: code, UOPCapacity: 24,
		}); err != nil {
			t.Fatalf("catalog %s: %v", code, err)
		}
	}

	// Six positions per style, taken from the press's own positions.
	nodes, err := db.ListProcessNodesByProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	var positions []string
	for _, n := range nodes {
		if len(positions) < budgetPositions {
			positions = append(positions, n.CoreNodeName)
		}
	}
	if len(positions) < budgetPositions {
		t.Fatalf("fixture has %d positions, need %d", len(positions), budgetPositions)
	}

	// A template claim off the fixture, so every seeded row is a shape a real
	// plant already runs and the validator already passes, not one invented here.
	existing, err := db.ListStylesByProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("list styles: %v", err)
	}
	tmplClaims, err := db.ListStyleNodeClaims(existing[0].ID)
	if err != nil || len(tmplClaims) == 0 {
		t.Fatalf("template claims: %v (%d)", err, len(tmplClaims))
	}
	tmpl := domain.InputFromClaim(tmplClaims[0])

	styleIDs := make([]int64, 0, budgetStyles)
	for _, s := range existing {
		styleIDs = append(styleIDs, s.ID)
	}
	for i := len(existing); i < budgetStyles; i++ {
		name := fmt.Sprintf("BUDGET STYLE %02d", i)
		id, err := db.CreateStyle(name, fmt.Sprintf("CAT%04d", i), seeded.ProcessID)
		if err != nil {
			t.Fatalf("create style %s: %v", name, err)
		}
		styleIDs = append(styleIDs, id)
	}
	// Every style gets the full six, the plant rows included, so the claim
	// count is 40x6 and not ten real ones plus thirty synthetic.
	for _, sid := range styleIDs {
		for p, node := range positions {
			in := tmpl
			in.StyleID = sid
			in.CoreNodeName = node
			in.PayloadCode = fmt.Sprintf("BUDGET-PART-%04d", (int(sid)*budgetPositions+p)%budgetParts)
			if _, err := db.UpsertStyleNodeClaim(in); err != nil {
				t.Fatalf("claim %s on style %d: %v", node, sid, err)
			}
		}
	}

	// 600 changeovers. Written directly: the point is the read, and the
	// service Create would also mint node tasks and runtime rows the history
	// read never looks at.
	for i := 0; i < budgetChangeovs; i++ {
		to := styleIDs[i%len(styleIDs)]
		from := styleIDs[(i+1)%len(styleIDs)]
		started := fmt.Sprintf("2026-%02d-%02d %02d:%02d:00", 1+(i%9), 1+(i%28), i%24, i%60)
		if _, err := db.Exec(`INSERT INTO process_changeovers
			(process_id, from_style_id, to_style_id, state, started_at, completed_at)
			VALUES (?,?,?,'completed',?,?)`,
			seeded.ProcessID, from, to, started, started); err != nil {
			t.Fatalf("changeover %d: %v", i, err)
		}
	}

	// 12 routing rows, enabled — an adopted set, the state a press in
	// production is in.
	roles := []string{domain.RoutingRoleSource, domain.RoutingRoleStaging, domain.RoutingRoleDestination}
	for i := 0; i < budgetRouting; i++ {
		id, err := db.UpsertRoutingNode(domain.RoutingNodeInput{
			ProcessID:    seeded.ProcessID,
			CoreNodeName: fmt.Sprintf("SMN_%02d", 10+i),
			Role:         roles[i%len(roles)],
			Sequence:     i,
			Enabled:      true,
			Origin:       domain.RoutingOriginEngineer,
			CalledBy:     "budget",
		})
		if err != nil {
			t.Fatalf("routing %d: %v", i, err)
		}
		if err := db.SetRoutingNodeEnabled(seeded.ProcessID, id, true, "budget"); err != nil {
			t.Fatalf("enable routing %d: %v", i, err)
		}
	}

	// Three presets.
	for i := 0; i < budgetPresets; i++ {
		cells := []domain.FlowCell{
			{CoreNodeName: positions[0], Role: tmpl.Role, SwapMode: tmpl.SwapMode},
			{CoreNodeName: positions[1], Role: tmpl.Role, SwapMode: tmpl.SwapMode},
		}
		shape, err := json.Marshal(map[string]any{"cells": cells})
		if err != nil {
			t.Fatalf("marshal preset: %v", err)
		}
		if _, err := db.CreateFlowPreset(domain.FlowPresetInput{
			ProcessID: seeded.ProcessID,
			Name:      fmt.Sprintf("Budget shape %d", i),
			FlowJSON:  string(shape),
			CreatedBy: "budget",
		}); err != nil {
			t.Fatalf("preset %d: %v", i, err)
		}
	}

	var stationID int64
	for _, id := range seeded.Stations {
		if stationID == 0 || id < stationID {
			stationID = id
		}
	}
	if stationID == 0 {
		t.Fatalf("fixture seeded no operator station")
	}

	budgetTemplatePath = path
	budgetTemplateProcess = seeded.ProcessID
	budgetTemplateStation = stationID
}

// mustJSON marshals or fails the test. A measurement taken from a failed
// marshal is a zero and would be reported as a very good number.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	testutil.MustNoErr(t, err, "marshal")
	return raw
}
