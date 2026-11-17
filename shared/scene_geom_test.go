package shared

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSceneGeomAgreesWithTheRecordedVectors runs the Node agreement test for
// shared/scene-geom.js against scene-geom.vectors.json — the output of the
// Core copy it was promoted from, recorded at the promoting commit. A vector
// that no longer matches is a behaviour change to both surfaces that draw the
// plant. Skipped if `node` is not on PATH (matches the other JS wrappers).
func TestSceneGeomAgreesWithTheRecordedVectors(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping scene-geom agreement test")
	}
	out, err := exec.Command(nodePath, filepath.Join("testdata", "js", "scene-geom.test.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("scene-geom agreement test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("scene-geom agreement: %s", out)
}

// TestSceneNetRoutesAsTheDesktopDoes runs scene-net.test.js: the scene network
// and the route model of shared/scene-net.js on the synthetic route scene. The
// route vectors are the Edge desktop process map's, whose drawing and own
// route pin were removed (2026-11-14), with the dead-end destination fixed;
// the lane list is the order and orientation Core's robot map has always drawn
// its aisles in.
func TestSceneNetRoutesAsTheDesktopDoes(t *testing.T) {
	runSceneJS(t, "scene-net.test.js")
}

// TestScenePathDrawsRDSPath runs scene-path.test.js: shared/scene-path.js
// drawing a robot's path along the stored rows (lead decision D6).
func TestScenePathDrawsRDSPath(t *testing.T) {
	runSceneJS(t, "scene-path.test.js")
}

func runSceneJS(t *testing.T, script string) {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping %s", script)
	}
	out, err := exec.Command(nodePath, filepath.Join("testdata", "js", script)).CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed:\n%s\nerror: %v", script, out, err)
	}
	t.Logf("%s: %s", script, out)
}

// TestSceneLook_OneSheetForBothSurfaces holds the owner's "all scene drawing
// runs on one machinery in shared/" for the look of it: shared/scene-view.css
// is the one sheet for the classes shared/scene-view.js and
// shared/comet-layer.js draw.
//
//   - it styles every class they emit, and no other (map-node-charge, the
//     charge ring, is drawn with presentation attributes only and never had a
//     rule; is-paused and is-offline are map-robot's two modifiers);
//   - neither surface's own sheet names one of those classes again: Core's
//     dashboard.css, the Edge station's operator.css (the panel kept a scoped
//     copy until 2026-11-14);
//   - both pages that draw a scene link it: Core's robot map and the Edge's
//     operator station (the robot panel).
//
// A second copy of a rule here would let the two screens of one floor draw the
// same robot two ways, which is what moving them was for.
func TestSceneLook_OneSheetForBothSurfaces(t *testing.T) {
	t.Parallel()
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return string(b)
	}
	emitted := map[string]bool{}
	literal := regexp.MustCompile(`'(map-[a-z-]+)'`)
	for _, f := range []string{"scene-view.js", "comet-layer.js"} {
		src := jsLineCommentPattern.ReplaceAllString(cssCommentPattern.ReplaceAllString(read(f), " "), " ")
		for _, m := range literal.FindAllStringSubmatch(src, -1) {
			emitted[m[1]] = true
		}
	}
	if len(emitted) < 15 {
		t.Fatalf("found only %d classes the shared drawing emits (%v): the class walk is broken", len(emitted), emitted)
	}
	styled := sceneSheetClasses(read("scene-view.css"))
	modifiers := map[string]bool{"is-paused": true, "is-offline": true}
	for c := range styled {
		if !emitted[c] && !modifiers[c] {
			t.Errorf("scene-view.css styles .%s, which neither scene-view.js nor comet-layer.js draws", c)
		}
	}
	for c := range emitted {
		if !styled[c] && c != "map-node-charge" {
			t.Errorf("scene-view.js or comet-layer.js draws .%s and scene-view.css has no rule for it", c)
		}
	}
	for _, own := range []string{
		filepath.Join("..", "shingo-core", "www", "static", "dashboard.css"),
		filepath.Join("..", "shingo-edge", "www", "static", "operator-station", "operator.css"),
	} {
		for c := range sceneSheetClasses(read(own)) {
			if emitted[c] {
				t.Errorf("%s names .%s again: the scene's look is shared/scene-view.css's alone", own, c)
			}
		}
	}
	for _, page := range []string{
		filepath.Join("..", "shingo-core", "www", "templates", "dashboard-map.html"),
		filepath.Join("..", "shingo-edge", "www", "templates", "operator-display.html"),
	} {
		if !strings.Contains(read(page), `{{assetURL "/static/shared/scene-view.css"}}`) {
			t.Errorf("%s draws a scene and does not link /static/shared/scene-view.css", page)
		}
	}
}

