package shared

import (
	"bytes"
	"encoding/json"
	"flag"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var updateHelperPinsGolden = flag.Bool("update-helper-pins-golden", false,
	"rewrite testdata/helper-pins/*.txt from the UI helpers as they run today")

// TestHelperPins characterises the UI helpers on each surface as they are
// BEFORE the shared-UI brief's S2 replaces the three copies with one set, so
// the swap behind each name shows up as that surface's golden moving:
//
//   - modal.<surface>.txt: showModal / hideModal — open and close classes,
//     Escape, backdrop click, body scroll lock, focus, fields cleared on hide;
//   - dialog.<surface>.txt: confirm / prompt (Core's uiConfirm, uiPrompt and
//     uiConfirmWithInput) — what each resolves to on OK, Cancel, Escape,
//     backdrop and a second open, the DOM it writes, focus;
//   - toast.<surface>.txt: the DOM, levels, lifetime, stacking, escaping;
//   - api.<surface>.txt: each api's shape, the request every verb sends, and
//     what every answer (200/204/empty/non-JSON, the servers' refusal bodies,
//     plain text, 5xx, no answer) resolves or rejects with;
//   - sse.*.txt: the Edge hub's createSSE, shared createSSE, the onSSE bus,
//     what Core's app.js does with the bus on import, and htmx-sse.js —
//     URL, dispatch, reconnect backoff, watchdog, build change, close;
//   - htmx-errors.edge-admin.txt: who on Edge admin handles an htmx request
//     error (nobody, today);
//   - hubs.txt: which copy each hub exports under each helper name.
//
// The surfaces are core-hub (shingo-core/www/static/app.js), edge-hub
// (shingo-edge/www/static/js/shingoedge.js) and shared (utils.js here; the
// bus pins load bus.js). The
// harness and its DOM, clock, fetch and EventSource stubs are in
// testdata/helper-pins-harness (pins.js says what each golden covers; world.js
// how a page is built). Every source file loads under node unmodified.
//
// Regenerate with
//
//	go test . -run TestHelperPins -update-helper-pins-golden
//
// and read the diff: a regenerated golden pins nothing unless someone read it.
func TestHelperPins(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the UI helper pins")
	}
	cmd := exec.Command(nodePath, filepath.Join("testdata", "helper-pins-harness", "pins.js"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the helper pins: %v\n%s", err, stderr.String())
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode the pins' output: %v\n%s", err, stderr.String())
	}
	if len(got) == 0 {
		t.Fatal("the pins produced no golden; the harness has drifted")
	}

	// ITS OWN DIRECTORY: the stale check below owns every file in it, so no
	// other pin may write there.
	dir := filepath.Join("testdata", "helper-pins")
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if *updateHelperPinsGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for _, n := range names {
		path := filepath.Join(dir, n)
		if *updateHelperPinsGolden {
			if err := os.WriteFile(path, []byte(got[n]), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: no golden (%v); regenerate with -update-helper-pins-golden and read it", n, err)
			continue
		}
		if line, w, g, ok := helperPinsFirstDiff(string(want), got[n]); !ok {
			t.Errorf("%s: the helpers moved at line %d; regenerate only for a named change\n  golden:  %s\n  helpers: %s",
				path, line, w, g)
		}
	}

	// A golden no pin writes any more pins nothing and reads as if it did.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if _, ok := got[e.Name()]; ok || e.IsDir() {
			continue
		}
		if *updateHelperPinsGolden {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				t.Fatalf("remove stale %s: %v", e.Name(), err)
			}
			continue
		}
		t.Errorf("%s: a golden no pin writes; delete it, or restore the pin that wrote it", filepath.Join(dir, e.Name()))
	}
	t.Logf("helper pins: %s", strings.Join(names, ", "))
}

// helperPinsFirstDiff finds the first line two texts disagree on, 1-based,
// with both sides of it; ok is true when they are equal.
func helperPinsFirstDiff(want, got string) (line int, w, g string, ok bool) {
	if want == got {
		return 0, "", "", true
	}
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		a, b := "(end of file)", "(end of file)"
		if i < len(wl) {
			a = wl[i]
		}
		if i < len(gl) {
			b = gl[i]
		}
		if a != b {
			return i + 1, a, b, false
		}
	}
	return len(wl), "", "", false
}

