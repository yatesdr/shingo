// Package clockglobals audits page templates for the globals shared/utils.js
// needs before it can render a time.
//
// THE RULE BELONGS TO shared BECAUSE THE CONTRACT DOES. utils.js is shipped
// from this module and read by both Core and Edge, and it is utils.js that
// says "if you import formatClock or serverNow, your page must inline
// window.PLANT_TZ and window.SHINGO_CLOCK". Auditing that in each tree
// separately would mean two copies of one rule, and the next person to relax
// one of them would not know the other existed — which is the same shape as
// the bug the rule exists to catch.
//
// WHAT IT CATCHES IS AN OMISSION, NOT AN ERROR. Those globals used to be
// inlined only in each tree's shared page chrome (Edge's header.html, Core's
// layout.html), so every page using that chrome got them without anyone
// deciding to. Five full pages do not use it — Edge's operator-display.html,
// a touch kiosk with its own header, and Core's dashboard-display,
// dashboard-map, dashboard-node-report and heartbeat. utils.js degrades rather
// than throwing when the globals are missing: plantTZ() returns undefined and
// formatting falls back to the VIEWER's timezone, serverNow() falls back to
// Date.now(). So those pages rendered every timestamp on the viewer's clock
// and computed every age against it, looking completely normal. At
// Hopkinsville, whose plant clock is Central, that is a silent one-hour error
// on the operator boards and the big display board.
//
// Nothing failed, which is why it survived. Hence a test.
package clockglobals

import (
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"shingo/shared"
)

// ClockFuncs are the utils.js exports that silently fall back when the globals
// are absent. Importing any of them makes a page subject to the rule.
var ClockFuncs = []string{
	"formatClock", "serverNow", "formatTime", "timeAgo", "formatDuration", "syncServerClock",
}

var (
	// anyFromRe follows the module graph: every `from '...'`, named or not.
	anyFromRe = regexp.MustCompile(`from\s+['"]([^'"]+)['"]`)
	// namedImportRe reads what a module actually TAKES, which is the part that
	// decides whether the clock is in play. Matching only the `from '...'`
	// tail cannot see the names, so a detector built on it matches nothing and
	// every page passes — the failure the subject-count assertion in each
	// tree's test exists to catch, and did.
	namedImportRe = regexp.MustCompile(`import\s*\{([^}]*)\}\s*from\s*['"]([^'"]+)['"]`)
	srcRe         = regexp.MustCompile(`<script[^>]*\ssrc="([^"]+)"`)
)

// Result is one page that needs the globals and does not carry them.
type Result struct {
	Page   string
	Script string
}

// resolver maps a served URL path to the file behind it.
type resolver func(url string) (string, bool)

// newResolver splits /static/shared/* from /static/* the same way each tree's
// router serves them: the former comes from this module's embed, the latter
// from the caller's.
func newResolver(static fs.FS) resolver {
	return func(url string) (string, bool) {
		clean := strings.SplitN(url, "?", 2)[0]
		switch {
		case strings.HasPrefix(clean, "/static/shared/"):
			b, err := fs.ReadFile(shared.Files, strings.TrimPrefix(clean, "/static/shared/"))
			return string(b), err == nil
		case strings.HasPrefix(clean, "/static/"):
			b, err := fs.ReadFile(static, strings.TrimPrefix(clean, "/static/"))
			return string(b), err == nil
		}
		return "", false
	}
}

// resolveSpec turns an import specifier into a served path, relative to the
// file that imported it.
func resolveSpec(fromURL, spec string) string {
	if strings.HasPrefix(spec, "/") {
		return spec
	}
	dir := fromURL[:strings.LastIndex(fromURL, "/")+1]
	return dir + strings.TrimPrefix(spec, "./")
}

// takesClockFunc reports whether this source imports a clock function OUT of
// shared/utils.js. Reading the import list is the whole point: matching only
// the `from '...'` tail cannot see the names, so a detector built on it matches
// nothing and every page trivially passes.
func takesClockFunc(src, fromURL string) bool {
	for _, m := range namedImportRe.FindAllStringSubmatch(src, -1) {
		if !strings.HasSuffix(resolveSpec(fromURL, m[2]), "/shared/utils.js") {
			continue
		}
		names := strings.FieldsFunc(m[1], func(r rune) bool {
			return r == ',' || unicode.IsSpace(r)
		})
		for _, name := range names {
			if slices.Contains(ClockFuncs, name) {
				return true
			}
		}
	}
	return false
}

// reachesClock walks a script and everything it imports. One level is not
// enough: Edge's operator.js reaches formatClock only through
// operator-render.js.
func reachesClock(res resolver, url string, seen map[string]bool) bool {
	clean := strings.SplitN(url, "?", 2)[0]
	if seen[clean] {
		return false
	}
	seen[clean] = true
	src, ok := res(clean)
	if !ok {
		return false
	}
	if takesClockFunc(src, clean) {
		return true
	}
	for _, m := range anyFromRe.FindAllStringSubmatch(src, -1) {
		if reachesClock(res, resolveSpec(clean, m[1]), seen) {
			return true
		}
	}
	return false
}

// carries reports whether a page body spells one of the accepted inclusions.
func carries(body string, markers []string) bool {
	for _, c := range markers {
		if strings.Contains(body, c) {
			return true
		}
	}
	return false
}

// Audit walks every full page in templates, follows the scripts it loads
// (including their transitive imports), and reports the pages that reach a
// clock function without carrying the globals.
//
// carriers are the substrings that count as carrying them — each tree spells
// its own inclusion differently, so the marker is the caller's to name.
// Returns the findings and the number of pages that were subject to the rule,
// which the caller should assert is non-zero: a resolver that quietly matches
// nothing would let every page pass.
func Audit(templates, static fs.FS, carriers []string) ([]Result, int, error) {
	res := newResolver(static)
	pages, err := fs.Glob(templates, "templates/*.html")
	if err != nil {
		return nil, 0, fmt.Errorf("glob templates: %w", err)
	}

	var out []Result
	var subject int
	for _, page := range pages {
		raw, err := fs.ReadFile(templates, page)
		if err != nil {
			return nil, 0, fmt.Errorf("read %s: %w", page, err)
		}
		body := string(raw)

		// Only a FULL page owns a <head>. A fragment rendered inside one
		// inherits whatever that page carries.
		if !strings.Contains(body, "<html") {
			continue
		}

		var hit string
		for _, m := range srcRe.FindAllStringSubmatch(body, -1) {
			if reachesClock(res, m[1], map[string]bool{}) {
				hit = m[1]
				break
			}
		}
		if hit == "" {
			continue
		}
		subject++
		if !carries(body, carriers) {
			out = append(out, Result{Page: page, Script: hit})
		}
	}
	return out, subject, nil
}

// Explain is the failure message both trees print, so the reason the rule
// exists travels with the failure rather than living only here.
func Explain(r Result) string {
	return fmt.Sprintf(
		"%s loads %s, which reads the plant clock, but the page carries no clock globals.\n"+
			"  Add {{template \"clock-globals\" .}} to its <head>.\n"+
			"  Without it shared/utils.js does not fail — it falls back to the VIEWER's timezone\n"+
			"  and the VIEWER's clock, so every timestamp on this page renders plausibly and\n"+
			"  wrongly. That is exactly how five pages shipped unnoticed.",
		r.Page, r.Script)
}
