package www

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/service"
)

// TestOperatorFlowRendersAWholePlantCell builds three station views over
// plant A — style 7 (two index pairs), style 11 (two staging
// moves) and style 7 with no scene cached (the schematic) — through the same
// BuildView the HMI calls, writes them out, and hands them to the Node test
// that renders operator-flow.js's picture and checks what is drawn. The Go
// side owns the data so the JS cannot be right against a shape the server
// never produces.
func TestOperatorFlowRendersAWholePlantCell(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping cell picture render test")
	}
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	stationID := seeded.Stations["screen-a4"]
	svc := service.NewStationService(db)
	svc.SetBinTypeResolver(testdb.BinTypeOf(fx))
	svc.SetCoreNodeGroupResolver(func() map[string][]string { return testdb.GroupsOf(fx) })
	geom := testdb.SceneGeometryOf(fx)

	dir := t.TempDir()
	write := func(name, styleName string, g *domain.SceneGeometry) string {
		t.Helper()
		styleID, ok := seeded.Styles[styleName]
		if !ok {
			t.Fatalf("style %q not seeded", styleName)
		}
		if err := db.SetActiveStyle(seeded.ProcessID, &styleID); err != nil {
			t.Fatalf("set active style: %v", err)
		}
		svc.SetSceneGeometryResolver(func() *domain.SceneGeometry { return g })
		view, err := svc.BuildView(context.Background(), stationID)
		if err != nil {
			t.Fatalf("BuildView %s: %v", name, err)
		}
		raw, err := json.Marshal(view)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		p := filepath.Join(dir, name+".json")
		if err := os.WriteFile(p, raw, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	style7 := write("style7", "PART 40421-RVJ56.37", geom)
	style11 := write("style11", "PART 68644-WSL97.20", geom)
	schematic := write("schematic", "PART 40421-RVJ56.37", nil)

	script := filepath.Join("static", "operator-station", "operator-flow.test.js")
	out, err := exec.Command(nodePath, script, style7, style11, schematic).CombinedOutput()
	if err != nil {
		t.Fatalf("operator-flow render test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator-flow: %s", out)
}
