package www

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"shingo/shared"
)

// The U5 collision, generalised.
//
// `.chip` was not a contrast bug. shared/components.css and Core's style.css
// both declared a bare `.chip`, both at (0,1,0), and layout.html loads
// style.css AFTER components.css — so the picker's navy fill silently won on
// every health chip in the app, and `.chip-err`'s near-black ink landed on that
// navy at 1.2:1. Nobody wrote a bug; two files that never mention each other
// agreed on a name.
//
// d01156b1 fixed the pixel by scoping one side to `.chip-container .chip`. That
// is a specificity fix for a naming problem: the names still collide, and the
// next bare `.chip` rule in a sheet loading after components.css re-breaks it.
// This test is what makes the `.chip` -> `.tag` rename durable — and on its
// first run it found six more instances of the same shape, two of them live
// overrides of a documented rule. See knownCSSCollisions.
//
// WHY CO-LOADED SETS AND NOT ALL PAIRS. dashboard.css and style.css both
// declared `.board-table`, but the only page using `.board-table` loads
// dashboard.css and not style.css. A test over all file pairs reports
// collisions no browser can ever see, and a test that reports unreachable
// problems gets an allowlist and then gets ignored. The sets come from the
// templates' own <link> tags, so a new page with a new sheet combination is
// covered without anyone remembering to add it here.
//
// INLINE <style> COUNTS, AND IT IS WHERE THE WORST ONES WERE. Every Phase 6
// page carries page-local CSS in an inline <style>, and a content template
// renders inside layout.html — so its inline block sits in the same document as
// layout's four linked sheets, LATER in the document, which means at equal
// specificity the inline block wins. demand.html was overriding bare `.col-num`
// and bare `.btn-danger` that way. Excluding inline blocks would have left this
// test green over the two live overrides it most needed to find.
//
// KNOWN HOLE: a stylesheet built by JS at runtime. sim-speed-strip.js creates a
// <style> element and is injected by layout.html, so its rules land in the same
// document and are invisible to this scan. Not hypothetical — it is where the
// `.chip` -> `.tag` rename nearly landed a second collision, because that
// script already styles a `.tag` (as `#sim-strip .tag`, so only the properties
// it does not itself set would have leaked in). Renamed to `.sim-tag` by hand.
// Prefer a real stylesheet over an injected one so this test can see it.

// knownCSSCollisions is every bare-class collision that survives, with the
// measured divergence and what resolving it would cost. It is a PIN, not a
// suppression: an entry that stops colliding fails this test too, so a pin
// cannot outlive the finding it records.
//
// Each is the `.chip` shape — same bare name, two layers, the later one wins —
// and each is live. None is free to resolve, which is why they are recorded
// with their cost rather than quietly changed: picking a winner moves pixels on
// a shipped page, and that is the owner's call, not a drift test's.
var knownCSSCollisions = map[string]string{
	"mb-2": "components.css 0.5rem vs style.css 1rem. 73 use sites in Core markup, 4 in Edge. " +
		"Core's value wins on Core, so the SAME class means a different amount of space on the two surfaces. " +
		"Resolving it moves 73 margins on Core admin or 4 on Edge — a visual change, not a cleanup.",
	"mt-2": "components.css 0.5rem vs style.css 1rem. 12 use sites in Core, 6 in Edge. Same divergence as mb-2. " +
		"The two files are running two different spacing scales: shared 0.5/0.75/1rem at 2/3/4, Core 0.5/1/1.5rem at 1/2/3.",
	"badge-sm": "status-classes.css (0.7rem / 0.1em 0.5em) vs style.css (.75em / 1px 5px). 7 use sites in Core, 0 in Edge. " +
		"Core's wins, so shared's declaration is unreachable in practice — but shared's is the documented primitive, so " +
		"the resolution is to delete Core's override and accept 7 badges changing size.",
	"col-num": "style.css `text-align: right` vs demand.html's inline <style> `text-align: center`. The inline block is " +
		"later in the document at equal specificity, so CENTRE wins on /demand — contradicting the number doctrine " +
		"(docs/ui-style-guide.md, § 'Tabular figures, always': tabular-nums aligns the glyphs, only right-alignment " +
		"aligns the MAGNITUDES) that style.css's rule exists to enforce. style.css already DOCUMENTED this override as " +
		"a tolerated fact rather than a defect, which is the reason to have a test instead of a comment. Left alone " +
		"here because it is a visual decision, not a naming one: the three numeric columns move centre -> right on a " +
		"shipped editable table whose cell-edit overlay is absolutely positioned over them. Right-aligning them is the " +
		"guide-correct answer and it is the owner's call.",
}