// ── the shared modules' census: one definition, an importer, one URL ─────────
//
// utils.js was split into one-job files. Three rules keep the split from
// growing back, and they read the same sources the browser does:
// shared/*.js and every page module and template of Core and Edge.
//
//   - TestSharedHelpersDefinedOnce: a name a shared module exports is defined
//     nowhere else, in any tree, except the copies sharedHelperCopies lists. A
//     second shared module may export the name only when it is one of those
//     copies (utils.js's, while the hubs still take it): the other module is
//     the name's home.
//   - TestSharedExportsHaveImporters: every export of a shared module is
//     imported by some page, hub or other shared module, except the ones
//     sharedExportsNoImporter lists.
//   - TestSharedModulesHaveOneURL: a shared module is imported by exactly one
//     spelling, /static/shared/<file>, and no shared module re-exports
//     another's names. A module reached by two URLs is two instances, so a page
//     would run two clocks or two buses.
//
// Both lists are ratchets: an entry that no longer matches fails, so the
// change that deletes a copy (a hub's, a classic script's escaper) deletes its
// entry with it.

// sharedHelperCopies are the definitions of a shared name outside its shared
// module that exist today, as "path name". Each is someone else's to remove.
var sharedHelperCopies = map[string]string{
	// The Edge hub's own copies, likewise.
	"shingo-edge/www/static/js/shingoedge.js createSSE": "Edge hub copy",
	"shingo-edge/www/static/js/shingoedge.js showModal": "Edge hub copy",
	"shingo-edge/www/static/js/shingoedge.js hideModal": "Edge hub copy",
	"shingo-edge/www/static/js/shingoedge.js confirm":   "Edge hub copy",
	"shingo-edge/www/static/js/shingoedge.js prompt":    "Edge hub copy",
	"shingo-edge/www/static/js/shingoedge.js toast":     "Edge hub copy",
	"shingo-edge/www/static/js/shingoedge.js api":       "Edge hub copy",
	// The station's own builder: it sets properties where dom.js's el sets
	// attributes (operator-util.js's comment), and the station's touch layer is
	// not extracted (brief v2 S2).
	"shingo-edge/www/static/operator-station/operator-util.js el": "station's own el",
	// Page-local helpers that share a name and not a signature.
	"shingo-core/www/static/pages/robots.js switchTab": "wraps dom.js's (imported as showTab) to load a panel on first view, style guide §22",
	// Private escapers: the two classic scripts cannot import a module, so
	// each carries its own.
	"shingo-edge/www/static/js/pages/catid-health.js esc": "classic script's private escaper",
	"shingo-edge/www/static/js/pages/station-card.js esc": "classic script's private escaper",
	// The components' names (layer.js, notice.js) still defined where the
	// pages take them today: utils.js's copies, which the hubs re-export, and
	// the Processes page's own sheet shell.
	"shared/utils.js showModal": "adopted in wave C (s2-hubs): the hubs move to layer.js's",
	"shared/utils.js hideModal": "adopted in wave C (s2-hubs): the hubs move to layer.js's",
	"shared/utils.js confirm":   "adopted in wave C (s2-hubs): the hubs move to layer.js's",
	"shared/utils.js prompt":    "adopted in wave C (s2-hubs): the hubs move to layer.js's",
	"shared/utils.js toast":     "adopted in wave C (s2-hubs): the hubs move to notice.js's",
	"shingo-edge/www/static/js/pages/processes-desktop.js openSheet": "adopted in wave C (s2-processes): the page moves to layer.js's",
}

// sharedExportsNoImporter are the exports no page imports, as "file name".
var sharedExportsNoImporter = map[string]string{
	// No page imports the shared copies: Edge's pages take the Edge hub's,
	// and Core's pages use uiConfirm / uiPrompt and the bus.
	"utils.js confirm":   "pages take the Edge hub's copy",
	"utils.js prompt":    "pages take the Edge hub's copy",
	"utils.js createSSE": "pages take the Edge hub's copy or the bus",
	// utils.js's own banner, still shown by its createSSE; Core's hub shows
	// notice.js's banner on the bus's build-changed instead.
	"utils.js showRefreshBanner": "read by createSSE alone; Core's hub shows notice.js's banner",
	// The bus's teardown and the clock's single repaint: exported for the
	// node tests (testdata/js) and for a page that needs one.
	"bus.js closeSSEBus":           "teardown, read by utils.dashboard.test.js",
	"clock.js renderLiveDurations": "one repaint, read by utils.livedurations.test.js",
	// The shared components, which no page imports until wave C swaps the
	// hubs and the Processes page onto them.
	"layer.js openSheet":         "adopted in wave C (s2-processes)",
	"layer.js closeLayer":        "adopted in wave C (s2-processes)",
	"layer.js layerDepth":        "adopted in wave C (s2-processes): the page's own Escape",
	"http.js http":               "adopted in wave C (s2-hubs / s2-processes)",
	"http.js hookHtmxErrors":     "adopted in wave C (s2-hubs): installed on Edge admin",
	"http.js REQUEST_TIMEOUT_MS": "the deadline, read by testdata/components/http.test.js",
	"picker.js createPicker":     "adopted in wave C (s2-processes), its adapters in S3",
	"notice.js clearBanner":      "adopted in S5 (the lost-connection banner)",
}

