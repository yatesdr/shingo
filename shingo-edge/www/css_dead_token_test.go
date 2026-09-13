package www

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// css_dead_token_test.go — a var() chain nobody declares.
//
// WHY THIS EXISTS. The U9 shots showed P0's "RUNNING" drawn in body text where
// the reference draws it green. The rule was there and read
// `color: var(--status-ok, var(--ok))`. Neither token has ever existed:
// shared/tokens.css declares --status-<state>-dot, --viz-*, --chip-ink-* and
// --success/--danger/--warning, and nothing called --status-ok or --ok. CSS
// treats a var() whose every branch is undeclared as an invalid value at
// computed-value time — the declaration is dropped in silence, the property
// inherits, and the page looks plausible. Nothing warns.
//
// One symptom was reported. The file carried SIXTEEN of them, and
// flow-picture.css carried three more whose fallback (--status-alarm) is dead
// on the desktop while the station's --os-red covers it — so a position with a
// finding drew no red outline on the Processes page and did on the HMI. That
// is the shape of this bug: a hand-written fallback to a token somebody
// assumed existed.
//
// WHAT IS CHECKED, AND WHAT IS NOT. Only a chain in which EVERY branch is
// undeclared anywhere in the CSS this repo ships. A `var(--os-r1,
// var(--robot-1))` is fine — that is the per-surface pattern flow-picture.css
// is built on, and one of the two always resolves. A chain with a literal
// fallback is fine: the literal resolves. This is a test about names nobody
// declared, not about which surface loads which file.

var (
	cssDeclRe = regexp.MustCompile(`(--[A-Za-z0-9_-]+)\s*:`)
	cssVarRe  = regexp.MustCompile(`var\(\s*(--[A-Za-z0-9_-]+)\s*(,)?`)
	// setProperty('--x', …) — a token declared at runtime rather than in a rule.
	cssSetPropRe = regexp.MustCompile(`setProperty\(\s*.(--[A-Za-z0-9_-]+)`)
)

// cssRoots are the stylesheets this repo ships, plus the templates, which
// declare tokens inline in a <style> block here and there.
var cssRoots = []string{
	filepath.Join("..", "..", "shared"),
	filepath.Join("..", "..", "shingo-edge", "www", "static"),
	filepath.Join("..", "..", "shingo-edge", "www", "templates"),
	filepath.Join("..", "..", "shingo-core", "www", "static"),
	filepath.Join("..", "..", "shingo-core", "www", "templates"),
}

func cssFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, root := range cssRoots {
		err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			if ext != ".css" && ext != ".html" && ext != ".js" {
				return nil
			}
			if strings.Contains(filepath.ToSlash(p), "/vendor/") || strings.HasSuffix(p, ".min.css") ||
				strings.HasSuffix(p, ".min.js") || strings.HasSuffix(p, ".test.js") {
				return nil
			}
			out = append(out, p)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(out) == 0 {
		t.Fatal("found no stylesheets at all — the walk has drifted and this test is a no-op")
	}
	return out
}

// chainsOf splits one declaration into its var() chains. `var(--a, var(--b))`
// is one chain [--a --b]; two unrelated var() on one line are two chains.
//
// A chain is REPORTABLE only when it can fail to resolve, which means its
// innermost var() has no fallback at all. `var(--a, 8px)` and
// `var(--a, var(--b, #fff))` always resolve, whatever is declared.
func chainsOf(decl string) [][]string {
	var out [][]string
	idx := cssVarRe.FindAllStringSubmatchIndex(decl, -1)
	consumed := map[int]bool{}
	for i, m := range idx {
		if consumed[i] {
			continue
		}
		chain := []string{decl[m[2]:m[3]]}
		last := m
		end := m[1]
		// Follow the fallback while it is itself a var(): the next match must
		// start immediately, with only whitespace between.
		for j := i + 1; j < len(idx); j++ {
			if last[4] == -1 || strings.TrimSpace(decl[end:idx[j][0]]) != "" {
				break
			}
			consumed[j] = true
			chain = append(chain, decl[idx[j][2]:idx[j][3]])
			last = idx[j]
			end = idx[j][1]
		}
		// The innermost var() carries a comma, so its fallback is a literal
		// (anything that was another var() was consumed into the chain above).
		// A literal always resolves.
		if last[4] != -1 {
			continue
		}
		out = append(out, chain)
	}
	return out
}

// knownDead is the debt this branch did not create and is not fixing: dead
// var() chains that predate U9, listed by name so the guard stays live over
// everything else. Each is a rename nobody finished. Removing a name from this
// list is how the next person is told to fix it.
var knownDead = map[string]string{
	"--warn-border":   "shingo-core/www/static/style.css — the warn/danger trio was never declared",
	"--warn-text":     "shingo-core/www/static/style.css",
	"--warn-bg":       "shingo-core/www/static/style.css",
	"--danger-border": "shingo-core/www/static/style.css",
	"--danger-text":   "shingo-core/www/static/style.css",
	"--danger-bg":     "shingo-core/www/static/style.css",
	"--mono":          "shingo-core/www/static/style.css — font stack, never declared",
	"--bg-card":       "shingo-edge/www/templates/manual-message.html — inline <style>",
	"--border-color":  "shingo-edge/www/templates/manual-message.html — inline <style>",
}