// sceneSheetClasses is every class a stylesheet names in a selector, at any
// depth (a compound or a descendant counts: `.os-x .map-aisle` names
// map-aisle), comments, at-rule preludes and keyframe stops left out.
func sceneSheetClasses(css string) map[string]bool {
	out := map[string]bool{}
	class := regexp.MustCompile(`\.([A-Za-z_][\w-]*)`)
	for _, m := range regexp.MustCompile(`([^{};]+)\{`).FindAllStringSubmatch(cssCommentPattern.ReplaceAllString(css, " "), -1) {
		sel := strings.TrimSpace(m[1])
		if sel == "" || strings.HasPrefix(sel, "@") || strings.HasSuffix(sel, "%") || sel == "from" || sel == "to" {
			continue
		}
		for _, c := range class.FindAllStringSubmatch(sel, -1) {
			out[c[1]] = true
		}
	}
	return out
}

// TestRobotMotion_OneDefinitionForBothSurfaces: neither page that draws a
// scene defines the calm's limit or the pickup radius of its own, both take
// them from shared/robot-motion.js (owner: one machinery in shared/; lead
// ruling on U10, 4), and neither judges for itself whether a robot is moving:
// that is Core's robot poll's word (shingo-core/engine/robot_motion.go), which
// both read, so no page draws a robot parked until it has seen it move.
// orderLeg stays per page: its inputs differ (Core reads the board's
// carrying), and each copy says so.
func TestRobotMotion_OneDefinitionForBothSurfaces(t *testing.T) {
	t.Parallel()
	own := regexp.MustCompile(`(?m)^\s*(?:var|let|const)\s+(COMET_LIMIT|PICKUP_RADIUS)\b`)
	judge := regexp.MustCompile(`\b(MOVE_LINGER_MS|movedFrom|movingAt|lastMoveAt)\b`)
	imp := regexp.MustCompile(`(?s)import\s*\{([^}]*)\}\s*from\s*'/static/shared/robot-motion\.js'`)
	for _, page := range []string{
		filepath.Join("..", "shingo-core", "www", "static", "pages", "dashboard-map.js"),
		filepath.Join("..", "shingo-edge", "www", "static", "operator-station", "operator-robot-map.js"),
	} {
		b, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		src := string(b)
		for _, m := range own.FindAllStringSubmatch(src, -1) {
			t.Errorf("%s declares its own %s: it is shared/robot-motion.js's", page, m[1])
		}
		if m := judge.FindString(src); m != "" {
			t.Errorf("%s names %s: whether a robot moves is Core's poll's word, read as `moving`", page, m)
		}
		m := imp.FindStringSubmatch(src)
		if m == nil {
			t.Errorf("%s does not import shared/robot-motion.js", page)
			continue
		}
		for _, want := range []string{"COMET_LIMIT", "PICKUP_RADIUS"} {
			if !regexp.MustCompile(`\b` + want + `\b`).MatchString(m[1]) {
				t.Errorf("%s does not take %s from shared/robot-motion.js", page, want)
			}
		}
		if !strings.Contains(src, "function orderLeg(") {
			t.Errorf("%s has no orderLeg of its own: the leg is per page (its inputs differ)", page)
		}
	}
}