var (
	sharedExportDeclRe = regexp.MustCompile(`(?m)^export[ \t]+(?:async[ \t]+)?(?:function[ \t]*\*?|const|let|var|class)[ \t]*([A-Za-z_$][\w$]*)`)
	sharedExportListRe = regexp.MustCompile(`(?m)^export[ \t]*\{([^}]*)\}[ \t]*(from\b)?`)
	// A definition: a function declaration at any depth; a top-level const,
	// let or var whose value is a function or an object; a nested one whose
	// value is a function.
	sharedDefFuncRe = regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?(?:async[ \t]+)?function[ \t]*\*?[ \t]*([A-Za-z_$][\w$]*)[ \t]*\(`)
	sharedDefTopRe  = regexp.MustCompile(`(?m)^(?:export[ \t]+)?(?:const|let|var)[ \t]+([A-Za-z_$][\w$]*)[ \t]*=[ \t]*(?:async\b|function\b|\(|\{|[A-Za-z_$][\w$]*[ \t]*=>)`)
	sharedDefNestRe = regexp.MustCompile(`(?m)^[ \t]+(?:const|let|var)[ \t]+([A-Za-z_$][\w$]*)[ \t]*=[ \t]*(?:async[ \t]*)?(?:function\b|\([^)\n]*\)[ \t]*=>|[A-Za-z_$][\w$]*[ \t]*=>)`)
	// What a module takes: named imports and re-exports, then every
	// specifier at all (side-effect and dynamic imports too) for the URL rule.
	sharedNamedImportRe = regexp.MustCompile(`\b(?:import|export)[ \t]*\{([^}]*)\}\s*from[ \t]*['"]([^'"]+)['"]`)
	sharedAnySpecRe     = regexp.MustCompile(`(?:\bfrom[ \t]*|\bimport[ \t]*\(?[ \t]*)['"]([^'"]+)['"]`)
	sharedLineCommentRe = regexp.MustCompile(`(?m)(^|[^:'"\\])//.*$`)
	sharedTmplCommentRe = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)
)

// sharedSource is one browser source: its repo path, the URL it is served at
// ("" for a template, whose inline modules use absolute specifiers), and its
// code with comments dropped (a comment naming a helper is not a copy of it).
type sharedSource struct {
	path, url, code string
}

// sharedCensusSources reads shared/*.js and the served Core and Edge trees
// (static and templates; vendored and minified files left out).
func sharedCensusSources(t *testing.T) []sharedSource {
	t.Helper()
	var out []sharedSource
	add := func(repoPath, url string, raw []byte) {
		code := sharedLineCommentRe.ReplaceAllString(string(raw), "$1")
		code = sharedTmplCommentRe.ReplaceAllString(code, "")
		out = append(out, sharedSource{path: repoPath, url: url, code: code})
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read shared/: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		raw, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		add("shared/"+e.Name(), "/static/shared/"+e.Name(), raw)
	}
	for _, tree := range []struct{ dir, url string }{
		{"shingo-core/www/static", "/static/"},
		{"shingo-core/www/templates", ""},
		{"shingo-edge/www/static", "/static/"},
		{"shingo-edge/www/templates", ""},
	} {
		root := filepath.Join("..", filepath.FromSlash(tree.dir))
		err := fs.WalkDir(os.DirFS(root), ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "vendor" {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, ".min.js") || !(strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".html")) {
				return nil
			}
			raw, err := fs.ReadFile(os.DirFS(root), p)
			if err != nil {
				return err
			}
			url := ""
			if tree.url != "" {
				url = tree.url + p
			}
			add(tree.dir+"/"+p, url, raw)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree.dir, err)
		}
	}
	if len(out) < 100 {
		t.Fatalf("read only %d sources; the walk is broken, not the trees", len(out))
	}
	return out
}

// sharedExports maps each shared module's file name to the names it exports.
func sharedExports(srcs []sharedSource) map[string][]string {
	out := map[string][]string{}
	for _, s := range srcs {
		if !strings.HasPrefix(s.path, "shared/") {
			continue
		}
		file := strings.TrimPrefix(s.path, "shared/")
		for _, m := range sharedExportDeclRe.FindAllStringSubmatch(s.code, -1) {
			out[file] = append(out[file], m[1])
		}
		for _, m := range sharedExportListRe.FindAllStringSubmatch(s.code, -1) {
			if m[2] != "" {
				continue // a re-export; TestSharedModulesHaveOneURL refuses it
			}
			for _, n := range strings.Split(m[1], ",") {
				if f := strings.Fields(n); len(f) > 0 {
					out[file] = append(out[file], f[len(f)-1])
				}
			}
		}
	}
	return out
}

// sharedResolve is the URL an import specifier names from a module served at
// fromURL, query dropped; "" when it cannot be told (a relative specifier in a
// template).
func sharedResolve(fromURL, spec string) string {
	spec = strings.SplitN(spec, "?", 2)[0]
	if strings.HasPrefix(spec, "/") {
		return path.Clean(spec)
	}
	if fromURL == "" {
		return ""
	}
	return path.Join(path.Dir(fromURL), spec)
}

func TestSharedHelpersDefinedOnce(t *testing.T) {
	t.Parallel()
	srcs := sharedCensusSources(t)
	home := map[string]string{} // name -> the shared file that exports it
	for file, names := range sharedExports(srcs) {
		for _, n := range names {
			other, dup := home[n]
			if !dup {
				home[n] = file
				continue
			}
			// Two exporters: one may be a listed copy, and the other is home.
			_, otherIsCopy := sharedHelperCopies["shared/"+other+" "+n]
			_, fileIsCopy := sharedHelperCopies["shared/"+file+" "+n]
			switch {
			case otherIsCopy && !fileIsCopy:
				home[n] = file
			case fileIsCopy && !otherIsCopy:
			default:
				t.Errorf("%s is exported by both shared/%s and shared/%s: one helper, one file", n, other, file)
			}
		}
	}
	seen := map[string]bool{}
	for _, s := range srcs {
		for _, re := range []*regexp.Regexp{sharedDefFuncRe, sharedDefTopRe, sharedDefNestRe} {
			for _, m := range re.FindAllStringSubmatch(s.code, -1) {
				file, ok := home[m[1]]
				if !ok || s.path == "shared/"+file {
					continue
				}
				key := s.path + " " + m[1]
				if seen[key] {
					continue
				}
				seen[key] = true
				if _, allowed := sharedHelperCopies[key]; !allowed {
					t.Errorf("%s defines %s, which shared/%s exports: import it from /static/shared/%s", s.path, m[1], file, file)
				}
			}
		}
	}
	for key, why := range sharedHelperCopies {
		if !seen[key] {
			t.Errorf("sharedHelperCopies lists %q (%s), which is no longer there: delete the entry", key, why)
		}
	}
}

func TestSharedExportsHaveImporters(t *testing.T) {
	t.Parallel()
	srcs := sharedCensusSources(t)
	exports := sharedExports(srcs)
	imported := map[string]bool{} // "file name"
	for _, s := range srcs {
		for _, m := range sharedNamedImportRe.FindAllStringSubmatch(s.code, -1) {
			u := sharedResolve(s.url, m[2])
			if !strings.HasPrefix(u, "/static/shared/") || u == s.url {
				continue
			}
			file := strings.TrimPrefix(u, "/static/shared/")
			for _, n := range strings.Split(m[1], ",") {
				if f := strings.Fields(n); len(f) > 0 {
					imported[file+" "+f[0]] = true
				}
			}
		}
	}
	exported := map[string]bool{}
	for file, names := range exports {
		for _, n := range names {
			key := file + " " + n
			exported[key] = true
			_, allowed := sharedExportsNoImporter[key]
			switch {
			case imported[key] && allowed:
				t.Errorf("sharedExportsNoImporter lists %q, which is imported now: delete the entry", key)
			case !imported[key] && !allowed:
				t.Errorf("shared/%s exports %s and nothing imports it: drop the export, or import it where it is used", file, n)
			}
		}
	}
	for key := range sharedExportsNoImporter {
		if !exported[key] {
			t.Errorf("sharedExportsNoImporter lists %q, which no shared module exports there: delete the entry", key)
		}
	}
	if len(imported) < 30 {
		t.Fatalf("found only %d imported shared names; the import walk is broken, not the pages", len(imported))
	}
}

func TestSharedModulesHaveOneURL(t *testing.T) {
	t.Parallel()
	srcs := sharedCensusSources(t)
	exports := sharedExports(srcs)
	for _, s := range srcs {
		for _, m := range sharedAnySpecRe.FindAllStringSubmatch(s.code, -1) {
			u := sharedResolve(s.url, m[1])
			if !strings.HasPrefix(u, "/static/shared/") {
				continue
			}
			if want := u; m[1] != want {
				t.Errorf("%s imports %q: spell it %q, as every other importer does (two URLs are two module instances)", s.path, m[1], want)
			}
			if _, ok := exports[strings.TrimPrefix(u, "/static/shared/")]; !ok {
				t.Errorf("%s imports %s, which is not a shared module", s.path, u)
			}
		}
		if !strings.HasPrefix(s.path, "shared/") {
			continue
		}
		for _, m := range sharedExportListRe.FindAllStringSubmatch(s.code, -1) {
			if m[2] != "" {
				t.Errorf("%s re-exports {%s}: a page imports a helper from the file that defines it", s.path, strings.TrimSpace(m[1]))
			}
		}
	}
}