// TestSharedRendererNamesBothSurfacesTokens is the per-surface half of the
// same failure, which the repo-wide scan above CANNOT see.
//
// --os-* IS declared — in operator.css — so a chain that names only an --os-*
// token passes the "declared somewhere" test and is still dead on the DESKTOP,
// which loads shared/tokens.css and never operator.css. flow-picture.css knows
// this and writes `var(--os-x, var(--shared-x))` in every rule, with a header
// paragraph saying why. The INLINE styles operator-flow.js writes are the same
// drawing on the same two surfaces and did not: eight of them named only the
// station's token, so on the Processes page the in/out corner glyphs and both
// dock notes lost the teal and indigo that say which robot.
//
// The rule for any file BOTH surfaces load: never a bare --os-* token.
func TestSharedRendererNamesBothSurfacesTokens(t *testing.T) {
	// Files the station and the desktop both load.
	shared := []string{
		filepath.Join("static", "operator-station", "operator-flow.js"),
		filepath.Join("static", "operator-station", "flow-picture.css"),
	}
	bare := regexp.MustCompile(`var\(\s*--os-[A-Za-z0-9_-]+\s*\)`)
	for _, f := range shared {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") || strings.HasPrefix(strings.TrimSpace(line), "*") {
				continue
			}
			for _, hit := range bare.FindAllString(line, -1) {
				t.Errorf("%s:%d  %s names only the station's token. Both surfaces draw this, and the desktop "+
					"never loads operator.css — write var(--os-x, var(--shared-x)).", f, i+1, hit)
			}
		}
	}
}

// TestBothSurfacesDeclareTheGlyphFilesTokens — a LITERAL fallback passes every
// test above and is still wrong on one of the two surfaces.
//
// composer-glyphs.css ships verbatim from the locked glyph set
// (GLYPHS-swap-mode-LOCKED-2026-09-10) and reads --os-station, --os-robot-1/-2
// and --os-sub-3/-4/-5, each with a literal fallback. That is deliberate: the
// file is the one drawing of the four choreographies and is not edited per
// surface, so the surfaces declare its names instead. operator.css says so in
// its own comment — "leaving the glyphs on their built-in literal fallbacks
// would mean the tokens did not actually drive them".
//
// processes-desktop.css declared none of them, and the page loads the glyph
// file. So on the desktop the fallbacks drove it, and they are the DARK
// values: every swap-mode glyph in D1's positions table and on the preset rows
// was #f2f5f8 — near-white on a white row in the light theme, invisible, and
// invisible to the shots too until D1 was taken in light rather than in
// headless Chrome's default.
//
// The rule: a file both surfaces load may name --os-* tokens, and then BOTH
// surfaces have to declare them. A literal fallback is not a declaration; it is
// one theme's answer to a question the other theme also asks.
func TestBothSurfacesDeclareTheGlyphFilesTokens(t *testing.T) {
	glyphs := filepath.Join("static", "operator-station", "composer-glyphs.css")
	body, err := os.ReadFile(glyphs)
	if err != nil {
		t.Fatalf("read %s: %v", glyphs, err)
	}
	wanted := map[string]bool{}
	for _, m := range cssVarRe.FindAllStringSubmatch(string(body), -1) {
		if strings.HasPrefix(m[1], "--os-") {
			wanted[m[1]] = true
		}
	}
	if len(wanted) == 0 {
		t.Fatalf("%s names no --os-* token — this test would pass vacuously", glyphs)
	}
	for _, surface := range []string{
		filepath.Join("static", "operator-station", "operator.css"),
		filepath.Join("static", "css", "processes-desktop.css"),
	} {
		sheet, err := os.ReadFile(surface)
		if err != nil {
			t.Fatalf("read %s: %v", surface, err)
		}
		declared := map[string]bool{}
		for _, m := range cssDeclRe.FindAllStringSubmatch(string(sheet), -1) {
			declared[m[1]] = true
		}
		missing := make([]string, 0, len(wanted))
		for tok := range wanted {
			if !declared[tok] {
				missing = append(missing, tok)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s loads composer-glyphs.css and declares none of %v.\n"+
				"  The glyph file's literal fallbacks are the DARK values, so this surface draws the "+
				"glyphs in a colour chosen for the other one.", surface, missing)
		}
	}
}

func TestNoCSSVarChainIsEntirelyUndeclared(t *testing.T) {
	files := cssFiles(t)
	declared := map[string]bool{}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range cssDeclRe.FindAllStringSubmatch(string(body), -1) {
			declared[m[1]] = true
		}
		// A property the page sets at runtime is declared too: the board grid
		// and the two scroll caps are style.setProperty() calls, and a static
		// scan that does not know that reads them as dead.
		for _, m := range cssSetPropRe.FindAllStringSubmatch(string(body), -1) {
			declared[m[1]] = true
		}
	}
	if len(declared) < 50 {
		t.Fatalf("only %d custom properties found across %d files — the declaration scanner has drifted and this test is a no-op", len(declared), len(files))
	}

	var dead []string
	for _, f := range files {
		if strings.EqualFold(filepath.Ext(f), ".js") {
			continue // JS is read for setProperty only; a var() inside it is a string
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, "var(") {
				continue
			}
			for _, chain := range chainsOf(line) {
				live := false
				for _, name := range chain {
					if declared[name] {
						live = true
						break
					}
				}
				if live {
					continue
				}
				if _, ok := knownDead[chain[len(chain)-1]]; ok {
					continue
				}
				dead = append(dead, fmt.Sprintf("%s:%d  var(%s) — no branch is declared anywhere", f, i+1, strings.Join(chain, ", ")))
			}
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("%d var() chain(s) resolve to nothing. CSS drops the whole declaration in silence, so the page looks plausible and the rule does not apply.\n%s",
			len(dead), strings.Join(dead, "\n"))
	}
}
