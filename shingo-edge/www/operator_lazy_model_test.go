package www

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// operator_lazy_model_test.go — what the operator station loads at boot, and
// what it loads only when a screen needs it.
//
// The station is a Pi on plant Wi-Fi. Its module graph takes three small
// shared files, and composer-model.js, its largest file, loads behind one
// loader, composer-boot.js's ensureComposerModel, which the flow panel and the
// composer both wait on. The Node test boots the shipping modules and watches
// what they request; the census tests below hold the source to the same
// shape, so a new import or a second loader fails here and not on the floor.

// TestOperatorLazyModelJS runs testdata/static/operator-station/
// operator-lazy-model.test.js once per mode: a cold board then the style chip
// and CHANGEOVER, a cold load at #flow, and a model request that fails.
// Skipped if `node` is not on PATH (matches the other JS test wrappers).
func TestOperatorLazyModelJS(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("testdata", "static", "operator-station", "operator-lazy-model.test.js")
	for _, mode := range []string{"cold", "flow-hash", "lost"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			out, err := exec.Command(nodePath, scriptPath, mode).CombinedOutput()
			if err != nil {
				t.Fatalf("operator lazy-model JS test (%s) failed:\n%s\nerror: %v", mode, out, err)
			}
			t.Logf("%s", out)
		})
	}
}

// stationEntry is the module operator-display.html boots the station from.
const stationEntry = "/static/operator-station/operator.js"

// TestStationBoot_SharedFiles: the station's module graph reaches these shared
// files and no others. Each is small and has a job the station does: the
// server clock (clock.js), the one event stream (bus.js), the quote-safe
// escape its renderers concatenate attributes with (esc.js). The touch layer
// (toasts, press guard, touch sheets) is the station's own and is not
// extracted, so a shared UI module here is a decision to make, not a
// convenience: add it to the list with the reason.
func TestStationBoot_SharedFiles(t *testing.T) {
	t.Parallel()
	want := []string{"/static/shared/bus.js", "/static/shared/clock.js", "/static/shared/esc.js"}
	reach := staticAssets().ImportMap(stationEntry)
	if len(reach) == 0 {
		t.Fatalf("the import closure of %s is empty: the handler no longer serves the entry", stationEntry)
	}
	var got []string
	for u := range reach {
		if strings.HasPrefix(u, "/static/shared/") {
			got = append(got, u)
		}
	}
	sort.Strings(got)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the station reaches these shared files:\n  got  %v\n  want %v", got, want)
	}
}

var (
	stationStringURLRe    = regexp.MustCompile(`['"][^'"\n]*composer-model\.js['"]`)
	stationFlowImportRe   = regexp.MustCompile(`import\s*\{([^}]*)\}\s*from\s*['"](?:\./|/static/operator-station/)operator-flow\.js['"]`)
	stationRenderImportRe = regexp.MustCompile(`(?:from|import)\s*\(?\s*['"](?:\./|/static/operator-station/)composer-render\.js['"]`)
)

// TestStationBoot_ComposerModelBehindOneLoader holds the two halves of "every
// call site waits for the loader" that a test of today's taps cannot see.
//
//  1. ONE LOADER. composer-boot.js is the only station file that names
//     composer-model.js's URL: a second loader would be a second request, and
//     a second place a lost load goes unsaid.
//  2. NO NEW WAY IN. Most of operator-flow.js's picture functions read
//     window.ComposerModel unguarded (modulesOf, stagingNamed, worldRows,
//     drawRelation, renderFlowPicture, relationPlacement). The station reaches
//     them two ways, each behind the loader: the flow panel, whose
//     syncFlowPanel waits on it, and composer-render.js, which only
//     ensureComposer loads and only after the model is in (so no other station
//     file imports it). A station file importing another operator-flow.js name
//     could call a picture function at boot with no model on window. The
//     Processes page imports those names too and loads the model with its own
//     tag (processes.html; TestProcessesPage_BootsTheDesktop holds it there).
func TestStationBoot_ComposerModelBehindOneLoader(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("static", "operator-station")
	files, err := filepath.Glob(filepath.Join(dir, "*.js"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob %s: %v (%d files)", dir, err, len(files))
	}
	panelNames := map[string]bool{
		"mountFlowPanel": true, "openFlowPanel": true, "syncFlowPanel": true,
		"closeFlowPanel": true, "isFlowPanelOpen": true,
	}
	namers := []string{}
	for _, f := range files {
		base := filepath.Base(f)
		if base == "composer-model.js" {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// A comment naming a file is not a request for it
		// (stripJSComments, composer_vocabulary_drift_test.go).
		src := stripJSComments(string(raw))
		if stationStringURLRe.MatchString(src) {
			namers = append(namers, base)
		}
		if base != "composer-boot.js" && stationRenderImportRe.MatchString(src) {
			t.Errorf("%s imports composer-render.js: the composer's module is loaded by ensureComposer "+
				"alone (composer-boot.js), after the model, and an import would load it at boot with none", base)
		}
		if base == "composer-render.js" {
			continue // loaded behind the model (above): any name is safe there
		}
		for _, m := range stationFlowImportRe.FindAllStringSubmatch(src, -1) {
			for _, name := range strings.Split(m[1], ",") {
				name = strings.TrimSpace(strings.SplitN(strings.TrimSpace(name), " as ", 2)[0])
				if name != "" && !panelNames[name] {
					t.Errorf("%s imports %s from operator-flow.js. The station calls the picture only through "+
						"the flow panel, which waits for the model's loader (syncFlowPanel); call it from behind "+
						"ensureComposerModel (composer-boot.js), or add the name here with why it reads no model",
						base, name)
				}
			}
		}
	}
	if strings.Join(namers, " ") != "composer-boot.js" {
		t.Errorf("the station files that name composer-model.js's URL: %v, want only composer-boot.js "+
			"(ensureComposerModel, the one loader)", namers)
	}
}
