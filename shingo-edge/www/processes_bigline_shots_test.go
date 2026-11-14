//go:build shots

package www

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
)

// processes_bigline_shots_test.go — the Processes page photographed on the
// big line (testdb.BigLinePlant: 23 positions, 108 nodes, 46 parts, 13
// styles, 3 recipes in 12 versions, 2 light curtains, 1 Next carrier), at
// desktop width, by the real engine, router and templates.
//
// Not CI. Each slice of the one-process-page build is photographed here before
// and after, so its report shows the page at a real press's size and not on a
// four-position line.
//
// Run:
//
//	BIGLINE_SHOTS_OUT=<dir> go test -tags shots ./www -run TestBigLineShots -count=1
//
// What it writes: bigline-list.png, bigline-styles.png, bigline-settings.png.
func TestBigLineShots(t *testing.T) {
	out := os.Getenv("BIGLINE_SHOTS_OUT")
	if out == "" {
		t.Skip("BIGLINE_SHOTS_OUT not set")
	}
	chrome := chromeBinary()
	if chrome == "" {
		t.Fatal("no Chrome binary: set COMPOSER_SHOTS_CHROME")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", out, err)
	}

	fx := testdb.BigLinePlant()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "BIG-PRESS")
	pid := seeded.ProcessID
	testdb.SeedRoutingSet(t, db, pid, "shots")
	// What a running big press has that the plant fixture does not carry: a
	// description and group, a counting production counter, its part set,
	// the light curtains on two positions, and the running style's Next
	// carrier (Robot 2).
	if err := db.UpdateProcess(pid, "BIG-PRESS", "Synthetic big press"); err != nil {
		t.Fatalf("describe: %v", err)
	}
	gid, err := db.CreateProcessGroup("Big Presses", "")
	if err != nil {
		t.Fatalf("group: %v", err)
	}
	if err := db.SetProcessGroupID(pid, &gid); err != nil {
		t.Fatalf("set group: %v", err)
	}
	if err := db.SetProcessCounter(pid, "PLC_BIG", "BigCount", true, "", false); err != nil {
		t.Fatalf("counter: %v", err)
	}
	if err := db.ReplaceProcessPayloads(pid, testdb.BigLineParts()); err != nil {
		t.Fatalf("part set: %v", err)
	}
	on := true
	for _, n := range testdb.BigLineCurtains {
		if err := processes.SetNodeCurtain(db.DB, seeded.Nodes[n], true, "PLC_BIG", "Curtain_"+n, &on); err != nil {
			t.Fatalf("curtain %s: %v", n, err)
		}
	}
	if _, err := db.DB.Exec(`UPDATE style_node_claims SET refill_by = ? WHERE style_id = ? AND core_node_name = 'BPN_007KLM'`,
		string(protocol.RefillByIndexRobot), seeded.Styles[fx.Styles[0].Name]); err != nil {
		t.Fatalf("next carrier: %v", err)
	}

	cfg := config.Defaults()
	cfg.StationUID = "shots.bigline"
	cfg.Namespace = "shots"
	cfg.LineID = "bigline"
	cfg.Messaging.StationID = cfg.StationUID
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf})
	eng.SetPayloadBinTypes(nil)
	eng.Start()
	t.Cleanup(eng.Stop)
	var nodes []protocol.NodeInfo
	for _, n := range fx.Nodes {
		name := n.Name
		if n.ParentName != nil && *n.ParentName != "" {
			name = *n.ParentName + "." + n.Name
		}
		typ := ""
		if n.NodeTypeCode != nil {
			typ = *n.NodeTypeCode
		}
		nodes = append(nodes, protocol.NodeInfo{Name: name, NodeType: typ})
	}
	eng.SetCoreNodes(nodes, false)
	var pbt []protocol.PayloadBinTypeInfo
	for code, types := range fx.PayloadBinTypeCodes() {
		for _, bt := range types {
			pbt = append(pbt, protocol.PayloadBinTypeInfo{PayloadCode: code, BinTypeCode: bt})
		}
	}
	eng.SetPayloadBinTypes(pbt)

	srv := shotsServer(t, shotsRouter(t, eng), nil)

	// THE RECIPES THROUGH THE REAL DOOR: "Save as recipe" from a style, the
	// same name again making the next version (store CreateFlowPreset), so
	// the real handler validates each shape. 6 + 2 + 4 = 12 versions.
	post := func(path string, body any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			var b bytes.Buffer
			_, _ = b.ReadFrom(resp.Body)
			t.Fatalf("POST %s: %d %s", path, resp.StatusCode, b.String())
		}
	}
	recipes := []struct {
		name     string
		style    int
		versions int
	}{{"Big Index - One Spot", 0, 6}, {"Big Index - Totes", 3, 2}, {"Big Index - Two Spots", 1, 4}}
	for _, r := range recipes {
		for v := 0; v < r.versions; v++ {
			post(fmt.Sprintf("/api/processes/%d/presets", pid),
				map[string]any{"name": r.name, "from_style_id": seeded.Styles[fx.Styles[r.style].Name]})
		}
	}

	shot := func(file, path string, w, h int) {
		t.Helper()
		target := filepath.Join(out, file)
		_ = os.Remove(target)
		args := []string{"--headless=new", "--disable-gpu", "--hide-scrollbars", "--force-dark-mode",
			"--blink-settings=preferredColorScheme=0",
			fmt.Sprintf("--window-size=%d,%d", w, h), "--virtual-time-budget=8000",
			"--screenshot=" + target, srv.URL + path}
		cmd := exec.Command(chrome, args...)
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("chrome %s: %v\n%s", file, err, raw)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("%s: no screenshot written", file)
		}
		t.Logf("wrote %s", target)
	}
	shot("bigline-list.png", "/processes", 1440, 900)
	shot("bigline-styles.png", fmt.Sprintf("/processes?process=%d", pid), 1440, 1500)
	shot("bigline-settings.png", fmt.Sprintf("/processes?process=%d#tab=settings", pid), 1440, 4200)
}