// sheet is one stylesheet in a page's cascade: a linked file or an inline
// <style> block, carrying its own source so nothing needs re-resolving later.
type sheet struct {
	name string
	src  string
}

func TestNoBareClassDeclaredInTwoCoLoadedStylesheets(t *testing.T) {
	pages := coLoadedStylesheetSets(t)
	if len(pages) == 0 {
		t.Fatal("no page was found with two or more stylesheets — the <link>/<style> scan has drifted from the templates, which makes this test a no-op")
	}

	classes := map[string][]string{}
	bareOf := func(s sheet) []string {
		if c, ok := classes[s.name]; ok {
			return c
		}
		classes[s.name] = shared.CSSBareClassSelectors(s.src)
		return classes[s.name]
	}

	// class -> the distinct "A <-> B on page P" facts about it, so a class
	// colliding on four pages reports once with all four.
	found := map[string]map[string]bool{}
	for page, sheets := range pages {
		for i := 0; i < len(sheets); i++ {
			for j := i + 1; j < len(sheets); j++ {
				for _, c := range intersect(bareOf(sheets[i]), bareOf(sheets[j])) {
					if found[c] == nil {
						found[c] = map[string]bool{}
					}
					found[c][fmt.Sprintf("%s <-> %s (both in %s)", sheets[i].name, sheets[j].name, page)] = true
				}
			}
		}
	}

	for _, class := range sortedCollisionKeys(found) {
		if _, pinned := knownCSSCollisions[class]; pinned {
			continue
		}
		t.Errorf("bare .%s is declared twice in one page's cascade:\n    %s\n"+
			"  This is the U5 `.chip` shape: the later declaration wins silently and neither side knows the other exists.\n"+
			"  Resolve it by giving one of them its own name, or by scoping it to a container — not by nudging a value until it looks right.\n"+
			"  If it genuinely cannot be resolved, pin it in knownCSSCollisions with the measured divergence and the cost.",
			class, strings.Join(sortedCollisionKeys(found[class]), "\n    "))
	}

	for _, class := range sortedCollisionKeys(knownCSSCollisions) {
		if len(found[class]) == 0 {
			t.Errorf("pinned collision .%s no longer collides — delete its entry from knownCSSCollisions.\n"+
				"  A pin that outlives its finding is a suppression, and the next real collision hides behind it.\n  Recorded note was: %s",
				class, knownCSSCollisions[class])
		}
	}
}

var (
	stylesheetLinkPattern  = regexp.MustCompile(`(?s)<link\s[^>]*rel="stylesheet"[^>]*>`)
	hrefPattern            = regexp.MustCompile(`href="([^"]+)"`)
	inlineStylePattern     = regexp.MustCompile(`(?s)<style[^>]*>(.*?)</style>`)
	contentTemplatePattern = regexp.MustCompile(`\{\{\s*define\s+"content"\s*\}\}`)
	contentBlockPattern    = regexp.MustCompile(`\{\{\s*block\s+"content"`)
)

