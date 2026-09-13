package www

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// classic_script_globals_test.go — two classic scripts on one page share one
// global scope, and the second one to declare a name dies.
//
// THE BUG THIS IS WRITTEN FROM (found 2026-09-13, present since U9d).
// processes.html loads composer-model.js and desktop-bodies.js as classic
// scripts, and both ended with `const api = {...}`. Top-level const in a
// classic script creates a binding in the page's ONE global lexical
// environment, so the second script threw `Identifier 'api' has already been
// declared` on evaluation — before its last line, which is the line that sets
// window.DesktopBodies.
//
// Nothing said so. The tag was in the DOM, the file was 200 with the right
// bytes, and the page rendered. The only trace was the first call into the
// global the dead script was meant to set: every write button on the Processes
// page — settings save, style rename, clone, active-style, operator-screen
// edit, routing add and enable, preset create and apply — threw
// `Cannot read properties of undefined`. The shots harness photographed the
// page happily for three rounds, because a screenshot never clicks.
//
// WHY A COLUMN-0 SCAN AND NOT A PARSER. Every declaration this can catch is
// one a human wrote at the top level of a file this repo owns, and this repo
// writes those at column 0 and indents everything else. That makes the scan
// exact for the thing it is looking for and free of the string-, comment- and
// regex-literal traps a hand-rolled tokenizer walks into. A minified vendor
// file yields nothing from it, which is the right answer: htmx ships as an
// IIFE and declares nothing at the top level.
//
// FUNCTIONS ARE REPORTED TOO, and they are the worse case: a redeclared
// top-level `function` does not throw, it silently replaces the first one, so
// the page runs the wrong body with no error anywhere.

var (
	classicTag   = regexp.MustCompile(`<script\s+src="(/static/[^"?]+)`)
	moduleTag    = regexp.MustCompile(`<script\s+type="module"`)
	templateCall = regexp.MustCompile(`{{template "(\w+)" \.}}`)
	topLevelDecl = regexp.MustCompile(`(?m)^(?:const|let|class|function)\s+([A-Za-z_$][\w$]*)`)
)

// scriptsOn returns the classic /static scripts a template puts on a page, in
// load order, following one level of {{template "x" .}} so header.html's tags
// count as being on every page that draws it.
func scriptsOn(t *testing.T, name string, depth int) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("templates", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if m := templateCall.FindStringSubmatch(line); m != nil && depth > 0 {
			if _, err := os.Stat(filepath.Join("templates", m[1]+".html")); err == nil {
				out = append(out, scriptsOn(t, m[1]+".html", depth-1)...)
			}
			continue
		}
		if moduleTag.MatchString(line) {
			continue
		}
		if m := classicTag.FindStringSubmatch(line); m != nil {
			out = append(out, strings.TrimPrefix(m[1], "/static/"))
		}
	}
	return out
}

func TestClassicScriptsDoNotCollideOnAPageGlobal(t *testing.T) {
	t.Parallel()
	pages, err := filepath.Glob(filepath.Join("templates", "*.html"))
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	// What each file declares at its top level, read once.
	declares := map[string][]string{}
	declaredBy := func(rel string) []string {
		if got, ok := declares[rel]; ok {
			return got
		}
		raw, err := os.ReadFile(filepath.Join("static", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("a template names /static/%s and the tree has no such file: %v", rel, err)
		}
		var names []string
		for _, m := range topLevelDecl.FindAllStringSubmatch(string(raw), -1) {
			names = append(names, m[1])
		}
		declares[rel] = names
		return names
	}

	checked := 0
	for _, page := range pages {
		name := filepath.Base(page)
		if name == "header.html" || name == "footer.html" {
			continue // counted through the pages that draw them
		}
		scripts := scriptsOn(t, name, 2)
		if len(scripts) < 2 {
			continue
		}
		checked++
		owner := map[string]string{}
		for _, rel := range scripts {
			for _, decl := range declaredBy(rel) {
				if first, clash := owner[decl]; clash && first != rel {
					t.Errorf("%s loads %s and %s, and both declare %q at the top level.\n"+
						"  Classic scripts share ONE global lexical scope: the second file throws on\n"+
						"  evaluation (const/let/class) or silently replaces the first (function), and\n"+
						"  everything after that line in it never runs. Put the declaration inside a\n"+
						"  function scope, or load the file as a module.",
						name, first, rel, decl)
					continue
				}
				owner[decl] = rel
			}
		}
		names := make([]string, 0, len(owner))
		for n := range owner {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Logf("%s: %d classic scripts, %d top-level names, no collision", name, len(scripts), len(names))
	}
	if checked == 0 {
		t.Fatal("no page was found loading two classic scripts — the scan found nothing to check, " +
			"which means the tag pattern has drifted, not that the risk is gone")
	}
}