// coLoadedStylesheetSets maps each rendered page to the stylesheets that end up
// in its cascade. Two shapes exist on this surface:
//
//   - a standalone template (layout.html, the kiosk pages) — its own <link>
//     tags plus its own inline <style>.
//   - a content template — it has no <link> tags of its own because it renders
//     inside the layout that declares `{{block "content"}}`. Its cascade is the
//     layout's sheets plus its own inline <style>, and the inline block comes
//     later in the document. Missing this is how demand.html's two overrides
//     stayed invisible.
//
// Pages ending up with fewer than two sheets are dropped: one sheet cannot
// collide with itself.
func coLoadedStylesheetSets(t *testing.T) map[string][]sheet {
	t.Helper()

	bodies := map[string]string{}
	err := fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(p) != ".html" {
			return nil
		}
		b, err := fs.ReadFile(templateFS, p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			return nil
		}
		bodies[p] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}

	// The layout(s) that host content templates, and their linked sheets.
	hosts := map[string][]sheet{}
	for p, src := range bodies {
		if contentBlockPattern.MatchString(src) {
			hosts[p] = linkedSheets(t, src)
		}
	}
	if len(hosts) == 0 {
		t.Fatal(`no template declares {{block "content"}} — content templates' inline <style> blocks would be compared against nothing`)
	}

	out := map[string][]sheet{}
	for p, src := range bodies {
		inline := inlineSheet(p, src)

		if contentTemplatePattern.MatchString(src) {
			if inline == nil {
				continue // no page-local CSS: nothing of its own to collide
			}
			for host, hostSheets := range hosts {
				page := fmt.Sprintf("%s inside %s", filepath.Base(p), filepath.Base(host))
				out[page] = append(append([]sheet{}, hostSheets...), *inline)
			}
			continue
		}

		sheets := linkedSheets(t, src)
		if inline != nil {
			sheets = append(sheets, *inline)
		}
		if len(sheets) >= 2 {
			out[filepath.Base(p)] = sheets
		}
	}
	return out
}

// inlineSheet concatenates every <style> block in a template into one
// pseudo-sheet, or returns nil when the template has none. They are joined
// rather than compared with each other: two blocks in one template are one
// author's cascade, and a name reused inside a single file is not the
// two-files-that-never-met failure this test is about.
func inlineSheet(path, src string) *sheet {
	var b strings.Builder
	for _, m := range inlineStylePattern.FindAllStringSubmatch(src, -1) {
		b.WriteString(m[1])
		b.WriteString("\n")
	}
	if strings.TrimSpace(b.String()) == "" {
		return nil
	}
	return &sheet{name: filepath.Base(path) + " <style>", src: b.String()}
}

// linkedSheets reads every <link rel="stylesheet"> in src back to the embedded
// file behind it. An href this cannot resolve is a failure, not a skip: an
// unresolvable stylesheet is silently excluded from every comparison, which is
// how a scan like this ends up measuring nothing.
func linkedSheets(t *testing.T, src string) []sheet {
	t.Helper()
	var out []sheet
	for _, tag := range stylesheetLinkPattern.FindAllString(src, -1) {
		m := hrefPattern.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		href := strings.SplitN(m[1], "?", 2)[0]
		if !strings.HasSuffix(href, ".css") {
			continue
		}
		switch {
		case strings.HasPrefix(href, "/static/shared/"):
			b, err := fs.ReadFile(shared.Files, strings.TrimPrefix(href, "/static/shared/"))
			if err != nil {
				t.Fatalf("read shared stylesheet %s: %v", href, err)
			}
			out = append(out, sheet{name: filepath.Base(href), src: string(b)})
		case strings.HasPrefix(href, "/static/"):
			b, err := fs.ReadFile(staticFS, "static/"+strings.TrimPrefix(href, "/static/"))
			if err != nil {
				t.Fatalf("read Core stylesheet %s: %v", href, err)
			}
			out = append(out, sheet{name: filepath.Base(href), src: string(b)})
		default:
			t.Fatalf("stylesheet href %s is served from neither shared/ nor Core's static/ — extend the resolver rather than leaving it uncompared", href)
		}
	}
	return out
}

func intersect(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range a {
		in[s] = true
	}
	var out []string
	for _, s := range b {
		if in[s] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func sortedCollisionKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
